//go:build integration && sqlserver

package sqlserver

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The shared-base second-unique-column regression, on SQL Server. A SharedBase
// base carrying a SECOND unique column (email, beside the natural-key-derived
// ID) must let a NEW-identity insert whose email already exists raise a CLEAN
// unique violation on that column — the write is an explicit INSERT (new) /
// UPDATE-by-ID (existing) branch, so the email constraint fires like any other
// unique violation (error 2627, classified by IsUniqueViolation with the
// constraint name extracted for the ConstraintBinding).

type sbEmailStudent struct {
	domain.BaseEntity
	Name     string // shared (sb2_persons)
	Email    string // shared, the SECOND unique column
	Document string // shared + natural key (derives the deterministic id)
	Enroll   string // role-own
}

func (e *sbEmailStudent) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (*sbEmailStudent) BuildRules(string, domain.Service, *domain.Rules) {}

func sbEmailSchema() *core.TableSchema {
	base := core.NewSharedBaseSchema("sb2_persons").Revision("revision").
		ID("id").
		Field("Document", "document").
		Field("Name", "name").
		Field("Email", "email").
		NaturalID("document").
		DeletedAt("deleted_at")
	return core.NewTableSchema[*sbEmailStudent]("sb2_students").
		ID("id").
		Field("Enroll", "enrollment").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		SharedBase(base, "id") // shared-ID: sb2_students.id == sb2_persons.id
}

func sbEmailSetup(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	eng, raw := setup(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE sb2_persons (
			id BINARY(16) NOT NULL PRIMARY KEY,
			document VARCHAR(64) NOT NULL,
			name NVARCHAR(255) NOT NULL,
			email NVARCHAR(255) NOT NULL,
			revision BIGINT NOT NULL DEFAULT 0,
			deleted_at DATETIME2(6) NULL,
			created_at DATETIME2(6) NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME2(6) NOT NULL DEFAULT CURRENT_TIMESTAMP,
			CONSTRAINT sb2_persons_document_key UNIQUE (document),
			CONSTRAINT sb2_persons_email_key UNIQUE (email)
		)`,
		`CREATE TABLE sb2_students (
			id BINARY(16) NOT NULL PRIMARY KEY,
			enrollment VARCHAR(64) NOT NULL,
			deleted_at DATETIME2(6) NULL,
			created_at DATETIME2(6) NOT NULL,
			updated_at DATETIME2(6) NOT NULL,
			CONSTRAINT fk_sb2_student_person FOREIGN KEY (id) REFERENCES sb2_persons (id)
		)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl: %v\n%s", err, stmt)
		}
	}
	return eng, raw
}

func sbEmailInsert(t *testing.T, eng *Engine, doc, email, action string) error {
	t.Helper()
	ins, err := domain.GetInsertable(&sbEmailStudent{Name: "Ana", Email: email, Document: doc, Enroll: "M1"}, nil, action)
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	_, err = eng.Insert(ctxFor(), ins, sbEmailSchema(), core.WriteHook{})
	return err
}

func TestSQLServer_SharedBase_SecondUniqueColumn_DupEmailRaisesCleanViolation(t *testing.T) {
	eng, raw := sbEmailSetup(t)

	// D1 / a@x.com — a fresh identity, cold insert. Succeeds.
	if err := sbEmailInsert(t, eng, "D1", "a@x.com", "GetInsertable"); err != nil {
		t.Fatalf("first insert must succeed: %v", err)
	}

	// D2 (a NEW document → new deterministic id) with a@x.com (DUP email). This
	// must fail with the EMAIL unique violation, cleanly.
	err := sbEmailInsert(t, eng, "D2", "a@x.com", "GetInsertable")
	if err == nil {
		t.Fatal("a new identity carrying a duplicate email must fail with a unique violation, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "email") {
		t.Errorf("the violation must name the email unique constraint (not the ParentID / not the document), got: %v", err)
	}

	// No corruption: exactly ONE identity (D1), no D2 base, no D2 role.
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb2_persons`); got != 1 {
		t.Errorf("the rejected dup-email insert must not create a second identity, persons = %d", got)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb2_persons WHERE document = 'D2'`); got != 0 {
		t.Errorf("no D2 identity may exist after the rejected insert, got %d", got)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb2_students`); got != 1 {
		t.Errorf("only D1's role may exist (the D2 role must have rolled back), students = %d", got)
	}
	// D1's identity must be untouched.
	var doc string
	if err := raw.QueryRowContext(context.Background(), `SELECT document FROM sb2_persons WHERE email = 'a@x.com'`).Scan(&doc); err != nil {
		t.Fatalf("read D1 identity: %v", err)
	}
	if doc != "D1" {
		t.Errorf("a@x.com must still belong to D1 (not hijacked to D2), got %q", doc)
	}
}
