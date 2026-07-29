//go:build mysql

package mysql

import (
	"bytes"
	"errors"
	"testing"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestIsUniqueViolation(t *testing.T) {
	d := mysqlDialect{}

	t.Run("plain key with table prefix", func(t *testing.T) {
		err := &driver.MySQLError{Number: 1062, Message: "Duplicate entry 'a@b.com' for key 'flat_persons.uniq_email'"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniq_email" {
			t.Fatalf("got (%q,%v), want (uniq_email,true)", key, ok)
		}
	})

	t.Run("value contains the for-key marker (injection)", func(t *testing.T) {
		// A duplicated value of `a' for key 'b` is printed unescaped, so the
		// message carries two "for key '" segments. The real index is the LAST
		// one; strings.Index would have locked onto the value's fake "b".
		err := &driver.MySQLError{Number: 1062, Message: "Duplicate entry 'a' for key 'b' for key 'uniq_email'"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniq_email" {
			t.Fatalf("got (%q,%v), want (uniq_email,true)", key, ok)
		}
	})

	t.Run("1062 without a parseable key", func(t *testing.T) {
		err := &driver.MySQLError{Number: 1062, Message: "Duplicate entry — no key clause"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',true)", key, ok)
		}
	})

	t.Run("non-1062 mysql error", func(t *testing.T) {
		if key, ok := d.IsUniqueViolation(&driver.MySQLError{Number: 1045, Message: "access denied"}); ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',false)", key, ok)
		}
	})

	t.Run("non-mysql error", func(t *testing.T) {
		if key, ok := d.IsUniqueViolation(errors.New("plain")); ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',false)", key, ok)
		}
	})
}

func TestIsForeignKeyViolation(t *testing.T) {
	d := mysqlDialect{}

	t.Run("1451 parent-row with constraint name", func(t *testing.T) {
		err := &driver.MySQLError{Number: 1451, Message: "Cannot delete or update a parent row: a foreign key constraint fails (`app`.`alunos`, CONSTRAINT `fk_aluno_pessoa` FOREIGN KEY (`pessoa_id`) REFERENCES `pessoas` (`id`))"}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "fk_aluno_pessoa" {
			t.Fatalf("got (%q,%v), want (fk_aluno_pessoa,true)", name, ok)
		}
	})

	t.Run("1452 child-row also classifies", func(t *testing.T) {
		err := &driver.MySQLError{Number: 1452, Message: "Cannot add or update a child row: a foreign key constraint fails (`app`.`alunos`, CONSTRAINT `fk_aluno_pessoa` FOREIGN KEY (`pessoa_id`) REFERENCES `pessoas` (`id`))"}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "fk_aluno_pessoa" {
			t.Fatalf("got (%q,%v), want (fk_aluno_pessoa,true)", name, ok)
		}
	})

	t.Run("1451 without a parseable constraint", func(t *testing.T) {
		err := &driver.MySQLError{Number: 1451, Message: "Cannot delete or update a parent row"}
		name, ok := d.IsForeignKeyViolation(err)
		if !ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',true)", name, ok)
		}
	})

	t.Run("non-ParentID mysql error", func(t *testing.T) {
		if name, ok := d.IsForeignKeyViolation(&driver.MySQLError{Number: 1062, Message: "Duplicate entry"}); ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',false)", name, ok)
		}
	})

	t.Run("non-mysql error", func(t *testing.T) {
		if name, ok := d.IsForeignKeyViolation(errors.New("plain")); ok || name != "" {
			t.Fatalf("got (%q,%v), want ('',false)", name, ok)
		}
	})
}

// TestILikeClause proves the case-insensitive LIKE renders LOWER on both sides so
// criteria.ILike/Contains/StartsWith/EndsWith are case-insensitive on ANY column
// collation (Postgres ILIKE parity), not only under a CI collation.
func TestILikeClause(t *testing.T) {
	got := mysqlDialect{}.ILikeClause("`name`", "?")
	if want := "LOWER(`name`) LIKE LOWER(?)"; got != want {
		t.Fatalf("ILikeClause = %q, want %q", got, want)
	}
}

