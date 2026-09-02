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
		"RecordNotFoundNotification":         "Record niet gevonden.",
		"ConcurrentModificationNotification": "Het record is door iemand anders gewijzigd. Laad het opnieuw en probeer het nog eens.",

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
		"UnsupportedCapabilityNotification": "Deze mogelijkheid wordt niet ondersteund door de opslag waaruit deze view wordt gelezen.",

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
		"MalformedIDNotification":         "De record-id is geen geldige UUID.",
		"UnknownIDAddressNotification":    "Er bestaat geen record op dit adres.",
		"InvalidFilterValueNotification":  "De filterwaarde is ongeldig voor dit veld.",
		"MethodNotAllowedNotification":    "HTTP-methode niet toegestaan voor deze route.",
		"PayloadTooLargeNotification":     "Verzoekbody overschrijdt de toegestane omvang.",
		// Transport semantics the framework maps but does not emit itself
		"TooManyRequestsNotification":             "Te veel verzoeken. Probeer het later opnieuw.",
		"ResourceGoneNotification":                "Deze bron is niet langer beschikbaar.",
		"PreconditionFailedNotification":          "Aan een in het verzoek gestelde voorwaarde is niet voldaan.",
		"UnsupportedMediaTypeNotification":        "Inhoudstype wordt niet ondersteund door dit endpoint.",
		"NotImplementedNotification":              "Deze functionaliteit is niet geïmplementeerd.",
		"BadGatewayNotification":                  "Een upstream-service gaf een ongeldig antwoord.",
		"PaymentRequiredNotification":             "Betaling is vereist voordat dit verzoek kan worden afgehandeld.",
		"NotAcceptableNotification":               "Geen beschikbare weergave komt overeen met het gevraagde inhoudstype.",
		"RangeNotSatisfiableNotification":         "Het gevraagde bereik is niet beschikbaar voor deze bron.",
		"ResourceLockedNotification":              "Deze bron is tijdelijk vergrendeld. Probeer het zo meteen opnieuw.",
		"PreconditionRequiredNotification":        "Dit verzoek moet een voorwaarde-header bevatten.",
		"UnavailableForLegalReasonsNotification":  "Deze bron is niet beschikbaar om juridische redenen.",
		"InsufficientStorageNotification":         "Het opslagquotum is uitgeput.",
		"RequestHeaderFieldsTooLargeNotification": "De verzoekheaders overschrijden de toegestane omvang.",
		"MalformedRequestNotification":            "Het verzoek kon niet worden gelezen.",

		// Notification context labels — the framework builds its own
		// NotificationContext values with these names (web.respondRouteNotFound,
		// respondMethodNotAllowed, respondPayloadTooLarge, the ErrorHandler's
		// "Server", the schema guards' "Schema", the auth middleware's
		// "Authorization", pipeline.contextNotInitialized's "Pipeline"), and
		// notifications.ToContextDTOs renders the context NAME through the
		// catalog. Without these entries every such response logged
		// translation.key.missing on the first hit and shipped the raw English
		// name on the wire in all seven languages.
		"Authorization": "Autorisatie",
		"Pipeline":      "Pipeline",
		"Request":       "Verzoek",
		"Route":         "Route",
		"Schema":        "Schema",
		"Server":        "Server",

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
