package web

import (
	"reflect"
	"strings"
	"testing"
)

// ─── parseExportSort: prefix handling, empty-token skip, unknown token ───────

func TestParseExportSort(t *testing.T) {
	wireToGo := map[string]string{"name": "Name", "age": "Age"}

	sf, bad, ok := parseExportSort("name,-age", wireToGo)
	if !ok || bad != "" {
		t.Fatalf("expected ok, got bad=%q ok=%v", bad, ok)
	}
	if len(sf) != 2 || sf[0].Field != "Name" || sf[0].Desc {
		t.Fatalf("first sort = %+v", sf[0])
	}
	if sf[1].Field != "Age" || !sf[1].Desc {
		t.Fatalf("second sort (desc) = %+v", sf[1])
	}

	// leading/empty tokens are skipped
	sf, _, ok = parseExportSort("name, ,", wireToGo)
	if !ok || len(sf) != 1 {
		t.Fatalf("empty tokens not skipped: %+v ok=%v", sf, ok)
	}

	// unknown token returns it verbatim and ok=false
	_, bad, ok = parseExportSort("name,-bogus", wireToGo)
	if ok || bad != "-bogus" {
		t.Fatalf("expected unknown token '-bogus', got bad=%q ok=%v", bad, ok)
	}
}

// ─── buildKeyfunc: the PEM-parse-error branch ────────────────────────────────

func TestBuildKeyfunc_InvalidPEMReturnsError(t *testing.T) {
	_, err := buildKeyfunc(AuthOptions{PublicKeyPEM: "not a real pem"})
	if err == nil {
		t.Fatal("expected error from an unparsable PEM public key")
	}
}

// ─── formatPathIDConflict: the rendered diagnostic ───────────────────────────

func TestFormatPathIDConflict_MentionsWrapperAndRequest(t *testing.T) {
	msg := formatPathIDConflict("CommandWithBodyID", reflect.TypeOf(struct{ X int }{}))
	if !strings.Contains(msg, "CommandWithBodyID") {
		t.Errorf("diagnostic must name the wrapper: %s", msg)
	}
	if !strings.Contains(msg, `path:"id"`) {
		t.Errorf("diagnostic must mention the path:%q tag: %s", "id", msg)
	}
}
