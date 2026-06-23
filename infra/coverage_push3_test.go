package infra

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ─── Target 2: aggregate child-write error branches ─────────────────────────

// childWriteErr returns a fakeTx.queryRowFn that succeeds on the root INSERT
// (cov_aggs table) but fails on the child INSERT/UPDATE (cov_children table),
// so insertChildren / applyChildChanges surface the per-child write error
// instead of the root write error.
func childWriteErr(scanID string) func(sql string, args []any) pgx.Row {
	return func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "cov_children") {
			return &fakeRow{err: errFake}
		}
		return &fakeRow{id: scanID}
	}
}

func TestInsertAggregate_ChildInsertError(t *testing.T) {
	root := &covAgg{Name: "agg"}
	domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")

	pool := newFakePool()
	pool.tx.queryRowFn = childWriteErr(pool.tx.scanID)
	pg := newFakePostgres(pool)
	if _, err := pg.Insert(newBuilderCtx(), ins, covAggSchema, writeHook{}); err == nil {
		t.Fatal("expected child INSERT error")
	}
	if !pool.tx.rolledBack {
		t.Error("expected rollback after child insert error")
	}
}

func TestUpdateAggregate_AddedChildInsertError(t *testing.T) {
	root := newCovAgg(t)
	u, _ := domain.GetUpdatable(root, func(r *covAgg) error {
		domain.AddAggregateChild(r, covChild{ID: "c2", Label: "new"})
		return nil
	}, nil, "GetUpdatable")

	pool := newFakePool()
	pool.tx.queryRowFn = childWriteErr(pool.tx.scanID)
	pg := newFakePostgres(pool)
	if _, err := pg.Update(newBuilderCtx(), u, covAggSchema, writeHook{}); err == nil {
		t.Fatal("expected Added-child INSERT error")
	}
}

func TestUpdateAggregate_ChangedChildUpdateError(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "old"})
	u, _ := domain.GetUpdatable(root, func(r *covAgg) error {
		domain.ChangeAggregateChild(r, covChild{ID: "c1", Label: "old"}, covChild{ID: "c1", Label: "changed"})
		return nil
	}, nil, "GetUpdatable")

	pool := newFakePool()
	pool.tx.queryRowFn = childWriteErr(pool.tx.scanID)
	pg := newFakePostgres(pool)
	if _, err := pg.Update(newBuilderCtx(), u, covAggSchema, writeHook{}); err == nil {
		t.Fatal("expected Changed-child UPDATE error")
	}
}

func TestUpdateAggregate_RemovedChildArchiveError(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "gone"})
	u, _ := domain.GetUpdatable(root, func(r *covAgg) error {
		domain.RemoveAggregateChild(r, covChild{ID: "c1", Label: "gone"})
		return nil
	}, nil, "GetUpdatable")

	// The Removed child triggers archiveChild → tx.Exec; force it (and every
	// Exec) to fail. Root UPDATE goes through QueryRow, so it succeeds first.
	pool := newFakePool()
	pool.tx.execErr = errFake
	pg := newFakePostgres(pool)
	if _, err := pg.Update(newBuilderCtx(), u, covAggSchema, writeHook{}); err == nil {
		t.Fatal("expected Removed-child archive Exec error")
	}
	if !pool.tx.rolledBack {
		t.Error("expected rollback after child archive error")
	}
}

// archiveChild surfaces a loud error when the Removed child has an empty id.
func TestArchiveChild_EmptyID_Errors(t *testing.T) {
	child := covAggSchema.childSchema("covChild")
	if child == nil {
		t.Fatal("covChild schema not found")
	}
	err := archiveChild(newBuilderCtx(), newFakePool().tx, child, "covChild", covChild{ID: "", Label: "x"})
	if err == nil {
		t.Fatal("expected error archiving child without id")
	}
}

// archiveChild surfaces an error when the child schema declares no SoftDelete.
func TestArchiveChild_NoSoftDelete_Errors(t *testing.T) {
	noSD := NewTableSchema[covChild]("cov_children").
		PK("id").FK("cov_agg_id").Field("Label", "label")
	err := archiveChild(newBuilderCtx(), newFakePool().tx, noSD, "covChild", covChild{ID: "c1", Label: "x"})
	if err == nil {
		t.Fatal("expected error archiving child without SoftDelete column")
	}
}

