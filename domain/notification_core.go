package domain

// RequiredFieldNotification is emitted when a required field is missing.
//
// Two paths emit it:
//   - Domain validation (BuildRules) — default Semantic = Validation → 422.
//   - Wire wrapper (CommandWithBody{,ID} with FullBody marker) — overrides
//     Semantic to Schema → 400. Use WithSemantic(SemanticSchema) to opt in.
//
// The same NotificationKey ("RequiredFieldNotification") preserves translation
// catalog compat between domain and wire emissions. Status differentiation
// happens via Semantic, not via distinct types.
type RequiredFieldNotification struct {
	DomainNotificationBase
	semanticOverride *NotificationSemantic
}

// Semantic returns the per-instance override when set, falling back to the
// domain default (Validation → 422).
func (n RequiredFieldNotification) Semantic() NotificationSemantic {
	if n.semanticOverride != nil {
		return *n.semanticOverride
	}
	return n.DomainNotificationBase.Semantic()
}

// WithSemantic returns a copy of the notification carrying the given Semantic
// override. Wire wrappers use WithSemantic(SemanticSchema) so missing required
// fields surface as 400 Bad Request instead of 422 Unprocessable Entity.
func (n RequiredFieldNotification) WithSemantic(s NotificationSemantic) RequiredFieldNotification {
	n.semanticOverride = &s
	return n
}

// SchemaViolationNotification is emitted by the wire wrapper when the request
// payload does not match the expected schema beyond simple missing fields:
//   - JSON body malformed (BodyParser cannot parse the body at all)
//   - Field present but with the wrong type (e.g. "age": "twenty" when int)
//
// Always carries Semantic = SemanticSchema → 400 Bad Request. NotificationKey
// "SchemaViolationNotification". The FieldName/Path identifies the offending
// field (empty when the whole body is malformed).
type SchemaViolationNotification struct{ DomainNotificationBase }

func (SchemaViolationNotification) Semantic() NotificationSemantic { return SemanticSchema }

// LimitExceededNotification is emitted by the read side when the requested page
// size (`?first=N`/`?last=N`) exceeds
// the resolved per-view ceiling (per-view override > yaml default > framework
// default 100). FieldName names the directional control the consumer sent
// ("first" or "last"); FieldValue carries the effective ceiling
// so the consumer can show "max is X" without parsing the message. Carries
// SemanticSchema → 400 Bad Request; translatable per language via the
// "LimitExceededNotification" key.
type LimitExceededNotification struct{ DomainNotificationBase }

func (LimitExceededNotification) Semantic() NotificationSemantic { return SemanticSchema }

type UnableToInsertWithIDNotification struct{ DomainNotificationBase }
type UnableToUpdateWithoutIDNotification struct{ DomainNotificationBase }
type UnableToDeleteWithoutIDNotification struct{ DomainNotificationBase }

type InsertNotAllowedNotification struct{ DomainNotificationBase }
type UpdateNotAllowedNotification struct{ DomainNotificationBase }
type DeleteNotAllowedNotification struct{ DomainNotificationBase }
type ArchiveNotAllowedNotification struct{ DomainNotificationBase }
type UnarchiveNotAllowedNotification struct{ DomainNotificationBase }
type ServiceIsRequiredNotification struct{ DomainNotificationBase }

type EntityAlreadyAddedNotification struct{ DomainNotificationBase }
type EntityDoesNotExistNotification struct{ DomainNotificationBase }
type EntityIsNotActiveNotification struct{ DomainNotificationBase }

// NaturalIDImmutableNotification is emitted by the SharedBase write path when
// an UPDATE carries a natural-key value that diverges from the persisted
// identity. The natural key derives the deterministic base id, so every
// SharedBase subsystem (identity upsert, refcount, lifecycle convergence, CDC
// fan-out, payload FKs) depends on it never changing after insert. Default
// Semantic (Validation → 422): the request is asking for something the model
// forbids. FieldName carries the natural key's Go field name.
type NaturalIDImmutableNotification struct{ DomainNotificationBase }

// InvalidAggregateChildNotification is emitted by AddAggregateChild,
// ChangeAggregateChild, RemoveAggregateChild and ReplaceAggregateChildrenOf
// when the item's typeName is not declared in root.AggregateChildren().
// FieldValue echoes the rejected typeName. Default Semantic (Validation → 422)
// is the right transport for "request shape violates aggregate boundary".
type InvalidAggregateChildNotification struct{ DomainNotificationBase }

