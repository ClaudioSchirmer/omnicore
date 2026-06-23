package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

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
