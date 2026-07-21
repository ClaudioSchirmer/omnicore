package write

import (
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Structural-key helpers of the outbox payload. DELETED carries the
// historical structural keys (the PK and, for a SharedBase role with a
// separate FK column, the FK to the shared identity) — a contract external
// consumers cascade on, so these keys only GROW (the v2 payload adds the _ids
// block on top, see outbox_payload.go). softWritePayload remains the LEGACY
// ARCHIVED/UNARCHIVED field-map builder (superseded by buildWritePayloadV2;
// kept per maintainer instruction).

// sharedBaseFKField resolves the role's separate FK column and the
// deterministic base id for payload purposes. ok=false when the schema has no
// shared base, when the role shares the base's id as its own PK (the id
// already travels as the outbox aggregate_id), or when the natural key cannot
// be read off the entity (payload assembly never vetoes a write the verb
// itself would allow).
func sharedBaseFKField(schema *TableSchema, src domain.Entity) (fkCol, baseID string, ok bool) {
	base, fkCol, has := schema.SharedBaseRef()
	if !has || fkCol == schema.PKColumn() {
		return "", "", false
	}
	_, nk := sharedBaseValues(base, src)
	if nk == "" {
		return "", "", false
	}
	return fkCol, deterministicBaseID(nk), true
}

// softWritePayload assembles the ARCHIVED/UNARCHIVED outbox payload: the same
// column→value map INSERTED/UPDATED carry, plus the soft-delete column — the
// operation's writeNow() stamp on ARCHIVED (the EXACT value bound into the
// archive UPDATE, since managed timestamps are app-clock authored) or an
// explicit JSON null on UNARCHIVED — plus the shared-base FK when the role
// links its base through a separate column (mirroring the INSERTED payload,
// where the FK is injected the same way).
func softWritePayload(schema *TableSchema, src domain.Entity, sdCol, eventType string, now time.Time) domain.Fields {
	fields := schema.WriteFields(src)
	if eventType == "ARCHIVED" {
		fields[sdCol] = now
	} else {
		fields[sdCol] = nil
	}
	if fkCol, baseID, ok := sharedBaseFKField(schema, src); ok {
		fields[fkCol] = domain.NewID(baseID)
	}
	return fields
}

// deleteKeysPayload assembles the DELETED outbox payload: structural keys only
// — the PK under its physical column name and, for a SharedBase role with a
// separate FK column, the FK to the shared identity.
func deleteKeysPayload(schema *TableSchema, src domain.Entity, id string) domain.Fields {
	keys := domain.Fields{schema.PKColumn(): domain.NewID(id)}
	if fkCol, baseID, ok := sharedBaseFKField(schema, src); ok {
		keys[fkCol] = domain.NewID(baseID)
	}
	return keys
}
