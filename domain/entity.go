package domain

import (
	"time"
)

type ValidEntity interface {
	entity()
}

type Fields map[string]any

type metadata struct {
	entityName string
	actionName string
	dateTime   time.Time
	events     []DomainEvent
}

func newMetadata() metadata {
	return metadata{dateTime: time.Now().UTC()}
}

// aggregateMeta is attached to a ValidEntity when the source entity implements
// AggregateRootProvider. infra.Postgres uses it to dispatch the aggregate-aware
// persistence path (single transaction across root + children + one outbox row).
// nil for non-aggregate entities — the simple single-table path is used.
//
// Phase 19: only carries the *AggregateRoot. Children types are discovered via
// reflection on root.AllAggregateItems(); table/ParentID names are inferred from
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
	// entityMode is what this write IS, finally: ModeUpdate, or ModeArchive
	// when a domain rule finished it as an archive (CompleteAsArchive). It is
	// never Unknown — the reader never has to combine it with anything else to
	// learn the operation. Sealed for the same reason partial is: it is a
	// decision about the OPERATION, not data about the entity, so infra reads it
	// from this value and never from the live entity, and nothing can change it
	// once Get* has returned.
	entityMode EntityMode
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

func (i Insertable) entity()    {}
func (u Updatable) entity()     {}
func (a Archivable) entity()    {}
func (d Deletable) entity()     {}
func (un Unarchivable) entity() {}

// Source returns the validated Entity. Infra inspects it via reflection to
// derive table name, columns, and field values. Domain remains DDD-pure.
func (i Insertable) Source() Entity { return i.source }
func (i Insertable) ID() *ID        { return i.id }

func (u Updatable) Source() Entity { return u.source }
func (u Updatable) ID() ID         { return u.id }

// IsPartial reports whether the Updatable was produced by GetPartialUpdatable
// (PATCH semantic) vs GetUpdatable (PUT semantic). The write path reads it to
// scope sibling updates — a PATCH leaves unmentioned siblings untouched. The
// audit event records the PUT vs PATCH distinction through actionName, not the
// verb: both emit verb=update (the root UPDATE is identical between the two).
func (u Updatable) IsPartial() bool { return u.partial }

// EntityMode answers what this write is: ModeUpdate, or ModeArchive when a
// domain rule finished it as an archive (CompleteAsArchive). The write path
// reads it from here — the sealed value — so the operation cannot be changed
// after the domain had its say.
func (u Updatable) EntityMode() EntityMode { return u.entityMode }

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
// Children types are discovered via reflection from root.AllAggregateItems();
// table/ParentID inferred by infra.
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

func (m metadata) EntityName() string    { return m.entityName }
func (m metadata) ActionName() string    { return m.actionName }
func (m metadata) DateTime() time.Time   { return m.dateTime }
func (m metadata) Events() []DomainEvent { return m.events }

type validEntityBuilder struct {
	entityName string
	actionName string
	dateTime   time.Time
	events     []DomainEvent
	aggregate  *aggregateMeta
}

func newBuilder(entityName, actionName string, events []DomainEvent) validEntityBuilder {
	return validEntityBuilder{
		entityName: entityName,
		actionName: actionName,
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
		entityName: b.entityName,
		actionName: b.actionName,
		dateTime:   b.dateTime,
		events:     b.events,
	}
}

func (b validEntityBuilder) insertable(source Entity, id *ID) Insertable {
	return Insertable{metadata: b.buildMetadata(), source: source, id: id, aggregate: b.aggregate}
}

func (b validEntityBuilder) updatable(source Entity, id ID, partial bool, entityMode EntityMode) Updatable {
	return Updatable{
		metadata:   b.buildMetadata(),
		source:     source,
		id:         id,
		partial:    partial,
		aggregate:  b.aggregate,
		entityMode: entityMode,
	}
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
