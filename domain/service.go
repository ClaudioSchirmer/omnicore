package domain

type Service interface {
	isService()
}

type ServiceBase struct{}

func (ServiceBase) isService() {}
