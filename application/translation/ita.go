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
		"NaturalKeyImmutableNotification":   "La chiave naturale è immutabile e non può essere modificata.",
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
		"RecordNotFoundNotification": "Record non trovato.",

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
