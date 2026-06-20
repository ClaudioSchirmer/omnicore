package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// --- joinPath: empty-segment branch ---

func TestJoinPath_EmptySegmentReturnsPrefix(t *testing.T) {
	if got := joinPath("prefix", ""); got != "prefix" {
		t.Errorf("empty segment must return prefix, got %q", got)
	}
	if got := joinPath("", "seg"); got != "seg" {
		t.Errorf("empty prefix must return segment, got %q", got)
	}
	if got := joinPath("a", "b"); got != "a.b" {
		t.Errorf("expected dotted join, got %q", got)
	}
}

// --- buildKeyfunc: config-arity + PEM branches (no network) ---

func TestBuildKeyfunc_ConfigArity(t *testing.T) {
	if _, err := buildKeyfunc(AuthOptions{}); err == nil {
		t.Error("expected error when neither JWKS nor PEM set")
	}
	if _, err := buildKeyfunc(AuthOptions{JWKSURL: "x", PublicKeyPEM: "y"}); err == nil {
		t.Error("expected error when both JWKS and PEM set")
	}
}

func TestBuildKeyfunc_PEMBranches(t *testing.T) {
	// Invalid PEM → parse error.
	if _, err := buildKeyfunc(AuthOptions{PublicKeyPEM: "not-a-pem"}); err == nil {
		t.Error("expected error for malformed PEM")
	}
	// Valid RSA public key PEM → keyfunc returned.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	kf, err := buildKeyfunc(AuthOptions{PublicKeyPEM: string(pemBytes)})
	if err != nil || kf == nil {
		t.Fatalf("expected a keyfunc from valid PEM, got kf=%v err=%v", kf, err)
	}
	if got, err := kf(nil); err != nil || got == nil {
		t.Errorf("keyfunc should return the parsed key, got %v err=%v", got, err)
	}
}

// --- setPathField: conversion-failure branches (no fiber needed) ---

func TestSetPathField_ConversionFailures(t *testing.T) {
	t.Run("uint", func(t *testing.T) {
		fv := reflect.New(reflect.TypeOf(uint64(0))).Elem()
		if !setPathField(fv, pathFieldPlan{kind: pathKindUint, bits: 64}, "-1") {
			t.Error("expected uint parse failure")
		}
	})
	t.Run("float", func(t *testing.T) {
		fv := reflect.New(reflect.TypeOf(float64(0))).Elem()
		if !setPathField(fv, pathFieldPlan{kind: pathKindFloat, bits: 64}, "notnum") {
			t.Error("expected float parse failure")
		}
	})
	t.Run("domainID", func(t *testing.T) {
		fv := reflect.New(reflect.TypeOf(domain.ID{})).Elem()
		if !setPathField(fv, pathFieldPlan{kind: pathKindDomainID}, "not-a-uuid") {
			t.Error("expected domain.ID parse failure")
		}
	})
	t.Run("unknown-kind", func(t *testing.T) {
		fv := reflect.New(reflect.TypeOf("")).Elem()
		if !setPathField(fv, pathFieldPlan{kind: pathFieldKind(9999)}, "x") {
			t.Error("expected default branch to report failure")
		}
	})
}

func TestSetPathField_Successes(t *testing.T) {
	uv := reflect.New(reflect.TypeOf(uint32(0))).Elem()
	if setPathField(uv, pathFieldPlan{kind: pathKindUint, bits: 32}, "7") || uv.Uint() != 7 {
		t.Error("uint success failed")
	}
	fv := reflect.New(reflect.TypeOf(float32(0))).Elem()
	if setPathField(fv, pathFieldPlan{kind: pathKindFloat, bits: 32}, "1.5") || fv.Float() != 1.5 {
		t.Error("float success failed")
	}
	dv := reflect.New(reflect.TypeOf(domain.ID{})).Elem()
	if setPathField(dv, pathFieldPlan{kind: pathKindDomainID}, uuid.NewString()) {
		t.Error("domain.ID success failed")
	}
}

// --- classifyPathFieldType: int classification + rejection branches ---

func TestClassifyPathFieldType(t *testing.T) {
	if k, bits, e := classifyPathFieldType(reflect.TypeOf(int(0))); k != pathKindInt || bits == 0 || e != "" {
		t.Errorf("int classification: %v %v %q", k, bits, e)
	}
	if _, _, e := classifyPathFieldType(reflect.TypeOf((*int)(nil))); e == "" {
		t.Error("pointer must be rejected")
	}
	if _, _, e := classifyPathFieldType(reflect.TypeOf([]int{})); e == "" {
		t.Error("slice must be rejected")
	}
	type custom struct{ X int }
	if _, _, e := classifyPathFieldType(reflect.TypeOf(custom{})); e == "" {
		t.Error("custom struct must be rejected")
	}
	if _, _, e := classifyPathFieldType(reflect.TypeOf(make(chan int))); e == "" {
		t.Error("unsupported kind must be rejected")
	}
}

