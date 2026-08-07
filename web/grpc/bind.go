package grpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The pb ↔ DTO bridge. The constructors receive the SAME Request/Response
// DTOs the REST surface consumes; the framework crosses the proto boundary
// mechanically, so every semantic transformation stays in the DTO seats
// (ToCommand / ToQuery / FromResult) — the design record lives in
// tasks/grpc.md ("Auto bindings redesign").
//
// Mechanics: a message plan is COMPILED AT REGISTER TIME by matching the
// message descriptor against the DTO struct — case- and
// underscore-insensitive name matching (`user_name` ≈ `userName` ≈
// `UserName` ≈ `ID`), json/query tags honored, recursion into nested
// messages and repeated fields, google.protobuf.Timestamp ↔ time.Time. A
// proto field with no DTO counterpart ABORTS BOOT (panic at Register): the
// wire contract must never carry a field the service silently ignores or
// never fills. Alias("wire_field", "DTOField") declares the exceptional
// pairing.
//
// At runtime the plan drives a protojson ↔ encoding/json bridge: protojson
// emits the camelCase JSON form (presence-aware — an absent `optional`
// field is omitted and lands as a nil pointer, exactly the REST
// absent-vs-set distinction), keys are renamed per plan only where the DTO
// key differs, the two dialects' value disagreements are settled (protojson
// quotes 64-bit integers, and renders non-finite floats as strings — see
// compileFieldPlan), and encoding/json binds the DTO — the same codec
// semantics the REST body bind uses, so both wires produce identical DTO
// values by construction.

// bindPlan is one message's compiled bridge: the per-key rewrites applied to
// the intermediate JSON plus the child plans for nested messages.
type bindPlan struct {
	// nodes maps the protojson key → the rewrite that key needs on the way
	// in (pb→DTO); the rename half applies inverted on the way out. nil when
	// the two dialects already agree on every key AND every value.
	nodes map[string]bindNode
	// wireRenames is true when this plan, or one below it, renames a key.
	// The DTO→pb direction only ever renames — protojson ACCEPTS a bare
	// number for a 64-bit integer — so a plan that merely unquotes skips the
	// outbound rewrite entirely.
	wireRenames bool
}

type bindNode struct {
	dtoKey string    // "" = key unchanged
	coerce coercion  // the value-level disagreement this field carries, if any
	child  *bindPlan // non-nil for nested message fields needing a rewrite
}

// coercion names the two places protojson's JSON and encoding/json's JSON
// disagree on a VALUE (they agree on every key by construction).
type coercion uint8

const (
	coerceNone coercion = iota
	// coerceUnquoteInt — the proto3 JSON mapping quotes 64-bit integers while
	// encoding/json wants a bare number for a numeric Go field. Reconcilable:
	// the quotes come off on the way in.
	coerceUnquoteInt
	// coerceGuardFloat — protojson renders non-finite floats as the strings
	// "NaN", "Infinity" and "-Infinity". NOT reconcilable: JSON has no literal
	// for them, so the value is rejected with a message naming the field.
	coerceGuardFloat
)

func (p *bindPlan) hasNodes() bool     { return p != nil && len(p.nodes) > 0 }
func (p *bindPlan) rewritesWire() bool { return p != nil && p.wireRenames }

// normalizeName folds a wire or Go spelling to its match key: lowercase,
// underscores dropped — `user_name`, `userName` and `UserName` all become
// `username`; `id` and `ID` become `id`.
func normalizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '_' {
			continue
		}
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// dtoField is one bindable field discovered on a DTO struct.
type dtoField struct {
	name    string // Go field name
	jsonKey string // the key encoding/json uses (json tag, else field name)
	typ     reflect.Type
}

