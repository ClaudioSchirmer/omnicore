package write

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// Archive and Unarchive no longer have a write path of their own: they emit the
// SAME statement the other verbs emit — the entity's full field set, managed
// timestamps, revision bump, guarded on the loaded revision — with the DeletedAt
// transition riding along as one more written column. What they keep is the
// cascade, the base convergence, the unarchive veto and the event type.

// tenantEntity flips a lifecycle field from INSIDE the archive rules — the shape
// that exposed the divergence: the mutation reached the entity and the payload,
// and never reached the database.
type tenantEntity struct {
	domain.BaseEntity
	Name   string
	Status string
}

func (e *tenantEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *tenantEntity) BuildRules(_ string, _ domain.Service, r *domain.Rules) {
	r.IfArchive(func() { e.Status = "suspended" })
	r.IfUnarchive(func() { e.Status = "active" })
}

var tenantSchema = NewTableSchema[*tenantEntity]("tenants").
	ID("id").
	Revision("revision").
	Field("Name", "name").
	Field("Status", "status").
	DeletedAt("deleted_at").
	CreatedAt("created_at").
	UpdatedAt("updated_at")

func loadedTenant(t *testing.T, status string, revision int64) *tenantEntity {
	t.Helper()
	e := &tenantEntity{Name: "acme", Status: status}
	e.SetID(domain.NewID(uuid.NewString()))
	if revision > 0 && !domain.SetManagedColumns(e, revision, nil, nil, nil) {
		t.Fatal("SetManagedColumns did not reach the entity")
	}
	domain.CaptureOld(e)
	return e
}

// stmtWithPrefix returns the first recorded statement starting with prefix and
// its bound args.
func stmtWithPrefix(t *testing.T, tx *recTx, prefix string) (string, []any) {
	t.Helper()
	for i, s := range tx.execs {
		if strings.HasPrefix(s, prefix) {
			return s, tx.execArgs[i]
		}
	}
	t.Fatalf("no statement starting with %q, got %v", prefix, tx.execs)
	return "", nil
}

func TestArchive_PersistsWhatTheDomainChanged(t *testing.T) {
	e := loadedTenant(t, "trial", 7)
	a, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	if e.Status != "suspended" {
		t.Fatalf("premise: IfArchive must have flipped the status, got %q", e.Status)
	}

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, tenantSchema, firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	sql, args := stmtWithPrefix(t, tx, "UPDATE tenants SET")
	if !strings.Contains(sql, "status = $") {
		t.Fatalf("the archive must write the field the domain changed, got %q", sql)
	}
	var wrote bool
	for _, a := range args {
		if a == "suspended" {
			wrote = true
		}
	}
	if !wrote {
		t.Errorf("the new status must be BOUND to the statement, got args %v", args)
	}
}

// The invariant the collapse exists for: whatever the event announces, the
// statement wrote.
func TestArchive_PayloadMatchesWhatTheStatementWrote(t *testing.T) {
	e := loadedTenant(t, "trial", 7)
	a, _ := domain.GetArchivable(e, nil, "GetArchivable")

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, tenantSchema, firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	p := outboxPayloadFor(t, tx, "tenants", "ARCHIVED")
	_, args := stmtWithPrefix(t, tx, "UPDATE tenants SET")

	if p["status"] != "suspended" {
		t.Errorf("payload status = %v, want the written value", p["status"])
	}
	var bound bool
	for _, v := range args {
		if v == p["status"] {
			bound = true
		}
	}
	if !bound {
		t.Errorf("the payload announces %v, which the statement never bound: %v", p["status"], args)
	}
	if v, present := p["deleted_at"]; !present || v == nil {
		t.Errorf("ARCHIVED payload must carry the DeletedAt stamp, got %v", p)
	}
}

func TestArchive_EmitsTheUpdateStatementShape(t *testing.T) {
	e := loadedTenant(t, "trial", 7)
	a, _ := domain.GetArchivable(e, nil, "GetArchivable")

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, tenantSchema, firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	sql, _ := stmtWithPrefix(t, tx, "UPDATE tenants SET")
	for _, want := range []string{
		"deleted_at = $",          // the transition, as a bound column
		"name = $",                // the full field set
		"updated_at = $",          // archiving IS a mutation, so it stamps
		"revision = revision + 1", // same commit-order token as any write
		"AND revision = $",        // guarded like any other root update
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("archive statement missing %q: %q", want, sql)
		}
	}
}

func TestUnarchive_ClearsDeletedAtAndWritesTheFieldSet(t *testing.T) {
	e := loadedTenant(t, "suspended", 7)
	u, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), u, tenantSchema, firingHook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}

	sql, args := stmtWithPrefix(t, tx, "UPDATE tenants SET")
	if !strings.Contains(sql, "deleted_at = $") {
		t.Errorf("unarchive must bind the DeletedAt column, got %q", sql)
	}
	var sawNil, sawActive bool
	for _, a := range args {
		if a == nil {
			sawNil = true
		}
		if a == "active" {
			sawActive = true
		}
	}
	if !sawNil {
		t.Errorf("unarchive must bind an explicit NULL for DeletedAt, got %v", args)
	}
	if !sawActive {
		t.Errorf("unarchive must persist what IfUnarchive changed, got %v", args)
	}

	p := outboxPayloadFor(t, tx, "tenants", "UNARCHIVED")
	if v, present := p["deleted_at"]; !present || v != nil {
		t.Errorf("UNARCHIVED payload must carry an explicit null DeletedAt, got %v", p)
	}
}

