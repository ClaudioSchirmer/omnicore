package grpc

import (
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	pb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// The read-side plan. A list request message composes the shared
// omnicore.v1 components — PaginationRequest, repeated OrderByField,
// FieldMask, and typed filter wrappers (top-level or grouped under one
// nested "filters" message). compileQueryPlan discovers them BY TYPE on the
// descriptor and binds each filter to the Request DTO's `filter:`-tagged
// leaf, inheriting its operator allowlist — the gRPC wire enforces exactly
// the vocabulary the REST query string enforces, per field. The fields/
// orderBy vocabulary comes from the RESPONSE side: item proto fields matched
// against the Response DTO's projection schema (wire → Go doc path), the
// same hardening that keeps unresolved spellings away from the reader.

type filterKind int

const (
	filterString filterKind = iota
	filterInt64
	filterDouble
	filterBool
	filterTimestamp
)

var wrapperKinds = map[protoreflect.FullName]filterKind{
	"omnicore.v1.StringFilter":    filterString,
	"omnicore.v1.Int64Filter":     filterInt64,
	"omnicore.v1.DoubleFilter":    filterDouble,
	"omnicore.v1.BoolFilter":      filterBool,
	"omnicore.v1.TimestampFilter": filterTimestamp,
}

// filterBinding is one wrapper field bound to a Request DTO filter leaf.
// ops keeps the `filter:` tag's declaration order — it IS the allowlist
// and the rejection message quotes it verbatim, mirroring REST.
type filterBinding struct {
	fd     protoreflect.FieldDescriptor
	goPath string
	ops    []string
	kind   filterKind
}

func (fb filterBinding) allows(op string) bool {
	for _, o := range fb.ops {
		if o == op {
			return true
		}
	}
	return false
}

// queryPlan is the compiled read-side plan for one list procedure.
type queryPlan struct {
	page    protoreflect.FieldDescriptor
	sort    protoreflect.FieldDescriptor
	mask    protoreflect.FieldDescriptor
	group   protoreflect.FieldDescriptor // the filters container, when grouped
	filters []filterBinding
	fields  map[string]string // wire → Go doc path (fields/orderBy vocabulary)
	// computed is the COMPUTED subset of fields: proto wire path → the Go
	// source paths the Response's `computed:"…"` tag names. Such a field has
	// no column (the Query's FromQueryResult derives it after the read), so
	// a read_mask entry pushes the SOURCES down instead of the path itself.
	// Kept as its own map so the consumer does not have to re-derive it from
	// the Response at request time.
	computed map[string][]string
	// reqSchema is the Request DTO's reflected schema — the canonical opt-in
	// gate (Reserved) plus the ordering vocabulary it declares. The shared
	// proto shows every control field; the DTO decides which ones THIS endpoint
	// honors, and buildCriteria feeds the declared set to
	// queryschema.ValidateControls before dispatch. A control set on the wire
	// without its `query:"…"` declaration is a wire-contract violation
	// (SchemaViolation → INVALID_ARGUMENT).
	reqSchema *queryschema.RequestSchema
	// sortIndex folds reqSchema.Sortable's keys to their normalized spelling,
	// which is how a proto `order_by` entry (`created_at`) meets the DTO's
	// declaration (`createdAt`). Compiled here, once at Register, because the
	// answer is a property of the DTO and not of any request.
	sortIndex map[string]string
}

// listEnvelope is the compiled response plan for one list procedure: the
// repeated items field + the omnicore.v1.PaginationInfo field, located by
// type, plus the item ↔ Response DTO bridge.
type listEnvelope struct {
	items      protoreflect.FieldDescriptor
	pagination protoreflect.FieldDescriptor
	itemPlan   *bindPlan
}

// normalizePath folds a dotted wire path segment-wise: "addresses.zipCode"
// and "addresses_zip_code" meet at "addresses.zipcode".
func normalizePath(p string) string {
	segs := strings.Split(p, ".")
	for i, s := range segs {
		segs[i] = normalizeName(s)
	}
	return strings.Join(segs, ".")
}

// compileQueryPlan builds the read-side plan: request components + filter
// bindings from the Request DTO + the mask/sort vocabulary from the item
// descriptor × Response DTO projection schema.
func compileQueryPlan(
	context string,
	reqMD protoreflect.MessageDescriptor,
	reqDTO reflect.Type,
	itemMD protoreflect.MessageDescriptor,
	respDTO reflect.Type,
	aliases map[string]string,
) (*queryPlan, error) {
	plan := &queryPlan{}

	// The Request DTO's reflected schema: the control set the canonical gate
	// consumes, the ordering vocabulary, and the CLASSIFIED declarations. Which
	// of them is a filter is the DTO's answer, decided once in
	// ExtractRequestSchema — this surface reads it, it does not re-derive it.
	plan.reqSchema = queryschema.ExtractRequestSchema(reqDTO)
	plan.sortIndex = sortableIndex(plan.reqSchema.Sortable)

	// Filter leaves keyed by normalized wire path AND normalized Go path — the
	// per-field operator allowlist, reachable by either spelling a proto field
	// or an Alias may use.
	leaves := map[string]queryschema.RequestLeaf{}
	for _, leaf := range plan.reqSchema.Leaves {
		if leaf.Kind != queryschema.LeafFilter {
			continue
		}
		leaves[normalizePath(leaf.WirePath)] = leaf
		leaves[normalizePath(leaf.GoPath)] = leaf
	}

	bindFilter := func(fd protoreflect.FieldDescriptor, prefix string) error {
		kind, ok := wrapperKinds[fd.Message().FullName()]
		if !ok {
			return fmt.Errorf("%s: request field %q is not a shared omnicore.v1 component nor a filter wrapper",
				context, prefix+string(fd.Name()))
		}
		wire := prefix + string(fd.Name())
		var rf queryschema.RequestLeaf
		var found bool
		if goName, aliased := aliases[string(fd.Name())]; aliased {
			rf, found = leaves[normalizePath(goName)]
		} else {
			rf, found = leaves[normalizePath(wire)]
		}
		if !found {
			return fmt.Errorf(
				"%s: filter %q has no `filter:`-tagged counterpart on %s — the Request DTO's tags are the operator allowlist; declare the field (or fwgrpc.Alias(%q, \"<GoField>\"))",
				context, wire, reqDTO, fd.Name())
		}
		plan.filters = append(plan.filters, filterBinding{fd: fd, goPath: rf.GoPath, ops: rf.Ops, kind: kind})
		return nil
	}

	fds := reqMD.Fields()
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		switch {
		case fd.Kind() == protoreflect.MessageKind && fd.IsList() &&
			fd.Message().FullName() == "omnicore.v1.OrderByField":
			plan.sort = fd
		case fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap():
			return nil, fmt.Errorf(
				"%s: request field %q is not part of the shared read contract — a list request carries only omnicore.v1 components (pagination/order_by/fields/filters); bespoke inputs belong to the query type's ToCriteria or a MountRaw procedure",
				context, fd.Name())
		case fd.Message().FullName() == "omnicore.v1.PaginationRequest":
			plan.page = fd
		case fd.Message().FullName() == "google.protobuf.FieldMask":
			plan.mask = fd
		default:
			if _, isWrapper := wrapperKinds[fd.Message().FullName()]; isWrapper {
				if err := bindFilter(fd, ""); err != nil {
					return nil, err
				}
				continue
			}
			// A nested message grouping the filters (the conventional
			// `Filters filters = N` block): every field inside must be a
			// wrapper.
			if plan.group != nil {
				return nil, fmt.Errorf("%s: request carries two filter groups (%q and %q)",
					context, plan.group.Name(), fd.Name())
			}
			plan.group = fd
			inner := fd.Message().Fields()
			for j := 0; j < inner.Len(); j++ {
				if err := bindFilter(inner.Get(j), ""); err != nil {
					return nil, err
				}
			}
		}
	}

	// Mask/sort vocabulary: item proto fields (the wire names FieldMask
	// speaks) → Go doc paths, resolved through the Response DTO's
	// projection schema. Only fields the DTO projects are addressable —
	// the allowlist that keeps unresolved spellings away from the reader.
	proj := queryschema.ExtractProjectionSchema(respDTO)
	normProj := make(map[string]string, len(proj.Paths))
	for wirePath, docPath := range proj.Paths {
		normProj[normalizePath(wirePath)] = docPath
	}
	// The COMPUTED cut of the same vocabulary, folded to the normalized
	// spelling so it meets the proto field names the same way normProj does.
	normComputed := make(map[string][]string, len(proj.Computed))
	for wirePath, sources := range proj.Computed {
		normComputed[normalizePath(wirePath)] = sources
	}
	plan.fields = map[string]string{}
	plan.computed = map[string][]string{}
	var walkItem func(md protoreflect.MessageDescriptor, wirePrefix, normPrefix string) error
	seen := map[protoreflect.FullName]bool{}
	walkItem = func(md protoreflect.MessageDescriptor, wirePrefix, normPrefix string) error {
		if seen[md.FullName()] {
			return nil
		}
		seen[md.FullName()] = true
		ifds := md.Fields()
		for i := 0; i < ifds.Len(); i++ {
			fd := ifds.Get(i)
			wire := wirePrefix + string(fd.Name())
			norm := normPrefix + normalizeName(string(fd.Name()))
			if docPath, ok := normProj[norm]; ok {
				plan.fields[wire] = docPath
				if sources, isComputed := normComputed[norm]; isComputed {
					plan.computed[wire] = sources
				}
			}
			if fd.Kind() == protoreflect.MessageKind && !fd.IsMap() &&
				!strings.HasPrefix(string(fd.Message().FullName()), "google.protobuf.") {
				if err := walkItem(fd.Message(), wire+".", norm+"."); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walkItem(itemMD, "", ""); err != nil {
		return nil, err
	}
	return plan, nil
}

// compileListEnvelope locates the response envelope by type: exactly one
// repeated message field (the items) and one omnicore.v1.PaginationInfo.
func compileListEnvelope(
	context string,
	respMD protoreflect.MessageDescriptor,
	respDTO reflect.Type,
	aliases map[string]string,
) (*listEnvelope, error) {
	env := &listEnvelope{}
	fds := respMD.Fields()
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		switch {
		case fd.Kind() == protoreflect.MessageKind && !fd.IsList() &&
			fd.Message().FullName() == "omnicore.v1.PaginationInfo":
			if env.pagination != nil {
				return nil, fmt.Errorf("%s: response carries two PaginationInfo fields", context)
			}
			env.pagination = fd
		case fd.Kind() == protoreflect.MessageKind && fd.IsList():
			if env.items != nil {
				return nil, fmt.Errorf("%s: response carries two repeated message fields (%q and %q) — a list envelope has exactly one items field",
					context, env.items.Name(), fd.Name())
			}
			env.items = fd
		default:
			return nil, fmt.Errorf(
				"%s: response field %q is not part of the list envelope — compose exactly one repeated items message + omnicore.v1.PaginationInfo",
				context, fd.Name())
		}
	}
	if env.items == nil {
		return nil, fmt.Errorf("%s: response declares no repeated items message", context)
	}
	if env.pagination == nil {
		return nil, fmt.Errorf("%s: response declares no omnicore.v1.PaginationInfo field", context)
	}
	itemPlan, err := compileBindPlan(context, "response item", env.items.Message(), respDTO, nil, aliases)
	if err != nil {
		return nil, err
	}
	env.itemPlan = itemPlan
	return env, nil
}

// buildCriteria executes the query plan against one request message: the
// INPUT criteria, exactly what the REST parser hands ToQuery. Operator
// enforcement mirrors REST: an operator outside the leaf's `filter:` tag
// is a wire-contract violation (SchemaViolation → INVALID_ARGUMENT). The
// canonical control gateway runs against the DTO's Reserved set BEFORE the
// handler — the full opt-in gate, the directional rule and the only-total
// conflict matrix, shared verbatim with REST and GraphQL. The computed cut of
// the field vocabulary rides along so read_mask pushes a computed field's
// sources and sort refuses it, exactly as `?fields=`/`?orderBy=` do. The
// second return value names the sources read ONLY to feed a masked computed
// field (HiddenComputedSources) — the wrapper blanks them on each Result
// before projection, so they never leak onto the masked wire.
func (plan *queryPlan) buildCriteria(msg protoreflect.Message) (queries.ReadCriteria, []string, error) {
	// The DTO's declarations ARE the gate here: same controls, same ordering
	// vocabulary, same assembler as its REST twin. A raw mount answers for its
	// own contract; a compiled one answers for the Request DTO's.
	//
	// withSchema borrows those two read-only; the FILTER gate is not among them
	// because it already ran, earlier and by name — compileQueryPlan bound every
	// proto filter field to a `filter:`-tagged leaf at boot, and each binding
	// checks the operator against that leaf's tag just below.
	b := NewCriteria().withSchema(plan.reqSchema, plan.sortIndex).
		Fields(plan.fields).ComputedFields(plan.computed)
	if plan.page != nil && msg.Has(plan.page) {
		if p, ok := msg.Get(plan.page).Message().Interface().(*pb.PaginationRequest); ok {
			b.Page(p)
		}
	}
	if plan.sort != nil {
		list := msg.Get(plan.sort).List()
		for i := 0; i < list.Len(); i++ {
			if sf, ok := list.Get(i).Message().Interface().(*pb.OrderByField); ok {
				b.OrderBy(sf)
			}
		}
	}
	if plan.mask != nil && msg.Has(plan.mask) {
		if fm, ok := msg.Get(plan.mask).Message().Interface().(*fieldmaskpb.FieldMask); ok {
			b.FieldMask(fm)
		}
	}
	var opErrs []string
	source := msg
	if plan.group != nil {
		if !msg.Has(plan.group) {
			source = nil
		} else {
			source = msg.Get(plan.group).Message()
		}
	}
	if source != nil {
		for _, fb := range plan.filters {
			if !source.Has(fb.fd) {
				continue
			}
			wrapper := source.Get(fb.fd).Message().Interface()
			if bad := fb.apply(b, wrapper); bad != "" {
				opErrs = append(opErrs, bad)
			}
		}
	}
	crit, err := b.Build()
	if len(opErrs) > 0 {
		return queries.ReadCriteria{}, nil, fmt.Errorf("grpc criteria: %s", strings.Join(opErrs, "; "))
	}
	if err != nil {
		return queries.ReadCriteria{}, nil, err
	}
	// The computed cut the RENDER needs: sources read only to feed a masked
	// computed field, blanked on each Result before projection so read_mask
	// shapes the wire exactly as `?fields=` does on REST and the exports.
	return crit, b.HiddenComputedSources(), nil
}

// apply feeds one wrapper into the builder, enforcing the leaf's operator
// allowlist first. Returns a violation description, or "".
func (fb filterBinding) apply(b *CriteriaBuilder, wrapper proto.Message) string {
	reject := func(op string) string {
		return fmt.Sprintf("filter %q: operator %q is not in the declared allowlist (%s)",
			fb.goPath, op, strings.Join(fb.ops, ","))
	}
	switch fb.kind {
	case filterString:
		f, ok := wrapper.(*pb.StringFilter)
		if !ok {
			return ""
		}
		for _, c := range f.GetConditions() {
			if op, known := stringOps[c.GetOp()]; known && !fb.allows(op) {
				return reject(op)
			}
		}
		b.String(fb.goPath, f)
	case filterInt64:
		f, ok := wrapper.(*pb.Int64Filter)
		if !ok {
			return ""
		}
		for _, c := range f.GetConditions() {
			if op, known := numberOps[c.GetOp()]; known && !fb.allows(op) {
				return reject(op)
			}
		}
		b.Int64(fb.goPath, f)
	case filterDouble:
		f, ok := wrapper.(*pb.DoubleFilter)
		if !ok {
			return ""
		}
		for _, c := range f.GetConditions() {
			if op, known := numberOps[c.GetOp()]; known && !fb.allows(op) {
				return reject(op)
			}
		}
		b.Double(fb.goPath, f)
	case filterBool:
		f, ok := wrapper.(*pb.BoolFilter)
		if !ok {
			return ""
		}
		for _, c := range f.GetConditions() {
			if op, known := boolOps[c.GetOp()]; known && !fb.allows(op) {
				return reject(op)
			}
		}
		b.Bool(fb.goPath, f)
	case filterTimestamp:
		f, ok := wrapper.(*pb.TimestampFilter)
		if !ok {
			return ""
		}
		for _, c := range f.GetConditions() {
			if op, known := numberOps[c.GetOp()]; known && !fb.allows(op) {
				return reject(op)
			}
		}
		b.Timestamp(fb.goPath, f)
	}
	return ""
}

// buildListResponse materializes the response envelope from a typed page:
// each Result runs through the responseProjection (the same FromResult seat
// REST uses) and the resulting Response DTO crosses to the item message via
// the bridge.
func buildListResponse[RPB any, TResult any, R any](
	env *listEnvelope,
	responseProjection func(TResult) R,
	page queries.PageOf[TResult],
) (*RPB, error) {
	out := new(RPB)
	pm, ok := any(out).(proto.Message)
	if !ok {
		return nil, fmt.Errorf("*%T is not a proto.Message", *out)
	}
	m := pm.ProtoReflect()
	info := &pb.PaginationInfo{
		TotalCount:      page.TotalCount,
		EndCursor:       page.EndCursor,
		StartCursor:     page.StartCursor,
		HasNextPage:     page.HasNextPage,
		HasPreviousPage: page.HasPreviousPage,
	}
	m.Set(env.pagination, protoreflect.ValueOfMessage(info.ProtoReflect()))
	list := m.Mutable(env.items).List()
	for _, r := range page.Items {
		item := list.NewElement()
		target := item.Message().Interface()
		mapped, err := dtoToMessage(env.itemPlan, responseProjection(r), target)
		if err != nil {
			return nil, err
		}
		list.Append(protoreflect.ValueOfMessage(mapped.ProtoReflect()))
	}
	return out, nil
}

// dtoToMessage bridges one DTO value into an existing message instance
// (the list element type is only known via its descriptor).
func dtoToMessage(plan *bindPlan, dto any, target proto.Message) (proto.Message, error) {
	raw, err := jsonMarshalDTO(plan, dto)
	if err != nil {
		return nil, err
	}
	if err := protojsonUnmarshal(raw, target); err != nil {
		return nil, err
	}
	return target, nil
}