// dtoFieldsOf lists the bindable fields of a DTO struct type, keyed by
// normalized name — every spelling a proto field may match: the json tag,
// the query tag (read-side DTOs carry query tags, not json) and the Go
// field name. Anonymous embedded structs are promoted, mirroring
// encoding/json — INCLUDING its collision rules: the shallowest declaration
// wins, and a tie between two equally deep promotions is ambiguous, so
// encoding/json fills NEITHER. An ambiguous name is therefore dropped here
// too; the proto field that wanted it then fails compileBindPlan's
// "no counterpart" check, which is the honest outcome — a boot abort instead
// of a field the codec silently leaves at its zero value.
func dtoFieldsOf(t reflect.Type) map[string]dtoField {
	acc := map[string]*collectedField{}
	collectDTOFields(t, 0, acc)
	out := make(map[string]dtoField, len(acc))
	for key, c := range acc {
		if !c.ambiguous {
			out[key] = c.df
		}
	}
	return out
}

// collectedField is one candidate for a normalized name: the field, how deep
// the promotion was, which struct declared it (so a field registering several
// spellings never collides with itself), and whether an equally deep sibling
// made the name ambiguous.
type collectedField struct {
	df        dtoField
	depth     int
	owner     reflect.Type
	ambiguous bool
}

func collectDTOFields(t reflect.Type, depth int, acc map[string]*collectedField) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	// This level's OWN fields first, embedded ones after: on a name collision
	// encoding/json binds the SHALLOWER field, so registering the promoted one
	// first would pair the plan with a field the JSON codec never fills.
	var embedded []reflect.Type
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				embedded = append(embedded, ft)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		jsonKey := f.Name
		if tag := f.Tag.Get("json"); tag != "" {
			name := strings.Split(tag, ",")[0]
			if name == "-" {
				continue
			}
			if name != "" {
				jsonKey = name
			}
		}
		df := dtoField{name: f.Name, jsonKey: jsonKey, typ: f.Type}
		register := func(spelling string) {
			if spelling == "" {
				return
			}
			key := normalizeName(spelling)
			prev, seen := acc[key]
			switch {
			case !seen:
				acc[key] = &collectedField{df: df, depth: depth, owner: t}
			case depth < prev.depth:
				// A shallower declaration wins outright and clears any
				// ambiguity recorded deeper down — exactly encoding/json.
				acc[key] = &collectedField{df: df, depth: depth, owner: t}
			case depth == prev.depth && (prev.owner != t || prev.df.name != f.Name):
				// Two equally deep declarations from different structs: the
				// codec fills neither, so neither may be bound.
				prev.ambiguous = true
			}
		}
		register(jsonKey)
		if qtag := f.Tag.Get("query"); qtag != "" {
			register(strings.Split(qtag, ",")[0])
		}
		register(f.Name)
	}
	for _, ft := range embedded {
		collectDTOFields(ft, depth+1, acc)
	}
}

// compileBindPlan matches a message descriptor against a DTO struct type
// and returns the bridge plan. context names the constructor + procedure
// for error messages; direction reads "request"/"response" in them.
// exempt lists proto field names excluded from matching (the ByID
// constructors' path id). aliases maps proto field name → DTO Go field
// name for pairs the normalized match cannot see.
func compileBindPlan(
	context, direction string,
	md protoreflect.MessageDescriptor,
	dtoType reflect.Type,
	exempt map[string]bool,
	aliases map[string]string,
) (*bindPlan, error) {
	for dtoType.Kind() == reflect.Pointer {
		dtoType = dtoType.Elem()
	}
	if dtoType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%s: %s DTO %s is not a struct", context, direction, dtoType)
	}
	fields := dtoFieldsOf(dtoType)
	plan := &bindPlan{}
	fds := md.Fields()
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		if exempt[string(fd.Name())] {
			continue
		}
		var df dtoField
		var ok bool
		if goName, aliased := aliases[string(fd.Name())]; aliased {
			df, ok = fields[normalizeName(goName)]
			if !ok {
				return nil, fmt.Errorf("%s: Alias(%q, %q) names no field of %s",
					context, fd.Name(), goName, dtoType)
			}
		} else {
			df, ok = fields[normalizeName(fd.JSONName())]
		}
		if !ok {
			return nil, fmt.Errorf(
				"%s: %s field %q has no counterpart on %s — rename one side to match (case/underscore-insensitive), or declare fwgrpc.Alias(%q, \"<GoField>\")",
				context, direction, fd.Name(), dtoType, fd.Name())
		}
		child, coerce, err := compileFieldPlan(context, direction, fd, df, aliases)
		if err != nil {
			return nil, err
		}
		renamed := df.jsonKey != fd.JSONName()
		if renamed || coerce != coerceNone || child != nil {
			if plan.nodes == nil {
				plan.nodes = map[string]bindNode{}
			}
			node := bindNode{child: child, coerce: coerce}
			if renamed {
				node.dtoKey = df.jsonKey
				plan.wireRenames = true
			}
			if child.rewritesWire() {
				plan.wireRenames = true
			}
			plan.nodes[fd.JSONName()] = node
		}
	}
	return plan, nil
}

