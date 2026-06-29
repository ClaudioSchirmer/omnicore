//go:build integration && postgres

package mongo

import (
	"context"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// --- read.BaseAggregateRepository.FindByID / FindArchivedByID ------------------

func TestBaseAggregateRepository_FindByID(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('Y', 'y@x') RETURNING id`).Scan(&id)

	bar := read.NewBaseAggregateRepository[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} })
	// loaderRoot.AggregateChildren() declares both children, and a write-capable
	// read.BaseAggregateRepository enforces aggregate-boundary agreement, so the full
	// schema is required (the partial TagsOnly/Flat schemas are loader-only).
	bar.WithSchema(loaderRootSchema())

	root, err := bar.FindByID(domain.NewID(id))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if root.Name != "Y" {
		t.Errorf("root = %+v", root)
	}
}

func TestBaseAggregateRepository_FindArchivedByID(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email, deleted_at) VALUES ('A', 'a@x', NOW()) RETURNING id`).Scan(&id)

	bar := read.NewBaseAggregateRepository[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} })
	bar.WithSchema(loaderRootSchema())
	root, err := bar.FindArchivedByID(domain.NewID(id))
	if err != nil {
		t.Fatalf("FindArchivedByID: %v", err)
	}
	if root.Name != "A" {
		t.Errorf("root = %+v", root)
	}
}

// --- core.InfrastructureError helpers ----------------------------------------

func TestNewInfrastructureError(t *testing.T) {
	ctxA := domain.NewNotificationContext("A")
	e := core.NewInfrastructureError([]*domain.NotificationContext{ctxA})
	if e == nil || len(e.Contexts) != 1 {
		t.Errorf("core.NewInfrastructureError = %+v", e)
	}
}

func TestInfrastructureError_ErrorMessage(t *testing.T) {
	e := core.NewInfrastructureError([]*domain.NotificationContext{
		domain.NewNotificationContext("A"),
		domain.NewNotificationContext("B"),
	})
	if e.Error() != "infrastructure error: 2 context(s)" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestInfrastructureError_NotificationContexts(t *testing.T) {
	ctxA := domain.NewNotificationContext("A")
	e := core.NewInfrastructureError([]*domain.NotificationContext{ctxA})
	if got := e.NotificationContexts(); len(got) != 1 || got[0] != ctxA {
		t.Errorf("NotificationContexts() did not preserve identity")
	}
}

func TestInfrastructureError_SatisfiesCarrier(t *testing.T) {
	var carrier domain.NotificationCarrier = core.NewInfrastructureError(nil)
	if carrier == nil {
		t.Error("expected non-nil carrier interface value")
	}
}

func TestInfrastructureSingleNotificationError(t *testing.T) {
	e := core.SingleNotificationError("X", "f", domain.RequiredFieldNotification{})
	if e == nil || len(e.Contexts) != 1 {
		t.Fatalf("%+v", e)
	}
	msgs := e.Contexts[0].Messages()
	if len(msgs) != 1 || msgs[0].FieldName != "f" {
		t.Errorf("msg = %+v", msgs)
	}
}

func TestInfrastructureFieldErrorWithCause(t *testing.T) {
	cause := errors.New("down")
	e := core.FieldErrorWithCause("X", "f", cause, domain.RequiredFieldNotification{})
	if e == nil || e.Contexts[0].Messages()[0].Err != cause {
		t.Errorf("cause not propagated: %+v", e)
	}
}
