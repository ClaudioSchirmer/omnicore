package domain

import "testing"

func TestWireFieldPath(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"", ""},
		{"Name", "name"},
		{"ID", "id"},
		{"URLPath", "urlPath"},
		{"Addresses.ZipCode", "addresses.zipCode"},
		{"Parts.Supplier.URL", "parts.supplier.url"},
		// wire-format input is idempotent
		{"cursor", "cursor"},
		{"addresses.zipCode", "addresses.zipCode"},
	}
	for _, c := range cases {
		if got := WireFieldPath(c.in); got != c.out {
			t.Errorf("WireFieldPath(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

// strayAVO is deliberately NOT declared in keyedProvider.AggregateChildren, so
// adding it exercises rejectChild.
type strayAVO struct {
	Managed
	Key string
}

func (strayAVO) BuildRules(string, Service, *Rules) {}
func (strayAVO) CollectionName() string             { return "Strays" }
func (s strayAVO) IsSameBusinessIdentity(other AggregateValueObject) bool {
	o, ok := other.(strayAVO)
	return ok && s.Key == o.Key
}

// The wire field of every child-collection rejection is the DECLARED
// collection segment cased for the wire — never the Go type name.
func TestAggregateChildNotifications_CarryCollectionSegment(t *testing.T) {
	onlyField := func(t *testing.T, p *keyedProvider, wantKey, wantField string) {
		t.Helper()
		msgs := p.NotificationContext().Messages()
		if len(msgs) != 1 {
			t.Fatalf("expected exactly 1 notification, got %d (%v)", len(msgs), msgs)
		}
		if got := NotificationKey(msgs[0].Notification); got != wantKey {
			t.Fatalf("expected %s, got %s", wantKey, got)
		}
		if got := msgs[0].ResolveFieldName(); got != wantField {
			t.Fatalf("expected field %q, got %q", wantField, got)
		}
	}

	t.Run("duplicate add", func(t *testing.T) {
		p := newKeyedProvider()
		AddAggregateChild(p, keyedAVO{Key: "a", Payload: "again"})
		onlyField(t, p, "EntityAlreadyAddedNotification", "keyedAVOs")
	})

	t.Run("change of a missing entry", func(t *testing.T) {
		p := newKeyedProvider()
		ChangeAggregateChild(p, keyedAVO{Key: "zz"}, keyedAVO{Key: "zz", Payload: "x"})
		onlyField(t, p, "EntityDoesNotExistNotification", "keyedAVOs")
	})

	t.Run("change onto a sibling identity", func(t *testing.T) {
		p := newKeyedProvider()
		items := keyedItems(p)
		ChangeAggregateChild(p, items[1].Item, WithID(keyedAVO{Key: "a", Payload: "moved"}, NewID("row-b")))
		onlyField(t, p, "EntityAlreadyAddedNotification", "keyedAVOs")
	})

	t.Run("remove of a missing entry", func(t *testing.T) {
		p := newKeyedProvider()
		RemoveAggregateChild(p, keyedAVO{Key: "zz"})
		onlyField(t, p, "EntityDoesNotExistNotification", "keyedAVOs")
	})

	t.Run("undeclared child type", func(t *testing.T) {
		p := newKeyedProvider()
		AddAggregateChild(p, strayAVO{Key: "s"})
		msgs := p.NotificationContext().Messages()
		if len(msgs) != 1 {
			t.Fatalf("expected exactly 1 notification, got %d (%v)", len(msgs), msgs)
		}
		if got := NotificationKey(msgs[0].Notification); got != "InvalidAggregateChildNotification" {
			t.Fatalf("expected InvalidAggregateChildNotification, got %s", got)
		}
		if got := msgs[0].ResolveFieldName(); got != "strays" {
			t.Fatalf("expected the declared collection segment %q, got %q", "strays", got)
		}
		if msgs[0].FieldValue != "strayAVO" {
			t.Fatalf("the Go type name stays in FieldValue as the diagnostic echo, got %q", msgs[0].FieldValue)
		}
	})
}