// compileFieldPlan validates one proto field ↔ DTO field pairing and returns
// the child plan for nested messages (nil when the JSON forms already agree
// end to end) plus whether the field's value needs unquoting on the way in.
func compileFieldPlan(
	context, direction string,
	fd protoreflect.FieldDescriptor,
	df dtoField,
	aliases map[string]string,
) (*bindPlan, coercion, error) {
	dt := df.typ
	for dt.Kind() == reflect.Pointer {
		dt = dt.Elem()
	}
	if fd.IsMap() {
		return nil, coerceNone, fmt.Errorf("%s: %s field %q is a proto map — not supported by the auto bridge; reshape the contract or serve the procedure via MountRaw",
			context, direction, fd.Name())
	}
	if fd.IsList() {
		if dt.Kind() != reflect.Slice {
			return nil, coerceNone, fmt.Errorf("%s: %s field %q is repeated but %s.%s is %s, not a slice",
				context, direction, fd.Name(), df.name, df.name, df.typ)
		}
		dt = dt.Elem()
		for dt.Kind() == reflect.Pointer {
			dt = dt.Elem()
		}
	}
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		// Scalars: the two dialects agree on most kinds, and a genuine kind
		// mismatch surfaces as a json.Unmarshal error. Exactly two forms do
		// NOT agree, and both are settled here, at boot:
		//
		//   64-bit integers — the proto3 JSON mapping renders int64/uint64/
		//   sint64/fixed64/sfixed64 as QUOTED strings, while encoding/json
		//   demands a bare number for a numeric Go field. Unquoting on the
		//   way in is what lets ONE DTO (money in minor units, counters, ids)
		//   serve REST, GraphQL and gRPC. A DTO field declared `string`
		//   carries the digits as text and keeps the quoted form.
		//
		//   enums — the wire carries the member NAME, so the DTO seat must be
		//   a string; a numeric seat can never receive it on the way in.
		switch fd.Kind() {
		case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
			protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			if isNumericKind(dt.Kind()) {
				return nil, coerceUnquoteInt, nil
			}
			return nil, coerceNone, nil
		case protoreflect.FloatKind, protoreflect.DoubleKind:
			if isNumericKind(dt.Kind()) {
				return nil, coerceGuardFloat, nil
			}
		case protoreflect.EnumKind:
			if direction == "request" && isNumericKind(dt.Kind()) {
				return nil, coerceNone, fmt.Errorf(
					"%s: %s field %q is an enum — the wire carries the member NAME, so %s must be a string, not %s",
					context, direction, fd.Name(), df.name, df.typ)
			}
		}
		return nil, coerceNone, nil
	}
	if fd.Message().FullName() == "google.protobuf.Timestamp" {
		if dt != reflect.TypeOf(time.Time{}) {
			return nil, coerceNone, fmt.Errorf("%s: %s field %q is google.protobuf.Timestamp but %s is %s, not time.Time",
				context, direction, fd.Name(), df.name, df.typ)
		}
		return nil, coerceNone, nil
	}
	if strings.HasPrefix(string(fd.Message().FullName()), "google.protobuf.") {
		return nil, coerceNone, fmt.Errorf("%s: %s field %q uses well-known type %s — not supported by the auto bridge",
			context, direction, fd.Name(), fd.Message().FullName())
	}
	if dt.Kind() != reflect.Struct {
		return nil, coerceNone, fmt.Errorf("%s: %s field %q is a message but %s is %s, not a struct",
			context, direction, fd.Name(), df.name, df.typ)
	}
	child, err := compileBindPlan(context, direction, fd.Message(), dt, nil, aliases)
	if err != nil {
		return nil, coerceNone, err
	}
	if !child.hasNodes() {
		return nil, coerceNone, nil
	}
	return child, coerceNone, nil
}

