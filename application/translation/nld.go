package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type coreNL struct{}

func CoreNL() Module { return coreNL{} }

func (coreNL) Language() configuration.Language { return configuration.LangNL }

func (coreNL) Translations() map[string]string {
	return map[string]string{
		// Domain validation
		"RequiredFieldNotification":   "Verplicht veld.",
		"SchemaViolationNotification": "De inhoud van de verzoekbody komt niet overeen met het verwachte schema.",
		"LimitExceededNotification":   "De gevraagde limiet overschrijdt het toegestane maximum.",

		// Domain entity
		"UnableToInsertWithIDNotification":    "Kan een record met een bestaande primaire sleutel niet invoegen.",
		"UnableToUpdateWithoutIDNotification": "Kan een record zonder primaire sleutel niet bijwerken.",
		"UnableToDeleteWithoutIDNotification": "Kan een record zonder primaire sleutel niet verwijderen.",
		"InsertNotAllowedNotification":        "Invoegen niet toegestaan.",
		"UpdateNotAllowedNotification":        "Bijwerken niet toegestaan.",
		"DeleteNotAllowedNotification":        "Verwijderen niet toegestaan.",
		"ArchiveNotAllowedNotification":       "Archiveren niet toegestaan.",
		"UnarchiveNotAllowedNotification":     "Herstellen niet toegestaan.",
		"ServiceIsRequiredNotification":       "Service is verplicht.",

		// Aggregate root
		"EntityAlreadyAddedNotification":    "Entiteit is al toegevoegd.",
		"NaturalIDImmutableNotification":    "De natuurlijke sleutel is onveranderlijk en kan niet worden gewijzigd.",
		"EntityDoesNotExistNotification":    "Entiteit bestaat niet.",
		"EntityIsNotActiveNotification":     "Entiteit is niet actief.",
		"InvalidAggregateChildNotification": "Dit objecttype behoort niet tot dit aggregaat.",

		// Repository
		"RepositoryFunctionNotImplementedNotification": "Repository-functie is niet geïmplementeerd.",

		// Value objects
		"InvalidIDUUIDNotification": "Ongeldige primaire sleutel.",

		// Events
		"InvalidEventTypeNotification": "Ongeldig gebeurtenistype.",

		// Generic
		"RecordNotFoundNotification": "Record niet gevonden.",

		// EntityMode descriptions
		"EntityMode.UNKNOWN":   "Onbekend",
		"EntityMode.DISPLAY":   "Weergave",
		"EntityMode.INSERT":    "Invoegen",
		"EntityMode.UPDATE":    "Bijwerken",
		"EntityMode.DELETE":    "Verwijderen",
		"EntityMode.ARCHIVE":   "Archiveren",
		"EntityMode.UNARCHIVE": "Herstellen",

		// AggregateItemStatus descriptions
		"AggregateItemStatus.UNKNOWN":     "Onbekend",
		"AggregateItemStatus.CONSTRUCTOR": "Constructor",
		"AggregateItemStatus.ADDED":       "Toegevoegd",
		"AggregateItemStatus.CHANGED":     "Gewijzigd",
		"AggregateItemStatus.REMOVED":     "Verwijderd",

		// Generic labels
		"ID": "Primaire sleutel",

		// Application
		"InvalidLanguageDomainNotification": "Ongeldige taal.",
		"ContextNotInitializedNotification": "Context is niet geïnitialiseerd.",
		"ServiceUnavailableNotification":    "Service niet beschikbaar.",
		"RequestTimeoutNotification":        "De aanvraag heeft de tijdslimiet overschreden.",
		"ReadTimeoutNotification":           "De aanvraag is niet binnen de tijdslimiet ontvangen.",

		// Auth
		"MissingAuthorizationNotification": "Authorization-header ontbreekt of is misvormd.",
		"InvalidTokenNotification":         "Ongeldig authenticatietoken.",
		"ExpiredTokenNotification":         "Authenticatietoken is verlopen.",

		// Authorization
		"MissingPermissionNotification":    "Vereiste machtiging ontbreekt.",
		"TenantMissingNotification":        "Tenant-identificatie ontbreekt in de geauthenticeerde principal.",
		"TenantMismatchNotification":       "De resource behoort tot een andere tenant.",
		"FieldAccessForbiddenNotification": "Veld niet toegankelijk.",

		// Server / routing
		"InternalServerErrorNotification": "Interne serverfout.",
		"RouteNotFoundNotification":       "Route niet gevonden.",
		"MethodNotAllowedNotification":    "HTTP-methode niet toegestaan voor deze route.",
		"PayloadTooLargeNotification":     "Verzoekbody overschrijdt de toegestane omvang.",

		// Language descriptions
		"Language.UNKNOWN": "Onbekend",
		"Language.PT_BR":   "Portugees",
		"Language.ENG":     "Engels",
		"Language.ES":      "Spaans",
		"Language.FR":      "Frans",
		"Language.DE":      "Duits",
		"Language.IT":      "Italiaans",
		"Language.NL":      "Nederlands",
	}
}
