package read

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for the SharedBase UPSERT-insert pre-flight: LoadSharedBaseIdentity
// probes the specialization table by the shared base id and, when a LIVE role already
// references the identity, returns the canonical conflict — before the handler re-applies
// the request, so it is never masked by a child-level validation (e.g. a re-sent address).

func carrierHasNotification(c domain.NotificationCarrier, key string) bool {
	for _, ctx := range c.NotificationContexts() {
		for _, msg := range ctx.Messages() {
			if domain.NotificationKey(msg.Notification) == key {
				return true
			}
		}
	}
	return false
}

// roleAggLoadSchemaSD is roleAggLoadSchema (shared_base_children_load_test.go) with a
// soft-delete column on the role, so the pre-flight probe filters archived rows out.
func roleAggLoadSchemaSD() *TableSchema {
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
		Child(NewTableSchema[addrLoad]("endereco").ID("id").ParentID("pessoa_id").Field("Street", "street"))
	return NewTableSchema[*roleAggLoad]("aluno").
		ID("id").Revision("revision").
		Field("Matricula", "matricula").
		SoftDelete("deleted_at").
		SharedBase(base, "pessoa_id")
}

func TestLoadSharedBaseIdentity_ActiveRoleConflict(t *testing.T) {
	var enderecoQueried bool
	query := func(sql string, args []any) (Rows, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				if p, ok := dest[0].(*string); ok {
					*p = "p1"
				}
				if p, ok := dest[1].(*string); ok {
					*p = "Ana"
				}
				return nil
			}}, nil
		case strings.Contains(sql, "FROM aluno"): // the active-role pre-flight probe
			return &fakeDBRows{rows: 1}, nil // a live role already exists
		case strings.Contains(sql, "FROM endereco"):
			enderecoQueried = true
			return &fakeDBRows{}, nil
		}
		return &fakeDBRows{}, nil
	}
	l := NewAggregateLoader[*roleAggLoad](fakeEngine(query), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(roleAggLoadSchema())

	fresh := &roleAggLoad{Name: "Ana", Matricula: "M1"}
	_, existed, err := l.LoadSharedBaseIdentity(context.Background(), fresh)

	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("an existing active role must be a conflict NotificationCarrier, got %T (%v)", err, err)
	}
	if existed {
		t.Error("a conflict must not report existed=true")
	}
	if enderecoQueried {
		t.Error("the load must short-circuit on the conflict, before loading base-children")
	}
	if !carrierHasNotification(carrier, "EntityAlreadyAddedNotification") {
		t.Errorf("conflict must carry EntityAlreadyAddedNotification (409), got %v", carrier.NotificationContexts())
	}
}

// A soft-deleted (archived) role is NOT a conflict: the probe filters it out with an
// `IS NULL` predicate, so the load falls through to the warm hydrate (and the persister's
// revive path takes over on write).
func TestLoadSharedBaseIdentity_ProbeExcludesArchivedViaSoftDelete(t *testing.T) {
	var probeSQL string
	query := func(sql string, args []any) (Rows, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				if p, ok := dest[0].(*string); ok {
					*p = "p1"
				}
				if p, ok := dest[1].(*string); ok {
					*p = "Ana"
				}
				return nil
			}}, nil
		case strings.Contains(sql, "FROM aluno"): // the pre-flight probe
			probeSQL = sql
			return &fakeDBRows{}, nil // no LIVE role (archived rows filtered by IS NULL)
		case strings.Contains(sql, "FROM endereco"):
			return &fakeDBRows{}, nil
		}
		return &fakeDBRows{}, nil
	}
	l := NewAggregateLoader[*roleAggLoad](fakeEngine(query), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(roleAggLoadSchemaSD())

	fresh := &roleAggLoad{Name: "Ana", Matricula: "M1"}
	_, existed, err := l.LoadSharedBaseIdentity(context.Background(), fresh)
	if err != nil {
		t.Fatalf("an archived role must not conflict: %v", err)
	}
	if !existed {
		t.Error("the identity exists (archived role) — the warm hydrate must still run")
	}
	if !strings.Contains(probeSQL, "deleted_at") || !strings.Contains(probeSQL, "IS NULL") {
		t.Errorf("the probe must exclude archived rows via the soft-delete column; got %q", probeSQL)
	}
}