// isNumericKind reports whether a DTO field holds a number — the seat that
// needs protojson's quoted 64-bit integer unquoted before encoding/json sees
// it.
func isNumericKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// rewriteToDTO rewrites protojson keys to DTO json keys and unquotes the
// 64-bit integers protojson quoted, recursively, in place. Values the plan
// does not touch keep their identity.
func (p *bindPlan) rewriteToDTO(m map[string]any) error {
	if !p.hasNodes() {
		return nil
	}
	for wireKey, node := range p.nodes {
		v, ok := m[wireKey]
		if !ok {
			continue
		}
		switch node.coerce {
		case coerceUnquoteInt:
			v = unquoteIntegers(v)
		case coerceGuardFloat:
			if err := guardFloatSpecials(wireKey, v); err != nil {
				return err
			}
		}
		if node.child != nil {
			switch tv := v.(type) {
			case map[string]any:
				if err := node.child.rewriteToDTO(tv); err != nil {
					return err
				}
			case []any:
				for _, item := range tv {
					if im, ok := item.(map[string]any); ok {
						if err := node.child.rewriteToDTO(im); err != nil {
							return err
						}
					}
				}
			}
		}
		switch {
		case node.dtoKey != "":
			delete(m, wireKey)
			m[node.dtoKey] = v
		case node.coerce == coerceUnquoteInt:
			m[wireKey] = v
		}
	}
	return nil
}

