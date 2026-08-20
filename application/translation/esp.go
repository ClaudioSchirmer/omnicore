package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type coreES struct{}

func CoreES() Module { return coreES{} }

func (coreES) Language() configuration.Language { return configuration.LangES }

func (coreES) Translations() map[string]string {
	return map[string]string{
		// Domain validation
		"RequiredFieldNotification":   "Campo obligatorio.",
		"SchemaViolationNotification": "El contenido del cuerpo de la solicitud no coincide con el esquema esperado.",
		"LimitExceededNotification":   "El límite solicitado excede el máximo permitido.",

		// Domain entity
		"UnableToInsertWithIDNotification":    "No es posible insertar un registro con una clave primaria existente.",
		"UnableToUpdateWithoutIDNotification": "No es posible actualizar un registro sin clave primaria.",
		"UnableToDeleteWithoutIDNotification": "No es posible eliminar un registro sin clave primaria.",
		"InsertNotAllowedNotification":        "Inserción no permitida.",
		"UpdateNotAllowedNotification":        "Actualización no permitida.",
		"DeleteNotAllowedNotification":        "Eliminación no permitida.",
		"ArchiveNotAllowedNotification":       "Archivado no permitido.",
		"UnarchiveNotAllowedNotification":     "Desarchivado no permitido.",
		"ServiceIsRequiredNotification":       "El servicio es obligatorio.",

		// Aggregate root
		"EntityAlreadyAddedNotification":    "La entidad ya fue agregada.",
		"NaturalIDImmutableNotification":    "La clave natural es inmutable y no puede modificarse.",
		"EntityDoesNotExistNotification":    "La entidad no existe.",
		"EntityIsNotActiveNotification":     "La entidad no está activa.",
		"InvalidAggregateChildNotification": "Este tipo de objeto no pertenece a este agregado.",

		// Repository
		"RepositoryFunctionNotImplementedNotification": "Función del repositorio no implementada.",

		// Value objects
		"InvalidIDUUIDNotification": "Clave primaria inválida.",

		// Events
		"InvalidEventTypeNotification": "Tipo de evento inválido.",

		// Generic
		"RecordNotFoundNotification":         "Registro no encontrado.",
		"ConcurrentModificationNotification": "El registro fue modificado por otra persona. Recárguelo e inténtelo de nuevo.",

		// EntityMode descriptions
		"EntityMode.UNKNOWN":   "Desconocido",
		"EntityMode.DISPLAY":   "Consulta",
		"EntityMode.INSERT":    "Insertar",
		"EntityMode.UPDATE":    "Actualizar",
		"EntityMode.DELETE":    "Eliminar",
		"EntityMode.ARCHIVE":   "Archivar",
		"EntityMode.UNARCHIVE": "Desarchivar",

		// AggregateItemStatus descriptions
		"AggregateItemStatus.UNKNOWN":     "Desconocido",
		"AggregateItemStatus.CONSTRUCTOR": "Constructor",
		"AggregateItemStatus.ADDED":       "Agregado",
		"AggregateItemStatus.CHANGED":     "Modificado",
		"AggregateItemStatus.REMOVED":     "Eliminado",

		// Generic labels
		"ID": "Clave primaria",

		// Application
		"InvalidLanguageDomainNotification": "Idioma inválido.",
		"ContextNotInitializedNotification": "El contexto no fue inicializado.",
		"ServiceUnavailableNotification":    "Servicio no disponible.",
		"RequestTimeoutNotification":        "La solicitud superó el tiempo límite.",
		"ReadTimeoutNotification":           "La solicitud no se recibió dentro del tiempo límite.",

		// Auth
		"MissingAuthorizationNotification": "Cabecera de autorización ausente o con formato inválido.",
		"InvalidTokenNotification":         "Token de autenticación inválido.",
		"ExpiredTokenNotification":         "El token de autenticación ha expirado.",

		// Authorization
		"MissingPermissionNotification":    "Permiso requerido ausente.",
		"TenantMissingNotification":        "Identificador del tenant ausente en el principal autenticado.",
		"TenantMismatchNotification":       "El recurso pertenece a otro tenant.",
		"FieldAccessForbiddenNotification": "Campo no accesible.",
		"RelationalCapabilityNotification": "Esta capacidad no es compatible con una vista leída del backend relacional; quite RelationalSource() para servirla desde Mongo.",

		// Server / routing
		"InternalServerErrorNotification": "Error interno del servidor.",
		"RouteNotFoundNotification":       "Ruta no encontrada.",
		"MethodNotAllowedNotification":    "Método HTTP no permitido para esta ruta.",
		"PayloadTooLargeNotification":     "El cuerpo de la solicitud supera el tamaño permitido.",

		// Notification context labels — the framework builds its own
		// NotificationContext values with these names (web.respondRouteNotFound,
		// respondMethodNotAllowed, respondPayloadTooLarge, the ErrorHandler's
		// "Server", the schema guards' "Schema", the auth middleware's
		// "Authorization", pipeline.contextNotInitialized's "Pipeline"), and
		// notifications.ToContextDTOs renders the context NAME through the
		// catalog. Without these entries every such response logged
		// translation.key.missing on the first hit and shipped the raw English
		// name on the wire in all seven languages.
		"Authorization": "Autorización",
		"Pipeline":      "Pipeline",
		"Request":       "Solicitud",
		"Route":         "Ruta",
		"Schema":        "Esquema",
		"Server":        "Servidor",

		// Language descriptions
		"Language.UNKNOWN": "Desconocido",
		"Language.PT_BR":   "Portugués",
		"Language.ENG":     "Inglés",
		"Language.ES":      "Español",
		"Language.FR":      "Francés",
		"Language.DE":      "Alemán",
		"Language.IT":      "Italiano",
		"Language.NL":      "Neerlandés",
	}
}
