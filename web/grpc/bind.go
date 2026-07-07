package grpc

import (
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
// key differs, and encoding/json binds the DTO — the same codec semantics
// the REST body bind uses, so both wires produce identical DTO values by
// construction.

// bindPlan is one message's compiled bridge: rename nodes (applied to the
// intermediate JSON) plus the child plans for nested messages.
type bindPlan struct {
	// renames maps the protojson key → DTO json key (pb→DTO direction);
	// inverse applies on the way out. nil when every key already agrees.
	renames map[string]renameNode
}

type renameNode struct {
	dtoKey string    // "" = key unchanged
	child  *bindPlan // non-nil for nested message fields needing renames
}

func (p *bindPlan) hasRenames() bool { return p != nil && len(p.renames) > 0 }

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
// encoding/json.
func dtoFieldsOf(t reflect.Type) map[string]dtoField {
	out := map[string]dtoField{}
	collectDTOFields(t, out)
	return out
}

func collectDTOFields(t reflect.Type, out map[string]dtoField) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectDTOFields(ft, out)
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
			if _, taken := out[key]; !taken {
				out[key] = df
			}
		}
		register(jsonKey)
		if qtag := f.Tag.Get("query"); qtag != "" {
			register(strings.Split(qtag, ",")[0])
		}
		register(f.Name)
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
		child, err := compileFieldPlan(context, direction, fd, df, aliases)
		if err != nil {
			return nil, err
		}
		if df.jsonKey != fd.JSONName() || child != nil {
			if plan.renames == nil {
				plan.renames = map[string]renameNode{}
			}
			node := renameNode{child: child}
			if df.jsonKey != fd.JSONName() {
				node.dtoKey = df.jsonKey
			}
			plan.renames[fd.JSONName()] = node
		}
	}
	return plan, nil
}

// compileFieldPlan validates one proto field ↔ DTO field pairing and
// returns the child plan for nested messages (nil when the JSON forms
// already agree end to end).
func compileFieldPlan(
	context, direction string,
	fd protoreflect.FieldDescriptor,
	df dtoField,
	aliases map[string]string,
) (*bindPlan, error) {
	dt := df.typ
	for dt.Kind() == reflect.Pointer {
		dt = dt.Elem()
	}
	if fd.IsMap() {
		return nil, fmt.Errorf("%s: %s field %q is a proto map — not supported by the auto bridge; reshape the contract or serve the procedure via MountRaw",
			context, direction, fd.Name())
	}
	if fd.IsList() {
		if dt.Kind() != reflect.Slice {
			return nil, fmt.Errorf("%s: %s field %q is repeated but %s.%s is %s, not a slice",
				context, direction, fd.Name(), df.name, df.name, df.typ)
		}
		dt = dt.Elem()
		for dt.Kind() == reflect.Pointer {
			dt = dt.Elem()
		}
	}
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		// Scalar (incl. enum ↔ string): the JSON forms line up by
		// construction; kind mismatches surface as a json.Unmarshal error in
		// tests, not silently — no extra boot check needed beyond structure.
		return nil, nil
	}
	if fd.Message().FullName() == "google.protobuf.Timestamp" {
		if dt != reflect.TypeOf(time.Time{}) {
			return nil, fmt.Errorf("%s: %s field %q is google.protobuf.Timestamp but %s is %s, not time.Time",
				context, direction, fd.Name(), df.name, df.typ)
		}
		return nil, nil
	}
	if strings.HasPrefix(string(fd.Message().FullName()), "google.protobuf.") {
		return nil, fmt.Errorf("%s: %s field %q uses well-known type %s — not supported by the auto bridge",
			context, direction, fd.Name(), fd.Message().FullName())
	}
	if dt.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%s: %s field %q is a message but %s is %s, not a struct",
			context, direction, fd.Name(), df.name, df.typ)
	}
	child, err := compileBindPlan(context, direction, fd.Message(), dt, nil, aliases)
	if err != nil {
		return nil, err
	}
	if !child.hasRenames() {
		return nil, nil
	}
	return child, nil
}

// renameToDTO rewrites protojson keys to DTO json keys, recursively, in
// place. Values under renamed keys keep their identity.
func (p *bindPlan) renameToDTO(m map[string]any) {
	if !p.hasRenames() {
		return
	}
	for wireKey, node := range p.renames {
		v, ok := m[wireKey]
		if !ok {
			continue
		}
		if node.child != nil {
			switch tv := v.(type) {
			case map[string]any:
				node.child.renameToDTO(tv)
			case []any:
				for _, item := range tv {
					if im, ok := item.(map[string]any); ok {
						node.child.renameToDTO(im)
					}
				}
			}
		}
		if node.dtoKey != "" {
			delete(m, wireKey)
			m[node.dtoKey] = v
		}
	}
}

// renameToWire is the inverse of renameToDTO: DTO json keys → protojson
// keys.
func (p *bindPlan) renameToWire(m map[string]any) {
	if !p.hasRenames() {
		return
	}
	for wireKey, node := range p.renames {
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
				node.child.renameToWire(tv)
			case []any:
				for _, item := range tv {
					if im, ok := item.(map[string]any); ok {
						node.child.renameToWire(im)
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
	if plan.hasRenames() {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return out, err
		}
		plan.renameToDTO(m)
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
	if plan.hasRenames() {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		plan.renameToWire(m)
		if raw, err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	return raw, nil
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
