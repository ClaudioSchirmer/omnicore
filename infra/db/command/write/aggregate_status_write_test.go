package write

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The mixed (originalStatus, currentStatus) cases the currentStatus-only reading
// got wrong, now driven through writeChildren via domain.OperationOf.

// A DB-loaded child removed then re-added (originalStatus Constructor, current
// Added) must UPDATE the existing row — NOT insert a duplicate.
func TestWriteChildren_ReAddedDBChildUpdatesNotInserts(t *testing.T) {
	id := uuid.NewString()
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{ID: id, Label: "x"}})
	upd, err := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
		domain.RemoveAggregateChild(r, aggWriteChild{ID: id, Label: "x"})
		domain.AddAggregateChild(r, aggWriteChild{ID: id, Label: "x"}) // re-add same → reactivate
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "INSERT INTO agg_w_children") {
			t.Errorf("a re-added DB child must NOT insert a duplicate, got %q", s)
		}
	}
	updated := false
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE agg_w_children") {
			updated = true
		}
	}
	if !updated {
		t.Errorf("a re-added DB child must UPDATE the existing row; execs=%v", tx.execs)
	}
}

// A brand-new child added then removed in the same write (originalStatus Added,
// current Removed) was never persisted → it must touch nothing.
func TestWriteChildren_NewThenRemovedIsNoop(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	upd, err := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
		domain.AddAggregateChild(r, aggWriteChild{Label: "tmp"})
		domain.RemoveAggregateChild(r, aggWriteChild{Label: "tmp"})
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, s := range tx.execs {
		if strings.Contains(s, "agg_w_children") {
			t.Errorf("a new-then-removed child must touch nothing, got %q", s)
		}
	}
}
