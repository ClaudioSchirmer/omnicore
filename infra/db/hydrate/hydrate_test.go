package hydrate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ─── the rendered SELECT ─────────────────────────────────────────────────────

// The DeletedAt gate is a POLICY the caller passes down, and the rendered SQL is
// where that policy becomes observable. These lock both shapes.
func TestBuildFetchSQL_IncludeArchivedOmitsTheGate(t *testing.T) {
	cases := []struct {
		name, verb, table, keyCol, want string
	}{
		{"row", "row", "orders", "id", "SELECT id, name FROM orders WHERE id = $1 LIMIT 1"},
		{"where", "where", "lines", "order_id", "SELECT id, name FROM lines WHERE order_id = $1"},
		{"all", "all", "orders", "", "SELECT id, name FROM orders"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildFetchSQL(pgLikeDialect{}, c.verb, c.table, []string{"id", "name"}, c.keyCol, "deleted_at", true)
			if got != c.want {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestBuildFetchSQL_GateAppliedWhenArchivedExcluded(t *testing.T) {
	cases := []struct {
		name, verb, table, keyCol, want string
	}{
		{"row", "row", "orders", "id", "SELECT id, name FROM orders WHERE id = $1 AND deleted_at IS NULL LIMIT 1"},
		{"where", "where", "lines", "order_id", "SELECT id, name FROM lines WHERE order_id = $1 AND deleted_at IS NULL"},
		{"all", "all", "orders", "", "SELECT id, name FROM orders WHERE deleted_at IS NULL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildFetchSQL(pgLikeDialect{}, c.verb, c.table, []string{"id", "name"}, c.keyCol, "deleted_at", false)
			if got != c.want {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// A source with no DeletedAt column has no archived state to gate on, so the
// clause is absent whatever the caller asks for.
func TestBuildFetchSQL_NoDeletedAtColumnNeverGates(t *testing.T) {
	got := BuildFetchSQL(pgLikeDialect{}, "where", "notes", []string{"id"}, "owner_id", "", false)
	if strings.Contains(got, "IS NULL") {
		t.Fatalf("a source without DeletedAt must emit no gate, got %q", got)
	}
}

// Never SELECT *: an explicit column list is what keeps a read stable across an
// online ADD COLUMN. The "*" fallback exists only for a caller that passes none,
// which a real schema never does.
func TestSelectList_FallsBackToStarOnlyWhenEmpty(t *testing.T) {
	if got := selectList(pgLikeDialect{}, nil); got != "*" {
		t.Errorf("empty column list = %q, want *", got)
	}
	if got := selectList(pgLikeDialect{}, []string{"a", "b"}); got != "a, b" {
		t.Errorf("column list = %q, want %q", got, "a, b")
	}
}

// ─── the fetch primitives ────────────────────────────────────────────────────

func TestFetchRow_ReturnsNilOnNoMatch(t *testing.T) {
	h := New(newScripted(nil))
	row, err := h.FetchRow(context.Background(), flatSchema(), "notes", "id", "n1", "", true)
	if err != nil {
		t.Fatalf("FetchRow: %v", err)
	}
	if row != nil {
		t.Fatalf("no match must be a nil row, got %v", row)
	}
}

func TestFetchRow_CoercesBoolOnTheWayOut(t *testing.T) {
	// MySQL yields a TINYINT(1) as int64 on the dynamic read; the schema is
	// type-anchored, so the value must come back a real bool.
	eng := newScripted(map[string][]map[string]any{
		"orders": {{"id": "o1", "name": "first", "active": int64(1)}},
	})
	h := New(eng)
	row, err := h.FetchRow(context.Background(), rootSchema(), "orders", "id", "o1", "deleted_at", false)
	if err != nil {
		t.Fatalf("FetchRow: %v", err)
	}
	if row["active"] != true {
		t.Errorf("active = %#v, want true", row["active"])
	}
	if !strings.Contains(eng.sqls[0], "deleted_at IS NULL") {
		t.Errorf("the gate must reach the statement: %q", eng.sqls[0])
	}
}

func TestFetchRow_PropagatesTheEngineError(t *testing.T) {
	eng := newScripted(nil)
	eng.err = errors.New("connection reset")
	h := New(eng)
	if _, err := h.FetchRow(context.Background(), flatSchema(), "notes", "id", "n1", "", true); err == nil {
		t.Fatal("a failed read must surface, not be swallowed")
	}
}

func TestFetchWhere_ReturnsEveryMatch(t *testing.T) {
	h := New(newScripted(map[string][]map[string]any{
		"lines": {{"id": "l1", "label": "a", "order_id": "o1"}, {"id": "l2", "label": "b", "order_id": "o1"}},
	}))
	rows, err := h.FetchWhere(context.Background(), lineSchema(), "lines", "order_id", "o1", "deleted_at", false)
	if err != nil {
		t.Fatalf("FetchWhere: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected both rows, got %d", len(rows))
	}
}

func TestFetchAll_ReadsTheWholeTable(t *testing.T) {
	h := New(newScripted(map[string][]map[string]any{
		"notes": {{"id": "n1", "label": "x"}},
	}))
	rows, err := h.FetchAll(context.Background(), flatSchema(), "notes", "", true)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
}

// The id set is chunked so no single IN (...) predicate exceeds the tightest
// backend's expression-list ceiling.
func TestFetchByIDs_ChunksBeyondTheClauseCeiling(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{"notes": {{"id": "n1", "label": "x"}}})
	h := New(eng)
	ids := make([]string, MaxInClauseSize+5)
	for i := range ids {
		ids[i] = "n" + string(rune('a'+i%26))
	}
	if _, err := h.FetchByIDs(context.Background(), flatSchema(), "notes", "id", ids, "", true); err != nil {
		t.Fatalf("FetchByIDs: %v", err)
	}
	if len(eng.sqls) != 2 {
		t.Fatalf("expected 2 chunked lookups for %d ids (cap %d), got %d", len(ids), MaxInClauseSize, len(eng.sqls))
	}
	if len(eng.args[0]) != MaxInClauseSize || len(eng.args[1]) != 5 {
		t.Errorf("chunk sizes = %d, %d; want %d, 5", len(eng.args[0]), len(eng.args[1]), MaxInClauseSize)
	}
}

func TestFetchByIDs_EmptySetIssuesNoStatement(t *testing.T) {
	eng := newScripted(nil)
	rows, err := New(eng).FetchByIDs(context.Background(), flatSchema(), "notes", "id", nil, "", true)
	if err != nil {
		t.Fatalf("FetchByIDs: %v", err)
	}
	if len(rows) != 0 || len(eng.sqls) != 0 {
		t.Fatalf("an empty id set must read nothing, got %d rows / %d statements", len(rows), len(eng.sqls))
	}
}

// The archived remnant pick is deterministic: the most recently archived row.
func TestFetchLatestArchived_OrdersByDeletedAtDesc(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{
		"students": {{"id": "s1", "grade": "A", "person_id": "p1"}},
	})
	row, err := New(eng).FetchLatestArchived(context.Background(), roleSchema(), "person_id", "p1", "deleted_at")
	if err != nil {
		t.Fatalf("FetchLatestArchived: %v", err)
	}
	if row == nil {
		t.Fatal("expected the remnant row")
	}
	sql := eng.sqls[0]
	if !strings.Contains(sql, "deleted_at IS NOT NULL") || !strings.Contains(sql, "ORDER BY deleted_at DESC") {
		t.Errorf("the remnant pick must be archived-only and deterministic: %q", sql)
	}
}

func TestFetchLatestArchived_NoRemnantIsNilNotError(t *testing.T) {
	row, err := New(newScripted(nil)).FetchLatestArchived(context.Background(), roleSchema(), "person_id", "p1", "deleted_at")
	if err != nil || row != nil {
		t.Fatalf("absent remnant = (%v, %v), want (nil, nil)", row, err)
	}
}

// ─── grouping ────────────────────────────────────────────────────────────────

func TestFetchInGrouped_DedupesKeysAndGroupsByThem(t *testing.T) {
	eng := newScripted(map[string][]map[string]any{
		"lines": {
			{"id": "l1", "label": "a", "order_id": "o1"},
			{"id": "l2", "label": "b", "order_id": "o1"},
			{"id": "l3", "label": "c", "order_id": "o2"},
		},
	})
	got, err := New(eng).FetchInGrouped(context.Background(), lineSchema(), "lines", "order_id",
		[]string{"o1", "o1", "o2"}, "deleted_at", false)
	if err != nil {
		t.Fatalf("FetchInGrouped: %v", err)
	}
	if len(got["o1"]) != 2 || len(got["o2"]) != 1 {
		t.Fatalf("grouping = %v", got)
	}
	if n := len(eng.args[0]); n != 2 {
		t.Errorf("duplicate keys must be deduped before binding: bound %d, want 2", n)
	}
}

func TestFetchInGrouped_NoKeysIssuesNoStatement(t *testing.T) {
	eng := newScripted(nil)
	got, err := New(eng).FetchInGrouped(context.Background(), lineSchema(), "lines", "order_id", nil, "", true)
	if err != nil {
		t.Fatalf("FetchInGrouped: %v", err)
	}
	if len(got) != 0 || len(eng.sqls) != 0 {
		t.Fatalf("no keys must read nothing, got %d groups / %d statements", len(got), len(eng.sqls))
	}
}

// ─── value helpers ───────────────────────────────────────────────────────────

func TestCoerceTypes_OnlyBoolColumnsAndOnlyIntegers(t *testing.T) {
	s := rootSchema()
	row := Document{"active": int64(1), "name": "x"}
	CoerceTypes(row, s)
	if row["active"] != true {
		t.Errorf("int64(1) must become true, got %#v", row["active"])
	}
	if row["name"] != "x" {
		t.Errorf("a non-bool column must pass through, got %#v", row["name"])
	}

	row = Document{"active": int(0)}
	CoerceTypes(row, s)
	if row["active"] != false {
		t.Errorf("int(0) must become false, got %#v", row["active"])
	}

	// A SQL NULL stays nil, and a real bool is already right.
	row = Document{"active": nil}
	CoerceTypes(row, s)
	if row["active"] != nil {
		t.Errorf("NULL must stay nil, got %#v", row["active"])
	}
	CoerceTypes(nil, s)
	CoerceTypes(Document{"active": int64(1)}, nil)
}

func TestRemapRevision_MovesThePhysicalColumnOntoTheWatermark(t *testing.T) {
	row := Document{"revision": int64(7), "name": "x"}
	RemapRevision(row, rootSchema(), "_revision")
	if row["_revision"] != int64(7) {
		t.Errorf("_revision = %#v, want 7", row["_revision"])
	}
	if _, still := row["revision"]; still {
		t.Error("the physical revision column must not survive as a document field")
	}
	// A schema with no revision column, a nil row and a nil schema are no-ops.
	RemapRevision(Document{"x": 1}, flatSchema(), "_revision")
	RemapRevision(nil, rootSchema(), "_revision")
	RemapRevision(Document{}, nil, "_revision")
}

func TestToDocuments_ConvertsAndCoerces(t *testing.T) {
	got := ToDocuments([]map[string]any{{"active": int64(1)}}, rootSchema())
	if len(got) != 1 || got[0]["active"] != true {
		t.Fatalf("ToDocuments = %v", got)
	}
}

func TestKeyOf_AbsentOrNullIsEmpty(t *testing.T) {
	if got := KeyOf(Document{"id": "o1"}, "id"); got != "o1" {
		t.Errorf("KeyOf = %q, want o1", got)
	}
	if got := KeyOf(Document{}, "id"); got != "" {
		t.Errorf("an absent column must key as empty, got %q", got)
	}
	if got := KeyOf(Document{"id": nil}, "id"); got != "" {
		t.Errorf("a NULL column must key as empty, got %q", got)
	}
}

func TestCollectKeys_SkipsTheUnkeyed(t *testing.T) {
	got := CollectKeys([]Document{{"id": "a"}, {}, {"id": nil}, {"id": "b"}}, "id")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("CollectKeys = %v, want [a b]", got)
	}
}

// A childless root must compose an EMPTY array, never a missing field: the
// reader downstream would render the two differently.
func TestEmptyIfNil_NormalizesTheAbsentGroup(t *testing.T) {
	if got := EmptyIfNil(nil); got == nil || len(got) != 0 {
		t.Fatalf("EmptyIfNil(nil) = %v, want an empty non-nil slice", got)
	}
	rows := []Document{{"id": "x"}}
	if got := EmptyIfNil(rows); len(got) != 1 {
		t.Fatalf("a populated group must pass through, got %v", got)
	}
}

func TestSchemaAccessors(t *testing.T) {
	if got := SchemaPK(rootSchema()); got != "id" {
		t.Errorf("SchemaPK = %q", got)
	}
	col, ok := SchemaDeletedAt(rootSchema())
	if !ok || col != "deleted_at" {
		t.Errorf("SchemaDeletedAt = (%q, %v)", col, ok)
	}
	if _, ok := SchemaDeletedAt(flatSchema()); ok {
		t.Error("a schema declaring no DeletedAt must report none")
	}
}

func TestEngine_IsExposedForACallerBuildingItsOwnRead(t *testing.T) {
	eng := newScripted(nil)
	if New(eng).Engine() == nil {
		t.Error("Engine must expose the wrapped engine")
	}
}
