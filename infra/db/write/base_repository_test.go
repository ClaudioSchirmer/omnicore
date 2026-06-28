package write

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeUniqueViolationNotification struct {
	domain.DomainNotificationBase
}

func TestMapErr_Nil(t *testing.T) {
	r := BaseRepository[any]{}
	if got := r.mapErr(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestMapErr_NonPgErr_PassThrough(t *testing.T) {
	r := BaseRepository[any]{ContextName: "Test"}
	in := errors.New("network down")
	if got := r.mapErr(in); got != in {
		t.Fatalf("expected pass-through of raw err, got %v", got)
	}
}

func TestMapErr_PgErrNon23505_PassThrough(t *testing.T) {
	r := BaseRepository[any]{
		Engine:      &fakeRelEngine{},
		ContextName: "Test",
		Constraints: map[string]ConstraintBinding{
			"users_email_active_idx": {Notification: fakeUniqueViolationNotification{}, Field: "email"},
		},
	}
	in := &pgconn.PgError{Code: "23503", ConstraintName: "users_email_active_idx"}
	if got := r.mapErr(in); got != in {
		t.Fatalf("expected pass-through for non-23505 pgErr, got %v", got)
	}
}

func TestMapErr_UniqueViolationUnmapped(t *testing.T) {
	r := BaseRepository[any]{Engine: &fakeRelEngine{}, ContextName: "Test"}
	in := &pgconn.PgError{Code: "23505", ConstraintName: "unknown_idx"}
	if got := r.mapErr(in); got != in {
		t.Fatalf("expected raw pgErr when constraint not registered, got %v", got)
	}
}

func TestMapErr_UniqueViolationMapped(t *testing.T) {
	cause := &pgconn.PgError{Code: "23505", ConstraintName: "users_email_active_idx"}
	r := BaseRepository[any]{
		Engine:      &fakeRelEngine{},
		ContextName: "User",
		Constraints: map[string]ConstraintBinding{
			"users_email_active_idx": {Notification: fakeUniqueViolationNotification{}, Field: "email"},
		},
	}
	err := r.mapErr(cause)
	if err == nil {
		t.Fatal("expected mapped *InfrastructureError, got nil")
	}

	var infraErr *InfrastructureError
	if !errors.As(err, &infraErr) {
		t.Fatalf("expected *InfrastructureError, got %T (%v)", err, err)
	}
	if len(infraErr.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(infraErr.Contexts))
	}
	ctx := infraErr.Contexts[0]
	if ctx.Context() != "User" {
		t.Errorf("expected context name 'User', got %q", ctx.Context())
	}
	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].FieldName != "email" {
		t.Errorf("expected FieldName 'email', got %q", msgs[0].FieldName)
	}
	if msgs[0].Err != cause {
		t.Errorf("expected message.Err to equal cause pgErr, got %v", msgs[0].Err)
	}
	if key := domain.NotificationKey(msgs[0].Notification); key != "fakeUniqueViolationNotification" {
		t.Errorf("expected NotificationKey 'fakeUniqueViolationNotification', got %q", key)
	}
}

// Compile-time check: a service repo embedding BaseRepository[*E] + adding
// FindByID satisfies domain.Repository[*E]. New() is promoted from
// BaseRepository — service only needs to wire NewEntity in the constructor.
type baseRepoTestEntity struct {
	domain.BaseEntity
}

type baseRepoTestRepo struct {
	BaseRepository[*baseRepoTestEntity]
}

func (r *baseRepoTestRepo) FindByID(domain.ID) (*baseRepoTestEntity, error) {
	return nil, nil
}

var _ persistence.ScopedRepository[*baseRepoTestEntity] = (*baseRepoTestRepo)(nil)

func TestNew_Configured(t *testing.T) {
	r := BaseRepository[*baseRepoTestEntity]{
		NewEntity: func() *baseRepoTestEntity { return &baseRepoTestEntity{} },
	}
	got := r.New()
	if got == nil {
		t.Fatal("expected non-nil entity from configured factory")
	}
}

func TestNew_PanicsWhenUnconfigured(t *testing.T) {
	r := BaseRepository[*baseRepoTestEntity]{}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when NewEntity is nil")
		}
	}()
	_ = r.New()
}

func TestEffectiveContextName_ExplicitWins(t *testing.T) {
	r := BaseRepository[*baseRepoTestEntity]{ContextName: "AdminUser"}
	if got := r.effectiveContextName(); got != "AdminUser" {
		t.Errorf("explicit ContextName must win, got %q", got)
	}
}

func TestEffectiveContextName_DerivedFromT(t *testing.T) {
	r := BaseRepository[*baseRepoTestEntity]{}
	if got := r.effectiveContextName(); got != "baseRepoTestEntity" {
		t.Errorf("expected derived name %q, got %q", "baseRepoTestEntity", got)
	}
}

func TestMapErr_UniqueViolationMapped_DerivedContextName(t *testing.T) {
	cause := &pgconn.PgError{Code: "23505", ConstraintName: "ents_x_idx"}
	r := BaseRepository[*baseRepoTestEntity]{
		Engine: &fakeRelEngine{},
		Constraints: map[string]ConstraintBinding{
			"ents_x_idx": {Notification: fakeUniqueViolationNotification{}, Field: "x"},
		},
	}
	err := r.mapErr(cause)
	var infraErr *InfrastructureError
	if !errors.As(err, &infraErr) {
		t.Fatalf("expected *InfrastructureError, got %T (%v)", err, err)
	}
	if infraErr.Contexts[0].Context() != "baseRepoTestEntity" {
		t.Errorf("expected derived context %q, got %q",
			"baseRepoTestEntity", infraErr.Contexts[0].Context())
	}
}
