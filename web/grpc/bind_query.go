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
// omnicore.v1 components — PageRequest, repeated SortField, FieldMask, and
// typed filter wrappers (top-level or grouped under one nested "filters"
// message). compileQueryPlan discovers them BY TYPE on the descriptor and
// binds each filter to the Request DTO's `filter:`-tagged leaf, inheriting
// its operator allowlist — the gRPC wire enforces exactly the vocabulary
// the REST query string enforces, per field. The mask/sort vocabulary
// comes from the RESPONSE side: item proto fields matched against the
// Response DTO's projection schema (wire → Go doc path), the same
// hardening that keeps unresolved spellings away from the reader.

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
	fields  map[string]string // wire → Go doc path (mask/sort vocabulary)
}

// listEnvelope is the compiled response plan for one list procedure: the
// repeated items field + the omnicore.v1.PageInfo field, located by type,
// plus the item ↔ Response DTO bridge.
type listEnvelope struct {
	items    protoreflect.FieldDescriptor
	pageInfo protoreflect.FieldDescriptor
	itemPlan *bindPlan
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

	// The Request DTO's filter leaves, keyed by normalized wire path — the
	// vocabulary AND the per-field operator allowlist (`filter:` tags), the
	// exact schema the REST query string is parsed against.
	leaves := map[string]queryschema.RequestField{}
	for _, rf := range queryschema.WalkRequest(reqDTO) {
		if rf.Ops != nil {
			leaves[normalizePath(rf.WirePath)] = rf
			leaves[normalizePath(rf.GoPath)] = rf
		}
	}

	bindFilter := func(fd protoreflect.FieldDescriptor, prefix string) error {
		kind, ok := wrapperKinds[fd.Message().FullName()]
		if !ok {
			return fmt.Errorf("%s: request field %q is not a shared omnicore.v1 component nor a filter wrapper",
				context, prefix+string(fd.Name()))
		}
		wire := prefix + string(fd.Name())
		var rf queryschema.RequestField
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
			fd.Message().FullName() == "omnicore.v1.SortField":
			plan.sort = fd
		case fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap():
			return nil, fmt.Errorf(
				"%s: request field %q is not part of the shared read contract — a list request carries only omnicore.v1 components (page/sort/read_mask/filters); bespoke inputs belong to the query type's ToCriteria or a MountRaw procedure",
				context, fd.Name())
		case fd.Message().FullName() == "omnicore.v1.PageRequest":
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
	plan.fields = map[string]string{}
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
// repeated message field (the items) and one omnicore.v1.PageInfo.
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
			fd.Message().FullName() == "omnicore.v1.PageInfo":
			if env.pageInfo != nil {
				return nil, fmt.Errorf("%s: response carries two PageInfo fields", context)
			}
			env.pageInfo = fd
		case fd.Kind() == protoreflect.MessageKind && fd.IsList():
			if env.items != nil {
				return nil, fmt.Errorf("%s: response carries two repeated message fields (%q and %q) — a list envelope has exactly one items field",
					context, env.items.Name(), fd.Name())
			}
			env.items = fd
		default:
			return nil, fmt.Errorf(
				"%s: response field %q is not part of the list envelope — compose exactly one repeated items message + omnicore.v1.PageInfo",
				context, fd.Name())
		}
	}
	if env.items == nil {
		return nil, fmt.Errorf("%s: response declares no repeated items message", context)
	}
	if env.pageInfo == nil {
		return nil, fmt.Errorf("%s: response declares no omnicore.v1.PageInfo field", context)
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
// is a wire-contract violation (SchemaViolation → INVALID_ARGUMENT).
func (plan *queryPlan) buildCriteria(msg protoreflect.Message) (queries.ReadCriteria, error) {
	b := NewCriteria().Fields(plan.fields)
	if plan.page != nil && msg.Has(plan.page) {
		if p, ok := msg.Get(plan.page).Message().Interface().(*pb.PageRequest); ok {
			b.Page(p)
		}
	}
	if plan.sort != nil {
		list := msg.Get(plan.sort).List()
		for i := 0; i < list.Len(); i++ {
			if sf, ok := list.Get(i).Message().Interface().(*pb.SortField); ok {
				b.Sort(sf)
			}
		}
	}
	if plan.mask != nil && msg.Has(plan.mask) {
		if fm, ok := msg.Get(plan.mask).Message().Interface().(*fieldmaskpb.FieldMask); ok {
			b.ReadMask(fm)
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
		return queries.ReadCriteria{}, fmt.Errorf("grpc criteria: %s", strings.Join(opErrs, "; "))
	}
	return crit, err
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

// buildListResponse materializes the response envelope from a page: each
// doc runs through the projector (the same AutoFromDoc/FromDoc seat REST
// uses) and the resulting DTO crosses to the item message via the bridge.
func buildListResponse[RPB any, R any](
	env *listEnvelope,
	projector func(map[string]any) R,
	page queries.Page,
) (*RPB, error) {
	out := new(RPB)
	pm, ok := any(out).(proto.Message)
	if !ok {
		return nil, fmt.Errorf("*%T is not a proto.Message", *out)
	}
	m := pm.ProtoReflect()
	info := &pb.PageInfo{Total: page.Total, NextCursor: page.NextCursor, PrevCursor: page.PrevCursor}
	m.Set(env.pageInfo, protoreflect.ValueOfMessage(info.ProtoReflect()))
	list := m.Mutable(env.items).List()
	for _, doc := range page.Items {
		item := list.NewElement()
		target := item.Message().Interface()
		mapped, err := dtoToMessage(env.itemPlan, projector(doc), target)
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
