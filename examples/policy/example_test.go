package policy_test

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/tool"
)

type gatedSource struct{}

func (gatedSource) AccessVersion() uint16                   { return gate.CurrentAccessVersion }
func (gatedSource) AccessFor(string, string) (uint8, error) { return gate.AccessGated, nil }

func Example_hooksAndHeadlessGateFallback() {
	var order []string
	runner, err := hook.Compile(hook.Set{
		PolicyRevision: "tool-policy-v1",
		Around: []hook.Around{{
			Operation: hook.OperationToolCall,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				order = append(order, "begin")
				return ctx, func(hook.Result) { order = append(order, "finish") }
			},
		}},
		Guards: []hook.Guard{{
			Operation: hook.OperationToolCall,
			Check: func(context.Context, hook.Call) error {
				order = append(order, "guard")
				return nil
			},
		}},
	})
	if err != nil {
		panic(err)
	}
	call := hook.Call{Operation: hook.OperationToolCall, ToolCall: &hook.ToolCallData{
		ToolUseID: "call-1", ToolName: "deploy", ArgsJSON: []byte(`{}`),
	}}
	_, finish, err := runner.Start(context.Background(), call)
	if err != nil {
		panic(err)
	}
	finish(hook.Result{Call: call, Outcome: hook.OutcomeCompleted})

	headless, err := gate.NewHeadlessEvaluator([]gate.AccessBinding{{
		Kind: "deployment.write", Source: gatedSource{},
	}}, nil, nil)
	if err != nil {
		panic(err)
	}
	_, err = headless.Authorize(context.Background(), tool.Request{
		ToolName: "deploy", Summary: "deploy release",
		Requirements: []tool.Requirement{{
			Kind: "deployment.write", Match: "production", Description: "deploy to production",
		}},
	})
	var denied *gate.EvaluationError
	fmt.Println(strings.Join(order, ","), errors.As(err, &denied), denied.Kind)
	// Output:
	// begin,guard,finish true approval_required
}
