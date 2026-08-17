package grpc

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	pb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// CriteriaBuilder converts the shared omnicore.v1 read-side components
// (PaginationRequest, OrderByField, FieldMask, the typed filter wrappers) into
// a queries.ReadCriteria — the INPUT criteria, exactly what the REST parser
// produces from the query string. Filter keys are GO FIELD PATHS; the query
// type's ToCriteria(ctx) keeps applying identity overlays (Restrict, tenant
// gates) on top, unchanged from every other surface.
//
// Filter emission delegates to queryschema.ApplyFilterValues — the same
// single emitter the REST/OpenAPI/GraphQL path uses — so operator semantics
// cannot drift between wires. The per-view proto filters message IS the
// allowlist: only the fields the mapper wires here are filterable.
//
//	crit, err := fwgrpc.NewCriteria().
//	    Page(pb.GetPage()).
//	    OrderBy(pb.GetOrderBy()...).
//	    FieldMask(pb.GetFields()).
//	    String("UserName", pb.GetFilters().GetUserName()).
//	    Build()
type CriteriaBuilder struct {
	crit     queries.ReadCriteria
	controls queryschema.Controls
	fields   map[string]string   // wire (proto snake_case) → Go field path
	computed map[string][]string // wire → Go source paths (COMPUTED fields)
	errs     []error
	// computedSorts collects the sort entries that named a COMPUTED field —
	// refused as a typed notification (not a prose error) so the envelope
	// tells the consumer WHY, exactly like the REST 400.
	computedSorts []string
}

// NewCriteria starts a builder with an empty filter map.
func NewCriteria() *CriteriaBuilder {
	return &CriteriaBuilder{crit: queries.ReadCriteria{Filter: map[string]any{}}}
}

// Controls exposes the canonical control snapshot the builder accumulated —
// the input the Auto path feeds queryschema.ValidateControls together with
// the Request DTO's Reserved set (the full opt-in gate). The raw path's own
// Build applies the DTO-less subset of the gateway (directional rule +
// only-total conflicts).
func (b *CriteriaBuilder) Controls() queryschema.Controls {
	return b.controls
}

// Fields declares the view's wire vocabulary for fields and order_by: proto
// field name (snake_case — FieldMask's canonical JSON form names fields of
// the RESPONSE message) → Go field path. It is the ALLOWLIST, exactly like
// the REST surface's struct tags: a masked or sorted path outside it fails
// Build as a SchemaViolation — never reaching the reader, where an
// unresolved path would otherwise pass through as a physical column and
// bypass ToCriteria overlays such as Restrict.
func (b *CriteriaBuilder) Fields(wireToGo map[string]string) *CriteriaBuilder {
	b.fields = wireToGo
	return b
}

// ComputedFields declares which of the Fields entries are COMPUTED — a
// Response field carrying a `computed:"…"` tag, derived by the Query's
// FromQueryResult after the read instead of read from a column — mapped to the
// Go source paths that feed it. The Auto path passes the compiled plan's cut;
// a raw (MountRaw) consumer passes its own, or nothing at all.
//
// The declaration splits the two consumers of the vocabulary the way REST
// splits `?fields=` from `?orderBy=`:
//
//   - read_mask (FieldMask) over a computed path pushes its SOURCES down, so
//     the reader returns what the derivation needs; the computed path itself
//     resolves to no column and never reaches the store.
//   - order_by over a computed path is REFUSED with
//     ComputedFieldNotSortableNotification (semantic Schema →
//     INVALID_ARGUMENT): ordering happens in the store and the keyset cursor
//     is built from stored ordering values, so a derived value is not
//     expressible.
func (b *CriteriaBuilder) ComputedFields(wireToSources map[string][]string) *CriteriaBuilder {
	b.computed = wireToSources
	return b
}

// goField resolves one wire path against the declared vocabulary.
func (b *CriteriaBuilder) goField(kind, wirePath string) (string, bool) {
	goPath, ok := b.fields[wirePath]
	if !ok {
		b.errs = append(b.errs, fmt.Errorf("%s path %q is not a declared field of this view", kind, wirePath))
		return "", false
	}
	return goPath, true
}

