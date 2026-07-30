package loopruntime

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

type inferenceHookContextKey struct{}

type hookProbeClient struct {
	mu           sync.Mutex
	calls        int
	context      context.Context
	request      inference.Request
	openErr      error
	nextErr      error
	panicNow     bool
	empty        bool
	mutateNested string
}

func (*hookProbeClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("Invoke not used")
}

func (c *hookProbeClient) Stream(ctx context.Context, request inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	c.calls++
	c.context = ctx
	c.request = request
	panicNow := c.panicNow
	openErr := c.openErr
	nextErr := c.nextErr
	empty := c.empty
	mutateNested := c.mutateNested
	c.mu.Unlock()
	if mutateNested != "" {
		request.Messages[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = mutateNested
	}
	if panicNow {
		panic("provider panic detail must stay private")
	}
	if openErr != nil {
		return nil, openErr
	}
	n := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if empty {
			return nil, io.EOF
		}
		if n == 0 {
			n++
			return textChunk("answer"), nil
		}
		if nextErr != nil {
			return nil, nextErr
		}
		return nil, io.EOF
	}, nil), nil
}

func TestInferenceHooksFinishRetainsFrozenRequestWhenProviderMutatesItsRequest(t *testing.T) {
	t.Parallel()
	client := &hookProbeClient{mutateNested: "provider mutation"}
	request := inference.Request{
		Model: testModel(),
		Messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: "frozen input"}},
		}}},
	}
	var finished hook.Result
	runner := compileInferenceHook(t, nil, func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
		return ctx, func(result hook.Result) { finished = result }
	})

	result := runStep(context.Background(), stepConfig{
		req: request, client: client, emit: func(event.Event) {}, hooks: runner,
	}, 1, newTestStep(t, 0))

	if result.terminal != nil {
		t.Fatalf("terminal = %T, want success", result.terminal)
	}
	got := finished.Inference.Request.Messages[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text
	if got != "frozen input" {
		t.Fatalf("finish request text = %q, want frozen input", got)
	}
}

func (c *hookProbeClient) snapshot() (int, context.Context, inference.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.context, c.request
}

func compileInferenceHook(t *testing.T, guard hook.GuardFunc, begin hook.BeginFunc) *hook.Runner {
	t.Helper()
	set := hook.Set{}
	if guard != nil {
		set.PolicyRevision = "inference-hook-test-v1"
		set.Guards = []hook.Guard{{Operation: hook.OperationInference, Check: guard}}
	}
	if begin != nil {
		set.Around = []hook.Around{{Operation: hook.OperationInference, Begin: begin}}
	}
	runner, err := hook.Compile(set)
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	return runner
}

func TestInferenceHooksCloneRequestAndPropagateDerivedContext(t *testing.T) {
	t.Parallel()
	client := &hookProbeClient{}
	request := inference.Request{
		Model:    testModel(),
		System:   "system",
		Messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "input"}}}}},
	}
	var observed inference.Request
	runner := compileInferenceHook(t, nil, func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
		observed = *hook.CloneCall(call).Inference.Request
		call.Inference.Request.System = "mutated observer copy"
		call.Inference.Request.Messages[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "mutated nested observer copy"
		return context.WithValue(ctx, inferenceHookContextKey{}, "derived"), nil
	})
	st := newTestStep(t, 3)
	cfg := stepConfig{
		req: request, client: client, emit: func(event.Event) {}, hooks: runner,
	}

	result := runStep(context.Background(), cfg, 7, st)

	if result.terminal != nil {
		t.Fatalf("terminal = %T, want success", result.terminal)
	}
	if !reflect.DeepEqual(observed, request) {
		t.Fatalf("hook request = %#v, want exact %#v", observed, request)
	}
	calls, gotCtx, gotRequest := client.snapshot()
	if calls != 1 {
		t.Fatalf("Stream calls = %d, want 1", calls)
	}
	if got := gotCtx.Value(inferenceHookContextKey{}); got != "derived" {
		t.Fatalf("Stream context value = %v, want derived", got)
	}
	if !reflect.DeepEqual(gotRequest, request) {
		t.Fatalf("provider request = %#v, want observer mutation isolated from %#v", gotRequest, request)
	}
}

func TestInferenceHooksDenialNeverOpensProviderAndFinishesDenied(t *testing.T) {
	t.Parallel()
	client := &hookProbeClient{}
	var finishes []hook.Result
	runner := compileInferenceHook(t,
		func(context.Context, hook.Call) error {
			return hook.Deny("inference_blocked", "policy refused inference")
		},
		func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(result hook.Result) { finishes = append(finishes, result) }
		},
	)
	result := runStep(context.Background(), stepConfig{
		req: inference.Request{Model: testModel()}, client: client, emit: func(event.Event) {}, hooks: runner,
	}, 1, newTestStep(t, 0))

	if calls, _, _ := client.snapshot(); calls != 0 {
		t.Fatalf("Stream calls = %d, want 0", calls)
	}
	failed, ok := result.terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", result.terminal)
	}
	if failed.Err == nil || failed.Err.Error() == "policy refused inference" {
		t.Fatalf("TurnFailed.Err = %v, want safe typed wrapper without guard detail", failed.Err)
	}
	if len(finishes) != 1 || finishes[0].Outcome != hook.OutcomeDenied {
		t.Fatalf("finishes = %#v, want exactly one denied result", finishes)
	}
}

func TestInferenceHooksTerminalPathsFinishExactlyOnce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		client      *hookProbeClient
		cancel      bool
		wantOutcome hook.Outcome
	}{
		{name: "success", client: &hookProbeClient{}, wantOutcome: hook.OutcomeCompleted},
		{name: "provider open error", client: &hookProbeClient{openErr: errors.New("open failed")}, wantOutcome: hook.OutcomeFailed},
		{name: "stream consumption error", client: &hookProbeClient{nextErr: errors.New("decode failed")}, wantOutcome: hook.OutcomeFailed},
		{name: "response assembly error", client: &hookProbeClient{empty: true}, wantOutcome: hook.OutcomeFailed},
		{name: "cancellation", client: &hookProbeClient{openErr: context.Canceled}, cancel: true, wantOutcome: hook.OutcomeCanceled},
		{name: "provider panic", client: &hookProbeClient{panicNow: true}, wantOutcome: hook.OutcomeFailed},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var finishes []hook.Result
			runner := compileInferenceHook(t, nil, func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return ctx, func(result hook.Result) {
					mu.Lock()
					defer mu.Unlock()
					finishes = append(finishes, result)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancel {
				cancel()
			} else {
				defer cancel()
			}
			run := func() {
				_ = runStep(ctx, stepConfig{
					req: inference.Request{Model: testModel()}, client: tc.client,
					emit: func(event.Event) {}, hooks: runner,
				}, 1, newTestStep(t, 0))
			}
			run()
			mu.Lock()
			defer mu.Unlock()
			if len(finishes) != 1 {
				t.Fatalf("finish count = %d, want 1", len(finishes))
			}
			if finishes[0].Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", finishes[0].Outcome, tc.wantOutcome)
			}
		})
	}
}
