//go:build oracle

package oracle

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/sijms/go-ora/v2/network"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestIsUniqueViolation(t *testing.T) {
	d := oracleDialect{}

	t.Run("ORA-00001 with the 23ai detail line", func(t *testing.T) {
		// The live 23ai shape: an ORA-03301 detail line follows, whose
		// parentheses carry the USER-CONTROLLED duplicate value — the reason
		// the FIRST `constraint (` marker is the trusted one. The catalog
		// stores the unquoted-declared name UPPERCASE; extraction lowercases
		// it back to the declared form (D11).
		err := &network.OracleError{ErrCode: 1, ErrMsg: "ORA-00001: unique constraint (USERS_APP.UNIQ_EMAIL) violated on table USERS_APP.USERS columns (EMAIL)\nORA-03301: (ORA-00001 details) row with column values (EMAIL:'a@b.c') already exists\n"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniq_email" {
			t.Fatalf("got (%q,%v), want (uniq_email,true)", key, ok)
		}
	})

	t.Run("value contains the marker (injection) — first marker wins", func(t *testing.T) {
		err := &network.OracleError{ErrCode: 1, ErrMsg: "ORA-00001: unique constraint (USERS_APP.UNIQ_EMAIL) violated on table USERS_APP.USERS columns (EMAIL)\nORA-03301: (ORA-00001 details) row with column values (EMAIL:'x constraint (HACK.fake) y') already exists\n"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniq_email" {
			t.Fatalf("got (%q,%v), want (uniq_email,true)", key, ok)
		}
	})

	t.Run("quoted mixed-case constraint name is stripped and lowercased", func(t *testing.T) {
		err := &network.OracleError{ErrCode: 1, ErrMsg: `ORA-00001: unique constraint (USERS_APP."UniqEmail") violated`}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniqemail" {
			t.Fatalf("got (%q,%v), want (uniqemail,true)", key, ok)
		}
	})

	t.Run("ORA-00001 without a parseable name", func(t *testing.T) {
		err := &network.OracleError{ErrCode: 1, ErrMsg: "ORA-00001: violation — no name clause"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',true)", key, ok)
		}
	})

	t.Run("non-unique oracle error", func(t *testing.T) {
		if key, ok := d.IsUniqueViolation(&network.OracleError{ErrCode: 2291, ErrMsg: "fk"}); ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',false)", key, ok)
		}
	})

	t.Run("non-oracle error", func(t *testing.T) {
		if key, ok := d.IsUniqueViolation(errors.New("plain")); ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',false)", key, ok)
		}
	})
}

func TestIsForeignKeyViolation(t *testing.T) {
	d := oracleDialect{}

	t.Run("ORA-02291 parent key not found (insert/update child)", func(t *testing.T) {
		err := &network.OracleError{ErrCode: 2291, ErrMsg: "ORA-02291: integrity constraint (USERS_APP.FK_ALUNO_PESSOA) violated - parent key not found"}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "fk_aluno_pessoa" {
			t.Fatalf("got (%q,%v), want (fk_aluno_pessoa,true)", name, ok)
		}
	})

	t.Run("ORA-02292 child record found (delete/update parent — the purge veto)", func(t *testing.T) {
		err := &network.OracleError{ErrCode: 2292, ErrMsg: "ORA-02292: integrity constraint (USERS_APP.FK_ALUNO_PESSOA) violated - child record found"}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "fk_aluno_pessoa" {
			t.Fatalf("got (%q,%v), want (fk_aluno_pessoa,true)", name, ok)
		}
	})

	t.Run("ORA-02291 without a parseable constraint", func(t *testing.T) {
		err := &network.OracleError{ErrCode: 2291, ErrMsg: "ORA-02291: violated"}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',true)", name, ok)
		}
	})

	t.Run("non-FK oracle error", func(t *testing.T) {
		if name, ok := d.IsForeignKeyViolation(&network.OracleError{ErrCode: 1, ErrMsg: "dup"}); ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',false)", name, ok)
		}
	})

	t.Run("non-oracle error", func(t *testing.T) {
		if name, ok := d.IsForeignKeyViolation(errors.New("plain")); ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',false)", name, ok)
		}
	})
}

// TestPlaceholder locks Oracle's positional placeholder flavor — go-ora binds
// ordinal args as :1..:N.
func TestPlaceholder(t *testing.T) {
	d := oracleDialect{}
	if got := d.Placeholder(1); got != ":1" {
		t.Fatalf("Placeholder(1) = %q, want :1", got)
	}
	if got := d.Placeholder(12); got != ":12" {
		t.Fatalf("Placeholder(12) = %q, want :12", got)
	}
}

