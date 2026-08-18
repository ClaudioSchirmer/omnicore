package queries

import (
	"fmt"
	"sync"
	"testing"
)

// The fill-plan cache publishes a plan only when it is COMPLETE: a plan
// visible to another goroutine while its lookup maps are still being written
// is a concurrent map read/write — a runtime crash, not a recoverable panic.
// These tests pin the two properties the publish-after-build cache must hold:
// a cold type filled from many goroutines at once (run under -race to prove
// the absence of the data race), and a self-referential type terminating its
// own plan build (the job the retired store-before-recurse trick did).

// concurrentFillResult is deliberately used by no other test, so the first
// fill in TestResultFromDoc_ConcurrentFirstFill is guaranteed to race for the
// cold-cache path.
type concurrentFillResult struct {
	ID    string
	Name  string
	Count int
	Tags  []string
	Child concurrentFillChild
}

type concurrentFillChild struct {
	City string
}

func TestResultFromDoc_ConcurrentFirstFill(t *testing.T) {
	const goroutines = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			doc := map[string]any{
				"ID":    fmt.Sprintf("id-%d", g),
				"Name":  "Ana",
				"Count": g,
				"Tags":  []any{"a", "b"},
				"Child": map[string]any{"City": "POA"},
			}
			out := ResultFromDoc[concurrentFillResult](doc)
			if out.ID != fmt.Sprintf("id-%d", g) || out.Name != "Ana" ||
				out.Count != g || len(out.Tags) != 2 || out.Child.City != "POA" {
				errs <- fmt.Errorf("goroutine %d: bad fill %+v", g, out)
			}
		}(g)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// selfRefFillResult exercises the cycle break of the plan builder: the type
// references itself through a pointer AND through a slice, the two shapes the
// in-progress set must terminate.
type selfRefFillResult struct {
	Name     string
	Parent   *selfRefFillResult
	Children []selfRefFillResult
}

func TestResultFromDoc_SelfReferentialType(t *testing.T) {
	doc := map[string]any{
		"Name":   "root",
		"Parent": map[string]any{"Name": "up"},
		"Children": []any{
			map[string]any{"Name": "kid", "Children": []any{map[string]any{"Name": "grandkid"}}},
		},
	}
	out := ResultFromDoc[selfRefFillResult](doc)
	if out.Name != "root" {
		t.Fatalf("root: %+v", out)
	}
	if out.Parent == nil || out.Parent.Name != "up" {
		t.Fatalf("pointer cycle leg: %+v", out.Parent)
	}
	if len(out.Children) != 1 || out.Children[0].Name != "kid" {
		t.Fatalf("slice cycle leg: %+v", out.Children)
	}
	if len(out.Children[0].Children) != 1 || out.Children[0].Children[0].Name != "grandkid" {
		t.Fatalf("nested cycle leg: %+v", out.Children[0].Children)
	}
}
