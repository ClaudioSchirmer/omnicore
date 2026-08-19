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

// ComputedFieldNotSortableNotification is emitted by the read side when a
// consumer orders by a COMPUTED field — one the Response declares with a
// `computed:"…"` tag, derived by the Query's FromQueryResult hook after the
// read instead of stored as a column. Ordering happens in the store and the
// keyset cursor is built from stored ordering values, so sorting by a derived
// value is not expressible; the field stays selectable and filterable-free,
// only the ordering is refused.
//
// FieldName names the offending control in the surface's own spelling
// ("orderBy[display]" on REST/export, the sort entry on gRPC). Carries
// SemanticSchema → 400 Bad Request; translatable per language via the
// "ComputedFieldNotSortableNotification" key.
type ComputedFieldNotSortableNotification struct{ DomainNotificationBase }

func (ComputedFieldNotSortableNotification) Semantic() NotificationSemantic {
	return SemanticSchema
}

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
func (RecordNotFoundNotification) Semantic() NotificationSemantic    { return SemanticNotFound }
func (EntityIsNotActiveNotification) Semantic() NotificationSemantic { return SemanticStateConflict }
func (ConcurrentModificationNotification) Semantic() NotificationSemantic {
	return SemanticStateConflict
}
func (EntityAlreadyAddedNotification) Semantic() NotificationSemantic  { return SemanticConflict }
func (InsertNotAllowedNotification) Semantic() NotificationSemantic    { return SemanticForbidden }
func (UpdateNotAllowedNotification) Semantic() NotificationSemantic    { return SemanticForbidden }
func (DeleteNotAllowedNotification) Semantic() NotificationSemantic    { return SemanticForbidden }
func (ArchiveNotAllowedNotification) Semantic() NotificationSemantic   { return SemanticForbidden }
func (UnarchiveNotAllowedNotification) Semantic() NotificationSemantic { return SemanticForbidden }