// Page applies the control keys. nil is a no-op (message absent). Presence
// is recorded for the canonical control gateway — every PaginationRequest
// field is proto3 optional, so presence and value separate exactly as on
// the REST query string: an explicitly-set empty search is still a
// presence, and an explicit only_total=false / include_archived=false is
// presence without activation (gated like REST's `?onlyTotal=false`).
func (b *CriteriaBuilder) Page(p *pb.PaginationRequest) *CriteriaBuilder {
	if p == nil {
		return b
	}
	if p.After != nil {
		b.controls.After = true
	}
	if p.Before != nil {
		b.controls.Before = true
	}
	if p.First != nil {
		n := p.GetFirst()
		b.controls.First = &n
	}
	if p.Last != nil {
		n := p.GetLast()
		b.controls.Last = &n
	}
	if p.Search != nil {
		b.controls.Search = true
	}
	if p.IncludeArchived != nil {
		b.controls.IncludeArchived = true
	}
	if p.OnlyTotal != nil {
		active := p.GetOnlyTotal()
		b.controls.OnlyTotal = &active
	}
	b.crit.After = p.GetAfter()
	b.crit.Before = p.GetBefore()
	// Materialize the Relay direction pair into the internal size+direction:
	// first=N → forward window; last=N → backward window (with no cursor, the
	// LAST N of the set). The mutual exclusivity is the gateway's check.
	if p.First != nil {
		b.crit.Limit = p.GetFirst()
	}
	if p.Last != nil {
		b.crit.Limit = p.GetLast()
		b.crit.Backward = true
	}
	b.crit.OnlyTotal = p.GetOnlyTotal()
	b.crit.IncludeArchived = p.GetIncludeArchived()
	b.crit.Search = p.GetSearch()
	return b
}

// OrderBy appends ordering fields in the declared order. Field names are WIRE
// names (proto snake_case), resolved against the Fields vocabulary — an
// undeclared field fails Build as a SchemaViolation, a COMPUTED one (see
// ComputedFields) as a ComputedFieldNotSortableNotification.
func (b *CriteriaBuilder) OrderBy(fields ...*pb.OrderByField) *CriteriaBuilder {
	for _, f := range fields {
		if f == nil || f.GetField() == "" {
			continue
		}
		b.controls.OrderBy = true
		// A computed field IS in the vocabulary (it is selectable), so the
		// refusal has to come first — otherwise the resolution below would
		// hand the reader a Go path with no column behind it.
		if _, isComputed := b.computed[f.GetField()]; isComputed {
			b.computedSorts = append(b.computedSorts, f.GetField())
			continue
		}
		goPath, ok := b.goField("orderBy", f.GetField())
		if !ok {
			continue
		}
		b.crit.OrderBy = append(b.crit.OrderBy, queries.OrderByField{Field: goPath, Desc: f.GetDesc()})
	}
	return b
}

// FieldMask applies the partial-response projection (the ?fields= sibling;
// the conventional request field name is `fields`). Paths are WIRE names
// (FieldMask's canonical snake_case — protojson converts the JSON camelCase
// form to snake for you), resolved against the Fields vocabulary; an
// undeclared path fails Build as a SchemaViolation. A COMPUTED path (see
// ComputedFields) contributes its SOURCES instead of itself — the REST
// `?fields=<computed>` pushdown, on the proto wire.
func (b *CriteriaBuilder) FieldMask(m *fieldmaskpb.FieldMask) *CriteriaBuilder {
	if m == nil || len(m.GetPaths()) == 0 {
		return b
	}
	b.controls.Fields = true
	if b.crit.Projection == nil {
		b.crit.Projection = map[string]int{}
	}
	for _, path := range m.GetPaths() {
		if path == "" {
			continue
		}
		goPath, ok := b.goField("fields", path)
		if !ok {
			continue
		}
		if sources, isComputed := b.computed[path]; isComputed {
			for _, src := range sources {
				b.crit.Projection[src] = 1
			}
			continue
		}
		b.crit.Projection[goPath] = 1
	}
	return b
}

// String wires a StringFilter onto goFieldPath. nil / empty filters are
// no-ops (field not sent).
func (b *CriteriaBuilder) String(goFieldPath string, f *pb.StringFilter) *CriteriaBuilder {
	if f == nil {
		return b
	}
	spec := queryschema.FilterSpec{DocPath: goFieldPath, GoKind: reflect.String}
	for _, c := range f.GetConditions() {
		op, ok := stringOps[c.GetOp()]
		if !ok {
			b.errs = append(b.errs, fmt.Errorf("filter %q: string op %v is not valid", goFieldPath, c.GetOp()))
			continue
		}
		queryschema.ApplyFilterValues(b.crit.Filter, spec, op, c.GetValues())
	}
	return b
}

