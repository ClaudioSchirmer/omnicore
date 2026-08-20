package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type coreDE struct{}

func CoreDE() Module { return coreDE{} }

func (coreDE) Language() configuration.Language { return configuration.LangDE }

func (coreDE) Translations() map[string]string {
	return map[string]string{
		// Domain validation
		"RequiredFieldNotification":            "Pflichtfeld.",
		"SchemaViolationNotification":          "Der Inhalt des Anfragerumpfs entspricht nicht dem erwarteten Schema.",
		"LimitExceededNotification":            "Das angeforderte Limit überschreitet das zulässige Maximum.",

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
		"NaturalIDImmutableNotification":    "Der natürliche Schlüssel ist unveränderlich und kann nicht geändert werden.",
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
		"RecordNotFoundNotification":         "Datensatz nicht gefunden.",
		"ConcurrentModificationNotification": "Der Datensatz wurde von jemand anderem geändert. Laden Sie ihn neu und versuchen Sie es erneut.",

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
		"RequestTimeoutNotification":        "Die Anfrage hat das Zeitlimit überschritten.",
		"ReadTimeoutNotification":           "Die Anfrage wurde nicht innerhalb des Zeitlimits empfangen.",

		// Auth
		"MissingAuthorizationNotification": "Authorization-Header fehlt oder ist fehlerhaft.",
		"InvalidTokenNotification":         "Ungültiges Authentifizierungstoken.",
		"ExpiredTokenNotification":         "Authentifizierungstoken ist abgelaufen.",

		// Authorization
		"MissingPermissionNotification":    "Erforderliche Berechtigung fehlt.",
		"TenantMissingNotification":        "Mandantenkennung fehlt im authentifizierten Principal.",
		"TenantMismatchNotification":       "Die Ressource gehört zu einem anderen Mandanten.",
		"FieldAccessForbiddenNotification": "Feld nicht zugänglich.",
		"RelationalCapabilityNotification": "Diese Funktion wird von einer aus dem relationalen Backend gelesenen View nicht unterstützt; entfernen Sie RelationalSource(), um sie aus Mongo zu bedienen.",

		// Server / routing
		"InternalServerErrorNotification": "Interner Serverfehler.",
		"RouteNotFoundNotification":       "Route nicht gefunden.",
		"MethodNotAllowedNotification":    "HTTP-Methode für diese Route nicht erlaubt.",
		"PayloadTooLargeNotification":     "Anfragerumpf überschreitet die zulässige Größe.",

		// Notification context labels — the framework builds its own
		// NotificationContext values with these names (web.respondRouteNotFound,
		// respondMethodNotAllowed, respondPayloadTooLarge, the ErrorHandler's
		// "Server", the schema guards' "Schema", the auth middleware's
		// "Authorization", pipeline.contextNotInitialized's "Pipeline"), and
		// notifications.ToContextDTOs renders the context NAME through the
		// catalog. Without these entries every such response logged
		// translation.key.missing on the first hit and shipped the raw English
		// name on the wire in all seven languages.
		"Authorization": "Autorisierung",
		"Pipeline":      "Pipeline",
		"Request":       "Anfrage",
		"Route":         "Route",
		"Schema":        "Schema",
		"Server":        "Server",

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
