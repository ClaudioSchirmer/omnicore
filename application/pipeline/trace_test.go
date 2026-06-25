package pipeline

import "testing"

type sampleReq struct{}

func TestDispatchName(t *testing.T) {
	cases := map[string]struct {
		req  any
		want string
	}{
		"pointer to named struct": {&sampleReq{}, "sampleReq"},
		"named struct value":      {sampleReq{}, "sampleReq"},
		"anonymous struct":        {struct{}{}, "request"},
		"nil":                     {nil, "request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := dispatchName(tc.req); got != tc.want {
				t.Errorf("dispatchName = %q, want %q", got, tc.want)
			}
		})
	}
}
