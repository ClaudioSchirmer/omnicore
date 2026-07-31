package domain

// T3 (aggregate child equality) migration for this package's test AVO fixtures.
//
// AggregateValueObject now requires IsSameBusinessIdentity, which replaces the
// change tracker's old reflect.DeepEqual structural guess. These fixtures carry
// no natural business key, so they delegate to IsSameByBusinessFields — the faithful,
// exported-field structural equivalent of the prior behavior — keeping the
// existing add/change/remove/dedup test expectations intact.

func (t testAVO) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(t, other)
}

func (o otherAVO) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(o, other)
}

func (e emittingAVO) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(e, other)
}

func (a aggChild) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(a, other)
}

func (r Rec) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(r, other)
}

func (u Tag) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(u, other)
}

func (a actionAwareChild) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(a, other)
}
