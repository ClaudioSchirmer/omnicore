package binding

import (
	"strings"
	"testing"
)

func TestParseHTTPTag_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want fieldBinding
	}{
		{"path,id", fieldBinding{kind: bindPath, name: "id"}},
		{"query,verbose", fieldBinding{kind: bindQuerySingle, name: "verbose"}},
		{"query,tags,csv", fieldBinding{kind: bindQueryCSV, name: "tags"}},
		{"query,tags,multi", fieldBinding{kind: bindQueryMulti, name: "tags"}},
		{"header,X-Tenant", fieldBinding{kind: bindHeader, name: "X-Tenant"}},
		{"headers", fieldBinding{kind: bindHeadersMap}},
		{"body,json", fieldBinding{kind: bindBody, codec: "json"}},
		{"body,xml", fieldBinding{kind: bindBody, codec: "xml"}},
		{"body,form", fieldBinding{kind: bindBody, codec: "form-urlencoded"}},
		{"body,form-urlencoded", fieldBinding{kind: bindBody, codec: "form-urlencoded"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, present, err := parseHTTPTag(tc.in)
			if err != nil {
				t.Fatalf("parseHTTPTag(%q) error: %v", tc.in, err)
			}
			if !present {
				t.Fatalf("parseHTTPTag(%q): not present", tc.in)
			}
			if got.kind != tc.want.kind || got.name != tc.want.name || got.codec != tc.want.codec {
				t.Errorf("parseHTTPTag(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseHTTPTag_AbsentTagSilent(t *testing.T) {
	_, present, err := parseHTTPTag("")
	if err != nil {
		t.Fatalf("empty tag error: %v", err)
	}
	if present {
		t.Fatal("empty tag should not be flagged present")
	}
}

func TestParseHTTPTag_Invalid(t *testing.T) {
	cases := []struct {
		in       string
		wantPart string
	}{
		{"path", "path requires a name"},
		{"path,", "path requires a name"},
		{"query", "query requires a name"},
		{"query,name,xml", "query style"},
		{"header", "header requires a name"},
		{"headers,extra", "headers takes no argument"},
		{"body", "body requires a codec name"},
		{"body,yaml", "not one of json|xml|form"},
		{"unknown,x", "unsupported kind"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			_, present, err := parseHTTPTag(tc.in)
			if !present {
				t.Fatalf("expected presence for %q", tc.in)
			}
			if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantPart) {
				t.Errorf("error %q should contain %q", err, tc.wantPart)
			}
		})
	}
}
