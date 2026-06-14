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
		"EntityAlreadyAddedNotification":      "Entity has already been added.",
		"EntityDoesNotExistNotification":      "Entity does not exist.",
		"EntityIsNotActiveNotification":       "Entity is not active.",
		"InvalidAggregateChildNotification":   "This object type does not belong to this aggregate.",

		// Repository
		"RepositoryFunctionNotImplementedNotification": "Repository function is not implemented.",

		// Value objects
		"InvalidIDUUIDNotification": "Invalid primary key.",

		// Events
		"InvalidEventTypeNotification": "Invalid event type.",

		// Generic
		"RecordNotFoundNotification": "Record not found.",

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

		// Auth
		"MissingAuthorizationNotification": "Authorization header missing or malformed.",
		"InvalidTokenNotification":         "Invalid authentication token.",
		"ExpiredTokenNotification":         "Authentication token has expired.",

		// Authorization
		"MissingPermissionNotification": "Missing required permission.",
		"TenantMissingNotification":     "Tenant identifier is missing from the authenticated principal.",
		"TenantMismatchNotification":    "Resource belongs to a different tenant.",

		// Server / routing
		"InternalServerErrorNotification": "Internal server error.",
		"RouteNotFoundNotification":       "Route not found.",
		"MethodNotAllowedNotification":    "HTTP method not allowed for this route.",
		"PayloadTooLargeNotification":     "Request payload exceeds the allowed size.",

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
