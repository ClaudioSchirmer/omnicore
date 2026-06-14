package pipeline

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

type Handler[TReq Request, TRes any] interface {
	Handle(ctx *configuration.AppContext, req TReq) (TRes, error)
}

type CommandHandler[TReq Command, TRes any] interface {
	Handler[TReq, TRes]
}

type QueryHandler[TReq Query, TRes any] interface {
	Handler[TReq, TRes]
}