// TestNowExpr_ApplyLimit proves the two portability seams every generated
// statement rides: the current-timestamp literal comes from the dialect (never
// baked into shared code) and the row cap lands as MySQL's native tail clause.
func TestNowExpr_ApplyLimit(t *testing.T) {
	d := mysqlDialect{}
	if got := d.NowExpr(); got != "NOW()" {
		t.Fatalf("NowExpr = %q, want NOW()", got)
	}
	if got := d.ApplyLimit("SELECT 1 FROM t WHERE x = ?", 1); got != "SELECT 1 FROM t WHERE x = ? LIMIT 1" {
		t.Fatalf("ApplyLimit = %q", got)
	}
	if got := d.ApplyLimit("SELECT `id` FROM t ORDER BY `id`", 25); got != "SELECT `id` FROM t ORDER BY `id` LIMIT 25" {
		t.Fatalf("ApplyLimit = %q", got)
	}
}

// TestApplyLimitOffset proves the windowed-page seam: MySQL appends the native
// `LIMIT n OFFSET m` tail after the (caller-guaranteed) ORDER BY.
func TestApplyLimitOffset(t *testing.T) {
	d := mysqlDialect{}
	if got := d.ApplyLimitOffset("SELECT `id` FROM t ORDER BY `id`", 25, 50); got != "SELECT `id` FROM t ORDER BY `id` LIMIT 25 OFFSET 50" {
		t.Fatalf("ApplyLimitOffset = %q", got)
	}
}

// TestEncodeArg covers the value codec the write path and the criteria
// translator bind through: TYPED identity values (domain.ID / *domain.ID /
// uuid.UUID) reach a BINARY(16) column as their 16-byte form — the type IS the
// declaration — while a PLAIN string, canonical uuid shape included, ALWAYS
// passes through as text (a string field means a CHAR/VARCHAR column; probes
// on id-typed fields are lifted into domain.ID by the criteria translator
// before they reach this codec).
func TestEncodeArg(t *testing.T) {
	d := mysqlDialect{}
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

	t.Run("nil *domain.ID binds SQL NULL", func(t *testing.T) {
		if got := d.EncodeArg((*domain.ID)(nil)); got != nil {
			t.Fatalf("EncodeArg(nil *domain.ID) = %v, want nil", got)
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
		// A plain string is text, ALWAYS: a string-typed field pairs with a
		// CHAR(36)/VARCHAR(36) column, so its value must arrive as-is.
		if got := d.EncodeArg(u.String()); got != u.String() {
			t.Fatalf("EncodeArg(canonical string) = %v, want it untouched", got)
		}
	})

	t.Run("non-uuid string passes through", func(t *testing.T) {
		if got := d.EncodeArg("bob@example.com"); got != "bob@example.com" {
			t.Fatalf("EncodeArg(plain string) = %v, want it untouched", got)
		}
	})

	t.Run("int passes through", func(t *testing.T) {
		if got := d.EncodeArg(42); got != 42 {
			t.Fatalf("EncodeArg(int) = %v, want 42", got)
		}
	})
}

// TestSavepointStmts locks the savepoint trio the shared-base orphan purge
// renders through the dialect (standard forms).
func TestSavepointStmts(t *testing.T) {
	d := mysqlDialect{}
	if got := d.Savepoint("sp"); got != "SAVEPOINT sp" {
		t.Errorf("Savepoint = %q", got)
	}
	if got := d.RollbackToSavepoint("sp"); got != "ROLLBACK TO SAVEPOINT sp" {
		t.Errorf("RollbackToSavepoint = %q", got)
	}
	if got := d.ReleaseSavepoint("sp"); got != "RELEASE SAVEPOINT sp" {
		t.Errorf("ReleaseSavepoint = %q", got)
	}
}