// insertChildren / applyChildChanges short-circuit on a nil root.
func TestChildWriters_NilRoot(t *testing.T) {
	if err := insertChildren(newBuilderCtx(), newFakePool().tx, nil, covAggSchema, "id"); err != nil {
		t.Errorf("insertChildren(nil root) = %v, want nil", err)
	}
	if err := applyChildChanges(newBuilderCtx(), newFakePool().tx, nil, covAggSchema, "id"); err != nil {
		t.Errorf("applyChildChanges(nil root) = %v, want nil", err)
	}
}

// ─── Target 3: audit_builder pure functions ─────────────────────────────────

func TestGoFieldValues_EdgeCases(t *testing.T) {
	if got := goFieldValues(nil, &builderTestEntity{}); len(got) != 0 {
		t.Errorf("nil schema → %v, want empty", got)
	}
	// Non-struct value (after deref) → empty map.
	if got := goFieldValues(builderTestSchema, 42); len(got) != 0 {
		t.Errorf("non-struct → %v, want empty", got)
	}
	// Happy path keyed by Go field name.
	got := goFieldValues(builderTestSchema, &builderTestEntity{Name: "alice", Email: "a@x.com"})
	if got["Name"] != "alice" || got["Email"] != "a@x.com" {
		t.Errorf("goFieldValues = %v", got)
	}
}

