package gate_test

import (
	"context"
	"fmt"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// This file backs the README example: pkg/gate/README.md §Example mirrors this
// code verbatim. Keep the two in sync.

// staticAllow is a minimal AccessSource that allows every routed scope.
type staticAllow struct{}

func (staticAllow) AccessVersion() uint16 { return gate.CurrentAccessVersion }
func (staticAllow) AccessFor(kind, scope string) (uint8, error) {
	return gate.AccessAllow, nil
}

func Example() {
	evaluator, err := gate.NewHeadlessEvaluator(
		[]gate.AccessBinding{{Kind: "fs.read", Source: staticAllow{}}},
		nil, // no stored rules
		nil, // no grant issuer: no requirement below requests a grant
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	resolution, err := evaluator.Authorize(context.Background(), tool.Request{
		ToolName: "Read",
		Requirements: []tool.Requirement{{
			Kind:        "fs.read",
			Match:       "Read(/repo/README.md)",
			Description: "Read /repo/README.md",
		}},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(resolution.Approved)
	// Output: true
}
