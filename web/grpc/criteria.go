package grpc

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
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
	// sortable is the ordering vocabulary: wire (proto snake_case) → the Go
	// field path and the directions the Request DTO declared. Separate from
	// fields because the two answer different questions — fields is what the
	// read_mask may project, sortable is what order_by may order by, and a
	// path may be one without being the other.
	sortable map[string]queryschema.SortSpec
	// sortViolations collects the order_by entries the vocabulary refused —
	// undeclared path, disallowed direction, or a path named twice. They are
	// rendered as TYPED notifications rather than prose, so the consumer reads
	// WHICH entry was refused off the error detail, exactly as REST reads it
	// off `orderBy[<token>]`.
	sortViolations []string
	errs           []error
	// masked records the read_mask's wire paths, so HiddenComputedSources can
	// tell "the mask asked for this" from "we read it as a computed source".
	masked map[string]bool
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
//   - order_by is a separate vocabulary (see Sortable): a computed path is
//     not part of it, because ordering happens in the store and a derived
//     value backs no column there.
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
// names (proto snake_case), resolved against the Sortable vocabulary — a field
// the Request DTO did not declare orderable, one asked for in a direction its
// declaration does not admit, or one named twice (the terms become the
// reader's sort document, where a duplicated key is malformed) fails Build as
// a SchemaViolation naming the offending entry.
func (b *CriteriaBuilder) OrderBy(fields ...*pb.OrderByField) *CriteriaBuilder {
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		if f == nil || f.GetField() == "" {
			continue
		}
		b.controls.OrderBy = true
		spec, declared := b.sortableSpec(f.GetField())
		if !declared || !spec.Allows(f.GetDesc()) || seen[f.GetField()] {
			b.sortViolations = append(b.sortViolations, f.GetField())
			continue
		}
		seen[f.GetField()] = true
		b.crit.OrderBy = append(b.crit.OrderBy, queries.OrderByField{Field: spec.GoPath, Desc: f.GetDesc()})
	}
	return b
}

// Sortable declares the ordering vocabulary — wire path → the Go field path it
// addresses plus the directions admitted. The Auto path feeds it from the
// Request DTO's `sort:` tags; a MountRaw consumer declares its own.
//
// Not calling it means nothing is orderable, which is the framework-wide
// default: ordering is a store operation whose cost is proportional to the
// matching set unless an index covers it, so an endpoint sorts by what it says
// it sorts by and nothing else.
func (b *CriteriaBuilder) Sortable(vocabulary map[string]queryschema.SortSpec) *CriteriaBuilder {
	b.sortable = vocabulary
	return b
}

// sortableSpec resolves one wire path against the declared vocabulary.
func (b *CriteriaBuilder) sortableSpec(wirePath string) (queryschema.SortSpec, bool) {
	spec, ok := b.sortable[wirePath]
	return spec, ok
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
	if b.masked == nil {
		b.masked = map[string]bool{}
	}
	for _, path := range m.GetPaths() {
		if path == "" {
			continue
		}
		b.masked[path] = true
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

// HiddenComputedSources returns the Result Go paths that were read ONLY to
// feed a masked computed field and that the mask did not name — the gRPC
// twin of queryschema.UnrequestedComputedSources. A computed field's sources
// must reach the store, but the read_mask shapes the WIRE: a source that is
// itself a declared field of the vocabulary would otherwise render beside
// the computed value even though the mask never named it. The Auto path
// blanks these paths on each Result before projection
// (queryschema.BlankResultPaths); a MountRaw consumer that maps by hand
// calls this after Build and does the same, keeping raw parity with
// `?fields=` on REST.
//
// A source the Response never declares needs no blanking — it has no wire
// slot to leak into. Returns nil when no mask was applied, nothing masked is
// computed, or every source was masked outright. Deterministic order.
func (b *CriteriaBuilder) HiddenComputedSources() []string {
	if len(b.computed) == 0 || len(b.masked) == 0 {
		return nil
	}
	// Go path → wire path, to answer "does this source have a wire slot".
	wireByGo := make(map[string]string, len(b.fields))
	for wire, goPath := range b.fields {
		wireByGo[goPath] = wire
	}
	hidden := map[string]bool{}
	for wire := range b.masked {
		sources, isComputed := b.computed[wire]
		if !isComputed {
			continue
		}
		for _, src := range sources {
			srcWire, onTheWire := wireByGo[src]
			if !onTheWire {
				continue // read-only source: no wire slot, nothing to hide
			}
			if b.masked[srcWire] {
				continue // the mask asked for it too
			}
			hidden[src] = true
		}
	}
	if len(hidden) == 0 {
		return nil
	}
	out := make([]string, 0, len(hidden))
	for p := range hidden {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
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
// opt-in gate at its compiled plan before reaching Build.
func (b *CriteriaBuilder) Build() (queries.ReadCriteria, error) {
	if violations := queryschema.ValidateControls(rawPathReserved, b.controls, nil); len(violations) > 0 {
		return queries.ReadCriteria{}, controlViolationError(violations)
	}
	if len(b.sortViolations) > 0 {
		return queries.ReadCriteria{}, sortViolationError(b.sortViolations)
	}
	if len(b.errs) > 0 {
		return queries.ReadCriteria{}, fmt.Errorf("grpc criteria: %w", errors.Join(b.errs...))
	}
	return b.crit, nil
}

// sortViolationError renders the refused order_by entries the same way
// controlViolationError renders a gateway violation — a NotificationCarrier
// under the "Schema" context, so conversionError funnels it through the
// pipeline's translation and the Connect shell emits INVALID_ARGUMENT with one
// google.rpc detail per entry. The field name is the entry exactly as the wire
// spelled it (proto snake_case), the gRPC dialect of REST's
// `orderBy[<token>]`: order_by is already a typed field on the request
// message, so the bracket prefix would name nothing on this wire.
func sortViolationError(entries []string) error {
	nctx := domain.NewNotificationContext("Schema")
	for _, entry := range entries {
		nctx.AddNotificationMessage(domain.NotificationMessage{
			FieldName:    entry,
			Notification: domain.SchemaViolationNotification{},
		})
	}
	return domain.NewDomainError([]*domain.NotificationContext{nctx})
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
