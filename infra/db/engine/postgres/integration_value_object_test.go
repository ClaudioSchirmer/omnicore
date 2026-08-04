//go:build integration && postgres

package postgres

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// --- value-object fixtures (a raw VO + an enum VO) --------------------------

type voEmail string

func (e voEmail) Value() string                                    { return string(e) }
func (e voEmail) IsValid(string, *domain.NotificationContext) bool { return e != "" }

type voTier int

const (
	voTierUnknown voTier = 0
	voTierGold    voTier = 1
	voTierSilver  voTier = 2
)

func (t voTier) Value() int                               { return int(t) }
func (t voTier) Values() []voTier                         { return []voTier{voTierGold, voTierSilver} }
func (t voTier) UnknownNotification() domain.Notification { return voTierNote{} }

type voTierNote struct{ domain.DomainNotificationBase }

type voRoot struct {
	domain.BaseEntity
	Email voEmail // raw VO over string
	Tier  voTier  // enum VO over int
	Rank  *voTier // nullable enum VO
}

func (e *voRoot) Modes() []domain.EntityMode                     { return []domain.EntityMode{domain.ModeInsert} }
func (*voRoot) BuildRules(string, domain.Service, *domain.Rules) {}

func voRootSchema() *core.TableSchema {
	return core.NewTableSchema[*voRoot]("vo_roots").
		ID("id").
		Field("Email", "email").
		Field("Tier", "tier").
		Field("Rank", "rank")
}

func createVORootTable(t *testing.T, pg *Postgres) {
	t.Helper()
	createTable(t, pg, `CREATE TABLE vo_roots (
		id    UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
		email TEXT    NOT NULL,
		tier  INTEGER NOT NULL,
		rank  INTEGER
	)`)
}

// TestPostgres_ValueObject_RoundTrip proves the full VO persistence path on real
// Postgres: the write binds the UNDERLYING scalar (not the named type), the read
// reconstructs the VO, an out-of-set enum converges to Unknown, and a nil nullable
// VO round-trips as SQL NULL.
func TestPostgres_ValueObject_RoundTrip(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createVORootTable(t, pg)

	gold := voTierGold
	root := &voRoot{Email: "ada@lovelace.dev", Tier: voTierSilver, Rank: &gold}
	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := pg.Insert(testCtx(), ins, voRootSchema(), noHook)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	id := res.ID.Value()

	// 1) WRITE unwrap — the physical columns hold the UNDERLYING scalar, never the
	// named VO type.
	rows, err := pg.Querier().QueryMaps(context.Background(),
		`SELECT email, tier, rank FROM vo_roots WHERE id = $1`, id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("raw read: err=%v rows=%d", err, len(rows))
	}
	if got := rows[0]["email"]; got != "ada@lovelace.dev" {
		t.Errorf("email column = %#v want raw string", got)
	}
	if got := toInt(rows[0]["tier"]); got != 2 {
		t.Errorf("tier column = %#v want int 2 (underlying)", rows[0]["tier"])
	}
	if got := toInt(rows[0]["rank"]); got != 1 {
		t.Errorf("rank column = %#v want int 1", rows[0]["rank"])
	}

	// 2) READ reconstruct — the loader rebuilds the VO-typed fields.
	loader := read.NewAggregateLoader[*voRoot](pg, func() *voRoot { return &voRoot{} }).WithSchema(voRootSchema())
	got, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.Email != voEmail("ada@lovelace.dev") {
		t.Errorf("Email = %q want reconstructed VO", got.Email)
	}
	if got.Tier != voTierSilver {
		t.Errorf("Tier = %d want voTierSilver", got.Tier)
	}
	if got.Rank == nil || *got.Rank != voTierGold {
		t.Errorf("Rank = %v want &voTierGold", got.Rank)
	}

	// 3) ENUM CONVERGE — tamper the column to an out-of-set value; the load
	// reconstructs Unknown, never a phantom member.
	if err := pg.Querier().Exec(context.Background(),
		`UPDATE vo_roots SET tier = 99 WHERE id = $1`, id); err != nil {
		t.Fatalf("tamper update: %v", err)
	}
	got2, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)))
	if err != nil {
		t.Fatalf("FindOne after tamper: %v", err)
	}
	if got2.Tier != voTierUnknown {
		t.Errorf("tampered Tier(99) = %d want voTierUnknown (converge)", got2.Tier)
	}
}

// TestPostgres_ValueObject_NullableNil proves a nil nullable VO binds as SQL NULL
// and reconstructs as nil.
func TestPostgres_ValueObject_NullableNil(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createVORootTable(t, pg)

	root := &voRoot{Email: "grace@hopper.dev", Tier: voTierGold, Rank: nil}
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
	res, err := pg.Insert(testCtx(), ins, voRootSchema(), noHook)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	id := res.ID.Value()

	rows, _ := pg.Querier().QueryMaps(context.Background(),
		`SELECT rank FROM vo_roots WHERE id = $1`, id)
	if len(rows) != 1 || rows[0]["rank"] != nil {
		t.Errorf("rank column = %#v want NULL", rows[0]["rank"])
	}

	loader := read.NewAggregateLoader[*voRoot](pg, func() *voRoot { return &voRoot{} }).WithSchema(voRootSchema())
	got, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.Rank != nil {
		t.Errorf("Rank = %v want nil", got.Rank)
	}
}

// toInt normalizes a driver numeric (int32/int64) to int for comparison.
func toInt(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int32:
		return int(n)
	case int:
		return n
	default:
		return -1
	}
}
