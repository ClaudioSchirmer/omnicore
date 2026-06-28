package write

import "github.com/ClaudioSchirmer/omnicore/domain"

// BuildAggregatePayload assembles the outbox JSON snapshot for an aggregate
// write: the root fields plus the active (non-Removed) children grouped by Go
// type name. It is dialect-neutral — it touches only domain.Fields, the
// AggregateRoot item map, and the TableSchema — so every relational engine
// emits the byte-identical snapshot shape. The payload is informational: the
// SyncEngine re-reads the authoritative state from the database; keeping a
// single builder here is what guarantees Postgres and MySQL never diverge on
// the outbox/CDC payload.
func BuildAggregatePayload(rootFields domain.Fields, root *domain.AggregateRoot, schema *TableSchema) map[string]any {
	payload := map[string]any{"root": rootFields}
	if root == nil {
		return payload
	}
	children := map[string][]domain.Fields{}
	for typeName, items := range root.AllAggregateItems() {
		child := schema.ChildSchema(typeName)
		if child == nil {
			continue
		}
		var active []domain.Fields
		for _, it := range items {
			if it.CurrentStatus == domain.StatusRemoved {
				continue
			}
			active = append(active, child.WriteFields(it.Item))
		}
		if len(active) > 0 {
			children[typeName] = active
		}
	}
	if len(children) > 0 {
		payload["children"] = children
	}
	return payload
}