// --- inspectPathTags: pointer deref + structural panics ---

type pathOKReq struct {
	Page int `path:"page"`
}

func TestInspectPathTags_PointerTypeDerefs(t *testing.T) {
	s := inspectPathTags(reflect.PointerTo(reflect.TypeOf(pathOKReq{})))
	if len(s.fields) != 1 || s.fields[0].kind != pathKindInt {
		t.Errorf("expected one int path field, got %+v", s.fields)
	}
}

func TestInspectPathTags_Panics(t *testing.T) {
	type anonInner struct{ A string }
	cases := []struct {
		name string
		typ  reflect.Type
	}{
		{"anonymous", reflect.TypeOf(struct {
			anonInner `path:"x"`
		}{})},
		{"json-conflict", reflect.TypeOf(struct {
			ID string `path:"id" json:"id"`
		}{})},
		{"bad-type", reflect.TypeOf(struct {
			Bad []string `path:"bad"`
		}{})},
		{"dup-segment", reflect.TypeOf(struct {
			A string `path:"same"`
			B string `path:"same"`
		}{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", tc.name)
				}
			}()
			// Fresh cache miss per type; inspectPathTags panics on the violation.
			inspectPathTags(tc.typ)
		})
	}
}

// --- BindPath: nil + non-pointer/non-struct panics ---

func TestBindPath_NilReqNoop(t *testing.T) {
	if bad, ok := BindPath(nil, nil); !ok || bad != "" {
		t.Errorf("nil req must be a no-op, got (%q,%v)", bad, ok)
	}
}

func TestBindPath_NonPointerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for non-pointer req")
		}
	}()
	BindPath(nil, pathOKReq{})
}

func TestBindPath_PointerToNonStructPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for pointer-to-non-struct req")
		}
	}()
	n := 5
	BindPath(nil, &n)
}

// --- projection: extractProjectionSchema + walkProjectionLevel + guard ---

type projChild struct {
	City *string `json:"city,omitempty"`
}

type projEmbed struct {
	Embedded *string `json:"embedded,omitempty"`
}

type projResp struct {
	*projEmbed              // anonymous pointer-to-struct → promoted
	Name       *string      `json:"name,omitempty"`
	Hidden     *string      `json:"-"` // skipped
	NoTag      *string      // empty json tag → falls back to field name
	Self       *projChild   `json:"self,omitempty"`
	Lines      []*projChild `json:"lines,omitempty"`
}

func TestExtractProjectionSchema_PointerAndNested(t *testing.T) {
	s := extractProjectionSchema(reflect.PointerTo(reflect.TypeOf(projResp{})))
	for _, wire := range []string{"name", "embedded", "NoTag", "self", "self.city", "lines", "lines.city"} {
		if _, ok := s.paths[wire]; !ok {
			t.Errorf("expected wire path %q in %v", wire, s.paths)
		}
	}
	if _, ok := s.paths["Hidden"]; ok {
		t.Error("json:\"-\" field must be skipped")
	}
}

func TestExtractProjectionSchema_NonStructIsEmpty(t *testing.T) {
	s := extractProjectionSchema(reflect.TypeOf(0))
	if len(s.paths) != 0 {
		t.Errorf("non-struct must yield empty schema, got %v", s.paths)
	}
}

// invalidResp violates the sparse-render contract in several ways so
// walkResponseGuard reports every rule branch.
type invalidGuardChild struct {
	City string `json:"city,omitempty"` // non-pointer scalar → violation
}

type invalidGuardEmbed struct {
	Promoted string `json:"promoted,omitempty"` // non-pointer scalar
}

type invalidResp struct {
	invalidGuardEmbed                      // anonymous struct → recurse
	Scalar            string               `json:"scalar"`        // missing omitempty + non-pointer
	Hidden            string               `json:"-"`             // skipped
	Bag               map[string]any       `json:"bag,omitempty"` // map → accepted
	Children          []*invalidGuardChild `json:"children,omitempty"`
}

func TestValidateFieldsResponse_ReportsViolations(t *testing.T) {
	errs := validateFieldsResponse(reflect.TypeOf(invalidResp{}))
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"scalar", "promoted", "children.city"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a violation mentioning %q in:\n%s", want, joined)
		}
	}
	// The boot-panic formatter should render the collected violations.
	msg := formatFieldsResponseGuard(reflect.TypeOf(invalidResp{}), errs)
	if !strings.Contains(msg, "sparse-render contract") {
		t.Errorf("unexpected guard message: %s", msg)
	}
}
