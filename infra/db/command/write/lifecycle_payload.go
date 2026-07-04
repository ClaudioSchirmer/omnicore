package write

import (
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Outbox payloads for the bodyless verbs. INSERTED/UPDATED always carried the
// bound column→value map; ARCHIVED/UNARCHIVED/DELETED historically carried
// NULL, which left CDC consumers (Debezium → external subscribers, including
// the framework's own UpstreamSubscriber) with nothing but the aggregate_id.
// These builders close that gap:
//
//   - ARCHIVED/UNARCHIVED follow the INSERTED/UPDATED pattern — the full
//     WriteFields map — plus the soft-delete column reflecting the verb's
//     outcome, so a consumer that keeps archived documents can mirror the
//     transition from the payload alone.
//   - DELETED carries the structural keys only (the row is gone): the PK and,
//     for a SharedBase role with a separate FK column, the FK to the shared
//     identity — enough for a consumer to cascade or clean up references.
//
// Like every outbox payload, these are informational for the local SyncEngine
// (it re-reads the source by aggregate_id) and contractual only in shape for
// external consumers.

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
// column→value map INSERTED/UPDATED carry, plus the soft-delete column — a
// Go-side UTC timestamp on ARCHIVED (informational; the authoritative value is
// the row's database-stamped NOW()) or an explicit JSON null on UNARCHIVED —
// plus the shared-base FK when the role links its base through a separate
// column (mirroring the INSERTED payload, where the FK is injected the same
// way).
func softWritePayload(schema *TableSchema, src domain.Entity, sdCol, eventType string) domain.Fields {
	fields := schema.WriteFields(src)
	if eventType == "ARCHIVED" {
		fields[sdCol] = time.Now().UTC()
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
