package domain

import (
	"strconv"
	"testing"
)

type singleTvarNotif struct {
	DomainNotificationBase
	MaxLength int `tvar:"maxLength"`
}

type multiTvarNotif struct {
	DomainNotificationBase
	Min  int    `tvar:"min"`
	Max  int    `tvar:"max"`
	Tier string `tvar:"tier"`
}

type pointerTvarNotif struct {
	DomainNotificationBase
	Limit *int `tvar:"limit"`
}

type noTvarNotif struct {
	DomainNotificationBase
	Something string
}

type emptyAndDashTvar struct {
	DomainNotificationBase
	A string `tvar:""`
	B string `tvar:"-"`
	C string `tvar:"actual"`
}

type unexportedTvarNotif struct {
	DomainNotificationBase
	private int `tvar:"private"` //nolint:unused // intentional: test that unexported fields are skipped
	Public  int `tvar:"public"`
}

type withMethodEscapeHatch struct {
	DomainNotificationBase
	Tagged int `tvar:"tagged"` // would have been picked up by tag walker if no method present
}

func (n withMethodEscapeHatch) TranslationVars() map[string]string {
	return map[string]string{"viaMethod": strconv.Itoa(n.Tagged * 10), "tagged": "via-method-wins"}
}

func TestExtractVarsFromTags_NilNotification(t *testing.T) {
	if got := ExtractVarsFromTags(nil); got != nil {
		t.Errorf("nil notif should return nil, got %v", got)
	}
}

func TestExtractVarsFromTags_SingleField(t *testing.T) {
	got := ExtractVarsFromTags(singleTvarNotif{MaxLength: 42})
	if len(got) != 1 || got["maxLength"] != "42" {
		t.Errorf("ExtractVarsFromTags = %v, want {maxLength: 42}", got)
	}
}

func TestExtractVarsFromTags_MultipleFields(t *testing.T) {
	got := ExtractVarsFromTags(multiTvarNotif{Min: 3, Max: 100, Tier: "gold"})
	if got["min"] != "3" || got["max"] != "100" || got["tier"] != "gold" {
		t.Errorf("ExtractVarsFromTags = %v, want {min: 3, max: 100, tier: gold}", got)
	}
}

func TestExtractVarsFromTags_PointerDereferenced(t *testing.T) {
	v := 7
	got := ExtractVarsFromTags(pointerTvarNotif{Limit: &v})
	if got["limit"] != "7" {
		t.Errorf("Render of *int = %q, want %q", got["limit"], "7")
	}
}

func TestExtractVarsFromTags_PointerNilEmptyString(t *testing.T) {
	got := ExtractVarsFromTags(pointerTvarNotif{Limit: nil})
	if got["limit"] != "" {
		t.Errorf("nil pointer should render empty, got %q", got["limit"])
	}
}

func TestExtractVarsFromTags_NoTagsReturnsNil(t *testing.T) {
	if got := ExtractVarsFromTags(noTvarNotif{Something: "ignored"}); got != nil {
		t.Errorf("no tag → nil, got %v", got)
	}
}

func TestExtractVarsFromTags_EmptyAndDashTagsSkipped(t *testing.T) {
	got := ExtractVarsFromTags(emptyAndDashTvar{A: "skip-empty", B: "skip-dash", C: "kept"})
	if len(got) != 1 || got["actual"] != "kept" {
		t.Errorf(`ExtractVarsFromTags = %v, want only {actual: "kept"}`, got)
	}
}

func TestExtractVarsFromTags_UnexportedSkipped(t *testing.T) {
	got := ExtractVarsFromTags(unexportedTvarNotif{Public: 9})
	if _, ok := got["private"]; ok {
		t.Errorf("unexported field with tvar tag must be skipped, got %v", got)
	}
	if got["public"] != "9" {
		t.Errorf("exported sibling must still resolve, got %v", got)
	}
}

func TestExtractVarsFromTags_MethodEscapeHatchOverridesTags(t *testing.T) {
	got := ExtractVarsFromTags(withMethodEscapeHatch{Tagged: 5})
	if got["viaMethod"] != "50" {
		t.Errorf("expected method-supplied entry, got %v", got)
	}
	if got["tagged"] != "via-method-wins" {
		t.Errorf("method must REPLACE (not merge with) tag extraction, got %v", got)
	}
}

