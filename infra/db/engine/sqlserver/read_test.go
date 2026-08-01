//go:build sqlserver

package sqlserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestIsUniqueViolation(t *testing.T) {
	d := sqlserverDialect{}

	t.Run("2627 ID/unique constraint", func(t *testing.T) {
		err := mssql.Error{Number: 2627, Message: "Violation of PRIMARY KEY constraint 'users_pkey'. Cannot insert duplicate key in object 'dbo.users'. The duplicate key value is (0x0102)."}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "users_pkey" {
			t.Fatalf("got (%q,%v), want (users_pkey,true)", key, ok)
		}
	})

	t.Run("2601 unique index", func(t *testing.T) {
		err := mssql.Error{Number: 2601, Message: "Cannot insert duplicate key row in object 'dbo.flat_persons' with unique index 'uniq_email'. The duplicate key value is (a@b.com)."}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniq_email" {
			t.Fatalf("got (%q,%v), want (uniq_email,true)", key, ok)
		}
	})

	t.Run("value contains the marker (injection) — value prints AFTER the name", func(t *testing.T) {
		// SQL Server prints the user-controlled duplicate value after the
		// trusted name segment, so the FIRST marker wins; a crafted value
		// carrying "unique index 'fake'" cannot divert the parse (the inverse
		// of MySQL, whose value precedes the key and forces LastIndex).
		err := mssql.Error{Number: 2601, Message: "Cannot insert duplicate key row in object 'dbo.t' with unique index 'uniq_email'. The duplicate key value is (x' with unique index 'fake)."}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniq_email" {
			t.Fatalf("got (%q,%v), want (uniq_email,true)", key, ok)
		}
	})

	t.Run("2627 without a parseable name", func(t *testing.T) {
		err := mssql.Error{Number: 2627, Message: "Violation — no name clause"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',true)", key, ok)
		}
	})

	t.Run("non-unique mssql error", func(t *testing.T) {
		if key, ok := d.IsUniqueViolation(mssql.Error{Number: 547, Message: "fk"}); ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',false)", key, ok)
		}
	})

	t.Run("non-mssql error", func(t *testing.T) {
		if key, ok := d.IsUniqueViolation(errors.New("plain")); ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',false)", key, ok)
		}
	})
}

func TestIsForeignKeyViolation(t *testing.T) {
	d := sqlserverDialect{}

	t.Run("547 REFERENCE constraint (delete/update parent)", func(t *testing.T) {
		err := mssql.Error{Number: 547, Message: `The DELETE statement conflicted with the REFERENCE constraint "fk_aluno_pessoa". The conflict occurred in database "app", table "dbo.alunos", column 'pessoa_id'.`}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "fk_aluno_pessoa" {
			t.Fatalf("got (%q,%v), want (fk_aluno_pessoa,true)", name, ok)
		}
	})

	t.Run("547 FOREIGN KEY constraint (insert/update child)", func(t *testing.T) {
		err := mssql.Error{Number: 547, Message: `The INSERT statement conflicted with the FOREIGN KEY constraint "fk_aluno_pessoa". The conflict occurred in database "app", table "dbo.pessoas".`}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "fk_aluno_pessoa" {
			t.Fatalf("got (%q,%v), want (fk_aluno_pessoa,true)", name, ok)
		}
	})

	t.Run("547 without a parseable constraint", func(t *testing.T) {
		err := mssql.Error{Number: 547, Message: "The statement conflicted"}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',true)", name, ok)
		}
	})

	t.Run("non-ParentID mssql error", func(t *testing.T) {
		if name, ok := d.IsForeignKeyViolation(mssql.Error{Number: 2627, Message: "dup"}); ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',false)", name, ok)
		}
	})

	t.Run("non-mssql error", func(t *testing.T) {
		if name, ok := d.IsForeignKeyViolation(errors.New("plain")); ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',false)", name, ok)
		}
	})
}

// TestPlaceholder locks SQL Server's positional placeholder flavor — go-mssqldb
// binds ordinal args as @p1..@pN.
func TestPlaceholder(t *testing.T) {
	d := sqlserverDialect{}
	if got := d.Placeholder(1); got != "@p1" {
		t.Fatalf("Placeholder(1) = %q, want @p1", got)
	}
	if got := d.Placeholder(12); got != "@p12" {
		t.Fatalf("Placeholder(12) = %q, want @p12", got)
	}
}

