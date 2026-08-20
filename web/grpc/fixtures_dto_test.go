package grpc

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	fwresponses "github.com/ClaudioSchirmer/omnicore/web/responses"
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
// wires), and its reserved keys are the control opt-in the gateway
// enforces (no `search` — that stays the searchGadgetsWithSearchDTO
// variant's opt-in).
type searchGadgetsDTO struct {
	Name      *string  `query:"name"      filter:"eq,icontains" sort:"asc,desc"`
	Rating    *int64   `query:"rating"    filter:"gte,lte" sort:"asc,desc"`
	Price     *float64 `query:"price"     filter:"gt,lt"`
	Active    *bool    `query:"active"    filter:"eq"`
	CreatedAt *string  `query:"createdAt" filter:"gte" sort:"asc,desc"`

	First           *int64  `query:"first"`
	Last            *int64  `query:"last"`
	After           *string `query:"after"`
	Before          *string `query:"before"`
	Fields          *string `query:"fields"`
	IncludeArchived *bool   `query:"includeArchived"`
	OnlyTotal       *bool   `query:"onlyTotal"`
}

func (searchGadgetsDTO) ToQuery(c queries.ReadCriteria) *searchGadgetsQuery {
	return &searchGadgetsQuery{Criteria: c}
}

// gadgetSearchResult is the application-layer Result of the search list —
// pure data, NO wire tags, fields named exactly like the view document's Go
// keys (the read-side Result seat of the fixture trio).
type gadgetSearchResult struct {
	ID   string
	Name string
	Kind *string
}

type searchGadgetsQuery struct {
	queries.QueryWithParamsBase
	Criteria queries.ReadCriteria
}

func (q searchGadgetsQuery) ToCriteria(*configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

func (q searchGadgetsQuery) FromQueryResult(_ *configuration.AppContext, r gadgetSearchResult) (gadgetSearchResult, error) {
	return r, nil
}

type searchGadgetsHandler struct{ sawCriteria *queries.ReadCriteria }

func (h searchGadgetsHandler) Handle(ctx *configuration.AppContext, q *searchGadgetsQuery) (queries.PageOf[gadgetSearchResult], error) {
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return queries.PageOf[gadgetSearchResult]{}, err
	}
	if h.sawCriteria != nil {
		*h.sawCriteria = crit
	}
	// A reader-level Page filled into the typed PageOf through the same
	// doc→Result seam the framework handler uses (ResultFromDoc + FromQueryResult).
	page := queries.Page{
		Items:       []map[string]any{{"ID": "g-1", "Name": "Drill", "Kind": "tool"}},
		TotalCount:  1,
		EndCursor:   "next-c",
		StartCursor: "prev-c",
		HasNextPage: true,
	}
	return queries.PageOfFrom(page, queries.FromQueryResultFiller[gadgetSearchResult](ctx, q))
}

// searchGadgetsWithSearchDTO is searchGadgetsDTO plus the `query:"search"`
// opt-in — the DTO shape that unlocks PaginationRequest.search, mirroring
// the REST Reserved gate.
type searchGadgetsWithSearchDTO struct {
	Name      *string  `query:"name"      filter:"eq,icontains" sort:"asc,desc"`
	Rating    *int64   `query:"rating"    filter:"gte,lte" sort:"asc,desc"`
	Price     *float64 `query:"price"     filter:"gt,lt"`
	Active    *bool    `query:"active"    filter:"eq"`
	CreatedAt *string  `query:"createdAt" filter:"gte" sort:"asc,desc"`
	Search    *string  `query:"search"`

	First           *int64  `query:"first"`
	Last            *int64  `query:"last"`
	After           *string `query:"after"`
	Before          *string `query:"before"`
	Fields          *string `query:"fields"`
	IncludeArchived *bool   `query:"includeArchived"`
	OnlyTotal       *bool   `query:"onlyTotal"`
}

func (searchGadgetsWithSearchDTO) ToQuery(c queries.ReadCriteria) *searchGadgetsQuery {
	return &searchGadgetsQuery{Criteria: c}
}

// gadgetItemDTO is the list Response DTO — the read_mask/sort vocabulary
// AND the Result→item projection target (the responseProjection seat).
type gadgetItemDTO struct {
	fwresponses.Auto
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Kind *string `json:"kind,omitempty"`
}

func (gadgetItemDTO) FromResult(r gadgetSearchResult) gadgetItemDTO {
	return fwresponses.AutoFromResult[gadgetItemDTO](r)
}

type getGadgetDTO struct {
	IncludeArchived bool `query:"includeArchived"`
}

func (d getGadgetDTO) ToQuery(criteria queries.ReadCriteria) *getGadgetQuery {
	return &getGadgetQuery{Criteria: criteria}
}

// gadgetDetailResult is the by-id Result — the typed value the QueryByID
// handler returns (the read-side twin of a command's Result struct).
type gadgetDetailResult struct {
	ID              string
	Name            string
	IncludeArchived bool
}

type getGadgetResponseDTO struct {
	fwresponses.Auto
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (getGadgetResponseDTO) FromResult(r gadgetDetailResult) getGadgetResponseDTO {
	return fwresponses.AutoFromResult[getGadgetResponseDTO](r)
}
