package domain

import "testing"

func TestToLowerCamel(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"", ""},
		{"Name", "name"},
		{"Email", "email"},
		{"ZipCode", "zipCode"},
		{"URL", "url"},
		{"ID", "id"},
		{"URLPath", "urlPath"},
		{"HTTPStatusCode", "httpStatusCode"},
		{"name", "name"},
		{"id", "id"},
		{"zipCode", "zipCode"},
	}
	for _, c := range cases {
		if got := toLowerCamel(c.in); got != c.out {
			t.Errorf("toLowerCamel(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestRenderPath_Empty(t *testing.T) {
	if got := renderPath(nil); got != "" {
		t.Errorf("renderPath(nil) = %q, want empty", got)
	}
	if got := renderPath([]PathSegment{}); got != "" {
		t.Errorf("renderPath([]) = %q, want empty", got)
	}
}

func TestRenderPath_RootField(t *testing.T) {
	cases := []struct {
		path []PathSegment
		want string
	}{
		{[]PathSegment{{Name: "Name"}}, "name"},
		{[]PathSegment{{Name: "URL"}}, "url"},
		{[]PathSegment{{Name: "ZipCode"}}, "zipCode"},
		{[]PathSegment{{Name: "id"}}, "id"},
	}
	for _, c := range cases {
		if got := renderPath(c.path); got != c.want {
			t.Errorf("renderPath(%v) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestRenderPath_AggregateChild(t *testing.T) {
	zero := 0
	two := 2
	cases := []struct {
		path []PathSegment
		want string
	}{
		{[]PathSegment{{Name: "addresses"}, {Index: &zero}, {Name: "ZipCode"}}, "addresses[0].zipCode"},
		{[]PathSegment{{Name: "Addresses"}, {Index: &zero}, {Name: "Street"}}, "addresses[0].street"},
		{[]PathSegment{{Name: "orderItems"}, {Index: &two}, {Name: "Quantity"}}, "orderItems[2].quantity"},
		{[]PathSegment{{Name: "phoneNumbers"}, {Index: &zero}, {Name: "CountryCode"}}, "phoneNumbers[0].countryCode"},
	}
	for _, c := range cases {
		if got := renderPath(c.path); got != c.want {
			t.Errorf("renderPath(%v) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestRenderPath_NestedAggregates(t *testing.T) {
	zero := 0
	one := 1
	p := []PathSegment{
		{Name: "orders"}, {Index: &zero},
		{Name: "lines"}, {Index: &one},
		{Name: "Quantity"},
	}
	want := "orders[0].lines[1].quantity"
	if got := renderPath(p); got != want {
		t.Errorf("renderPath nested = %q, want %q", got, want)
	}
}

func TestResolveFieldName_PrecedenceOverride(t *testing.T) {
	zero := 0
	msg := NotificationMessage{
		Path:      []PathSegment{{Name: "addresses"}, {Index: &zero}, {Name: "ZipCode"}},
		Override:  "_root",
		FieldName: "raw",
	}
	if got := msg.ResolveFieldName(); got != "_root" {
		t.Errorf("override should win: got %q", got)
	}
}

func TestResolveFieldName_PrecedencePath(t *testing.T) {
	msg := NotificationMessage{
		Path:      []PathSegment{{Name: "Name"}},
		FieldName: "raw",
	}
	if got := msg.ResolveFieldName(); got != "name" {
		t.Errorf("path should win over FieldName: got %q", got)
	}
}

func TestResolveFieldName_FallbackFieldName(t *testing.T) {
	msg := NotificationMessage{FieldName: "id"}
	if got := msg.ResolveFieldName(); got != "id" {
		t.Errorf("FieldName fallback: got %q", got)
	}
}

func TestNotificationContext_AddNotification_Root(t *testing.T) {
	ctx := NewNotificationContext("User")
	ctx.AddNotification("Name", RequiredFieldNotification{})

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if got := msgs[0].ResolveFieldName(); got != "name" {
		t.Errorf("expected %q, got %q", "name", got)
	}
}

func TestNotificationContext_Scoped_ComposesPrefix(t *testing.T) {
	ctx := NewNotificationContext("User")
	scoped := ctx.Scoped(NameSegment("addresses"), IndexSegment(0))
	scoped.AddNotification("ZipCode", RequiredFieldNotification{})

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in root context, got %d", len(msgs))
	}
	if got := msgs[0].ResolveFieldName(); got != "addresses[0].zipCode" {
		t.Errorf("expected %q, got %q", "addresses[0].zipCode", got)
	}
}

func TestNotificationContext_Scoped_NestedScopes(t *testing.T) {
	ctx := NewNotificationContext("User")
	outer := ctx.Scoped(NameSegment("orders"), IndexSegment(0))
	inner := outer.Scoped(NameSegment("lines"), IndexSegment(1))
	inner.AddNotification("Quantity", RequiredFieldNotification{})

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	want := "orders[0].lines[1].quantity"
	if got := msgs[0].ResolveFieldName(); got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestNotificationContext_Scoped_LegacyFieldNameWrapped(t *testing.T) {
	ctx := NewNotificationContext("User")
	scoped := ctx.Scoped(NameSegment("addresses"), IndexSegment(0))
	// Legacy: AVO that still passes FieldName (no Path) — context wraps it so
	// the prefix can apply.
	scoped.AddNotificationMessage(NotificationMessage{
		FieldName:    "street",
		Notification: RequiredFieldNotification{},
	})

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	// "street" was lowercase already → renders as-is, prefix composes.
	if got := msgs[0].ResolveFieldName(); got != "addresses[0].street" {
		t.Errorf("expected %q, got %q", "addresses[0].street", got)
	}
}

func TestNotificationContext_ChangeFieldName_OverridesPath(t *testing.T) {
	ctx := NewNotificationContext("User")
	scoped := ctx.Scoped(NameSegment("addresses"), IndexSegment(0))
	scoped.AddNotification("ZipCode", RequiredFieldNotification{})

	// Manual handler renames the wire field.
	ctx.ChangeFieldName("addresses[0].zipCode", "_root")

	msgs := ctx.Messages()
	if got := msgs[0].ResolveFieldName(); got != "_root" {
		t.Errorf("expected override to win, got %q", got)
	}
	// Path is preserved for diagnostics — only Override changes.
	if len(msgs[0].Path) != 3 {
		t.Errorf("Path should remain intact, got %v", msgs[0].Path)
	}
}

func TestNotificationContext_Scoped_HasErrorsForwarded(t *testing.T) {
	ctx := NewNotificationContext("User")
	scoped := ctx.Scoped(NameSegment("addresses"), IndexSegment(0))
	if ctx.HasErrors() || scoped.HasErrors() {
		t.Fatal("expected no errors before AddNotification")
	}
	scoped.AddNotification("ZipCode", RequiredFieldNotification{})
	if !ctx.HasErrors() || !scoped.HasErrors() {
		t.Fatal("expected HasErrors=true on both root and scoped after AddNotification")
	}
}

func TestNotificationContext_Clear_FromScoped(t *testing.T) {
	ctx := NewNotificationContext("User")
	scoped := ctx.Scoped(NameSegment("addresses"), IndexSegment(0))
	scoped.AddNotification("ZipCode", RequiredFieldNotification{})
	scoped.Clear()
	if ctx.HasErrors() {
		t.Fatal("expected Clear from scoped to clear root messages")
	}
}
