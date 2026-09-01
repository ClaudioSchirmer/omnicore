package write

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// builderTestEntity is the flat entity exercised by Build*Event tests.
type builderTestEntity struct {
	domain.BaseEntity
	Name  string
	Email string
}

func (e *builderTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *builderTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

var builderTestSchema = NewTableSchema[*builderTestEntity]("builder_test_entities").
	ID("id").
	Revision("revision").
	Field("Name", "name").
	Field("Email", "email").
	DeletedAt("deleted_at").
	CreatedAt("created_at").
	UpdatedAt("updated_at")

func newBuilderCtx() persistence.RequestContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func newBuilderCtxWithIdentity(subject, issuer string, claims map[string]any) persistence.RequestContext {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetIdentity(&configuration.Identity{
		Subject: subject,
		Issuer:  issuer,
		Claims:  claims,
	})
	return ctx
}

// ─── Build* surface — verb/kind discrimination ──────────────────────────────

func TestBuildInsertEvent_KindSnapshot(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	id := domain.NewID(uuid.NewString())
	i, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	ev := BuildInsertEvent(newBuilderCtx(), i, id, builderTestSchema, nil)

	if ev.Verb != "insert" {
		t.Errorf("Verb = %q, want insert", ev.Verb)
	}
	if ev.Kind != "snapshot" {
		t.Errorf("Kind = %q, want snapshot", ev.Kind)
	}
	if ev.EntityType != "builderTestEntity" {
		t.Errorf("EntityType = %q", ev.EntityType)
	}
	if ev.EntityID != id.Value() {
		t.Errorf("EntityID = %q, want %q", ev.EntityID, id.Value())
	}
	if ev.ActionName != "GetInsertable" {
		t.Errorf("ActionName = %q", ev.ActionName)
	}
	if ev.Snapshot == nil {
		t.Fatal("Snapshot is nil, want populated")
	}
	if ev.Snapshot["Name"] != "alice" || ev.Snapshot["Email"] != "a@x.com" {
		t.Errorf("Snapshot = %+v", ev.Snapshot)
	}
	if ev.Changes != nil {
		t.Errorf("Insert must NOT carry changes, got %+v", ev.Changes)
	}
}

func TestBuildUpdateEvent_KindDeltaWithChangesOnly(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))

	apply := func(x *builderTestEntity) error { x.Name = "bob"; return nil }
	u, err := domain.GetUpdatable(e, apply, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	ev := BuildUpdateEvent(newBuilderCtx(), u, builderTestSchema, nil)

	if ev.Verb != "update" {
		t.Errorf("Verb = %q, want update", ev.Verb)
	}
	if ev.Kind != "delta" {
		t.Errorf("Kind = %q, want delta", ev.Kind)
	}
	if ev.Snapshot != nil {
		t.Errorf("delta event must NOT carry snapshot, got %+v", ev.Snapshot)
	}
	if len(ev.Changes) != 1 {
		t.Fatalf("Changes len = %d, want 1 (only name mutated): %+v", len(ev.Changes), ev.Changes)
	}
	if ev.Changes[0].Field != "Name" || ev.Changes[0].From != "alice" || ev.Changes[0].To != "bob" {
		t.Errorf("Changes[0] = %+v", ev.Changes[0])
	}
}

func TestBuildUpdateEvent_PartialUpdateSharesUpdateVerb(t *testing.T) {
	// SQL-grounded: PUT and PATCH both emit `UPDATE col=val, updated_at=NOW()`
	// — verb is identical, distinction lives in ActionName.
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))

	apply := func(x *builderTestEntity) error { x.Name = "bob"; return nil }
	u, err := domain.GetPartialUpdatable(e, apply, nil, "GetPartialUpdatable")
	if err != nil {
		t.Fatalf("GetPartialUpdatable: %v", err)
	}
	ev := BuildUpdateEvent(newBuilderCtx(), u, builderTestSchema, nil)
	if ev.Verb != "update" {
		t.Errorf("Verb = %q, want update (PATCH shares the verb)", ev.Verb)
	}
	if ev.ActionName != "GetPartialUpdatable" {
		t.Errorf("ActionName = %q", ev.ActionName)
	}
}

