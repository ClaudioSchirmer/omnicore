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
	// in is the surface-neutral request this builder accumulates. Build hands
	// it to queryschema.BuildCriteria, which owns every rule: the opt-in gate,
	// the directional rule, the only-total matrix, the operator allowlist and
	// the ordering vocabulary. This surface owns only how proto spells a value.
	in queryschema.Read
	// declared is the vocabulary Build validates against, and it is ALWAYS
	// owned by this builder — never a schema someone else holds. The typed
	// filter calls below record what they were handed into declared.Filters,
	// so a schema shared with another surface would be mutated by every
	// request that carries a filter: a concurrent map write against the
	// process-wide ExtractRequestSchema cache, and a Go-path spelling leaking
	// into the wire vocabulary REST validates against. What the Auto path
	// borrows from the DTO is read-only and copied in by withSchema.
	declared *queryschema.RequestSchema
	fields   map[string]string   // wire (proto snake_case) → Go field path
	computed map[string][]string // wire → Go source paths (COMPUTED fields)
	// sortIndex folds the ordering vocabulary's keys to their normalized form
	// (case- and separator-insensitive), so resolving one wire token is a
	// single lookup instead of a scan that re-normalizes the whole vocabulary
	// per term. Built once where the vocabulary is set — at Register on the
	// Auto path, at the Sortable(...) call on the raw one.
	sortIndex map[string]string // normalized wire path → the declared spelling
	errs      []error
	// masked records the read_mask's wire paths, so HiddenComputedSources can
	// tell "the mask asked for this" from "we read it as a computed source".
	masked map[string]bool
}

// NewCriteria starts a builder that declares nothing yet: what it accepts is
// what the calls below name.
func NewCriteria() *CriteriaBuilder {
	return &CriteriaBuilder{declared: &queryschema.RequestSchema{
		Filters:  map[string]queryschema.FilterSpec{},
		Reserved: rawPathReserved,
		Sortable: map[string]queryschema.SortSpec{},
	}}
}

// withSchema hands the builder the read-only halves of a Request DTO's schema:
// the declared control set (the opt-in gate) and the ordering vocabulary. The
// Auto path uses it, so a procedure compiled from a DTO answers for that DTO
// exactly as its REST twin does; MountRaw declares its own, which is what "no
// DTO" means.
//
// The two maps are BORROWED and never written — a RequestSchema is memoized
// per reflect.Type and shared by every surface and every request, so writing to
// one is a data race by construction. The filter map is deliberately NOT
// borrowed: the typed filter calls record into it, so the builder gets a fresh
// one of its own. That costs the Auto path nothing, because the DTO's filter
// vocabulary is already enforced twice, earlier and by name: compileQueryPlan
// refuses at boot any proto filter field with no `filter:`-tagged counterpart,
// and filterBinding.apply refuses at request time any operator outside that
// leaf's tag. What reaches this builder has already passed the DTO's gate.
//
// sortIndex is the vocabulary's normalized key index, compiled once alongside
// the plan. A nil index means an empty vocabulary — every ordering token is
// then outside it and refused by name, which is the same answer the index
// would give.
func (b *CriteriaBuilder) withSchema(s *queryschema.RequestSchema, sortIndex map[string]string) *CriteriaBuilder {
	b.declared = &queryschema.RequestSchema{
		Filters:  map[string]queryschema.FilterSpec{},
		Reserved: s.Reserved,
		Sortable: s.Sortable,
	}
	b.sortIndex = sortIndex
	return b
}

// Controls exposes the canonical control snapshot the builder accumulated, for
// a consumer that wants to inspect what the wire asked for before Build decides
// what it means. The gate itself runs inside Build, on both paths — the full
// opt-in gate when a Request DTO is in play, the directional rule and the
// only-total conflict matrix always.
func (b *CriteriaBuilder) Controls() queryschema.Controls {
	return b.in.Controls
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
		b.in.Controls.After = true
	}
	if p.Before != nil {
		b.in.Controls.Before = true
	}
	if p.First != nil {
		n := p.GetFirst()
		b.in.Controls.First = &n
	}
	if p.Last != nil {
		n := p.GetLast()
		b.in.Controls.Last = &n
	}
	if p.Search != nil {
		b.in.Controls.Search = true
	}
	if p.IncludeArchived != nil {
		b.in.Controls.IncludeArchived = true
	}
	if p.OnlyTotal != nil {
		active := p.GetOnlyTotal()
		b.in.Controls.OnlyTotal = &active
	}
	b.in.After = p.GetAfter()
	b.in.Before = p.GetBefore()
	b.in.IncludeArchived = p.GetIncludeArchived()
	b.in.Search = p.GetSearch()
	return b
}

// OrderBy appends ordering terms in the order given. Field names are WIRE
// names (proto snake_case); they are folded to the vocabulary's spelling here
// and travel with the entry verbatim, so a refusal names what the wire sent —
// the gRPC dialect of REST's `orderBy[<token>]` (order_by is already a typed
// field on the request message, so a bracket prefix would name nothing here).
//
// Whether the path is orderable, in that direction, and only once, is decided
// by the shared assembler at Build.
func (b *CriteriaBuilder) OrderBy(fields ...*pb.OrderByField) *CriteriaBuilder {
	for _, f := range fields {
		if f == nil {
			continue
		}
		// PRESENCE is the entry being on the wire, not its field being
		// non-empty — the same cut REST makes, where `?orderBy=` is the control
		// asked for with no answer. Recording it only for a usable entry would
		// let an endpoint that never declared the control IGNORE what its REST
		// twin refuses; carrying no term for it is what makes the two identical
		// once the control IS declared.
		b.in.Controls.OrderBy = true
		if f.GetField() == "" {
			continue
		}
		b.in.OrderBy = append(b.in.OrderBy, queryschema.OrderTerm{
			Path: b.vocabularyPath(f.GetField()),
			Desc: f.GetDesc(),
			Raw:  f.GetField(),
		})
	}
	return b
}

