package web

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/web/responses"
)

// The opt-in contract, from the constructor's seat: what a Response buys by
// declaring fwresponses.Auto, and what it keeps by not declaring it.

type gmResult struct {
	ID      string
	Nome    string
	Salario float64   // sensitive: no Response below exposes it
	Quando  time.Time // awkward type: no auto Response below maps it
}

// Auto-mapped and aligned: the happy path.
type gmAutoResp struct {
	responses.Auto
	ID   string `json:"id"`
	Nome string `json:"nome"`
}

// Auto-mapped SUBSET: fewer fields than the Result. Must pass — this is how a
// DTO cuts fields off the wire.
type gmSubsetResp struct {
	responses.Auto
	Nome string `json:"nome"`
}

// Auto-mapped but RENAMED: no Result field named Apelido. Must boot-fail.
type gmRenamedAutoResp struct {
	responses.Auto
	Apelido string `json:"apelido"`
}

// Hand-mapped and renamed: no marker, so the framework does not police it.
type gmRenamedManualResp struct {
	Apelido string `json:"apelido"`
}

// Auto-mapped over a field it cannot receive: names align, types do not.
type gmUnconvertibleResp struct {
	responses.Auto
	Quando string `json:"quando"`
}

func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a boot panic mentioning %q", want)
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, want) {
			t.Fatalf("panic must mention %q, got: %v", want, r)
		}
	}()
	fn()
}

func TestMappingGuard_AutoAlignedPasses(t *testing.T) {
	validateResponseMapping(reflect.TypeOf(gmResult{}), reflect.TypeOf(gmAutoResp{}))
}

// The one-way rule: a Response may expose a SUBSET of the Result. Fields the
// Response omits are never examined — which is what keeps a Result free to
// carry sensitive or awkward values (Salario, Quando) no wire shows.
func TestMappingGuard_SubsetResponsePasses(t *testing.T) {
	validateResponseMapping(reflect.TypeOf(gmResult{}), reflect.TypeOf(gmSubsetResp{}))
}

func TestMappingGuard_AutoRenamedIsRefused(t *testing.T) {
	mustPanic(t, "Apelido", func() {
		validateResponseMapping(reflect.TypeOf(gmResult{}), reflect.TypeOf(gmRenamedAutoResp{}))
	})
}

// The capability the marker gives back: WITHOUT it, renaming is the
// consumer's business and the framework stays out of the way.
func TestMappingGuard_ManualRenamedIsAllowed(t *testing.T) {
	validateResponseMapping(reflect.TypeOf(gmResult{}), reflect.TypeOf(gmRenamedManualResp{}))
}

// Names align but the values cannot travel: refused at boot with the field
// named, instead of a silent per-request serialization round trip.
func TestMappingGuard_AutoUnconvertibleIsRefused(t *testing.T) {
	mustPanic(t, "Quando", func() {
		validateResponseMapping(reflect.TypeOf(gmResult{}), reflect.TypeOf(gmUnconvertibleResp{}))
	})
}

// Purity binds EVERY Result, marker or not: wire tags belong to web/.
type gmTaggedResult struct {
	ID string `json:"id"`
}

func TestMappingGuard_ResultPurityIsUnconditional(t *testing.T) {
	mustPanic(t, "wire tags", func() {
		validateResponseMapping(reflect.TypeOf(gmTaggedResult{}), reflect.TypeOf(gmRenamedManualResp{}))
	})
}

func TestMappingGuard_DeclaresAutoMap(t *testing.T) {
	if !declaresAutoMap(reflect.TypeOf(gmAutoResp{})) {
		t.Error("an embedding Response must be detected as auto-mapped")
	}
	if declaresAutoMap(reflect.TypeOf(gmRenamedManualResp{})) {
		t.Error("a plain Response must not be detected as auto-mapped")
	}
	if declaresAutoMap(nil) {
		t.Error("nil must not be detected as auto-mapped")
	}
}