// Archive is guarded like every other root update: a stale entity is refused
// instead of reverting the concurrent writer's columns.
func TestArchive_StaleRevisionIsRefused(t *testing.T) {
	e := loadedTenant(t, "trial", 7)
	a, _ := domain.GetArchivable(e, nil, "GetArchivable")

	tx := &recTx{count: 0, queryFn: func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func([]any) error { return nil }}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})

	err := be.Archive(newBuilderCtx(), a, tenantSchema, WriteHook{})
	if keys := notificationKeys(t, err); !hasKey(keys, "ConcurrentModificationNotification") {
		t.Errorf("a stale archive must be refused, got %v", keys)
	}
	if tx.committed {
		t.Error("a refused archive must not commit")
	}
}

// The row-count check now covers the soft verbs too: archiving an id that is not
// there answers 404 instead of committing an event about nothing.
func TestArchive_MissingRowIsNotFound(t *testing.T) {
	e := loadedTenant(t, "trial", 0) // never loaded → unguarded, so 0 rows means gone
	a, _ := domain.GetArchivable(e, nil, "GetArchivable")

	tx := &recTx{count: 0}
	be := newFlatBE(&recBeginner{tx: tx})

	err := be.Archive(newBuilderCtx(), a, tenantSchema, WriteHook{})
	if keys := notificationKeys(t, err); !hasKey(keys, "RecordNotFoundNotification") {
		t.Errorf("archiving a missing row must answer NotFound, got %v", keys)
	}
	if tx.committed {
		t.Error("nothing may commit when the row is absent")
	}
}

// The audit trail stops hiding the state the verb carried.
func TestArchive_AuditRecordsTheDeltaItPersisted(t *testing.T) {
	e := loadedTenant(t, "trial", 7)
	a, _ := domain.GetArchivable(e, nil, "GetArchivable")

	ev := BuildArchiveEvent(newBuilderCtx(), a, tenantSchema, nil)

	if ev.Kind != "transition" {
		t.Errorf("the verb is still a transition, got kind %q", ev.Kind)
	}
	var found bool
	for _, c := range ev.Changes {
		if c.Field == "Status" && c.From == "trial" && c.To == "suspended" {
			found = true
		}
	}
	if !found {
		t.Errorf("the archive's own state change must reach the trail, got %+v", ev.Changes)
	}
}

// A plain archive that changed nothing keeps the bare transition shape.
func TestArchive_AuditStaysBareWhenNothingChanged(t *testing.T) {
	e := &tenantEntity{Name: "acme", Status: "suspended"} // IfArchive is a no-op here
	e.SetID(domain.NewID(uuid.NewString()))
	domain.CaptureOld(e)
	a, _ := domain.GetArchivable(e, nil, "GetArchivable")

	ev := BuildArchiveEvent(newBuilderCtx(), a, tenantSchema, nil)

	if len(ev.Changes) != 0 {
		t.Errorf("a pure transition must carry no Changes block, got %+v", ev.Changes)
	}
}

// A SharedBase role's archive must NOT restate the shared identity's business
// fields. Several roles converge on that row, it is last-write-wins and
// deliberately unguarded, so a bodyless verb rewriting it from a snapshot taken
// before the transaction would clobber whichever role wrote it last. The base
// participates through LIFECYCLE convergence only.
func TestArchiveRole_DoesNotRewriteTheSharedIdentity(t *testing.T) {
	e := &roleArchTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	domain.CaptureOld(e)
	a, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}

	tx := &recTx{count: 1, queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), a, roleArchTestSchema(), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	for _, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE pessoa SET") && strings.Contains(s, "name = $") {
			t.Errorf("an archive must not restate the shared identity's business fields, got %q", s)
		}
	}
	// The role's own row, on the other hand, is written in full.
	sql, _ := stmtWithPrefix(t, tx, "UPDATE aluno SET")
	if !strings.Contains(sql, "matricula = $") || !strings.Contains(sql, "deleted_at = $") {
		t.Errorf("the role row must take the full field set plus the transition, got %q", sql)
	}
}

// An update the DOMAIN asked to finish as an archive executes as the archive
// verb, carrying the update's field changes along. The request is read from the
// sealed Updatable, never from the entity.

type seatEntity struct {
	domain.BaseEntity
	Name  string
	Seats int
}

func (e *seatEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}

func (e *seatEntity) BuildRules(_ string, _ domain.Service, r *domain.Rules) {
	r.IfUpdate(func() {
		if e.Seats == 0 {
			e.CompleteAsArchive()
		}
	})
}