// Int64 wires an Int64Filter onto goFieldPath.
func (b *CriteriaBuilder) Int64(goFieldPath string, f *pb.Int64Filter) *CriteriaBuilder {
	if f == nil {
		return b
	}
	spec := queryschema.FilterSpec{DocPath: goFieldPath, GoKind: reflect.Int64}
	for _, c := range f.GetConditions() {
		op, ok := numberOps[c.GetOp()]
		if !ok {
			b.errs = append(b.errs, fmt.Errorf("filter %q: number op %v is not valid", goFieldPath, c.GetOp()))
			continue
		}
		values := make([]string, len(c.GetValues()))
		for i, v := range c.GetValues() {
			values[i] = strconv.FormatInt(v, 10)
		}
		queryschema.ApplyFilterValues(b.crit.Filter, spec, op, values)
	}
	return b
}

// Double wires a DoubleFilter onto goFieldPath.
func (b *CriteriaBuilder) Double(goFieldPath string, f *pb.DoubleFilter) *CriteriaBuilder {
	if f == nil {
		return b
	}
	spec := queryschema.FilterSpec{DocPath: goFieldPath, GoKind: reflect.Float64}
	for _, c := range f.GetConditions() {
		op, ok := numberOps[c.GetOp()]
		if !ok {
			b.errs = append(b.errs, fmt.Errorf("filter %q: number op %v is not valid", goFieldPath, c.GetOp()))
			continue
		}
		values := make([]string, len(c.GetValues()))
		for i, v := range c.GetValues() {
			values[i] = strconv.FormatFloat(v, 'f', -1, 64)
		}
		queryschema.ApplyFilterValues(b.crit.Filter, spec, op, values)
	}
	return b
}

// Bool wires a BoolFilter onto goFieldPath.
func (b *CriteriaBuilder) Bool(goFieldPath string, f *pb.BoolFilter) *CriteriaBuilder {
	if f == nil {
		return b
	}
	spec := queryschema.FilterSpec{DocPath: goFieldPath, GoKind: reflect.Bool}
	for _, c := range f.GetConditions() {
		op, ok := boolOps[c.GetOp()]
		if !ok {
			b.errs = append(b.errs, fmt.Errorf("filter %q: bool op %v is not valid", goFieldPath, c.GetOp()))
			continue
		}
		queryschema.ApplyFilterValues(b.crit.Filter, spec, op, []string{strconv.FormatBool(c.GetValue())})
	}
	return b
}

// Timestamp wires a TimestampFilter onto goFieldPath. Values convert to the
// RFC3339 string form the REST string-leaf coercion produces, so both wires
// filter date-carrying string columns identically.
func (b *CriteriaBuilder) Timestamp(goFieldPath string, f *pb.TimestampFilter) *CriteriaBuilder {
	if f == nil {
		return b
	}
	spec := queryschema.FilterSpec{DocPath: goFieldPath, GoKind: reflect.String}
	for _, c := range f.GetConditions() {
		op, ok := numberOps[c.GetOp()]
		if !ok {
			b.errs = append(b.errs, fmt.Errorf("filter %q: timestamp op %v is not valid", goFieldPath, c.GetOp()))
			continue
		}
		values := make([]string, 0, len(c.GetValues()))
		for _, ts := range c.GetValues() {
			if ts == nil {
				continue
			}
			values = append(values, ts.AsTime().UTC().Format(time.RFC3339Nano))
		}
		queryschema.ApplyFilterValues(b.crit.Filter, spec, op, values)
	}
	return b
}

// rawPathReserved is the permissive Reserved set the raw path builds
// against: MountRaw consumers carry no Request DTO, so the opt-in gate has
// no declaration to check — only the directional rule and the only-total
// conflict matrix apply. The Auto path runs the FULL gate (its compiled
// plan carries the DTO's Reserved set) before Build.
var rawPathReserved = map[string]bool{
	queryschema.KeyFirst: true, queryschema.KeyLast: true,
	queryschema.KeyAfter: true, queryschema.KeyBefore: true,
	queryschema.KeyOrderBy: true, queryschema.KeyFields: true,
	queryschema.KeySearch: true, queryschema.KeyIncludeArchived: true,
	queryschema.KeyOnlyTotal: true,
}