func TestExtractTenantID_Shapes(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"nil", nil, ""},
		{"absent", map[string]any{"x": "y"}, ""},
		{"string", map[string]any{"tenant_id": "acme"}, "acme"},
		{"sliceString", map[string]any{"tenant_id": []string{"acme"}}, "acme"},
		{"sliceStringMulti", map[string]any{"tenant_id": []string{"a", "b"}}, ""},
		{"sliceAny", map[string]any{"tenant_id": []any{"acme"}}, "acme"},
		{"sliceAnyNonString", map[string]any{"tenant_id": []any{42}}, ""},
		{"unsupported", map[string]any{"tenant_id": 99}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractTenantID(c.claims); got != c.want {
				t.Errorf("extractTenantID = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOldFieldsOf_Branches(t *testing.T) {
	// nil entity → nil.
	if got := oldFieldsOf(builderTestSchema, nil); got != nil {
		t.Errorf("nil entity → %v, want nil", got)
	}
	// Entity with no Old (insert path) → nil.
	fresh := &builderTestEntity{Name: "alice"}
	if got := oldFieldsOf(builderTestSchema, fresh); got != nil {
		t.Errorf("no-Old entity → %v, want nil", got)
	}
	// Entity carrying Old (update path) → pre-mutation snapshot.
	e := newFlatEntity()
	u, err := domain.GetUpdatable(e, func(x *builderTestEntity) error { x.Name = "bob"; return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	got := oldFieldsOf(builderTestSchema, u.Source())
	if got == nil || got["Name"] != "alice" {
		t.Errorf("old snapshot = %v, want Name=alice", got)
	}
}

// childEventOf's remaining branches via direct construction of AggregateItem.
func TestChildEventOf_RemainingBranches(t *testing.T) {
	child := covAggSchema.childSchema("covChild")
	mk := func(status domain.AggregateItemStatus) domain.AggregateItem[domain.AggregateValueObject] {
		return domain.NewAggregateItem[domain.AggregateValueObject](covChild{ID: "c1", Label: "x"}, status)
	}

	// update + Changed → updated + changes.
	prev := map[string]map[string]map[string]any{
		"covChild": {"c1": {"Label": "old"}},
	}
	ev, ok := childEventOf(mk(domain.StatusChanged), child, "covChild", "update", prev)
	if !ok || ev.Op != "updated" {
		t.Errorf("update/Changed → %+v ok=%v, want updated", ev, ok)
	}

	// update + Constructor → skipped (default).
	if _, ok := childEventOf(mk(domain.StatusConstructor), child, "covChild", "update", nil); ok {
		t.Error("update/Constructor should be skipped")
	}

	// archive + Removed → skipped.
	if _, ok := childEventOf(mk(domain.StatusRemoved), child, "covChild", "archive", nil); ok {
		t.Error("archive/Removed should be skipped")
	}
	// unarchive + Removed → skipped.
	if _, ok := childEventOf(mk(domain.StatusRemoved), child, "covChild", "unarchive", nil); ok {
		t.Error("unarchive/Removed should be skipped")
	}
	// delete + Removed → skipped.
	if _, ok := childEventOf(mk(domain.StatusRemoved), child, "covChild", "delete", nil); ok {
		t.Error("delete/Removed should be skipped")
	}
	// unknown verb → skipped.
	if _, ok := childEventOf(mk(domain.StatusAdded), child, "covChild", "bogus", nil); ok {
		t.Error("unknown verb should be skipped")
	}
}

// ─── Target 4: execWithTx remaining op-type / error branches ────────────────

// flatNoSoftDelete is builderTestEntity's schema without SoftDelete, so the
// Archive/Unarchive requireSoftDelete error branch in execWithTx is reachable.
var flatNoSoftDelete = NewTableSchema[*builderTestEntity]("builder_test_entities").
	PK("id").Field("Name", "name").Field("Email", "email")

func runBatchOp(t *testing.T, op domain.ValidEntity, schema *TableSchema, mutate func(*fakePool)) error {
	t.Helper()
	pool := newFakePool()
	if mutate != nil {
		mutate(pool)
	}
	pg := newFakePostgres(pool)
	_, err := pg.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{op}), []*TableSchema{schema})
	return err
}

func TestExecWithTx_InsertOutboxError(t *testing.T) {
	ins := mustInsertable(t, &builderTestEntity{Name: "i", Email: "i@x.com"})
	// Root INSERT succeeds via QueryRow; outbox Exec fails.
	if err := runBatchOp(t, ins, builderTestSchema, func(p *fakePool) { p.tx.execErr = errFake }); err == nil {
		t.Fatal("expected insert outbox Exec error")
	}
}

func TestExecWithTx_ArchiveNoSoftDelete(t *testing.T) {
	arch := mustArchivable(t, newFlatEntity())
	if err := runBatchOp(t, arch, flatNoSoftDelete, nil); err == nil {
		t.Fatal("expected requireSoftDelete error on Archive")
	}
}

func TestExecWithTx_UnarchiveNoSoftDelete(t *testing.T) {
	una := mustUnarchivable(t, newFlatEntity())
	if err := runBatchOp(t, una, flatNoSoftDelete, nil); err == nil {
		t.Fatal("expected requireSoftDelete error on Unarchive")
	}
}

func TestExecWithTx_ArchiveExecError(t *testing.T) {
	arch := mustArchivable(t, newFlatEntity())
	if err := runBatchOp(t, arch, builderTestSchema, func(p *fakePool) { p.tx.execErr = errFake }); err == nil {
		t.Fatal("expected Archive Exec error")
	}
}

func TestExecWithTx_DeleteExecError(t *testing.T) {
	del := mustDeletable(t, newFlatEntity())
	if err := runBatchOp(t, del, builderTestSchema, func(p *fakePool) { p.tx.execErr = errFake }); err == nil {
		t.Fatal("expected Delete Exec error")
	}
}

func TestExecWithTx_UpdateOutboxError(t *testing.T) {
	upd := mustUpdatable(t, newFlatEntity())
	if err := runBatchOp(t, upd, builderTestSchema, func(p *fakePool) { p.tx.execErr = errFake }); err == nil {
		t.Fatal("expected Update outbox Exec error")
	}
}

// A Batch nested inside a Batch op hits the execWithTx default "unknown entity
// type" branch (Batch is a ValidEntity but none of the write interfaces).
func TestExecWithTx_UnknownEntityType(t *testing.T) {
	inner := mustInsertable(t, &builderTestEntity{Name: "i", Email: "i@x.com"})
	nested := domain.NewBatch([]domain.ValidEntity{inner})
	if err := runBatchOp(t, nested, builderTestSchema, nil); err == nil {
		t.Fatal("expected unknown entity type error for nested Batch")
	}
}
