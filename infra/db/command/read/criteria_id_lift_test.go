package read

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The identity lift: a probe against a domain.ID / *domain.ID-typed field —
// the field's Go TYPE is the declaration, reported by the schema-derived
// idKind resolver — is lifted into domain.ID before EncodeArg, so it binds in
// the dialect's native id form (16 bytes on MySQL, uuid text on PG). A probe
// on a string-typed field is NEVER touched, whatever its shape: string fields
// pair with text columns.

func testIDKinds() func(string) core.IDKind {
	kinds := map[string]core.IDKind{
		"ID":        core.IDValue, // the managed PK slot — always identity
		"BuyerID":   core.IDValue,
		"PartnerID": core.IDPointer,
	}
	return func(f string) core.IDKind { return kinds[f] }
}

func liftResolver() core.FieldResolver {
	m := map[string]string{
		"ID": "id", "Name": "name",
		"BuyerID": "buyer_id", "PartnerID": "partner_id",
	}
	return func(f string) (string, bool) { c, ok := m[f]; return c, ok }
}

func TestIDLift_MySQL(t *testing.T) {
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	d := testMySQLDialect{}

	wantBytes := func(t *testing.T, arg any) {
		t.Helper()
		b, ok := arg.([]byte)
		if !ok || !bytes.Equal(b, u[:]) {
			t.Fatalf("arg = %v (%T), want the 16-byte id form", arg, arg)
		}
	}

	t.Run("bare-string probe on a domain.ID field binds as 16 bytes", func(t *testing.T) {
		_, args, err := compileWhere(criteria.Eq("BuyerID", u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		wantBytes(t, args[0])
	})

	t.Run("bare-string PK probe binds as 16 bytes (exclude-self parity)", func(t *testing.T) {
		_, args, err := compileWhere(criteria.Ne("ID", u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		wantBytes(t, args[0])
	})

	t.Run("*string probe on a *domain.ID field binds as 16 bytes", func(t *testing.T) {
		s := u.String()
		_, args, err := compileWhere(criteria.Eq("PartnerID", &s), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		wantBytes(t, args[0])
	})

	t.Run("IN lifts every member", func(t *testing.T) {
		_, args, err := compileWhere(criteria.In("BuyerID", u.String(), u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		wantBytes(t, args[0])
		wantBytes(t, args[1])
	})

	t.Run("uuid-shaped probe on a STRING field stays text", func(t *testing.T) {
		_, args, err := compileWhere(criteria.Eq("Name", u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		if got, ok := args[0].(string); !ok || got != u.String() {
			t.Fatalf("arg = %v (%T), want the untouched text", args[0], args[0])
		}
	})

	t.Run("synthetic (non-uuid) id degrades to text inside the codec", func(t *testing.T) {
		_, args, err := compileWhere(criteria.Eq("BuyerID", "the-id"), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		if got, ok := args[0].(string); !ok || got != "the-id" {
			t.Fatalf("arg = %v (%T), want text (the column rejects, never the codec)", args[0], args[0])
		}
	})

	t.Run("nil idKind resolver lifts nothing", func(t *testing.T) {
		_, args, err := compileWhere(criteria.Eq("BuyerID", u.String()), liftResolver(), d, nil)
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		if got, ok := args[0].(string); !ok || got != u.String() {
			t.Fatalf("arg = %v (%T), want text (no resolver, no lift)", args[0], args[0])
		}
	})
}

func TestIDLift_Postgres(t *testing.T) {
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	d := testPGDialect{}

	t.Run("lifted probe resolves to its canonical text (pgx binds uuid as text)", func(t *testing.T) {
		_, args, err := compileWhere(criteria.Eq("BuyerID", u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		if got, ok := args[0].(string); !ok || got != u.String() {
			t.Fatalf("arg = %v (%T), want the canonical text", args[0], args[0])
		}
	})

	t.Run("nil *string probe on a *domain.ID field binds SQL NULL", func(t *testing.T) {
		var s *string
		_, args, err := compileWhere(criteria.Eq("PartnerID", s), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("compileWhere: %v", err)
		}
		if args[0] != nil {
			t.Fatalf("arg = %v (%T), want nil (SQL NULL)", args[0], args[0])
		}
	})
}