func TestBuildArchiveEvent_KindTransition(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))
	ar, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	ev := BuildArchiveEvent(newBuilderCtx(), ar, builderTestSchema, nil, CascadeStamps{})

	if ev.Verb != "archive" {
		t.Errorf("Verb = %q", ev.Verb)
	}
	if ev.Kind != "transition" {
		t.Errorf("Kind = %q", ev.Kind)
	}
	if ev.Snapshot != nil {
		t.Errorf("transition must NOT carry snapshot, got %+v", ev.Snapshot)
	}
	if ev.Changes != nil {
		t.Errorf("transition must NOT carry changes, got %+v", ev.Changes)
	}
}

func TestBuildUnarchiveEvent_KindTransition(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	ev := BuildUnarchiveEvent(newBuilderCtx(), un, builderTestSchema, nil, CascadeStamps{})
	if ev.Verb != "unarchive" || ev.Kind != "transition" {
		t.Errorf("verb=%q kind=%q want unarchive/transition", ev.Verb, ev.Kind)
	}
}

func TestBuildDeleteEvent_KindSnapshotForForensics(t *testing.T) {
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))
	d, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	ev := BuildDeleteEvent(newBuilderCtx(), d, builderTestSchema, nil)

	if ev.Verb != "delete" || ev.Kind != "snapshot" {
		t.Errorf("verb=%q kind=%q want delete/snapshot", ev.Verb, ev.Kind)
	}
	if ev.Snapshot == nil || ev.Snapshot["Name"] != "alice" {
		t.Errorf("Delete snapshot should capture pre-delete state, got %+v", ev.Snapshot)
	}
}

// ─── Context propagation (actor / threadId / tenant / claims allowlist) ─────

func TestPopulateContext_ActorAndThreadID(t *testing.T) {
	ctx := newBuilderCtxWithIdentity("alice-42", "https://idp.test", nil)
	e := &builderTestEntity{Name: "x"}
	i, _ := domain.GetInsertable(e, nil, "GetInsertable")
	ev := BuildInsertEvent(ctx, i, domain.NewID(uuid.NewString()), builderTestSchema, nil)

	if ev.Actor != "alice-42" {
		t.Errorf("Actor = %q, want alice-42", ev.Actor)
	}
	if ev.ActorIssuer != "https://idp.test" {
		t.Errorf("ActorIssuer = %q", ev.ActorIssuer)
	}
	if ev.ThreadID == "" {
		t.Error("ThreadID empty")
	}
}

func TestPopulateContext_AnonymousActorWhenNoIdentity(t *testing.T) {
	ev := BuildInsertEvent(newBuilderCtx(),
		insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, nil)
	if ev.Actor != persistence.AnonymousActor {
		t.Errorf("Actor = %q, want %q (anonymous when no Identity attached)", ev.Actor, persistence.AnonymousActor)
	}
	if ev.ActorIssuer != "" {
		t.Errorf("ActorIssuer = %q, want empty when no Identity", ev.ActorIssuer)
	}
}

func TestPopulateContext_TenantIDExtractedFromClaim(t *testing.T) {
	ctx := newBuilderCtxWithIdentity("alice", "iss", map[string]any{
		"tenant_id": "acme",
		"roles":     []string{"admin"},
	})
	ev := BuildInsertEvent(ctx,
		insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, nil)
	if ev.TenantID != "acme" {
		t.Errorf("TenantID = %q, want acme", ev.TenantID)
	}
}

func TestPopulateContext_TenantIDEmptyWhenClaimAbsent(t *testing.T) {
	ctx := newBuilderCtxWithIdentity("alice", "iss", map[string]any{"roles": []string{"admin"}})
	ev := BuildInsertEvent(ctx,
		insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, nil)
	if ev.TenantID != "" {
		t.Errorf("TenantID = %q, want empty (no tenant_id claim)", ev.TenantID)
	}
}