func TestMessageVars_NoSourcesReturnsNil(t *testing.T) {
	msg := NotificationMessage{Notification: noTvarNotif{}}
	if got := MessageVars(msg); got != nil {
		t.Errorf("empty sources → nil, got %v", got)
	}
}

func TestMessageVars_NotifVarsOnly(t *testing.T) {
	msg := NotificationMessage{Notification: singleTvarNotif{MaxLength: 8}}
	got := MessageVars(msg)
	if got["maxLength"] != "8" {
		t.Errorf("notif vars not surfaced, got %v", got)
	}
}

func TestMessageVars_PerMessageVarsWinOnCollision(t *testing.T) {
	msg := NotificationMessage{
		Notification: singleTvarNotif{MaxLength: 8},
		Vars:         map[string]string{"maxLength": "OVERRIDE"},
	}
	got := MessageVars(msg)
	if got["maxLength"] != "OVERRIDE" {
		t.Errorf("per-message Vars must win on key collision, got %v", got)
	}
}

func TestMessageVars_MergesDistinctKeys(t *testing.T) {
	msg := NotificationMessage{
		Notification: singleTvarNotif{MaxLength: 8},
		Vars:         map[string]string{"context": "tenant-A"},
	}
	got := MessageVars(msg)
	if got["maxLength"] != "8" || got["context"] != "tenant-A" {
		t.Errorf("merge should keep both keys, got %v", got)
	}
}

func TestExtractVarsFromTags_CacheHitOnRepeatCall(t *testing.T) {
	// Use a fresh struct type not used elsewhere so this test owns the cache slot.
	type freshNotifForCache struct {
		DomainNotificationBase
		N int `tvar:"n"`
	}
	first := ExtractVarsFromTags(freshNotifForCache{N: 1})
	second := ExtractVarsFromTags(freshNotifForCache{N: 2})
	if first["n"] != "1" || second["n"] != "2" {
		t.Errorf("cached plan must still resolve runtime values, got %v / %v", first, second)
	}
}

func TestAddNotificationWithVars_PopulatesVarsAndFieldValue(t *testing.T) {
	ctx := NewNotificationContext("Test")
	r := NewRules(ModeInsert, ctx)

	r.AddNotificationWithVars(
		"Name",
		singleTvarNotif{MaxLength: 100},
		map[string]string{"override": "yes"},
		"the-rejected-input",
	)

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Vars["override"] != "yes" {
		t.Errorf("expected per-emit Vars on message, got %v", msgs[0].Vars)
	}
	if msgs[0].FieldValue != "the-rejected-input" {
		t.Errorf("expected FieldValue populated, got %q", msgs[0].FieldValue)
	}
}

func TestContextSetVarsAndContextVars(t *testing.T) {
	ctx := NewNotificationContext("User")
	ctx.SetVars(map[string]string{"tenantId": "acme"})

	if got := ctx.ContextVars(); got["tenantId"] != "acme" {
		t.Errorf("ContextVars round-trip failed, got %v", got)
	}

	// SetVars with nil clears.
	ctx.SetVars(nil)
	if got := ctx.ContextVars(); got != nil {
		t.Errorf("nil SetVars should clear, got %v", got)
	}

	// SetVars with empty map clears too.
	ctx.SetVars(map[string]string{"x": "y"})
	ctx.SetVars(map[string]string{})
	if got := ctx.ContextVars(); got != nil {
		t.Errorf("empty SetVars should clear, got %v", got)
	}
}

func TestContextSetVars_ScopedForwardsToRoot(t *testing.T) {
	root := NewNotificationContext("User")
	root.SetVars(map[string]string{"tenantId": "acme"})

	scoped := root.Scoped(NameSegment("addresses"), IndexSegment(0))
	if got := scoped.ContextVars(); got["tenantId"] != "acme" {
		t.Errorf("scoped view must read root contextVars, got %v", got)
	}

	// SetVars on scoped also writes to root.
	scoped.SetVars(map[string]string{"override": "via-scoped"})
	if got := root.ContextVars(); got["override"] != "via-scoped" {
		t.Errorf("SetVars on scoped should write through to root, got %v", got)
	}
}
