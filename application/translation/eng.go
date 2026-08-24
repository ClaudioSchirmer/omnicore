package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type coreENG struct{}

func CoreENG() Module { return coreENG{} }

func (coreENG) Language() configuration.Language { return configuration.LangENG }

func (coreENG) Translations() map[string]string {
	return map[string]string{
		// Domain validation
		"RequiredFieldNotification":   "Required field.",
		"SchemaViolationNotification": "Request body content does not match the expected schema.",
		"LimitExceededNotification":   "Requested limit exceeds the maximum allowed.",

		// Domain entity
		"UnableToInsertWithIDNotification":    "Cannot insert a record with an existing primary key.",
		"UnableToUpdateWithoutIDNotification": "Cannot update a record without a primary key.",
		"UnableToDeleteWithoutIDNotification": "Cannot delete a record without a primary key.",
		"InsertNotAllowedNotification":        "Insert not allowed.",
		"UpdateNotAllowedNotification":        "Update not allowed.",
		"DeleteNotAllowedNotification":        "Delete not allowed.",
		"ArchiveNotAllowedNotification":       "Archive not allowed.",
		"UnarchiveNotAllowedNotification":     "Unarchive not allowed.",
		"ServiceIsRequiredNotification":       "Service is required.",

		// Aggregate root
		"EntityAlreadyAddedNotification":    "Entity has already been added.",
		"NaturalIDImmutableNotification":    "The natural key is immutable and cannot be changed.",
		"EntityDoesNotExistNotification":    "Entity does not exist.",
		"EntityIsNotActiveNotification":     "Entity is not active.",
		"InvalidAggregateChildNotification": "This object type does not belong to this aggregate.",

		// Repository
		"RepositoryFunctionNotImplementedNotification": "Repository function is not implemented.",

		// Value objects
		"InvalidIDUUIDNotification": "Invalid primary key.",

		// Events
		"InvalidEventTypeNotification": "Invalid event type.",

		// Generic
		"RecordNotFoundNotification":         "Record not found.",
		"ConcurrentModificationNotification": "The record was changed by someone else. Reload it and try again.",

		// EntityMode descriptions
		"EntityMode.UNKNOWN":   "Unknown",
		"EntityMode.DISPLAY":   "Display",
		"EntityMode.INSERT":    "Insert",
		"EntityMode.UPDATE":    "Update",
		"EntityMode.DELETE":    "Delete",
		"EntityMode.ARCHIVE":   "Archive",
		"EntityMode.UNARCHIVE": "Unarchive",

		// AggregateItemStatus descriptions
		"AggregateItemStatus.UNKNOWN":     "Unknown",
		"AggregateItemStatus.CONSTRUCTOR": "Constructor",
		"AggregateItemStatus.ADDED":       "Added",
		"AggregateItemStatus.CHANGED":     "Changed",
		"AggregateItemStatus.REMOVED":     "Removed",

		// Generic labels
		"ID": "Primary key",

		// Application
		"InvalidLanguageDomainNotification": "Invalid language.",
		"ContextNotInitializedNotification": "Context was not initialized.",
		"ServiceUnavailableNotification":    "Service unavailable.",
		"RequestTimeoutNotification":        "The request exceeded the time limit.",
		"ReadTimeoutNotification":           "The request was not received within the time limit.",
		"UnsupportedCapabilityNotification": "This capability is not supported by the store this view is read from.",

		// Auth
		"MissingAuthorizationNotification": "Authorization header missing or malformed.",
		"InvalidTokenNotification":         "Invalid authentication token.",
		"ExpiredTokenNotification":         "Authentication token has expired.",

		// Authorization
		"MissingPermissionNotification":    "Missing required permission.",
		"TenantMissingNotification":        "Tenant identifier is missing from the authenticated principal.",
		"TenantMismatchNotification":       "Resource belongs to a different tenant.",
		"FieldAccessForbiddenNotification": "Field is not accessible.",

		// Server / routing
		"InternalServerErrorNotification": "Internal server error.",
		"RouteNotFoundNotification":       "Route not found.",
		"MethodNotAllowedNotification":    "HTTP method not allowed for this route.",
		"PayloadTooLargeNotification":     "Request payload exceeds the allowed size.",

		// Notification context labels — the framework builds its own
		// NotificationContext values with these names (web.respondRouteNotFound,
		// respondMethodNotAllowed, respondPayloadTooLarge, the ErrorHandler's
		// "Server", the schema guards' "Schema", the auth middleware's
		// "Authorization", pipeline.contextNotInitialized's "Pipeline"), and
		// notifications.ToContextDTOs renders the context NAME through the
		// catalog. Without these entries every such response logged
		// translation.key.missing on the first hit and shipped the raw English
		// name on the wire in all seven languages.
		"Authorization": "Authorization",
		"Pipeline":      "Pipeline",
		"Request":       "Request",
		"Route":         "Route",
		"Schema":        "Schema",
		"Server":        "Server",

		// Language descriptions
		"Language.UNKNOWN": "Unknown",
		"Language.PT_BR":   "Portuguese",
		"Language.ENG":     "English",
		"Language.ES":      "Spanish",
		"Language.FR":      "French",
		"Language.DE":      "German",
		"Language.IT":      "Italian",
		"Language.NL":      "Dutch",
	}
}
