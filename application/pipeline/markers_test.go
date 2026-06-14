package pipeline

import "testing"

// Marker methods on the seal types are intentionally private — they cover the
// "implements interface" check at compile time. From inside this package we
// can call them directly to register coverage of the empty bodies and to lock
// the seal contract: every base IS its own interface.

func TestSealInterfaces_BaseImplementsItsContract(t *testing.T) {
	t.Run("RequestBase implements Request", func(t *testing.T) {
		var _ Request = RequestBase{}
		RequestBase{}.isRequest()
	})

	t.Run("CommandBase implements Command", func(t *testing.T) {
		var _ Command = CommandBase{}
		CommandBase{}.isCommand()
		// And Command is itself a Request — call the promoted marker too.
		CommandBase{}.isRequest()
	})

	t.Run("QueryBase implements Query", func(t *testing.T) {
		var _ Query = QueryBase{}
		QueryBase{}.isQuery()
		QueryBase{}.isRequest()
	})
}

func TestFullBody_PrivateMarkerIsCallable(t *testing.T) {
	// From inside the package the unexported method is reachable. Outside
	// callers can only satisfy the interface by embedding FullBody. Same role
	// as TestFullBody_SatisfiesEnforcer but counts the method body for the
	// coverage profile.
	FullBody{}.enforceFullBody()
}

func TestPathIDRequired_PrivateMarkerIsCallable(t *testing.T) {
	PathIDRequired{}.pathIDRequired()
}

func TestPathIDRequired_SatisfiesEnforcer(t *testing.T) {
	var v any = PathIDRequired{}
	if _, ok := v.(PathIDRequiredEnforcer); !ok {
		t.Fatal("PathIDRequired{} should satisfy PathIDRequiredEnforcer")
	}
}

func TestPathIDRequired_EmbeddingSatisfiesEnforcer(t *testing.T) {
	type handler struct {
		PathIDRequired
	}
	var v any = &handler{}
	if _, ok := v.(PathIDRequiredEnforcer); !ok {
		t.Fatal("struct embedding PathIDRequired should satisfy PathIDRequiredEnforcer")
	}
}

func TestPathIDRequired_AbsentDoesNotSatisfy(t *testing.T) {
	type other struct{}
	var v any = &other{}
	if _, ok := v.(PathIDRequiredEnforcer); ok {
		t.Fatal("struct without PathIDRequired must NOT satisfy PathIDRequiredEnforcer")
	}
}

func TestCommandBaseWithID_SetPathID_PathID(t *testing.T) {
	c := &CommandBaseWithID{}
	if got := c.PathID(); got != "" {
		t.Errorf("default PathID = %q, want empty", got)
	}
	c.SetPathID("abc-123")
	if got := c.PathID(); got != "abc-123" {
		t.Errorf("PathID after SetPathID = %q, want %q", got, "abc-123")
	}
	// Overwrite stays last-write-wins.
	c.SetPathID("xyz")
	if got := c.PathID(); got != "xyz" {
		t.Errorf("PathID after overwrite = %q, want %q", got, "xyz")
	}
}

func TestCommandBaseWithID_SatisfiesCommandWithID(t *testing.T) {
	type cmd struct {
		CommandBaseWithID
	}
	var v any = &cmd{}
	if _, ok := v.(CommandWithID); !ok {
		t.Fatal("struct embedding CommandBaseWithID should satisfy CommandWithID")
	}
}