// TestILikeClause proves the case-insensitive LIKE renders LOWER on both sides
// so criteria.ILike/Contains/StartsWith/EndsWith are case-insensitive under ANY
// column collation — the framework must not depend on the server's default CI
// collation. Postgres ILIKE / MySQL LOWER-LIKE parity.
func TestILikeClause(t *testing.T) {
	got := sqlserverDialect{}.ILikeClause("[name]", "@p1")
	// ESCAPE '\' (Fix #11): SQL Server LIKE has no default escape, so the backslash
	// the criteria pattern builder uses must be declared explicitly.
	if want := `LOWER([name]) LIKE LOWER(@p1) ESCAPE '\'`; got != want {
		t.Fatalf("ILikeClause = %q, want %q", got, want)
	}
}

// TestLikeClause proves the case-SENSITIVE LIKE forces byte-exact comparison via
// an inline COLLATE so criteria.OpLike is case-sensitive regardless of the
// server's default CI collation — honoring OpLike's documented contract.
func TestLikeClause(t *testing.T) {
	got := sqlserverDialect{}.LikeClause("[name]", "@p1")
	if want := `[name] LIKE @p1 COLLATE Latin1_General_BIN ESCAPE '\'`; got != want {
		t.Fatalf("LikeClause = %q, want %q", got, want)
	}
}

// TestNowExpr_ApplyLimit proves the two portability seams every generated
// statement rides: the current-timestamp literal is CURRENT_TIMESTAMP
// (server-tz parity with NOW() on PG/MySQL) and the row cap is a SELECT-head
// TOP — the statement rewrite that motivated ApplyLimit taking the COMPLETE
// SELECT instead of returning a tail fragment.
func TestNowExpr_ApplyLimit(t *testing.T) {
	d := sqlserverDialect{}
	if got := d.NowExpr(); got != "CURRENT_TIMESTAMP" {
		t.Fatalf("NowExpr = %q, want CURRENT_TIMESTAMP", got)
	}
	if got := d.ApplyLimit("SELECT 1 FROM t WHERE x = @p1", 1); got != "SELECT TOP 1 1 FROM t WHERE x = @p1" {
		t.Fatalf("ApplyLimit = %q", got)
	}
	if got := d.ApplyLimit("SELECT [id] FROM t ORDER BY [id]", 25); got != "SELECT TOP 25 [id] FROM t ORDER BY [id]" {
		t.Fatalf("ApplyLimit = %q", got)
	}

	t.Run("non-SELECT statement panics (contract violation)", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("ApplyLimit did not panic on a non-SELECT statement")
			}
		}()
		_ = d.ApplyLimit("UPDATE t SET x = 1", 1)
	})
}

// TestApplyLimitOffset proves the windowed-page seam: unlike ApplyLimit's
// SELECT-head TOP (which cannot skip), SQL Server appends the T-SQL
// `OFFSET m ROWS FETCH NEXT n ROWS ONLY` tail after the (caller-guaranteed)
// ORDER BY — no head rewrite.
func TestApplyLimitOffset(t *testing.T) {
	d := sqlserverDialect{}
	if got := d.ApplyLimitOffset("SELECT [id] FROM t ORDER BY [id]", 25, 50); got != "SELECT [id] FROM t ORDER BY [id] OFFSET 50 ROWS FETCH NEXT 25 ROWS ONLY" {
		t.Fatalf("ApplyLimitOffset = %q", got)
	}
}

