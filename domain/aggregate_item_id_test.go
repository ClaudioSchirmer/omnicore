package domain

import "testing"

type assignIDRoot struct {
	AggregateRoot
}

func (r *assignIDRoot) GetAggregateRoot() *AggregateRoot { return &r.AggregateRoot }
func (r *assignIDRoot) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{testAVO{}}
}

func TestAssignAggregateItemID_StampsTrackedItem(t *testing.T) {
	root := &assignIDRoot{}
	AddAggregateChild(root, testAVO{Name: "a"})

	if ok := root.AssignAggregateItemID(testAVO{Name: "a"}, "id-1"); !ok {
		t.Fatal("expected write-back to find and stamp the tracked item")
	}

	items := GetCurrentItemsOf[testAVO](&root.AggregateRoot)
	if len(items) != 1 || items[0].GetID().Value() != "id-1" || items[0].Name != "a" {
		t.Fatalf("expected the tracked item to carry the assigned id, got %+v", items)
	}
	// Statuses must be untouched — the item is still an OpInsert candidate.
	if added := GetAddedItemsOf[testAVO](&root.AggregateRoot); len(added) != 1 {
		t.Fatalf("expected the stamped item to remain Added, got %+v", added)
	}
}

// The write-back matches a tracked item by IsSameBusinessIdentity — the business
// fields, NEVER the id: the id is framework-managed (domain.Managed) and excluded
// from identity, so a value with the same business identity keeps matching
// regardless of the id it or the tracked entry currently carries, and re-stamping
// simply overwrites the id.
func TestAssignAggregateItemID_MatchesByBusinessIdentity(t *testing.T) {
	root := &assignIDRoot{}
	AddAggregateChild(root, testAVO{Name: "a"})
	if ok := root.AssignAggregateItemID(testAVO{Name: "a"}, "id-1"); !ok {
		t.Fatal("first stamp must succeed")
	}
	// The same business value keeps matching after the id was assigned — the id
	// is not part of identity — so a second stamp re-assigns it.
	if ok := root.AssignAggregateItemID(testAVO{Name: "a"}, "id-2"); !ok {
		t.Fatal("the business value must keep matching regardless of the stamped id")
	}
	// A value carrying the already-stamped id (business identity unchanged) also
	// matches.
	if ok := root.AssignAggregateItemID(WithID(testAVO{Name: "a"}, NewID("id-2")), "id-3"); !ok {
		t.Fatal("a same-identity value must match for a re-assignment")
	}
	// A DIFFERENT business identity does not match.
	if ok := root.AssignAggregateItemID(testAVO{Name: "zz"}, "id-x"); ok {
		t.Fatal("a different business identity must not match")
	}
	items := GetCurrentItemsOf[testAVO](&root.AggregateRoot)
	if len(items) != 1 || items[0].GetID().Value() != "id-3" {
		t.Fatalf("expected the re-assigned id, got %+v", items)
	}
}

func TestAssignAggregateItemID_ReportsFalseWithoutMutating(t *testing.T) {
	root := &assignIDRoot{}
	AddAggregateChild(root, testAVO{Name: "a"})

	cases := []struct {
		name string
		item AggregateValueObject
	}{
		{"untracked item", testAVO{Name: "zz"}},
		{"nil item", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ok := root.AssignAggregateItemID(tc.item, "id-x"); ok {
				t.Fatalf("expected false for %s", tc.name)
			}
		})
	}

	if items := GetCurrentItemsOf[testAVO](&root.AggregateRoot); len(items) != 1 || items[0].GetID().Value() != "" {
		t.Fatalf("failed assignments must not mutate tracked items, got %+v", items)
	}
}

func TestAssignAggregateItemID_EmptyRootReportsFalse(t *testing.T) {
	root := &assignIDRoot{}
	if ok := root.AssignAggregateItemID(testAVO{Name: "a"}, "id-1"); ok {
		t.Fatal("a root with no tracked items must report false")
	}
}