var seatSchema = NewTableSchema[*seatEntity]("seats").
	ID("id").
	Revision("revision").
	Field("Name", "name").
	Field("Seats", "seats").
	DeletedAt("deleted_at").
	CreatedAt("created_at").
	UpdatedAt("updated_at")

func seatUpdatable(t *testing.T, seats int) domain.Updatable {
	t.Helper()
	e := &seatEntity{Name: "acme", Seats: 5}
	e.SetID(domain.NewID(uuid.NewString()))
	if !domain.SetManagedColumns(e, 7, nil, nil, nil) {
		t.Fatal("SetManagedColumns did not reach the entity")
	}
	domain.CaptureOld(e)

	upd, err := domain.GetUpdatable(e, func(x *seatEntity) error {
		x.Seats = seats // the request's own change
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	return upd
}

func TestUpdate_CompletedAsArchive_ExecutesTheArchive(t *testing.T) {
	upd := seatUpdatable(t, 0) // the rule fires
	if upd.EntityMode() != domain.ModeArchive {
		t.Fatalf("premise: the seal must carry the request, got %v", upd.EntityMode())
	}

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, seatSchema, firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}

	sql, args := stmtWithPrefix(t, tx, "UPDATE seats SET")
	if !strings.Contains(sql, "deleted_at = $") {
		t.Errorf("the write must carry the archive transition, got %q", sql)
	}
	if !strings.Contains(sql, "seats = $") {
		t.Errorf("the update's own field change must ride along, got %q", sql)
	}
	var sawZeroSeats bool
	for _, a := range args {
		if a == 0 {
			sawZeroSeats = true
		}
	}
	if !sawZeroSeats {
		t.Errorf("the new seat count must be bound, got %v", args)
	}

	// The event the read side routes on is the ARCHIVE one, not UPDATED.
	p := outboxPayloadFor(t, tx, "seats", "ARCHIVED")
	if v, present := p["deleted_at"]; !present || v == nil {
		t.Errorf("the payload must carry the DeletedAt stamp, got %v", p)
	}
}

func TestUpdate_WithoutTheRequest_StaysAPlainUpdate(t *testing.T) {
	upd := seatUpdatable(t, 2) // the rule's condition is false
	if upd.EntityMode() != domain.ModeUpdate {
		t.Fatalf("premise: no request expected, got %v", upd.EntityMode())
	}

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, seatSchema, firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for i, sql := range tx.execs {
		if strings.HasPrefix(sql, "INSERT INTO outbox") && tx.execArgs[i][2] == "ARCHIVED" {
			t.Error("a plain update must not emit an archive event")
		}
	}
	p := outboxPayloadFor(t, tx, "seats", "UPDATED")
	if v, present := p["deleted_at"]; present && v != nil {
		t.Errorf("a plain update must not stamp DeletedAt, got %v", p)
	}
}

// The audit trail calls it what it is.
func TestUpdate_CompletedAsArchive_AuditsAsArchive(t *testing.T) {
	upd := seatUpdatable(t, 0)

	ev := BuildArchiveEvent(newBuilderCtx(), upd, seatSchema, nil)

	if ev.Verb != "archive" || ev.Kind != "transition" {
		t.Errorf("verb/kind = %q/%q, want archive/transition", ev.Verb, ev.Kind)
	}
	if ev.ActionName != "GetUpdatable" {
		t.Errorf("actionName must keep the door it came through, got %q", ev.ActionName)
	}
	var seats bool
	for _, c := range ev.Changes {
		if c.Field == "Seats" && c.To == 0 {
			seats = true
		}
	}
	if !seats {
		t.Errorf("the field change must reach the trail, got %+v", ev.Changes)
	}
}

// The write path verifies that the entity still holds the state the domain
// validated. Two things must be true at once: the framework's own bookkeeping
// must never trip it, and a handler's mutation must.

func TestWrite_FrameworkWriteBackDoesNotTripTheSignature(t *testing.T) {
	// An aggregate insert is the hard case: the persister stamps a minted id
	// onto every new child mid-write (AssignAggregateItemID), and the handler
	// stamps the root id afterwards. Neither is tampering.
	root := &aggWriteRoot{Name: "r"}
	root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{Label: "a"}})
	domain.AddAggregateChild(root, aggWriteChild{Label: "b"})

	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !tx.committed {
		t.Fatal("expected commit")
	}
}

func TestWrite_MutationBetweenTheSealAndTheWriteIsRefused(t *testing.T) {
	e := loadedTenant(t, "trial", 7)
	upd, err := domain.GetUpdatable(e, nil, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	// A handler reaching for the entity after the domain finished with it.
	e.Name = "renamed behind the seal"

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a write on a mutated entity must be refused")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "modified after the domain validated it") {
			t.Errorf("the panic must explain the refusal, got %v", r)
		}
		if tx.committed {
			t.Error("nothing may commit")
		}
	}()
	_, _ = be.Update(newBuilderCtx(), upd, tenantSchema, firingHook)
}
