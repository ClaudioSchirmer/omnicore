package translation

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type corePTBR struct{}

func CorePTBR() Module { return corePTBR{} }

func (corePTBR) Language() configuration.Language { return configuration.LangPTBR }

func (corePTBR) Translations() map[string]string {
	return map[string]string{
		// Domain validation
		"RequiredFieldNotification":   "Campo obrigatório.",
		"SchemaViolationNotification": "Conteúdo do corpo da requisição não corresponde ao esquema esperado.",
		"LimitExceededNotification":   "O limite solicitado excede o máximo permitido.",

		// Domain entity
		"UnableToInsertWithIDNotification":    "Impossível efetuar a inclusão de um registro com a chave primária informada.",
		"UnableToUpdateWithoutIDNotification": "Impossível efetuar a atualização do registro sem a chave primária.",
		"UnableToDeleteWithoutIDNotification": "Impossível efetuar a exclusão do registro sem a chave primária.",
		"InsertNotAllowedNotification":        "Inclusão não permitida.",
		"UpdateNotAllowedNotification":        "Atualização não permitida.",
		"DeleteNotAllowedNotification":        "Exclusão não permitida.",
		"ArchiveNotAllowedNotification":       "Arquivamento não permitido.",
		"UnarchiveNotAllowedNotification":     "Desarquivamento não permitido.",
		"ServiceIsRequiredNotification":       "Serviço é obrigatório.",

		// Aggregate root
		"EntityAlreadyAddedNotification":    "Entidade já foi adicionada.",
		"EntityDoesNotExistNotification":    "Entidade não existe.",
		"EntityIsNotActiveNotification":     "Entidade não está ativa.",
		"InvalidAggregateChildNotification": "Este tipo de objeto não pertence a este agregado.",

		// Repository
		"RepositoryFunctionNotImplementedNotification": "A função do repositório não foi implementada.",

		// Value objects
		"InvalidIDUUIDNotification": "Chave primária do registro é inválida.",

		// Events
		"InvalidEventTypeNotification": "Tipo de evento é inválido.",

		// Generic
		"RecordNotFoundNotification": "Registro não encontrado.",

		// EntityMode descriptions
		"EntityMode.UNKNOWN":   "Desconhecido",
		"EntityMode.DISPLAY":   "Consulta",
		"EntityMode.INSERT":    "Inserir",
		"EntityMode.UPDATE":    "Atualizar",
		"EntityMode.DELETE":    "Excluir",
		"EntityMode.ARCHIVE":   "Arquivar",
		"EntityMode.UNARCHIVE": "Desarquivar",

		// AggregateItemStatus descriptions
		"AggregateItemStatus.UNKNOWN":     "Desconhecido",
		"AggregateItemStatus.CONSTRUCTOR": "Construtor",
		"AggregateItemStatus.ADDED":       "Adicionado",
		"AggregateItemStatus.CHANGED":     "Alterado",
		"AggregateItemStatus.REMOVED":     "Removido",

		// Generic labels
		"ID": "Chave primária",

		// Application
		"InvalidLanguageDomainNotification": "Idioma não é válido.",
		"ContextNotInitializedNotification": "Contexto não foi inicializado.",
		"ServiceUnavailableNotification":    "Serviço indisponível.",
		"RequestTimeoutNotification":        "A requisição excedeu o tempo limite.",

		// Auth
		"MissingAuthorizationNotification": "Cabeçalho de autorização ausente ou em formato inválido.",
		"InvalidTokenNotification":         "Token de autenticação inválido.",
		"ExpiredTokenNotification":         "Token de autenticação expirado.",

		// Authorization
		"MissingPermissionNotification":    "Permissão necessária ausente.",
		"TenantMissingNotification":        "Identificador do tenant ausente no principal autenticado.",
		"TenantMismatchNotification":       "Recurso pertence a outro tenant.",
		"FieldAccessForbiddenNotification": "Campo não acessível.",

		// Server / routing
		"InternalServerErrorNotification": "Erro interno do servidor.",
		"RouteNotFoundNotification":       "Rota não encontrada.",
		"MethodNotAllowedNotification":    "Método HTTP não permitido para esta rota.",
		"PayloadTooLargeNotification":     "Corpo da requisição excede o tamanho permitido.",

		// Language descriptions
		"Language.UNKNOWN": "Desconhecido",
		"Language.PT_BR":   "Português",
		"Language.ENG":     "Inglês",
		"Language.ES":      "Espanhol",
		"Language.FR":      "Francês",
		"Language.DE":      "Alemão",
		"Language.IT":      "Italiano",
		"Language.NL":      "Holandês",
	}
}
