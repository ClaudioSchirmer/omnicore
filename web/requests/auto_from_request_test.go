package requests

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// The Request→Command contract, from both ends: what an annotated Request
// buys, and what an unannotated one keeps.

type addrReq struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

type addrCmd struct {
	Street string
	City   string
}

type okReq struct {
	Auto
	Name      string    `json:"name"`
	Age       int32     `json:"age"`
	Phone     *string   `json:"phone,omitempty"`
	Addresses []addrReq `json:"addresses,omitempty"`
	Ignored   string    `json:"-"` // never travels; not expected on the Command
}

type okCmd struct {
	base             // a Command may carry state the wire never supplies
	Name      string // (a path id, an identity overlay, a handler default)
	Age       int64  // widened
	Phone     *string
	Addresses []addrCmd
}

type base struct{ id string }

func TestAutoFromRequest_BuildsTheCommand(t *testing.T) {
	phone := "555"
	got := AutoFromRequest[*okCmd](okReq{
		Name: "Alice", Age: 41, Phone: &phone,
		Addresses: []addrReq{{Street: "Loop", City: "Cupertino"}},
		Ignored:   "must not travel",
	})
	if got == nil {
		t.Fatal("expected an allocated Command")
	}
	if got.Name != "Alice" || got.Age != 41 || got.Phone == nil || *got.Phone != "555" {
		t.Fatalf("scalars did not travel: %+v", got)
	}
	if len(got.Addresses) != 1 || got.Addresses[0].City != "Cupertino" {
		t.Fatalf("nested slice did not travel: %+v", got.Addresses)
	}
}

// The one-way rule: the Command may carry more than the wire supplies.
func TestAutoRequestReason_CommandExtrasAreFine(t *testing.T) {
	if r := AutoRequestReason(reflect.TypeOf(okReq{}), reflect.TypeOf(okCmd{})); r != "" {
		t.Fatalf("a Command with extra state must still accept the Request, got %q", r)
	}
}

// A wire field with nowhere to land is the failure this guard exists for.
func TestAutoRequestReason_OrphanWireFieldIsRefused(t *testing.T) {
	type orphanReq struct {
		Auto
		Name    string `json:"name"`
		Apelido string `json:"apelido"` // no Command field
	}
	r := AutoRequestReason(reflect.TypeOf(orphanReq{}), reflect.TypeOf(okCmd{}))
	if !strings.Contains(r, "Apelido") || !strings.Contains(r, "nowhere to land") {
		t.Fatalf("reason must name the orphan wire field, got %q", r)
	}
}

// The reshape case the example really has: a flat wire address folded into a
// nested Command value. Refused for auto — which is exactly why ToCommand
// stays hand-written there.
func TestAutoRequestReason_ReshapeIsRefused(t *testing.T) {
	type flatReq struct {
		Auto
		Street string `json:"street"`
		City   string `json:"city"`
	}
	type nestedCmd struct {
		Address addrCmd
	}
	if r := AutoRequestReason(reflect.TypeOf(flatReq{}), reflect.TypeOf(nestedCmd{})); r == "" {
		t.Fatal("a flat wire shape folding into a nested Command must not auto-map")
	}
}

// Names align but the value cannot travel.
func TestAutoRequestReason_UnconvertibleIsRefused(t *testing.T) {
	type whenReq struct {
		Auto
		When string `json:"when"`
	}
	type whenCmd struct{ When time.Time }
	r := AutoRequestReason(reflect.TypeOf(whenReq{}), reflect.TypeOf(whenCmd{}))
	if !strings.Contains(r, "When") || !strings.Contains(r, "codec") {
		t.Fatalf("reason must name the field and the cause, got %q", r)
	}
}

func TestAutoFromRequest_RefusesInsteadOfDegrading(t *testing.T) {
	type whenReq struct {
		Auto
		When string `json:"when"`
	}
	type whenCmd struct{ When time.Time }
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an unbuildable pair must be refused, not silently zeroed")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "auto-request") {
			t.Fatalf("diagnostic must identify the seat, got: %v", r)
		}
	}()
	_ = AutoFromRequest[*whenCmd](whenReq{When: "2024-12-31"})
}

func TestAutoRequestReason_NonStructShapes(t *testing.T) {
	if r := AutoRequestReason(reflect.TypeOf("x"), reflect.TypeOf(okCmd{})); !strings.Contains(r, "not a struct") {
		t.Fatalf("a non-struct Request must be reported, got %q", r)
	}
	if r := AutoRequestReason(reflect.TypeOf(okReq{}), reflect.TypeOf("x")); !strings.Contains(r, "not a struct") {
		t.Fatalf("a non-struct Command must be reported, got %q", r)
	}
}

// A value Command (not a pointer) is built just the same.
func TestAutoFromRequest_ValueCommand(t *testing.T) {
	type vCmd struct{ Name string }
	type vReq struct {
		Auto
		Name string `json:"name"`
	}
	got := AutoFromRequest[vCmd](vReq{Name: "Bob"})
	if got.Name != "Bob" {
		t.Fatalf("value Command must be built, got %+v", got)
	}
}
