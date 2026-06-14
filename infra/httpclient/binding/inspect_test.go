package binding

import (
	"reflect"
	"strings"
	"testing"
)

type validReqStruct struct {
	ID      string            `http:"path,id"`
	Verbose bool              `http:"query,verbose"`
	Tags    []string          `http:"query,tags,csv"`
	Extra   map[string]string `http:"headers"`
	Body    payloadStruct     `http:"body,json"`
}

type payloadStruct struct {
	Name string `json:"name"`
}

func TestInspectRequestType_HappyPath(t *testing.T) {
	resetPlanCache()
	plan, err := inspectRequestType(reflect.TypeOf(validReqStruct{}), "/users/{id}")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !plan.hasBody {
		t.Error("plan should report hasBody for the body field")
	}
	if got := len(plan.bindings); got != 5 {
		t.Errorf("bindings = %d, want 5", got)
	}
}

func TestInspectRequestType_Cached(t *testing.T) {
	resetPlanCache()
	a, errA := inspectRequestType(reflect.TypeOf(validReqStruct{}), "/users/{id}")
	b, errB := inspectRequestType(reflect.TypeOf(validReqStruct{}), "/users/{id}")
	if errA != nil || errB != nil {
		t.Fatalf("inspect: %v / %v", errA, errB)
	}
	if a != b {
		t.Errorf("cached plan should be the same pointer; got %p vs %p", a, b)
	}
}

func TestInspectRequestType_MissingPathPlaceholder(t *testing.T) {
	resetPlanCache()
	type bad struct {
		ID string `http:"path,id"`
	}
	_, err := inspectRequestType(reflect.TypeOf(bad{}), "/users/{userId}")
	if err == nil {
		t.Fatal("expected error: path placeholder userId has no tagged field")
	}
	if !strings.Contains(err.Error(), "userId") {
		t.Errorf("error should mention userId; got %v", err)
	}
}

func TestInspectRequestType_OrphanPathTag(t *testing.T) {
	resetPlanCache()
	type bad struct {
		ID string `http:"path,id"`
	}
	_, err := inspectRequestType(reflect.TypeOf(bad{}), "/users")
	if err == nil {
		t.Fatal("expected error: path tag id has no matching placeholder")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("error should mention id; got %v", err)
	}
}

func TestInspectRequestType_MultipleBodiesRejected(t *testing.T) {
	resetPlanCache()
	type bad struct {
		A payloadStruct `http:"body,json"`
		B payloadStruct `http:"body,json"`
	}
	_, err := inspectRequestType(reflect.TypeOf(bad{}), "/x")
	if err == nil {
		t.Fatal("expected error for multiple body fields")
	}
	if !strings.Contains(err.Error(), "more than one") {
		t.Errorf("error should mention multiple body; got %v", err)
	}
}

func TestInspectRequestType_BadFieldTypeForKind(t *testing.T) {
	resetPlanCache()
	type bad struct {
		Tags string `http:"query,tags,csv"`
	}
	_, err := inspectRequestType(reflect.TypeOf(bad{}), "/x")
	if err == nil {
		t.Fatal("expected error: csv requires slice")
	}
	if !strings.Contains(err.Error(), "slice or array") {
		t.Errorf("error should mention slice/array; got %v", err)
	}
}

func TestInspectRequestType_HeadersMapType(t *testing.T) {
	resetPlanCache()
	type bad struct {
		H map[string]int `http:"headers"`
	}
	_, err := inspectRequestType(reflect.TypeOf(bad{}), "/x")
	if err == nil {
		t.Fatal("expected error: headers map value must be string")
	}
}

func TestInspectRequestType_RejectsNilType(t *testing.T) {
	resetPlanCache()
	if _, err := inspectRequestType(nil, "/x"); err == nil {
		t.Fatal("expected error for nil type")
	}
}

func TestInspectRequestType_RejectsNonStruct(t *testing.T) {
	resetPlanCache()
	if _, err := inspectRequestType(reflect.TypeOf(42), "/x"); err == nil {
		t.Fatal("expected error for non-struct type")
	}
}
