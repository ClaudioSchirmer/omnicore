package queryschema

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ── fixtures: a Response whose `display` is derived from two Result fields
// that the Response itself does NOT expose, plus a nested segment carrying the
// same shape one level down ──────────────────────────────────────────────────

type computedAddressResult struct {
	City    *string
	State   *string
	Locale  *string
	Ignored *string
}

type computedUserResult struct {
	ID        *string
	Name      *string
	UserName  *string
	Display   *string
	Addresses []computedAddressResult
}

type computedAddressResponse struct {
	// Locale is derived from City+State; neither is declared here, proving a
	// source may live only on the Result.
	Locale *string `json:"locale,omitempty" computed:"City,State"`
}

type computedUserResponse struct {
	ID   *string `json:"id,omitempty"`
	Name *string `json:"name,omitempty"`
	// Display is derived from Name+UserName. Name IS declared above (so it also
	// reaches the wire); UserName is not (read-only source).
	Display   *string                   `json:"display,omitempty" computed:"Name,UserName"`
	Addresses []computedAddressResponse `json:"addresses,omitempty"`
}

func schemaFor(t *testing.T, v any) *ProjectionSchema {
	t.Helper()
	return ExtractProjectionSchema(reflect.TypeOf(v))
}

// TestExtractProjectionSchema_RecordsComputedSources — the tag is reflected
// into Computed, keyed by wire path, valued by the Result Go paths, and a
// nested computed field carries its segment prefix.
func TestExtractProjectionSchema_RecordsComputedSources(t *testing.T) {
	s := schemaFor(t, computedUserResponse{})

	got, ok := s.Computed["display"]
	if !ok {
		t.Fatalf("display must be recorded as computed; Computed=%v", s.Computed)
	}
	if len(got) != 2 || got[0] != "Name" || got[1] != "UserName" {
		t.Errorf("display sources = %v, want [Name UserName]", got)
	}
	nested, ok := s.Computed["addresses.locale"]
	if !ok {
		t.Fatalf("a nested computed field must be recorded; Computed=%v", s.Computed)
	}
	if len(nested) != 2 || nested[0] != "Addresses.City" || nested[1] != "Addresses.State" {
		t.Errorf("addresses.locale sources = %v, want [Addresses.City Addresses.State]", nested)
	}
	// A computed field stays a legal wire path — it is selectable, it just has
	// no column behind it.
	if s.Paths["display"] != "Display" {
		t.Errorf("a computed field must stay in the wire vocabulary; Paths[display]=%q", s.Paths["display"])
	}
	// A plain field is never recorded as computed.
	if _, isComputed := s.Computed["name"]; isComputed {
		t.Error("an untagged field must not be recorded as computed")
	}
}

// TestParseProjection_ComputedPushesSourcesNotItself — the whole point of the
// tag: `?fields=display` reads Name+UserName so FromQueryResult has data, and
// never asks the store for a column that does not exist.
func TestParseProjection_ComputedPushesSourcesNotItself(t *testing.T) {
	s := schemaFor(t, computedUserResponse{})

	proj, wireSet, bad, ok := ParseProjection([]string{"display"}, s)
	if !ok {
		t.Fatalf("a computed token must be accepted, got bad=%q", bad)
	}
	if !proj.Selects("Name") || !proj.Selects("UserName") {
		t.Errorf("the sources must be pushed down; proj=%v", proj)
	}
	if proj.Selects("Display") {
		t.Errorf("the computed path itself must NOT be pushed down (no column behind it); proj=%v", proj)
	}
	// The wire set still records the token, so the `id` auto-exclusion and the
	// export pruning see what the consumer actually asked for.
	if !wireSet["display"] {
		t.Errorf("wireSet must record the computed token; got %v", wireSet)
	}
}

// TestParseProjection_ComputedCombinesWithPlainTokens — mixing a computed
// token with a stored one yields the union, deduped.
func TestParseProjection_ComputedCombinesWithPlainTokens(t *testing.T) {
	s := schemaFor(t, computedUserResponse{})

	proj, _, bad, ok := ParseProjection([]string{"id", "display", "name"}, s)
	if !ok {
		t.Fatalf("unexpected rejection of %q", bad)
	}
	for _, want := range []string{"ID", "Name", "UserName"} {
		if !proj.Selects(want) {
			t.Errorf("proj must include %q; got %v", want, proj)
		}
	}
	if proj.Selects("Display") {
		t.Errorf("Display must never be pushed down; got %v", proj)
	}
}

