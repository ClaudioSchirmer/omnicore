package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type coreDE struct{}

func CoreDE() Module { return coreDE{} }

func (coreDE) Language() configuration.Language { return configuration.LangDE }

func (coreDE) Translations() map[string]string {
	return map[string]string{
		// Domain validation
		"RequiredFieldNotification":   "Pflichtfeld.",
		"SchemaViolationNotification": "Der Inhalt des Anfragerumpfs entspricht nicht dem erwarteten Schema.",
		"LimitExceededNotification":   "Das angeforderte Limit überschreitet das zulässige Maximum.",

		// Domain entity
		"UnableToInsertWithIDNotification":    "Ein Datensatz mit bereits vorhandenem Primärschlüssel kann nicht eingefügt werden.",
		"UnableToUpdateWithoutIDNotification": "Ein Datensatz ohne Primärschlüssel kann nicht aktualisiert werden.",
		"UnableToDeleteWithoutIDNotification": "Ein Datensatz ohne Primärschlüssel kann nicht gelöscht werden.",
		"InsertNotAllowedNotification":        "Einfügen nicht erlaubt.",
		"UpdateNotAllowedNotification":        "Aktualisierung nicht erlaubt.",
		"DeleteNotAllowedNotification":        "Löschen nicht erlaubt.",
		"ArchiveNotAllowedNotification":       "Archivierung nicht erlaubt.",
		"UnarchiveNotAllowedNotification":     "Wiederherstellung nicht erlaubt.",
		"ServiceIsRequiredNotification":       "Dienst ist erforderlich.",

		// Aggregate root
		"EntityAlreadyAddedNotification":    "Entität wurde bereits hinzugefügt.",
		"EntityDoesNotExistNotification":    "Entität existiert nicht.",
		"EntityIsNotActiveNotification":     "Entität ist nicht aktiv.",
		"InvalidAggregateChildNotification": "Dieser Objekttyp gehört nicht zu diesem Aggregat.",

		// Repository
		"RepositoryFunctionNotImplementedNotification": "Repository-Funktion ist nicht implementiert.",

		// Value objects
		"InvalidIDUUIDNotification": "Ungültiger Primärschlüssel.",

		// Events
		"InvalidEventTypeNotification": "Ungültiger Ereignistyp.",

		// Generic
		"RecordNotFoundNotification": "Datensatz nicht gefunden.",

		// EntityMode descriptions
		"EntityMode.UNKNOWN":   "Unbekannt",
		"EntityMode.DISPLAY":   "Anzeige",
		"EntityMode.INSERT":    "Einfügen",
		"EntityMode.UPDATE":    "Aktualisieren",
		"EntityMode.DELETE":    "Löschen",
		"EntityMode.ARCHIVE":   "Archivieren",
		"EntityMode.UNARCHIVE": "Wiederherstellen",

		// AggregateItemStatus descriptions
		"AggregateItemStatus.UNKNOWN":     "Unbekannt",
		"AggregateItemStatus.CONSTRUCTOR": "Konstruktor",
		"AggregateItemStatus.ADDED":       "Hinzugefügt",
		"AggregateItemStatus.CHANGED":     "Geändert",
		"AggregateItemStatus.REMOVED":     "Entfernt",

		// Generic labels
		"ID": "Primärschlüssel",

		// Application
		"InvalidLanguageDomainNotification": "Ungültige Sprache.",
		"ContextNotInitializedNotification": "Kontext wurde nicht initialisiert.",
		"ServiceUnavailableNotification":    "Dienst nicht verfügbar.",

		// Auth
		"MissingAuthorizationNotification": "Authorization-Header fehlt oder ist fehlerhaft.",
		"InvalidTokenNotification":         "Ungültiges Authentifizierungstoken.",
		"ExpiredTokenNotification":         "Authentifizierungstoken ist abgelaufen.",

		// Authorization
		"MissingPermissionNotification":    "Erforderliche Berechtigung fehlt.",
		"TenantMissingNotification":        "Mandantenkennung fehlt im authentifizierten Principal.",
		"TenantMismatchNotification":       "Die Ressource gehört zu einem anderen Mandanten.",
		"FieldAccessForbiddenNotification": "Feld nicht zugänglich.",

		// Server / routing
		"InternalServerErrorNotification": "Interner Serverfehler.",
		"RouteNotFoundNotification":       "Route nicht gefunden.",
		"MethodNotAllowedNotification":    "HTTP-Methode für diese Route nicht erlaubt.",
		"PayloadTooLargeNotification":     "Anfragerumpf überschreitet die zulässige Größe.",

		// Language descriptions
		"Language.UNKNOWN": "Unbekannt",
		"Language.PT_BR":   "Portugiesisch",
		"Language.ENG":     "Englisch",
		"Language.ES":      "Spanisch",
		"Language.FR":      "Französisch",
		"Language.DE":      "Deutsch",
		"Language.IT":      "Italienisch",
		"Language.NL":      "Niederländisch",
	}
}