// Build returns the criteria, or the accumulated wire-contract violations —
// the wrappers' conversionError path renders them as SchemaViolation
// (INVALID_ARGUMENT), the same rejection an unknown REST operator gets.
// The canonical control gateway runs here for the raw path (directional
// rule + only-total conflicts); the Auto path additionally applies the DTO
// opt-in gate at its compiled plan before reaching Build. A sort over a
// COMPUTED field is refused right after the gateway, as its own typed
// notification rather than a prose error.
func (b *CriteriaBuilder) Build() (queries.ReadCriteria, error) {
	if violations := queryschema.ValidateControls(rawPathReserved, b.controls, nil); len(violations) > 0 {
		return queries.ReadCriteria{}, controlViolationError(violations)
	}
	if len(b.computedSorts) > 0 {
		return queries.ReadCriteria{}, computedSortError(b.computedSorts)
	}
	if len(b.errs) > 0 {
		return queries.ReadCriteria{}, fmt.Errorf("grpc criteria: %w", errors.Join(b.errs...))
	}
	return b.crit, nil
}

// controlViolationError renders gateway violations as the framework's typed
// notification error — a NotificationCarrier, so conversionError preserves
// the per-field triple and the Connect shell maps it to INVALID_ARGUMENT
// with google.rpc details, the gRPC self-translation of the same canonical
// rejection REST serves as a 400 envelope. A NotDeclared violation carries
// the missing `query:"…"` declaration as the field value, so the consumer
// reads the fix straight off the error detail.
func controlViolationError(violations []queryschema.ControlViolation) error {
	nctx := domain.NewNotificationContext("Schema")
	for _, v := range violations {
		msg := v.Message()
		if v.Kind == queryschema.ViolationNotDeclared {
			msg.FieldValue = fmt.Sprintf("control not enabled on this endpoint — declare query:%q on its Request DTO", v.Key)
		}
		nctx.AddNotificationMessage(msg)
	}
	return domain.NewDomainError([]*domain.NotificationContext{nctx})
}

// computedSortError renders the refused computed-field sort entries the same
// way controlViolationError renders a gateway violation — a
// NotificationCarrier under the "Schema" context, so conversionError funnels
// it through the pipeline's translation and the Connect shell emits
// INVALID_ARGUMENT (semantic Schema) with one google.rpc ErrorInfo per entry,
// reason=ComputedFieldNotSortableNotification. The field name is the sort
// entry exactly as the wire spelled it (proto snake_case), the gRPC dialect of
// REST's `orderBy[<token>]`.
func computedSortError(entries []string) error {
	nctx := domain.NewNotificationContext("Schema")
	for _, entry := range entries {
		nctx.AddNotificationMessage(domain.NotificationMessage{
			FieldName:    entry,
			Notification: domain.ComputedFieldNotSortableNotification{},
		})
	}
	return domain.NewDomainError([]*domain.NotificationContext{nctx})
}

var stringOps = map[pb.StringOp]string{
	pb.StringOp_STRING_OP_EQ:          queryschema.OpEq,
	pb.StringOp_STRING_OP_NE:          queryschema.OpNe,
	pb.StringOp_STRING_OP_IN:          queryschema.OpIn,
	pb.StringOp_STRING_OP_NIN:         queryschema.OpNin,
	pb.StringOp_STRING_OP_STARTSWITH:  queryschema.OpStartsWith,
	pb.StringOp_STRING_OP_CONTAINS:    queryschema.OpContains,
	pb.StringOp_STRING_OP_IEQ:         queryschema.OpIEq,
	pb.StringOp_STRING_OP_INE:         queryschema.OpINe,
	pb.StringOp_STRING_OP_IIN:         queryschema.OpIIn,
	pb.StringOp_STRING_OP_ININ:        queryschema.OpINin,
	pb.StringOp_STRING_OP_ISTARTSWITH: queryschema.OpIStartsWith,
	pb.StringOp_STRING_OP_ICONTAINS:   queryschema.OpIContains,
}

var numberOps = map[pb.NumberOp]string{
	pb.NumberOp_NUMBER_OP_EQ:  queryschema.OpEq,
	pb.NumberOp_NUMBER_OP_NE:  queryschema.OpNe,
	pb.NumberOp_NUMBER_OP_IN:  queryschema.OpIn,
	pb.NumberOp_NUMBER_OP_NIN: queryschema.OpNin,
	pb.NumberOp_NUMBER_OP_GT:  queryschema.OpGt,
	pb.NumberOp_NUMBER_OP_GTE: queryschema.OpGte,
	pb.NumberOp_NUMBER_OP_LT:  queryschema.OpLt,
	pb.NumberOp_NUMBER_OP_LTE: queryschema.OpLte,
}

var boolOps = map[pb.BoolOp]string{
	pb.BoolOp_BOOL_OP_EQ: queryschema.OpEq,
	pb.BoolOp_BOOL_OP_NE: queryschema.OpNe,
}
