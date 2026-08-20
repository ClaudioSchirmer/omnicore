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

// ─── TranslateUniqueViolation ────────────────────────────────────────────────

type seatTakenNotification struct{ domain.DomainNotificationBase }

func (seatTakenNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticConflict
}

func seatConstraints() map[string]ConstraintBinding {
	return map[string]ConstraintBinding{
		"admin_seats_email_key": {Notification: seatTakenNotification{}, Field: "email"},
	}
}

func pgUnique(constraint string) error {
	return &pgconn.PgError{Code: "23505", ConstraintName: constraint}
}

// The translation is the seat a lifecycle hook reaches for: the hook owns its
// own tables and its own constraints, and the error it returns travels to the
// surface verbatim, so this is what turns a raw driver error into the typed
// envelope the repository path produces.
func TestTranslateUniqueViolation_BoundConstraintBecomesTheDeclaredNotification(t *testing.T) {
	err := TranslateUniqueViolation(testPGDialect{}, pgUnique("admin_seats_email_key"), "AdminSeat", seatConstraints())

	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected a NotificationCarrier, got %T", err)
	}
	ctxs := carrier.NotificationContexts()
	if len(ctxs) != 1 || ctxs[0].Context() != "AdminSeat" {
		t.Fatalf("the context must carry the caller's name, got %+v", ctxs)
	}
	msgs := ctxs[0].Messages()
	if len(msgs) != 1 || msgs[0].FieldName != "email" {
		t.Fatalf("the binding's field must be reported, got %+v", msgs)
	}
	if _, typed := msgs[0].Notification.(seatTakenNotification); !typed {
		t.Fatalf("the binding's notification must be carried, got %T", msgs[0].Notification)
	}
	// The driver error rides along on the message for logs and diagnostics.
	var pgErr *pgconn.PgError
	if !errors.As(msgs[0].Err, &pgErr) {
		t.Errorf("the driver cause must be carried on the message, got %v", msgs[0].Err)
	}
}

func TestTranslateUniqueViolation_PassesThroughWhatItCannotClassify(t *testing.T) {
	plain := errors.New("connection reset")
	for name, tc := range map[string]struct {
		dialect Dialect
		err     error
	}{
		"not a unique violation": {testPGDialect{}, plain},
		"no binding for this constraint": {
			testPGDialect{}, pgUnique("admin_seats_pkey"),
		},
		"nil error":   {testPGDialect{}, nil},
		"nil dialect": {nil, plain},
	} {
		t.Run(name, func(t *testing.T) {
			got := TranslateUniqueViolation(tc.dialect, tc.err, "AdminSeat", seatConstraints())
			if !errors.Is(got, tc.err) {
				t.Fatalf("the error must pass through untouched, got %v", got)
			}
			var carrier domain.NotificationCarrier
			if tc.err != nil && errors.As(got, &carrier) {
				t.Error("an unclassified error must not become a notification")
			}
		})
	}
}
