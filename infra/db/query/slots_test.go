package query

import (
	"context"
	"strings"
	"testing"
)

func TestBeginSlotRebuild_WritesShadow(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	q := &fakeQuerier{execFn: func(sql string, args []any) error { gotSQL, gotArgs = sql, args; return nil }}
	if err := beginSlotRebuild(context.Background(), q, fakeDialect{}, "v", "v__0"); err != nil {
		t.Fatalf("beginSlotRebuild: %v", err)
	}
	if !strings.Contains(gotSQL, "shadow_collection =") {
		t.Errorf("SQL missing shadow set: %q", gotSQL)
	}
	// Placeholders are numbered in appearance order: SET value first, WHERE key last.
	if len(gotArgs) != 2 || gotArgs[0] != "v__0" || gotArgs[1] != "v" {
		t.Errorf("args = %v, want [v__0 v]", gotArgs)
	}
}

func TestFlipSlot_PromotesShadowUnderGuard(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	q := &fakeQuerier{execFn: func(sql string, args []any) error { gotSQL, gotArgs = sql, args; return nil }}
	if err := flipSlot(context.Background(), q, fakeDialect{}, "v"); err != nil {
		t.Fatalf("flipSlot: %v", err)
	}
	if !strings.Contains(gotSQL, "active_collection = shadow_collection") {
		t.Errorf("SQL missing promote: %q", gotSQL)
	}
	if !strings.Contains(gotSQL, "IS NOT NULL") {
		t.Errorf("SQL missing no-op guard: %q", gotSQL)
	}
	if len(gotArgs) != 1 || gotArgs[0] != "v" {
		t.Errorf("args = %v, want [v]", gotArgs)
	}
}

func TestFlipSlot_ErrorPropagates(t *testing.T) {
	q := &fakeQuerier{execFn: func(string, []any) error { return errFake }}
	if err := flipSlot(context.Background(), q, fakeDialect{}, "v"); err == nil {
		t.Fatal("expected flip error to propagate")
	}
}

func TestBeginAndAbortSlot_ErrorsPropagate(t *testing.T) {
	q := &fakeQuerier{execFn: func(string, []any) error { return errFake }}
	if err := beginSlotRebuild(context.Background(), q, fakeDialect{}, "v", "v__0"); err == nil {
		t.Error("beginSlotRebuild must propagate the exec error")
	}
	if err := abortSlotRebuild(context.Background(), q, fakeDialect{}, "v"); err == nil {
		t.Error("abortSlotRebuild must propagate the exec error")
	}
}
