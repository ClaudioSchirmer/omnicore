package domain

import (
	"time"

	"github.com/google/uuid"
)

type ValidEntity interface {
	entity()
}

type Fields map[string]any

type metadata struct {
	signature  uuid.UUID
	entityName string
	actionName string
	dateTime   time.Time
	events     []DomainEvent
}

func newMetadata() metadata {
	return metadata{
		signature: uuid.New(),
		dateTime:  time.Now().UTC(),
	}
}

// aggregateMeta is attached to a ValidEntity when the source entity implements
// AggregateRootProvider. infra.Postgres uses it to dispatch the aggregate-aware
// persistence path (single transaction across root + children + one outbox row).
// nil for non-aggregate entities — the simple single-table path is used.
//
// Phase 19: only carries the *AggregateRoot. Children types are discovered via
// reflection on root.AllAggregateItems(); table/FK names are inferred from
// the Go type names by infra (with optional per-Repository overrides).
type aggregateMeta struct {
	root *AggregateRoot
}

// Phase 19: ValidEntity flavors now carry the Entity directly — a validation
// attestation ("this entity passed the checks"). Infra resolves table/fields
// via reflection at the moment of persistence. DDD-pure domain doesn't
// pronounce table or column names.
type Insertable struct {
	metadata
	source    Entity
	id        *ID
	aggregate *aggregateMeta
}

type Updatable struct {
	metadata
	source    Entity
	id        ID
	partial   bool
	aggregate *aggregateMeta
}

type Archivable struct {
	metadata
	source    Entity
	id        ID
	aggregate *aggregateMeta
}

type Deletable struct {
	metadata
	source    Entity
	id        ID
	aggregate *aggregateMeta
}

type Unarchivable struct {
	metadata
	source    Entity
	id        ID
	aggregate *aggregateMeta
}

type Batch struct {
	operations []ValidEntity
}

func (i Insertable) entity()    {}
func (u Updatable) entity()     {}
func (a Archivable) entity()    {}
func (d Deletable) entity()     {}
func (un Unarchivable) entity() {}
func (b Batch) entity()         {}

// Source returns the validated Entity. Infra inspects it via reflection to
// derive table name, columns, and field values. Domain remains DDD-pure.
func (i Insertable) Source() Entity { return i.source }
func (i Insertable) ID() *ID        { return i.id }

func (u Updatable) Source() Entity { return u.source }
func (u Updatable) ID() ID         { return u.id }

// IsPartial reports whether the Updatable was produced by GetPartialUpdatable
// (PATCH semantic) vs GetUpdatable (PUT semantic). The auditor reads it to
// emit verb=partialUpdate vs verb=update on the v2 audit event shape; SQL
// persistence ignores the flag (UPDATE is identical between full and partial).
func (u Updatable) IsPartial() bool { return u.partial }

func (a Archivable) Source() Entity { return a.source }
func (a Archivable) ID() ID         { return a.id }

func (d Deletable) Source() Entity { return d.source }
func (d Deletable) ID() ID         { return d.id }

func (un Unarchivable) Source() Entity { return un.source }
func (un Unarchivable) ID() ID         { return un.id }

// AggregateInfo returns the aggregate root and ok=true when this ValidEntity
// was produced from an entity implementing AggregateRootProvider.
// ok=false means the simple single-table path applies.
//
// Phase 19: signature lost the mapping return — children types are discovered
// via reflection from root.AllAggregateItems(); table/FK inferred by infra.
func (i Insertable) AggregateInfo() (root *AggregateRoot, ok bool) {
	return aggregateInfo(i.aggregate)
}
func (u Updatable) AggregateInfo() (root *AggregateRoot, ok bool) {
	return aggregateInfo(u.aggregate)
}
func (a Archivable) AggregateInfo() (root *AggregateRoot, ok bool) {
	return aggregateInfo(a.aggregate)
}
func (d Deletable) AggregateInfo() (root *AggregateRoot, ok bool) {
	return aggregateInfo(d.aggregate)
}
func (un Unarchivable) AggregateInfo() (root *AggregateRoot, ok bool) {
	return aggregateInfo(un.aggregate)
}

func aggregateInfo(m *aggregateMeta) (*AggregateRoot, bool) {
	if m == nil {
		return nil, false
	}
	return m.root, true
}

func (b Batch) Operations() []ValidEntity { return b.operations }

func (m metadata) Signature() uuid.UUID  { return m.signature }
func (m metadata) EntityName() string    { return m.entityName }
func (m metadata) ActionName() string    { return m.actionName }
func (m metadata) DateTime() time.Time   { return m.dateTime }
func (m metadata) Events() []DomainEvent { return m.events }

func NewBatch(ops []ValidEntity) Batch {
	return Batch{operations: ops}
}

type validEntityBuilder struct {
	entityName string
	actionName string
	signature  uuid.UUID
	dateTime   time.Time
	events     []DomainEvent
	aggregate  *aggregateMeta
}

func newBuilder(entityName, actionName string, signature uuid.UUID, events []DomainEvent) validEntityBuilder {
	return validEntityBuilder{
		entityName: entityName,
		actionName: actionName,
		signature:  signature,
		dateTime:   time.Now().UTC(),
		events:     events,
	}
}

func (b validEntityBuilder) withAggregate(meta *aggregateMeta) validEntityBuilder {
	b.aggregate = meta
	return b
}

func (b validEntityBuilder) buildMetadata() metadata {
	return metadata{
		signature:  b.signature,
		entityName: b.entityName,
		actionName: b.actionName,
		dateTime:   b.dateTime,
		events:     b.events,
	}
}

func (b validEntityBuilder) insertable(source Entity, id *ID) Insertable {
	return Insertable{metadata: b.buildMetadata(), source: source, id: id, aggregate: b.aggregate}
}

func (b validEntityBuilder) updatable(source Entity, id ID, partial bool) Updatable {
	return Updatable{metadata: b.buildMetadata(), source: source, id: id, partial: partial, aggregate: b.aggregate}
}

func (b validEntityBuilder) archivable(source Entity, id ID) Archivable {
	return Archivable{metadata: b.buildMetadata(), source: source, id: id, aggregate: b.aggregate}
}

func (b validEntityBuilder) deletable(source Entity, id ID) Deletable {
	return Deletable{metadata: b.buildMetadata(), source: source, id: id, aggregate: b.aggregate}
}

func (b validEntityBuilder) unarchivable(source Entity, id ID) Unarchivable {
	return Unarchivable{metadata: b.buildMetadata(), source: source, id: id, aggregate: b.aggregate}
}
