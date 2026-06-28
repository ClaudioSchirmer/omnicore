package write

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// effectiveContextName resolution. These are pure unit tests of a db internal —
// relocated from the former db/pg integration file (they need no live database).

// ctxNameFlatPerson is a minimal flat entity whose Go type name "ctxNameFlatPerson"
// is what effectiveContextName derives when ContextName is unset.
type ctxNameFlatPerson struct {
	domain.BaseEntity
	Name string
}

func (e *ctxNameFlatPerson) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert}
}
func (e *ctxNameFlatPerson) BuildRules(string, domain.Service, *domain.Rules) {}

func TestBaseRepository_EffectiveContextName_FromTypeName(t *testing.T) {
	repo := &BaseRepository[*ctxNameFlatPerson]{
		NewEntity: func() *ctxNameFlatPerson { return &ctxNameFlatPerson{} },
	}
	if got := repo.effectiveContextName(); got != "ctxNameFlatPerson" {
		t.Errorf("effectiveContextName() = %q, want ctxNameFlatPerson", got)
	}
}

func TestBaseRepository_EffectiveContextName_Override(t *testing.T) {
	repo := &BaseRepository[*ctxNameFlatPerson]{
		NewEntity:   func() *ctxNameFlatPerson { return &ctxNameFlatPerson{} },
		ContextName: "OverrideName",
	}
	if got := repo.effectiveContextName(); got != "OverrideName" {
		t.Errorf("effectiveContextName() = %q, want OverrideName", got)
	}
}
