package write

import "github.com/ClaudioSchirmer/omnicore/domain"

// BuildAggregatePayload is the LEGACY (pre-v2) outbox snapshot builder:
// {"root": <fields>, "children": {...active}}. The write path now emits the
// v2 shape via buildWritePayloadV2 (outbox_payload.go) — flat scalars + _ids +
// _children/_base_children with per-item ops. Kept per maintainer instruction
// as the reference of the old wire shape (the upstream decoder still unwraps
// it for pre-v2 backlog events).
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
