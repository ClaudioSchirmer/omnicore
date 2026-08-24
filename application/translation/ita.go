package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type coreIT struct{}

func CoreIT() Module { return coreIT{} }

func (coreIT) Language() configuration.Language { return configuration.LangIT }

func (coreIT) Translations() map[string]string {
	return map[string]string{
		// Domain validation
		"RequiredFieldNotification":   "Campo obbligatorio.",
		"SchemaViolationNotification": "Il contenuto del corpo della richiesta non corrisponde allo schema previsto.",
		"LimitExceededNotification":   "Il limite richiesto supera il massimo consentito.",

		// Domain entity
		"UnableToInsertWithIDNotification":    "Impossibile inserire un record con una chiave primaria esistente.",
		"UnableToUpdateWithoutIDNotification": "Impossibile aggiornare un record senza chiave primaria.",
		"UnableToDeleteWithoutIDNotification": "Impossibile eliminare un record senza chiave primaria.",
		"InsertNotAllowedNotification":        "Inserimento non consentito.",
		"UpdateNotAllowedNotification":        "Aggiornamento non consentito.",
		"DeleteNotAllowedNotification":        "Eliminazione non consentita.",
		"ArchiveNotAllowedNotification":       "Archiviazione non consentita.",
		"UnarchiveNotAllowedNotification":     "Ripristino non consentito.",
		"ServiceIsRequiredNotification":       "Il servizio è obbligatorio.",

		// Aggregate root
		"EntityAlreadyAddedNotification":    "Entità già aggiunta.",
		"NaturalIDImmutableNotification":    "La chiave naturale è immutabile e non può essere modificata.",
		"EntityDoesNotExistNotification":    "Entità inesistente.",
		"EntityIsNotActiveNotification":     "Entità non attiva.",
		"InvalidAggregateChildNotification": "Questo tipo di oggetto non appartiene a questo aggregato.",

		// Repository
		"RepositoryFunctionNotImplementedNotification": "Funzione del repository non implementata.",

		// Value objects
		"InvalidIDUUIDNotification": "Chiave primaria non valida.",

		// Events
		"InvalidEventTypeNotification": "Tipo di evento non valido.",

		// Generic
		"RecordNotFoundNotification":         "Record non trovato.",
		"ConcurrentModificationNotification": "Il record è stato modificato da qualcun altro. Ricaricalo e riprova.",

		// EntityMode descriptions
		"EntityMode.UNKNOWN":   "Sconosciuto",
		"EntityMode.DISPLAY":   "Visualizzazione",
		"EntityMode.INSERT":    "Inserisci",
		"EntityMode.UPDATE":    "Aggiorna",
		"EntityMode.DELETE":    "Elimina",
		"EntityMode.ARCHIVE":   "Archivia",
		"EntityMode.UNARCHIVE": "Ripristina",

		// AggregateItemStatus descriptions
		"AggregateItemStatus.UNKNOWN":     "Sconosciuto",
		"AggregateItemStatus.CONSTRUCTOR": "Costruttore",
		"AggregateItemStatus.ADDED":       "Aggiunto",
		"AggregateItemStatus.CHANGED":     "Modificato",
		"AggregateItemStatus.REMOVED":     "Rimosso",

		// Generic labels
		"ID": "Chiave primaria",

		// Application
		"InvalidLanguageDomainNotification": "Lingua non valida.",
		"ContextNotInitializedNotification": "Il contesto non è stato inizializzato.",
		"ServiceUnavailableNotification":    "Servizio non disponibile.",
		"RequestTimeoutNotification":        "La richiesta ha superato il limite di tempo.",
		"ReadTimeoutNotification":           "La richiesta non è stata ricevuta entro il limite di tempo.",
		"UnsupportedCapabilityNotification": "Questa funzionalità non è supportata dall'archivio da cui viene letta questa vista.",

		// Auth
		"MissingAuthorizationNotification": "Header di autorizzazione mancante o malformato.",
		"InvalidTokenNotification":         "Token di autenticazione non valido.",
		"ExpiredTokenNotification":         "Il token di autenticazione è scaduto.",

		// Authorization
		"MissingPermissionNotification":    "Permesso richiesto mancante.",
		"TenantMissingNotification":        "Identificatore del tenant assente nel principal autenticato.",
		"TenantMismatchNotification":       "La risorsa appartiene a un altro tenant.",
		"FieldAccessForbiddenNotification": "Campo non accessibile.",

		// Server / routing
		"InternalServerErrorNotification": "Errore interno del server.",
		"RouteNotFoundNotification":       "Rotta non trovata.",
		"MethodNotAllowedNotification":    "Metodo HTTP non consentito per questa rotta.",
		"PayloadTooLargeNotification":     "Il corpo della richiesta supera la dimensione consentita.",

		// Notification context labels — the framework builds its own
		// NotificationContext values with these names (web.respondRouteNotFound,
		// respondMethodNotAllowed, respondPayloadTooLarge, the ErrorHandler's
		// "Server", the schema guards' "Schema", the auth middleware's
		// "Authorization", pipeline.contextNotInitialized's "Pipeline"), and
		// notifications.ToContextDTOs renders the context NAME through the
		// catalog. Without these entries every such response logged
		// translation.key.missing on the first hit and shipped the raw English
		// name on the wire in all seven languages.
		"Authorization": "Autorizzazione",
		"Pipeline":      "Pipeline",
		"Request":       "Richiesta",
		"Route":         "Rotta",
		"Schema":        "Schema",
		"Server":        "Server",

		// Language descriptions
		"Language.UNKNOWN": "Sconosciuto",
		"Language.PT_BR":   "Portoghese",
		"Language.ENG":     "Inglese",
		"Language.ES":      "Spagnolo",
		"Language.FR":      "Francese",
		"Language.DE":      "Tedesco",
		"Language.IT":      "Italiano",
		"Language.NL":      "Olandese",
	}
}
