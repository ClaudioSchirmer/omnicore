package domain

import (
	"errors"
	"testing"
	"time"
)

// The DOMAIN gains nothing for composite value objects: a composite is a struct
// that owns its rule, and the existing field discovery (validateValueObjectFields
// → validatorFor, which only looks for IsValid) already finds and validates it.
// These tests pin that, because "the domain needs no change" is a load-bearing
// claim of the design — if discovery ever narrows, the whole feature silently
// stops validating.

type cvoPeriod struct {
	From time.Time  `labelKey:"PeriodFromField"`
	To   *time.Time `labelKey:"PeriodToField"`
}

// IsValid carries a CROSS-FIELD rule — the thing a single-scalar value object
// cannot express, and the reason composites exist.
func (p cvoPeriod) IsValid(_ string, ctx *NotificationContext) bool {
	if p.To != nil && p.To.Before(p.From) {
		ctx.AddNotificationNamed("To", cvoEndsBeforeStartNote{})
		return false
	}
	return true
}

type cvoEndsBeforeStartNote struct{ DomainNotificationBase }

type cvoBooking struct {
	BaseEntity
	Stay     cvoPeriod
	Optional *cvoPeriod
}

func (b *cvoBooking) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (b *cvoBooking) BuildRules(string, Service, *Rules) {}

func cvoInsert(t *testing.T, b *cvoBooking) error {
	t.Helper()
	_, err := GetInsertable(b, ServiceBase{}, "GetInsertable")
	return err
}

func TestComposite_IsValidRunsWithoutAnyDeclaration(t *testing.T) {
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	err := cvoInsert(t, &cvoBooking{Stay: cvoPeriod{From: start, To: &end}})
	if err == nil {
		t.Fatal("the composite's cross-field rule must fire with no BuildRules entry and no schema involved")
	}
	var carrier NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("error %v must carry notifications", err)
	}
}

func TestComposite_PartNotificationCarriesTheValueObjectsLabel(t *testing.T) {
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	b := &cvoBooking{Stay: cvoPeriod{From: start, To: &end}}
	_ = cvoInsert(t, b)

	msgs := b.NotificationContext().Messages()
	if len(msgs) == 0 {
		t.Fatal("expected a notification from the composite's rule")
	}
	found := false
	for _, m := range msgs {
		if len(m.Path) == 0 || m.Path[0].Name != "To" {
			continue
		}
		found = true
		// The label comes from the tag INSIDE the value object — the entity has no
		// field called "To", so without the label plan's hop this would be empty.
		if m.LabelKey != "PeriodToField" {
			t.Errorf("LabelKey = %q, want PeriodToField (the tag on cvoPeriod.To)", m.LabelKey)
		}
	}
	if !found {
		t.Errorf("no notification on the part %q: %+v", "To", msgs)
	}
}

func TestComposite_AbsentOptionalIsNotAViolation(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Optional is nil: a nil pointer field is skipped by the discovery pass, so
	// absence never validates.
	if err := cvoInsert(t, &cvoBooking{Stay: cvoPeriod{From: start, To: &end}}); err != nil {
		t.Fatalf("a valid entity with an absent optional composite must pass: %v", err)
	}
}

func TestComposite_OldGhostCrossesTheValueObject(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	b := &cvoBooking{Stay: cvoPeriod{From: start, To: &end}, Optional: &cvoPeriod{From: start}}
	captureOld(b)

	old := Old[*cvoBooking](b)
	if old == nil {
		t.Fatal("no ghost captured")
	}
	// The clone is a JSON round-trip, but what comes back is the TYPED entity —
	// a transition rule reads the composite as plain Go.
	if !old.Stay.From.Equal(start) || old.Stay.To == nil || !old.Stay.To.Equal(end) {
		t.Errorf("ghost Stay = %+v, want the pre-mutation value", old.Stay)
	}
	if old.Optional == nil || !old.Optional.From.Equal(start) {
		t.Fatalf("ghost Optional = %+v, want a copy of the optional composite", old.Optional)
	}
	// The ghost owns its own allocation: comparing pointers is always wrong.
	if old.Optional == b.Optional {
		t.Error("the ghost's optional composite must be a fresh allocation, not an alias")
	}
}
