package grpc

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// The DTO seats the PUBLIC Auto constructors consume — the same shapes a
// real service already declares for its REST surface (json/query/filter
// tags), so these fixtures double as the reference for the bridge
// semantics: presence-aware optionals, normalized name matching, filter
// tags as the operator allowlist.

type createGadgetDTO struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Rating int32  `json:"rating"`
}

func (d createGadgetDTO) ToCommand() *createGadgetCommand {
	return &createGadgetCommand{Name: d.Name, Kind: d.Kind, Rating: d.Rating}
}

type gadgetResponseDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (gadgetResponseDTO) FromResult(r *gadgetResult) gadgetResponseDTO {
	return gadgetResponseDTO{ID: r.ID, Name: r.Name}
}

type updateGadgetDTO struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (d updateGadgetDTO) ToCommand() *updateGadgetCommand {
	return &updateGadgetCommand{Name: d.Name}
}

// searchGadgetsDTO is the list Request DTO: its `filter:` tags are the
// operator allowlist the proto filters inherit (one vocabulary, both
// wires).
type searchGadgetsDTO struct {
	Name      *string  `query:"name"      filter:"eq,icontains"`
	Rating    *int64   `query:"rating"    filter:"gte,lte"`
	Price     *float64 `query:"price"     filter:"gt,lt"`
	Active    *bool    `query:"active"    filter:"eq"`
	CreatedAt *string  `query:"createdAt" filter:"gte"`
}

func (searchGadgetsDTO) ToQuery(c queries.ReadCriteria) *searchGadgetsQuery {
	return &searchGadgetsQuery{Criteria: c}
}

type searchGadgetsQuery struct {
	queries.QueryWithParamsBase
	Criteria queries.ReadCriteria
}

func (q searchGadgetsQuery) ToCriteria(*configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

type searchGadgetsHandler struct{ sawCriteria *queries.ReadCriteria }

func (h searchGadgetsHandler) Handle(ctx *configuration.AppContext, q *searchGadgetsQuery) (queries.Page, error) {
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return queries.Page{}, err
	}
	if h.sawCriteria != nil {
		*h.sawCriteria = crit
	}
	return queries.Page{
		Items:      []map[string]any{{"ID": "g-1", "Name": "Drill", "Kind": "tool"}},
		Total:      1,
		NextCursor: "next-c",
		PrevCursor: "prev-c",
		HasNext:    true,
	}, nil
}

// searchGadgetsWithSearchDTO is searchGadgetsDTO plus the `query:"search"`
// opt-in — the DTO shape that unlocks PaginationRequest.search, mirroring
// the REST Reserved gate.
type searchGadgetsWithSearchDTO struct {
	Name      *string  `query:"name"      filter:"eq,icontains"`
	Rating    *int64   `query:"rating"    filter:"gte,lte"`
	Price     *float64 `query:"price"     filter:"gt,lt"`
	Active    *bool    `query:"active"    filter:"eq"`
	CreatedAt *string  `query:"createdAt" filter:"gte"`
	Search    *string  `query:"search"`
}

func (searchGadgetsWithSearchDTO) ToQuery(c queries.ReadCriteria) *searchGadgetsQuery {
	return &searchGadgetsQuery{Criteria: c}
}

// gadgetItemDTO is the list Response DTO — the read_mask/sort vocabulary
// AND the doc→item projection target (fwresponses.AutoFromDoc seat).
type gadgetItemDTO struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Kind *string `json:"kind,omitempty"`
}

type getGadgetDTO struct {
	IncludeArchived bool `query:"includeArchived"`
}

func (d getGadgetDTO) ToQuery() *getGadgetQuery {
	return &getGadgetQuery{IncludeArchived: d.IncludeArchived}
}

type getGadgetResponseDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