// guardFloatSpecials rejects protojson's non-finite float renderings with a
// message that NAMES the field and the value. JSON has no literal for them, so
// encoding/json can neither read one into a float field nor write one back —
// the auto bridge cannot carry them in either direction, and a contract that
// must is a MountRaw procedure. Without this the caller saw only the codec's
// generic "cannot unmarshal string into Go struct field … of type float64".
func guardFloatSpecials(wireKey string, v any) error {
	check := func(s string) error {
		switch s {
		case "NaN", "Infinity", "-Infinity":
			return fmt.Errorf(
				"field %q carries %s: JSON has no literal for a non-finite float, so the auto bridge cannot bind it — serve this procedure via MountRaw if the contract must carry one",
				wireKey, s)
		}
		return nil
	}
	switch tv := v.(type) {
	case string:
		return check(tv)
	case []any:
		for _, item := range tv {
			if s, ok := item.(string); ok {
				if err := check(s); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// rewriteToWire is the inverse of rewriteToDTO's rename half: DTO json keys
// → protojson keys. Values need no inverse — protojson accepts a bare number
// wherever it emits a quoted 64-bit integer.
func (p *bindPlan) rewriteToWire(m map[string]any) {
	if !p.rewritesWire() {
		return
	}
	for wireKey, node := range p.nodes {
		if node.dtoKey == "" && !node.child.rewritesWire() {
			continue
		}
		key := wireKey
		if node.dtoKey != "" {
			key = node.dtoKey
		}
		v, ok := m[key]
		if !ok {
			continue
		}
		if node.child != nil {
			switch tv := v.(type) {
			case map[string]any:
				node.child.rewriteToWire(tv)
			case []any:
				for _, item := range tv {
					if im, ok := item.(map[string]any); ok {
						node.child.rewriteToWire(im)
					}
				}
			}
		}
		if node.dtoKey != "" {
			delete(m, key)
			m[wireKey] = v
		}
	}
}

// unquoteIntegers turns protojson's quoted 64-bit integers back into JSON
// numbers, in the intermediate map — scalar or repeated. The digits travel
// as json.RawMessage, never through float64, so a value beyond 2^53 (money
// in minor units, a snowflake id) is re-emitted EXACTLY as it arrived.
func unquoteIntegers(v any) any {
	switch tv := v.(type) {
	case string:
		if isJSONInteger(tv) {
			return json.RawMessage(tv)
		}
	case []any:
		for i, item := range tv {
			if s, ok := item.(string); ok && isJSONInteger(s) {
				tv[i] = json.RawMessage(s)
			}
		}
	}
	return v
}

// isJSONInteger reports whether s is the plain integer literal protojson
// emits. Anything else is left alone, so a genuine mismatch still surfaces
// as the normal json.Unmarshal error instead of forging invalid JSON.
func isJSONInteger(s string) bool {
	digits := strings.TrimPrefix(s, "-")
	if digits == "" {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return true
}

// pbToDTO executes the plan: proto message → DTO value. Errors are wire
// data the DTO cannot carry (e.g. a string where a number is declared) —
// the callers surface them as SchemaViolation, the REST body-parse
// rejection.
func pbToDTO[TReq any](plan *bindPlan, msg proto.Message) (TReq, error) {
	var out TReq
	raw, err := protojson.Marshal(msg)
	if err != nil {
		return out, err
	}
	if plan.hasNodes() {
		m, err := decodeJSONMap(raw)
		if err != nil {
			return out, err
		}
		if err := plan.rewriteToDTO(m); err != nil {
			return out, err
		}
		if raw, err = json.Marshal(m); err != nil {
			return out, err
		}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

// dtoToPB executes the plan in reverse: DTO value → a freshly allocated
// proto message. DTO fields with no wire counterpart are dropped
// (DiscardUnknown — the proto exposes a subset of the DTO by design).
func dtoToPB[RPB any](plan *bindPlan, dto any) (*RPB, error) {
	raw, err := jsonMarshalDTO(plan, dto)
	if err != nil {
		return nil, err
	}
	out := new(RPB)
	pm, ok := any(out).(proto.Message)
	if !ok {
		return nil, fmt.Errorf("*%T is not a proto.Message", *out)
	}
	if err := protojsonUnmarshal(raw, pm); err != nil {
		return nil, err
	}
	return out, nil
}

// jsonMarshalDTO renders a DTO value as wire-keyed JSON (renames applied).
func jsonMarshalDTO(plan *bindPlan, dto any) ([]byte, error) {
	raw, err := json.Marshal(dto)
	if err != nil {
		return nil, err
	}
	if plan.rewritesWire() {
		m, err := decodeJSONMap(raw)
		if err != nil {
			return nil, err
		}
		plan.rewriteToWire(m)
		if raw, err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

// decodeJSONMap decodes the intermediate rewrite map WITHOUT routing numbers
// through float64: UseNumber keeps every literal as json.Number (re-emitted
// verbatim by encoding/json), so a 64-bit integer crosses the rewrite EXACTLY
// — `math.MaxInt64` stays 9223372036854775807 instead of rounding to
// …776000. A payload that is not a JSON object fails here, as it must.
func decodeJSONMap(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// protojsonUnmarshal binds wire-keyed JSON onto a message, dropping the
// DTO-only keys the proto contract does not expose.
func protojsonUnmarshal(raw []byte, pm proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, pm)
}

// descriptorOf yields the message descriptor for a value type parameter —
// new(PB) must implement proto.Message (the generated pointer type).
func descriptorOf[PB any](context, role string) protoreflect.MessageDescriptor {
	pm, ok := any(new(PB)).(proto.Message)
	if !ok {
		bootFail("%s: %s type %T is not a generated proto message", context, role, *new(PB))
	}
	return pm.ProtoReflect().Descriptor()
}

// bootFail aborts boot with a clear, prefixed message. Plan compilation
// runs at Register time — inside the consumer's Wiring, before the
// listener accepts traffic — so a contract/DTO mismatch can never reach a
// request.
func bootFail(format string, args ...any) {
	panic(fmt.Sprintf("omnicore grpc: "+format, args...))
}