// vocabularyPath folds a proto field name onto the declared vocabulary's own
// spelling. The vocabulary arrives in the Request DTO's spelling (`createdAt`)
// and the wire sends the proto's (`created_at`); the two meet through
// normalizePath, the same equivalence the rest of this binder uses to match a
// proto field against its DTO twin. An unknown name is returned untouched and
// refused by the assembler, which names it back verbatim.
//
// The fold is one lookup against the index the vocabulary was compiled into,
// not a scan: an ordering carries several terms and the vocabulary several
// paths, and re-normalizing the whole vocabulary per term is work whose answer
// never changes between requests.
func (b *CriteriaBuilder) vocabularyPath(wire string) string {
	if path, ok := b.sortIndex[normalizePath(wire)]; ok {
		return path
	}
	return wire
}

// sortableIndex folds a vocabulary's keys to their normalized spelling, so
// vocabularyPath resolves a wire token with one lookup. A collision — two
// declared paths that normalize alike — keeps the first in sorted order, so the
// resolution is deterministic rather than map-iteration order.
func sortableIndex(vocabulary map[string]queryschema.SortSpec) map[string]string {
	paths := make([]string, 0, len(vocabulary))
	for path := range vocabulary {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	index := make(map[string]string, len(paths))
	for _, path := range paths {
		if norm := normalizePath(path); index[norm] == "" {
			index[norm] = path
		}
	}
	return index
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
	b.declared.Sortable = vocabulary
	b.sortIndex = sortableIndex(vocabulary)
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
	b.in.Controls.Fields = true
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
				b.in.Projection.Include(src)
			}
			continue
		}
		b.in.Projection.Include(goPath)
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

// filter records one operand set against the path it names. The typed methods
// above are how proto spells a value; the SPEC they declare is what makes the
// path part of this endpoint's vocabulary, which is what the assembler checks
// the operator against. A MountRaw consumer therefore declares its filters by
// calling for them, which is the raw path's whole contract.
//
// It WRITES to b.declared, which is why that schema is always this builder's
// own (see the field doc and withSchema): the Auto path borrows the DTO's
// control set and ordering vocabulary read-only and never its filter map.
func (b *CriteriaBuilder) filter(spec queryschema.FilterSpec, op string, values []string) {
	if _, already := b.declared.Filters[spec.DocPath]; !already {
		b.declared.Filters[spec.DocPath] = queryschema.FilterSpec{
			Ops: map[string]bool{}, DocPath: spec.DocPath, GoKind: spec.GoKind,
		}
	}
	b.declared.Filters[spec.DocPath].Ops[op] = true
	b.in.Filters = append(b.in.Filters, queryschema.FilterTerm{
		Path: spec.DocPath, Op: op, Values: values, Raw: spec.DocPath,
	})
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
		b.filter(spec, op, c.GetValues())
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
		b.filter(spec, op, values)
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
		b.filter(spec, op, values)
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
		b.filter(spec, op, []string{strconv.FormatBool(c.GetValue())})
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
		b.filter(spec, op, values)
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

// Build hands the accumulated request to queryschema.BuildCriteria — the one
// assembler every surface goes through — and renders its refusal in this
// wire's idiom.
//
// What the raw path validates against is the vocabulary the builder itself was
// given: the paths the typed filter calls named, the ordering vocabulary
// Sortable declared, and every control (a raw mount answers for its own
// contract). The Auto path swaps in the Request DTO's schema, so a procedure
// compiled from a DTO is gated by that DTO exactly as its REST twin is.
func (b *CriteriaBuilder) Build() (queries.ReadCriteria, error) {
	crit, _, violation, ok := queryschema.BuildCriteria(b.declared, nil, b.in)
	if !ok {
		return queries.ReadCriteria{}, violationError(violation)
	}
	if len(b.errs) > 0 {
		return queries.ReadCriteria{}, fmt.Errorf("grpc criteria: %w", errors.Join(b.errs...))
	}
	return crit, nil
}

// violationError renders one refusal as the framework's typed notification
// error — a NotificationCarrier, so conversionError funnels it through the
// pipeline's translation and the Connect shell emits INVALID_ARGUMENT with a
// google.rpc detail carrying the field.
//
// An ordering refusal drops REST's `orderBy[…]` wrapper and names the entry
// verbatim: order_by is already a typed field on this request message, so the
// prefix would name nothing here. Same rule, this wire's spelling.
func violationError(v *queryschema.Violation) error {
	msg := v.Message()
	if token, wrapped := queryschema.OrderByToken(msg.Override); wrapped {
		msg.Override = token
	}
	nctx := domain.NewNotificationContext(v.ContextName())
	nctx.AddNotificationMessage(msg)
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