func TestPopulateContext_TenantIDFromSingletonSlice(t *testing.T) {
	// JWT decoders sometimes hand string claims as []string{one} or []any{one};
	// extractTenantID coerces those shapes for parity with Identity.TenantID.
	ctx := newBuilderCtxWithIdentity("alice", "iss", map[string]any{
		"tenant_id": []any{"acme"},
	})
	ev := BuildInsertEvent(ctx,
		insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, nil)
	if ev.TenantID != "acme" {
		t.Errorf("TenantID = %q, want acme (from []any{string})", ev.TenantID)
	}
}

func TestPopulateContext_ActorClaimsFilteredByAllowlist(t *testing.T) {
	ctx := newBuilderCtxWithIdentity("alice", "iss", map[string]any{
		"tenant_id":          "acme",
		"roles":              []string{"admin"},
		"sub":                "alice",
		"signing_secret":     "should-never-leak",
		"preferred_username": "alice@x.test",
	})
	allowlist := []string{"preferred_username", "roles"}
	ev := BuildInsertEvent(ctx,
		insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, allowlist)

	if len(ev.ActorClaims) != 2 {
		t.Fatalf("ActorClaims len = %d, want 2: %+v", len(ev.ActorClaims), ev.ActorClaims)
	}
	if _, ok := ev.ActorClaims["signing_secret"]; ok {
		t.Error("signing_secret leaked into actorClaims — allowlist breach")
	}
	if ev.ActorClaims["preferred_username"] != "alice@x.test" {
		t.Errorf("preferred_username = %v", ev.ActorClaims["preferred_username"])
	}
}

func TestPopulateContext_ActorClaimsNilWhenAllowlistEmpty(t *testing.T) {
	ctx := newBuilderCtxWithIdentity("alice", "iss", map[string]any{"tenant_id": "acme"})
	ev := BuildInsertEvent(ctx,
		insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, nil) // empty allowlist
	if ev.ActorClaims != nil {
		t.Errorf("ActorClaims = %+v, want nil (empty allowlist drops the block)", ev.ActorClaims)
	}
}

func TestPopulateContext_NilCtxLeavesFieldsZero(t *testing.T) {
	// Build* tolerates a nil ctx so direct callers (tests, fixtures, future
	// fire-and-forget code paths) need not synthesize a stub.
	ev := BuildInsertEvent(nil, insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, nil)
	if ev.Actor != "" || ev.ActorIssuer != "" || ev.ThreadID != "" {
		t.Errorf("nil ctx should leave actor/issuer/threadId empty, got actor=%q issuer=%q threadId=%q",
			ev.Actor, ev.ActorIssuer, ev.ThreadID)
	}
}

func insertableOf(t *testing.T, e domain.Entity) domain.Insertable {
	t.Helper()
	i, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	return i
}

// The trail answers "from where", not only "who": the resolved origin rides
// every event the four builders produce.
func TestPopulateContext_ClientIP(t *testing.T) {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetClientIP("198.51.100.23")
	ev := BuildInsertEvent(ctx, insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, nil)
	if ev.ClientIP != "198.51.100.23" {
		t.Errorf("ClientIP = %q, want 198.51.100.23", ev.ClientIP)
	}
}

// A write that did not come from an inbound request (consumer handler,
// background job) leaves it empty rather than inventing an origin.
func TestPopulateContext_ClientIPEmptyOffTheRequestPath(t *testing.T) {
	ev := BuildInsertEvent(newBuilderCtx(), insertableOf(t, &builderTestEntity{Name: "x"}),
		domain.NewID(uuid.NewString()), builderTestSchema, nil)
	if ev.ClientIP != "" {
		t.Errorf("ClientIP = %q, want empty off the request path", ev.ClientIP)
	}
}
