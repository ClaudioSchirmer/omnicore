package grpc

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	pb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// CriteriaBuilder converts the shared omnicore.v1 read-side components
// (PaginationRequest, SortField, FieldMask, the typed filter wrappers) into a
// queries.ReadCriteria — the INPUT criteria, exactly what the REST parser
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
//	    Sort(pb.GetSort()...).
//	    ReadMask(pb.GetReadMask()).
//	    String("UserName", pb.GetFilters().GetUserName()).
//	    Build()
type CriteriaBuilder struct {
	crit   queries.ReadCriteria
	fields map[string]string // wire (proto snake_case) → Go field path
	errs   []error
}

// NewCriteria starts a builder with an empty filter map.
func NewCriteria() *CriteriaBuilder {
	return &CriteriaBuilder{crit: queries.ReadCriteria{Filter: map[string]any{}}}
}

// Fields declares the view's wire vocabulary for read_mask and sort: proto
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

// goField resolves one wire path against the declared vocabulary.
func (b *CriteriaBuilder) goField(kind, wirePath string) (string, bool) {
	goPath, ok := b.fields[wirePath]
	if !ok {
		b.errs = append(b.errs, fmt.Errorf("%s path %q is not a declared field of this view", kind, wirePath))
		return "", false
	}
	return goPath, true
}

// Page applies the control keys. nil is a no-op (message absent).
func (b *CriteriaBuilder) Page(p *pb.PaginationRequest) *CriteriaBuilder {
	if p == nil {
		return b
	}
	// The REST onlyTotal conflict matrix, verbatim: a count-only request
	// carrying page-shaping controls is a wire-contract violation, rejected
	// eagerly (silent ignore would hide the consumer's bug). Presence-based,
	// like the query-string check — proto3 optional carries presence.
	if p.GetOnlyTotal() {
		for _, c := range []struct {
			key     string
			present bool
		}{
			{"after", p.After != nil},
			{"before", p.Before != nil},
			{"limit", p.Limit != nil},
		} {
			if c.present {
				b.errs = append(b.errs, fmt.Errorf("onlyTotal[%s]: incompatible with only_total=true", c.key))
			}
		}
	}
	b.crit.After = p.GetAfter()
	b.crit.Before = p.GetBefore()
	b.crit.Limit = p.GetLimit()
	b.crit.OnlyTotal = p.GetOnlyTotal()
	b.crit.IncludeArchived = p.GetIncludeArchived()
	b.crit.Search = p.GetSearch()
	return b
}

// Sort appends sort fields in the declared order. Field names are WIRE
// names (proto snake_case), resolved against the Fields vocabulary — an
// undeclared field fails Build as a SchemaViolation.
func (b *CriteriaBuilder) Sort(fields ...*pb.SortField) *CriteriaBuilder {
	for _, f := range fields {
		if f == nil || f.GetField() == "" {
			continue
		}
		goPath, ok := b.goField("sort", f.GetField())
		if !ok {
			continue
		}
		b.crit.Sort = append(b.crit.Sort, queries.SortField{Field: goPath, Desc: f.GetDesc()})
	}
	return b
}

// ReadMask applies the partial-response projection (the ?fields= sibling).
// Paths are WIRE names (FieldMask's canonical snake_case — protojson
// converts the JSON camelCase form to snake for you), resolved against the
// Fields vocabulary; an undeclared path fails Build as a SchemaViolation.
func (b *CriteriaBuilder) ReadMask(m *fieldmaskpb.FieldMask) *CriteriaBuilder {
	if m == nil || len(m.GetPaths()) == 0 {
		return b
	}
	if b.crit.Projection == nil {
		b.crit.Projection = map[string]int{}
	}
	for _, path := range m.GetPaths() {
		if path == "" {
			continue
		}
		goPath, ok := b.goField("read_mask", path)
		if !ok {
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

// Build returns the criteria, or the accumulated wire-contract violations —
// the wrappers' conversionError path renders them as SchemaViolation
// (INVALID_ARGUMENT), the same rejection an unknown REST operator gets.
func (b *CriteriaBuilder) Build() (queries.ReadCriteria, error) {
	// Sort/read_mask arrive through their own builder calls, so their
	// onlyTotal conflicts are only visible here — the page-key conflicts
	// fire in Page. Same matrix as the REST wrapper.
	if b.crit.OnlyTotal {
		if len(b.crit.Sort) > 0 {
			b.errs = append(b.errs, fmt.Errorf("onlyTotal[sort]: incompatible with only_total=true"))
		}
		if b.crit.Projection != nil {
			b.errs = append(b.errs, fmt.Errorf("onlyTotal[read_mask]: incompatible with only_total=true"))
		}
	}
	if len(b.errs) > 0 {
		return queries.ReadCriteria{}, fmt.Errorf("grpc criteria: %w", errors.Join(b.errs...))
	}
	return b.crit, nil
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
