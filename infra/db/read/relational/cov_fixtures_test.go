package relational

import "github.com/ClaudioSchirmer/omnicore/domain"

// covAgg/covChild are the aggregate fixtures the loader children tests drive.
// (The write-layer audit tests keep their own copy — fixtures don't cross a
// package boundary.)
type covAgg struct {
	domain.AggregateRoot
	Name string
}

func (e *covAgg) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *covAgg) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *covAgg) GetAggregateRoot() *domain.AggregateRoot { return &e.AggregateRoot }
func (e *covAgg) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{covChild{}}
}

type covChild struct {
	ID    string
	Label string
}

func (c covChild) GetID() string                                    { return c.ID }
func (c covChild) BuildRules(string, domain.Service, *domain.Rules) {}

var covAggSchema = NewTableSchema[*covAgg]("cov_aggs").
	PK("id").
	Field("Name", "name").
	SoftDelete("deleted_at").
	Child(NewTableSchema[covChild]("cov_children").
		PK("id").
		FK("cov_agg_id").
		Field("Label", "label").
		SoftDelete("deleted_at"))