type RepositoryFunctionNotImplementedNotification struct{ DomainNotificationBase }

type InvalidIDUUIDNotification struct{ DomainNotificationBase }

// MalformedIDNotification is emitted by the wire wrappers when the `:id` path
// segment of a WRITE route is not a UUID. The segment is part of the Request
// SHAPE, so the refusal is a schema violation: SemanticSchema → 400 on HTTP,
// InvalidArgument on gRPC, semantic "Schema" on GraphQL. FieldName carries the
// wire spelling of the segment, FieldValue echoes what arrived.
//
// It is deliberately not InvalidIDUUIDNotification: that one is the
// domain-validation key an aggregate raises through ID.IsValid when a value
// INSIDE a payload is wrong (422). This one says the ADDRESS the consumer
// wrote is not an address at all.
type MalformedIDNotification struct{ DomainNotificationBase }

// UnknownIDAddressNotification is the read-side sibling of
// MalformedIDNotification: a malformed `:id` on a read names no record, and a
// read answers absence with absence — SemanticNotFound → 404 on HTTP, NotFound
// on gRPC, semantic "NotFound" on GraphQL. Answering 400 there would make the
// same id return two different statuses depending on the view's backing, which
// is the divergence this whole refusal exists to remove.
//
// It stays distinct from RecordNotFoundNotification on purpose: both answer
// 404, and the separate key is what lets a consumer tell "no record has this
// id" from "what you sent is not an id" without parsing the message.
type UnknownIDAddressNotification struct{ DomainNotificationBase }

// InvalidFilterValueNotification is emitted when a filter value on the query
// string cannot be the thing the column it probes holds — "abc" against an
// integer column, a non-uuid against an identity one.
//
// It is a schema violation (400): the consumer wrote a value the endpoint's
// declared vocabulary cannot take. Before it existed, such a probe travelled to
// the driver and came back as a 500 on every relational backing, while a
// Mongo-backed view of the same view answered 200 with an empty page — the
// same request, two contracts.
//
// FieldName carries the wire key when the wire caught it and the Go field name
// when the reader did; FieldValue echoes the rejected value.
type InvalidFilterValueNotification struct{ DomainNotificationBase }

type InvalidEventTypeNotification struct{ DomainNotificationBase }

type RecordNotFoundNotification struct{ DomainNotificationBase }

// ConcurrentModificationNotification is emitted by the write path when an
// UPDATE guarded on the loaded revision matches no row while the row itself
// still exists: someone committed a write to it between the load and this
// write. The framework refuses rather than proceeding, because every write
// carries the FULL field set — letting a stale snapshot land would silently
// revert the columns the concurrent writer changed, and would put a value on
// the outbox payload that the row never held.
//
// The caller's recovery is to reload and reapply. FieldName carries the id
// column, FieldValue the id. SemanticStateConflict → 409 on HTTP and
// FailedPrecondition on gRPC: this is a precondition failure, not a duplicate.
type ConcurrentModificationNotification struct{ DomainNotificationBase }

// Kernel notification Semantic overrides — encapsulate the natural HTTP/transport
// semantics in the notification itself, so no global registry is needed.
func (RecordNotFoundNotification) Semantic() NotificationSemantic     { return SemanticNotFound }
func (MalformedIDNotification) Semantic() NotificationSemantic        { return SemanticSchema }
func (UnknownIDAddressNotification) Semantic() NotificationSemantic   { return SemanticNotFound }
func (InvalidFilterValueNotification) Semantic() NotificationSemantic { return SemanticSchema }
func (EntityIsNotActiveNotification) Semantic() NotificationSemantic  { return SemanticStateConflict }
func (ConcurrentModificationNotification) Semantic() NotificationSemantic {
	return SemanticStateConflict
}
func (EntityAlreadyAddedNotification) Semantic() NotificationSemantic  { return SemanticConflict }
func (InsertNotAllowedNotification) Semantic() NotificationSemantic    { return SemanticForbidden }
func (UpdateNotAllowedNotification) Semantic() NotificationSemantic    { return SemanticForbidden }
func (DeleteNotAllowedNotification) Semantic() NotificationSemantic    { return SemanticForbidden }
func (ArchiveNotAllowedNotification) Semantic() NotificationSemantic   { return SemanticForbidden }
func (UnarchiveNotAllowedNotification) Semantic() NotificationSemantic { return SemanticForbidden }
