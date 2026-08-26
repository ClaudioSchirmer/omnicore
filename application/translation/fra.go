package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type coreFR struct{}

func CoreFR() Module { return coreFR{} }

func (coreFR) Language() configuration.Language { return configuration.LangFR }

func (coreFR) Translations() map[string]string {
	return map[string]string{
		// Domain validation
		"RequiredFieldNotification":   "Champ obligatoire.",
		"SchemaViolationNotification": "Le contenu du corps de la requête ne correspond pas au schéma attendu.",
		"LimitExceededNotification":   "La limite demandée dépasse le maximum autorisé.",

		// Domain entity
		"UnableToInsertWithIDNotification":    "Impossible d'insérer un enregistrement avec une clé primaire existante.",
		"UnableToUpdateWithoutIDNotification": "Impossible de mettre à jour un enregistrement sans clé primaire.",
		"UnableToDeleteWithoutIDNotification": "Impossible de supprimer un enregistrement sans clé primaire.",
		"InsertNotAllowedNotification":        "Insertion non autorisée.",
		"UpdateNotAllowedNotification":        "Mise à jour non autorisée.",
		"DeleteNotAllowedNotification":        "Suppression non autorisée.",
		"ArchiveNotAllowedNotification":       "Archivage non autorisé.",
		"UnarchiveNotAllowedNotification":     "Désarchivage non autorisé.",
		"ServiceIsRequiredNotification":       "Le service est obligatoire.",

		// Aggregate root
		"EntityAlreadyAddedNotification":    "L'entité a déjà été ajoutée.",
		"NaturalIDImmutableNotification":    "La clé naturelle est immuable et ne peut pas être modifiée.",
		"EntityDoesNotExistNotification":    "L'entité n'existe pas.",
		"EntityIsNotActiveNotification":     "L'entité n'est pas active.",
		"InvalidAggregateChildNotification": "Ce type d'objet n'appartient pas à cet agrégat.",

		// Repository
		"RepositoryFunctionNotImplementedNotification": "Fonction du référentiel non implémentée.",

		// Value objects
		"InvalidIDUUIDNotification": "Clé primaire invalide.",

		// Events
		"InvalidEventTypeNotification": "Type d'événement invalide.",

		// Generic
		"RecordNotFoundNotification":         "Enregistrement introuvable.",
		"ConcurrentModificationNotification": "L'enregistrement a été modifié par quelqu'un d'autre. Rechargez-le et réessayez.",

		// EntityMode descriptions
		"EntityMode.UNKNOWN":   "Inconnu",
		"EntityMode.DISPLAY":   "Consultation",
		"EntityMode.INSERT":    "Insérer",
		"EntityMode.UPDATE":    "Mettre à jour",
		"EntityMode.DELETE":    "Supprimer",
		"EntityMode.ARCHIVE":   "Archiver",
		"EntityMode.UNARCHIVE": "Désarchiver",

		// AggregateItemStatus descriptions
		"AggregateItemStatus.UNKNOWN":     "Inconnu",
		"AggregateItemStatus.CONSTRUCTOR": "Constructeur",
		"AggregateItemStatus.ADDED":       "Ajouté",
		"AggregateItemStatus.CHANGED":     "Modifié",
		"AggregateItemStatus.REMOVED":     "Supprimé",

		// Generic labels
		"ID": "Clé primaire",

		// Application
		"InvalidLanguageDomainNotification": "Langue invalide.",
		"ContextNotInitializedNotification": "Le contexte n'a pas été initialisé.",
		"ServiceUnavailableNotification":    "Service indisponible.",
		"RequestTimeoutNotification":        "La requête a dépassé le délai imparti.",
		"ReadTimeoutNotification":           "La requête n'a pas été reçue dans le délai imparti.",
		"UnsupportedCapabilityNotification": "Cette capacité n'est pas prise en charge par le magasin depuis lequel cette vue est lue.",

		// Auth
		"MissingAuthorizationNotification": "En-tête d'autorisation absent ou mal formé.",
		"InvalidTokenNotification":         "Jeton d'authentification invalide.",
		"ExpiredTokenNotification":         "Le jeton d'authentification a expiré.",

		// Authorization
		"MissingPermissionNotification":    "Permission requise manquante.",
		"TenantMissingNotification":        "Identifiant de tenant absent du principal authentifié.",
		"TenantMismatchNotification":       "La ressource appartient à un autre tenant.",
		"FieldAccessForbiddenNotification": "Champ non accessible.",

		// Server / routing
		"InternalServerErrorNotification": "Erreur interne du serveur.",
		"RouteNotFoundNotification":       "Route introuvable.",
		"MethodNotAllowedNotification":    "Méthode HTTP non autorisée pour cette route.",
		"PayloadTooLargeNotification":     "La charge utile de la requête dépasse la taille autorisée.",
		// Transport semantics the framework maps but does not emit itself
		"TooManyRequestsNotification":      "Trop de requêtes. Veuillez réessayer plus tard.",
		"ResourceGoneNotification":         "Cette ressource n'est plus disponible.",
		"PreconditionFailedNotification":   "Une condition préalable indiquée dans la requête n'est pas remplie.",
		"UnsupportedMediaTypeNotification": "Type de contenu non pris en charge par ce point d'accès.",
		"NotImplementedNotification":       "Cette fonctionnalité n'est pas implémentée.",
		"BadGatewayNotification":           "Un service en amont a renvoyé une réponse invalide.",

		// Notification context labels — the framework builds its own
		// NotificationContext values with these names (web.respondRouteNotFound,
		// respondMethodNotAllowed, respondPayloadTooLarge, the ErrorHandler's
		// "Server", the schema guards' "Schema", the auth middleware's
		// "Authorization", pipeline.contextNotInitialized's "Pipeline"), and
		// notifications.ToContextDTOs renders the context NAME through the
		// catalog. Without these entries every such response logged
		// translation.key.missing on the first hit and shipped the raw English
		// name on the wire in all seven languages.
		"Authorization": "Autorisation",
		"Pipeline":      "Pipeline",
		"Request":       "Requête",
		"Route":         "Route",
		"Schema":        "Schéma",
		"Server":        "Serveur",

		// Language descriptions
		"Language.UNKNOWN": "Inconnu",
		"Language.PT_BR":   "Portugais",
		"Language.ENG":     "Anglais",
		"Language.ES":      "Espagnol",
		"Language.FR":      "Français",
		"Language.DE":      "Allemand",
		"Language.IT":      "Italien",
		"Language.NL":      "Néerlandais",
	}
}
