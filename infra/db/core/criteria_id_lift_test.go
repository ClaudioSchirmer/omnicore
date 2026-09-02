package core

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The identity lift: a probe against a domain.ID / *domain.ID-typed field —
// the field's Go TYPE is the declaration, reported by the schema-derived
// idKind resolver — is lifted into domain.ID before EncodeArg, so it binds in
// the dialect's native id form (16 bytes on MySQL, uuid text on PG). A probe
// on a string-typed field is NEVER touched, whatever its shape: string fields
// pair with text columns.

func testIDKinds() func(string) IDKind {
	kinds := map[string]IDKind{
		"ID":        IDValue, // the managed ID slot — always identity
		"BuyerID":   IDValue,
		"PartnerID": IDPointer,
	}
	return func(f string) IDKind { return kinds[f] }
}

func liftResolver() FieldResolver {
	m := map[string]string{
		"ID": "id", "Name": "name",
		"BuyerID": "buyer_id", "PartnerID": "partner_id",
	}
	return func(f string) (ResolvedField, bool) {
		c, ok := m[f]
		return ResolvedField{Column: c}, ok
	}
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
		_, args, err := CompileWhere(criteria.Eq("BuyerID", u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("CompileWhere: %v", err)
		}
		wantBytes(t, args[0])
	})

	t.Run("bare-string ID probe binds as 16 bytes (exclude-self parity)", func(t *testing.T) {
		_, args, err := CompileWhere(criteria.Ne("ID", u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("CompileWhere: %v", err)
		}
		wantBytes(t, args[0])
	})

	t.Run("*string probe on a *domain.ID field binds as 16 bytes", func(t *testing.T) {
		s := u.String()
		_, args, err := CompileWhere(criteria.Eq("PartnerID", &s), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("CompileWhere: %v", err)
		}
		wantBytes(t, args[0])
	})

	t.Run("IN lifts every member", func(t *testing.T) {
		_, args, err := CompileWhere(criteria.In("BuyerID", u.String(), u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("CompileWhere: %v", err)
		}
		wantBytes(t, args[0])
		wantBytes(t, args[1])
	})

	t.Run("uuid-shaped probe on a STRING field stays text", func(t *testing.T) {
		_, args, err := CompileWhere(criteria.Eq("Name", u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("CompileWhere: %v", err)
		}
		if got, ok := args[0].(string); !ok || got != u.String() {
			t.Fatalf("arg = %v (%T), want the untouched text", args[0], args[0])
		}
	})

	// A non-uuid probe on an identity column used to degrade to text and let
	// the COLUMN reject it — which reaches the consumer as a 500, since a
	// driver error carries no notification. The reader refuses it instead, so
	// the same typo answers 400 on every engine rather than 500 on Postgres
	// and a codec failure on MySQL/SQL Server/Oracle.
	t.Run("synthetic (non-uuid) id is refused, not bound", func(t *testing.T) {
		_, _, err := CompileWhere(criteria.Eq("BuyerID", "the-id"), liftResolver(), d, testIDKinds())
		if err == nil {
			t.Fatal("expected a typed refusal for a non-uuid probe on an identity column")
		}
		var infra *InfrastructureError
		if !errors.As(err, &infra) {
			t.Fatalf("expected *InfrastructureError, got %T", err)
		}
		msgs := infra.Contexts[0].Messages()
		if got := domain.NotificationKey(msgs[0].Notification); got != "InvalidFilterValueNotification" {
			t.Fatalf("notification = %q, want InvalidFilterValueNotification", got)
		}
		if msgs[0].FieldValue != "the-id" {
			t.Fatalf("echo = %q, want the rejected probe", msgs[0].FieldValue)
		}
	})

	t.Run("nil idKind resolver lifts nothing", func(t *testing.T) {
		_, args, err := CompileWhere(criteria.Eq("BuyerID", u.String()), liftResolver(), d, nil)
		if err != nil {
			t.Fatalf("CompileWhere: %v", err)
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
		_, args, err := CompileWhere(criteria.Eq("BuyerID", u.String()), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("CompileWhere: %v", err)
		}
		if got, ok := args[0].(string); !ok || got != u.String() {
			t.Fatalf("arg = %v (%T), want the canonical text", args[0], args[0])
		}
	})

	t.Run("nil *string probe on a *domain.ID field binds SQL NULL", func(t *testing.T) {
		var s *string
		_, args, err := CompileWhere(criteria.Eq("PartnerID", s), liftResolver(), d, testIDKinds())
		if err != nil {
			t.Fatalf("CompileWhere: %v", err)
		}
		if args[0] != nil {
			t.Fatalf("arg = %v (%T), want nil (SQL NULL)", args[0], args[0])
		}
	})
}
