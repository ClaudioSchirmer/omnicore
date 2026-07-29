package pipeline

import "testing"

// The embeddable markers are contracts BY TYPE: embedding the token is what
// makes a handler satisfy the wrapper's type assertion. These pin that the
// promotion actually happens — a rename of an unexported marker method would
// silently detach every wrapper behavior that keys on it.

func TestFullBodyMarker_PromotesThroughEmbedding(t *testing.T) {
	type strictHandler struct{ FullBody }
	var h any = strictHandler{}
	e, ok := h.(FullBodyEnforcer)
	if !ok {
		t.Fatal("embedding FullBody must satisfy FullBodyEnforcer via promotion")
	}
	e.enforceFullBody() // the marker itself — a no-op, but the wrapper calls through the interface
	if _, ok := any(struct{}{}).(FullBodyEnforcer); ok {
		t.Fatal("a type without the embed must NOT satisfy the marker")
	}
}

func TestPathIDRequiredMarker_PromotesThroughEmbedding(t *testing.T) {
	type idHandler struct{ PathIDRequired }
	var h any = idHandler{}
	e, ok := h.(PathIDRequiredEnforcer)
	if !ok {
		t.Fatal("embedding PathIDRequired must satisfy PathIDRequiredEnforcer via promotion")
	}
	e.pathIDRequired()
}

// The Request/Command/Query seals: CommandBase and QueryBase must each carry
// BOTH their own marker and the Request marker (via the embedded RequestBase),
// and the two families must not satisfy each other.
func TestRequestSeals(t *testing.T) {
	type myCommand struct{ CommandBase }
	type myQuery struct{ QueryBase }

	var c any = myCommand{}
	if _, ok := c.(Command); !ok {
		t.Fatal("CommandBase must satisfy Command")
	}
	if _, ok := c.(Request); !ok {
		t.Fatal("a Command IS a Request")
	}
	if _, ok := c.(Query); ok {
		t.Fatal("a Command must not satisfy Query")
	}

	var q any = myQuery{}
	if _, ok := q.(Query); !ok {
		t.Fatal("QueryBase must satisfy Query")
	}
	if _, ok := q.(Command); ok {
		t.Fatal("a Query must not satisfy Command")
	}
	// Exercise the seal methods directly so the contract is executed, not only
	// type-asserted.
	myCommand{}.isCommand()
	myCommand{}.isRequest()
	myQuery{}.isQuery()
}
