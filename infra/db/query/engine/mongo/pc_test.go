package mongo

import "github.com/ClaudioSchirmer/omnicore/infra/db/query"

// pc wraps a raw collection name in a query.PhysicalCollection for tests in this
// package. Production names flow through the shared ViewResolver; a nil-backed
// resolver resolves to identity (the bare name), which is all these unit tests
// need — they drive the store against a fake collection keyed by that name.
var testResolver = query.NewViewResolver(nil)

func pc(name string) query.PhysicalCollection { return testResolver.Active(name) }