// TestILikeClause proves the case-insensitive LIKE renders LOWER on both sides
// so criteria.ILike/Contains/StartsWith/EndsWith are case-insensitive under ANY
// NLS_COMP/NLS_SORT session setting — the framework must not depend on how the
// operator created the database. Postgres ILIKE / MySQL LOWER-LIKE parity.
func TestILikeClause(t *testing.T) {
	got := oracleDialect{}.ILikeClause("name", ":1")
	if want := "LOWER(name) LIKE LOWER(:1)"; got != want {
		t.Fatalf("ILikeClause = %q, want %q", got, want)
	}
}

// TestNowExpr_ApplyLimit proves the two portability seams every generated
// statement rides: the current-timestamp literal is SYSTIMESTAMP (server-tz
// parity with NOW() on PG/MySQL — Oracle's CURRENT_TIMESTAMP is session-tz and
// was rejected) and the row cap is the tail FETCH FIRST clause, valid without
// an ORDER BY.
func TestNowExpr_ApplyLimit(t *testing.T) {
	d := oracleDialect{}
	if got := d.NowExpr(); got != "SYSTIMESTAMP" {
		t.Fatalf("NowExpr = %q, want SYSTIMESTAMP", got)
	}
	if got := d.ApplyLimit("SELECT 1 FROM t WHERE x = :1", 1); got != "SELECT 1 FROM t WHERE x = :1 FETCH FIRST 1 ROWS ONLY" {
		t.Fatalf("ApplyLimit = %q", got)
	}
	if got := d.ApplyLimit("SELECT id FROM t ORDER BY id", 25); got != "SELECT id FROM t ORDER BY id FETCH FIRST 25 ROWS ONLY" {
		t.Fatalf("ApplyLimit = %q", got)
	}
}

// TestEncodeArg covers the value codec the write path and the criteria
// translator bind through: TYPED identity values (domain.ID / *domain.ID /
// uuid.UUID) reach a RAW(16) column as their 16-byte form — the type IS the
// declaration — while a PLAIN string, canonical uuid shape included, ALWAYS
// passes through as text. json.RawMessage binds as a string: the native 23ai
// JSON column accepts a text bind, while the driver's []byte mapping would
// reach it as RAW.
func TestEncodeArg(t *testing.T) {
	d := oracleDialect{}
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

	t.Run("nil *domain.ID binds a TYPED raw SQL NULL", func(t *testing.T) {
		// []byte(nil) — the typed RAW NULL, mirroring the SQL Server engine's
		// typed binary NULL.
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

	t.Run("json.RawMessage binds as string (native JSON column)", func(t *testing.T) {
		raw := json.RawMessage(`{"a":1}`)
		got, ok := d.EncodeArg(raw).(string)
		if !ok || got != `{"a":1}` {
			t.Fatalf("EncodeArg(json.RawMessage) = %v (ok=%v), want the JSON text", got, ok)
		}
	})

	t.Run("plain []byte passes through (BLOB column)", func(t *testing.T) {
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

	t.Run("bool passes through (native BOOLEAN column)", func(t *testing.T) {
		if got := d.EncodeArg(true); got != true {
			t.Fatalf("EncodeArg(bool) = %v, want true", got)
		}
	})
}

// TestQuoteIdentViaDialect locks the quoted-uppercase flavor end to end
// through the Dialect surface (quoteIdent itself is covered in
// engine_pure_test.go).
func TestQuoteIdentViaDialect(t *testing.T) {
	if got := (oracleDialect{}).QuoteIdent("user_id"); got != `"USER_ID"` {
		t.Fatalf("QuoteIdent(user_id) = %q, want the quoted-uppercase form", got)
	}
}

// TestSavepointStmts locks the savepoint trio the shared-base orphan purge
// renders through the dialect (standard Oracle forms; empty release =
// discarded at COMMIT, caller skips — the T-SQL precedent).
func TestSavepointStmts(t *testing.T) {
	d := oracleDialect{}
	if got := d.Savepoint("sp"); got != "SAVEPOINT sp" {
		t.Errorf("Savepoint = %q", got)
	}
	if got := d.RollbackToSavepoint("sp"); got != "ROLLBACK TO SAVEPOINT sp" {
		t.Errorf("RollbackToSavepoint = %q", got)
	}
	if got := d.ReleaseSavepoint("sp"); got != "" {
		t.Errorf("ReleaseSavepoint = %q", got)
	}
}
