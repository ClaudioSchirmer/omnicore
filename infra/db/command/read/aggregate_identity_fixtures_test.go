package read

import "github.com/ClaudioSchirmer/omnicore/domain"

// T3 (aggregate child equality) migration for this package's test AVO fixtures.
// AggregateValueObject now requires IsSameBusinessIdentity; these fixtures have
// no natural business key, so they delegate to IsSameByBusinessFields — the
// faithful structural equivalent of the change tracker's prior DeepEqual guess.

func (c addrLoad) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c covChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c csLoadChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c fakeVO) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c guardChildVO) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c *ptrChildVO) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (c noColsChild) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(c, o)
}

func (addrLoad) CollectionName() string     { return "AddrLoads" }
func (covChild) CollectionName() string     { return "CovChilds" }
func (csLoadChild) CollectionName() string  { return "CsLoadChilds" }
func (fakeVO) CollectionName() string       { return "FakeVOs" }
func (guardChildVO) CollectionName() string { return "GuardChildVOs" }
func (ptrChildVO) CollectionName() string   { return "PtrChildVOs" }
func (noColsChild) CollectionName() string  { return "NoColsChilds" }