// TestEncodeArg covers the value codec the write path and the criteria
// translator bind through: TYPED identity values (domain.ID / *domain.ID /
// uuid.UUID) reach a BINARY(16) column as their 16-byte form — the type IS the
// declaration — while a PLAIN string, canonical uuid shape included, ALWAYS
// passes through as text. The SQL Server-only case: json.RawMessage binds as a
// string, because its column shape is NVARCHAR(MAX) and SQL Server will not
// implicitly convert the driver's varbinary mapping of []byte into NVARCHAR.
func TestEncodeArg(t *testing.T) {
	d := sqlserverDialect{}
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	want := u[:]

	t.Run("domain.ID encodes to 16 bytes", func(t *testing.T) {
		got, ok := d.EncodeArg(domain.NewID(u.String())).([]byte)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("EncodeArg(domain.ID) = %v (ok=%v), want %x", got, ok, want)
		}
	})

	t.Run("non-nil *domain.ID encodes to 16 bytes", func(t *testing.T) {
		id := domain.NewID(u.String())
		got, ok := d.EncodeArg(&id).([]byte)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("EncodeArg(*domain.ID) = %v (ok=%v), want %x", got, ok, want)
		}
	})

	t.Run("nil *domain.ID binds a TYPED binary SQL NULL", func(t *testing.T) {
		// []byte(nil) — not untyped nil: go-mssqldb sends untyped nil as an
		// nvarchar NULL, which SQL Server refuses to convert into BINARY(16).
		got, ok := d.EncodeArg((*domain.ID)(nil)).([]byte)
		if !ok || got != nil {
			t.Fatalf("EncodeArg(nil *domain.ID) = %v (%T), want []byte(nil)", got, got)
		}
	})

	t.Run("non-uuid domain.ID degrades to its text value", func(t *testing.T) {
		if got := d.EncodeArg(domain.NewID("synthetic-id")); got != "synthetic-id" {
			t.Fatalf("EncodeArg(non-uuid domain.ID) = %v, want text passthrough", got)
		}
	})

	t.Run("uuid.UUID encodes to 16 bytes", func(t *testing.T) {
		got, ok := d.EncodeArg(u).([]byte)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("EncodeArg(uuid.UUID) = %v (ok=%v), want %x", got, ok, want)
		}
	})

	t.Run("canonical UUID string passes through as text", func(t *testing.T) {
		if got := d.EncodeArg(u.String()); got != u.String() {
			t.Fatalf("EncodeArg(canonical string) = %v, want it untouched", got)
		}
	})

	t.Run("json.RawMessage binds as string (NVARCHAR(MAX) column)", func(t *testing.T) {
		raw := json.RawMessage(`{"a":1}`)
		got, ok := d.EncodeArg(raw).(string)
		if !ok || got != `{"a":1}` {
			t.Fatalf("EncodeArg(json.RawMessage) = %v (ok=%v), want the JSON text", got, ok)
		}
	})

	t.Run("plain []byte passes through (VARBINARY column)", func(t *testing.T) {
		b := []byte{0x01, 0x02}
		got, ok := d.EncodeArg(b).([]byte)
		if !ok || !bytes.Equal(got, b) {
			t.Fatalf("EncodeArg([]byte) = %v (ok=%v), want the bytes untouched", got, ok)
		}
	})

	t.Run("int passes through", func(t *testing.T) {
		if got := d.EncodeArg(42); got != 42 {
			t.Fatalf("EncodeArg(int) = %v, want 42", got)
		}
	})
}

// TestQuoteIdentViaDialect locks the bracket flavor end to end through the
// Dialect surface (quoteIdent itself is covered in engine_pure_test.go).
func TestQuoteIdentViaDialect(t *testing.T) {
	if got := (sqlserverDialect{}).QuoteIdent("user_id"); got != "[user_id]" {
		t.Fatalf("QuoteIdent(user_id) = %q, want [user_id]", got)
	}
}

// TestSavepointStmts locks the savepoint trio the shared-base orphan purge
// renders through the dialect (T-SQL forms; empty release = discarded at COMMIT, caller skips).
func TestSavepointStmts(t *testing.T) {
	d := sqlserverDialect{}
	if got := d.Savepoint("sp"); got != "SAVE TRANSACTION sp" {
		t.Errorf("Savepoint = %q", got)
	}
	if got := d.RollbackToSavepoint("sp"); got != "ROLLBACK TRANSACTION sp" {
		t.Errorf("RollbackToSavepoint = %q", got)
	}
	if got := d.ReleaseSavepoint("sp"); got != "" {
		t.Errorf("ReleaseSavepoint = %q", got)
	}
}
