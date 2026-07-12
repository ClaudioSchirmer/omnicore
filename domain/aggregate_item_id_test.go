package domain

import "testing"

// noIDFieldAVO has no exported string "ID" field — the write-back must report
// false for it instead of panicking or silently corrupting the map.
type noIDFieldAVO struct {
	Num int
}

func (n noIDFieldAVO) GetID() string                            { return "" }
func (n noIDFieldAVO) BuildRules(_ string, _ Service, _ *Rules) {}

type assignIDRoot struct {
	AggregateRoot
}

func (r *assignIDRoot) GetAggregateRoot() *AggregateRoot { return &r.AggregateRoot }
func (r *assignIDRoot) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{testAVO{}, noIDFieldAVO{}}
}

func TestAssignAggregateItemID_StampsTrackedItem(t *testing.T) {
	root := &assignIDRoot{}
	AddAggregateChild(root, testAVO{Name: "a"})

	if ok := root.AssignAggregateItemID(testAVO{Name: "a"}, "id-1"); !ok {
		t.Fatal("expected write-back to find and stamp the tracked item")
	}

	items := GetCurrentItemsOf[testAVO](&root.AggregateRoot)
	if len(items) != 1 || items[0].ID != "id-1" || items[0].Name != "a" {
		t.Fatalf("expected the tracked item to carry the assigned id, got %+v", items)
	}
	// Statuses must be untouched — the item is still an OpInsert candidate.
	if added := GetAddedItemsOf[testAVO](&root.AggregateRoot); len(added) != 1 {
		t.Fatalf("expected the stamped item to remain Added, got %+v", added)
	}
}

func TestAssignAggregateItemID_MatchesPreAssignmentValueOnly(t *testing.T) {
	root := &assignIDRoot{}
	AddAggregateChild(root, testAVO{Name: "a"})
	if ok := root.AssignAggregateItemID(testAVO{Name: "a"}, "id-1"); !ok {
		t.Fatal("first stamp must succeed")
	}
	// The old (pre-stamp) value no longer matches…
	if ok := root.AssignAggregateItemID(testAVO{Name: "a"}, "id-2"); ok {
		t.Fatal("the pre-stamp value must no longer match after the id was assigned")
	}
	// …but the current (stamped) value does.
	if ok := root.AssignAggregateItemID(testAVO{ID: "id-1", Name: "a"}, "id-3"); !ok {
		t.Fatal("the stamped value must match for a re-assignment")
	}
	items := GetCurrentItemsOf[testAVO](&root.AggregateRoot)
	if len(items) != 1 || items[0].ID != "id-3" {
		t.Fatalf("expected the re-assigned id, got %+v", items)
	}
}

func TestAssignAggregateItemID_ReportsFalseWithoutMutating(t *testing.T) {
	root := &assignIDRoot{}
	AddAggregateChild(root, testAVO{Name: "a"})
	AddAggregateChild(root, noIDFieldAVO{Num: 7})

	cases := []struct {
		name string
		item AggregateValueObject
	}{
		{"untracked item", testAVO{Name: "zz"}},
		{"no settable string ID field", noIDFieldAVO{Num: 7}},
		{"nil item", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ok := root.AssignAggregateItemID(tc.item, "id-x"); ok {
				t.Fatalf("expected false for %s", tc.name)
			}
		})
	}

	if items := GetCurrentItemsOf[testAVO](&root.AggregateRoot); len(items) != 1 || items[0].ID != "" {
		t.Fatalf("failed assignments must not mutate tracked items, got %+v", items)
	}
}

func TestAssignAggregateItemID_EmptyRootReportsFalse(t *testing.T) {
	root := &assignIDRoot{}
	if ok := root.AssignAggregateItemID(testAVO{Name: "a"}, "id-1"); ok {
		t.Fatal("a root with no tracked items must report false")
	}
}
