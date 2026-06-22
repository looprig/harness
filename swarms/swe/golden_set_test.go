package swe

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/eval"
)

// TestGoldenSetLoads proves the checked-in golden cases are valid JSON that
// LoadCases can parse, and that at least one case is present. It is the offline
// (untagged) validity check for the operator golden-set; the live run lives in
// operator_eval_integration_test.go behind the integration tag.
func TestGoldenSetLoads(t *testing.T) {
	t.Parallel()
	cases, err := eval.LoadCases("golden-set/cases")
	if err != nil {
		t.Fatalf("LoadCases() error = %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("golden-set/cases has no cases")
	}
	for _, c := range cases {
		if c.Name == "" || c.Input == "" {
			t.Errorf("case %+v missing Name or Input", c)
		}
	}
}