// TestParseProjection_NestedComputedPushesSegmentSources — a computed field
// inside a segment pushes its siblings under the same segment prefix.
func TestParseProjection_NestedComputedPushesSegmentSources(t *testing.T) {
	s := schemaFor(t, computedUserResponse{})

	proj, _, bad, ok := ParseProjection([]string{"addresses.locale"}, s)
	if !ok {
		t.Fatalf("unexpected rejection of %q", bad)
	}
	if !proj.Selects("Addresses.City") || !proj.Selects("Addresses.State") {
		t.Errorf("nested sources must be pushed under the segment; proj=%v", proj)
	}
	if proj.Selects("Addresses.Locale") {
		t.Errorf("the nested computed path must NOT be pushed down; proj=%v", proj)
	}
}

// TestViolation_MessageDefaultsToSchemaViolation locks the zero-Notification
// contract every generic rejection relies on.
func TestViolation_MessageDefaultsToSchemaViolation(t *testing.T) {
	msg := SchemaViolation("fields[bogus]").Message()
	if msg.ResolveFieldName() != "fields[bogus]" {
		t.Errorf("FieldName = %q", msg.ResolveFieldName())
	}
	if _, isSchema := msg.Notification.(domain.SchemaViolationNotification); !isSchema {
		t.Errorf("a nil Notification must default to SchemaViolationNotification, got %T", msg.Notification)
	}
}

// ── the boot guard ──────────────────────────────────────────────────────────

type badSourceResponse struct {
	Display *string `json:"display,omitempty" computed:"Name,Nope"`
}

type chainedSourceResponse struct {
	Display *string `json:"display,omitempty" computed:"Name,Other"`
}

type chainedSourceResult struct {
	Name    *string
	Other   *string `computed:"Name"`
	Display *string
}

func TestValidateComputedSources_UnknownSourceIsReported(t *testing.T) {
	errs := ValidateComputedSources(reflect.TypeOf(computedUserResult{}), reflect.TypeOf(badSourceResponse{}))
	if len(errs) == 0 {
		t.Fatal("a source naming no Result field must be reported")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "Nope") {
		t.Errorf("the diagnostic must name the offending source; got %s", joined)
	}
	if strings.Contains(joined, `"Name"`) {
		t.Errorf("a resolvable source must not be reported; got %s", joined)
	}
	msg := FormatComputedSourcesGuard(reflect.TypeOf(computedUserResult{}), reflect.TypeOf(badSourceResponse{}), errs)
	if !strings.Contains(msg, "[computed]") || !strings.Contains(msg, "Nope") {
		t.Errorf("the panic text must be actionable; got %s", msg)
	}
}

func TestValidateComputedSources_SourceMayBeAbsentFromTheResponse(t *testing.T) {
	// UserName and the nested City/State are sources that the Response does not
	// declare — the guard must accept them: they are read, feed the hook, and
	// never reach the wire.
	errs := ValidateComputedSources(reflect.TypeOf(computedUserResult{}), reflect.TypeOf(computedUserResponse{}))
	if len(errs) != 0 {
		t.Fatalf("sources living only on the Result must be accepted; got %v", errs)
	}
}

func TestValidateComputedSources_ChainedComputedIsRejected(t *testing.T) {
	errs := ValidateComputedSources(reflect.TypeOf(chainedSourceResult{}), reflect.TypeOf(chainedSourceResponse{}))
	if len(errs) == 0 {
		t.Fatal("a source that is itself computed must be rejected")
	}
	if joined := strings.Join(errs, "\n"); !strings.Contains(joined, "itself computed") {
		t.Errorf("the diagnostic must explain the chain rejection; got %s", joined)
	}
}

func TestValidateComputedSources_NoTagsIsClean(t *testing.T) {
	type plainResp struct {
		Name *string `json:"name,omitempty"`
	}
	if errs := ValidateComputedSources(reflect.TypeOf(computedUserResult{}), reflect.TypeOf(plainResp{})); len(errs) != 0 {
		t.Errorf("a Response with no computed tag must produce no violations; got %v", errs)
	}
}
