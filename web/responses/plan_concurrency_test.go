package responses

import (
	"fmt"
	"sync"
	"testing"
)

// The normalization-plan cache publishes a plan only when it is COMPLETE — a
// half-built plan visible to a concurrent AutoFromResult call is a data race
// on its fields slice. Run under -race to prove the property; the plain run
// still asserts the mapping is correct from every goroutine. The types are
// used by no other test, so the first call is guaranteed cold.

type concurrentPlanResult struct {
	ID   string
	Name string
	Tags []string
}

type concurrentPlanResponse struct {
	Auto

	ID   string   `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

func TestAutoFromResult_ConcurrentFirstUse(t *testing.T) {
	const goroutines = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			out := AutoFromResult[concurrentPlanResponse](concurrentPlanResult{
				ID:   fmt.Sprintf("id-%d", g),
				Name: "Ana",
			})
			if out.ID != fmt.Sprintf("id-%d", g) || out.Name != "Ana" {
				errs <- fmt.Errorf("goroutine %d: bad mapping %+v", g, out)
				return
			}
			if out.Tags == nil {
				errs <- fmt.Errorf("goroutine %d: nil slice must normalize to empty", g)
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

// selfRefPlanResult / selfRefPlanResponse exercise the plan builder's cycle
// break on the response side: the pair references itself through a pointer
// and a slice.
type selfRefPlanResult struct {
	Name     string
	Parent   *selfRefPlanResult
	Children []selfRefPlanResult
}

type selfRefPlanResponse struct {
	Auto

	Name     string                `json:"name"`
	Parent   *selfRefPlanResponse  `json:"parent,omitempty"`
	Children []selfRefPlanResponse `json:"children"`
}

func TestAutoFromResult_SelfReferentialType(t *testing.T) {
	out := AutoFromResult[selfRefPlanResponse](selfRefPlanResult{
		Name:     "root",
		Parent:   &selfRefPlanResult{Name: "up"},
		Children: []selfRefPlanResult{{Name: "kid"}},
	})
	if out.Name != "root" {
		t.Fatalf("root: %+v", out)
	}
	if out.Parent == nil || out.Parent.Name != "up" {
		t.Fatalf("pointer cycle leg: %+v", out.Parent)
	}
	if len(out.Children) != 1 || out.Children[0].Name != "kid" {
		t.Fatalf("slice cycle leg: %+v", out.Children)
	}
}
