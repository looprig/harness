package loop

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/core/uuid"
)

// prepareError is the typed error a Preparer fake returns to exercise the
// fail-secure path (call not executed, no gate opened, model-visible result).
type prepareError struct{ reason string }

func (e *prepareError) Error() string { return "prepare: " + e.reason }

// preparerTool is a fake InvokableTool + Preparer + PermissionPrompter used to
// prove the runner's Preparer seam end-to-end:
//
//   - Prepare records its call-count and the callID it was bound to, then returns
//     a tool.TokenArtifact whose Token IS the callID string (so the artifact's
//     token == the ToolExecutionID). A configured prepareErr makes Prepare fail.
//   - BuildRequest builds an UnknownRequest whose Summary embeds the prepared
//     artifact's token — that is the PERMISSION-TIME token, surfaced on the wire
//     via the emitted PermissionRequested.Request.Description().
//   - InvokableRun reads the artifact back from ctx via PreparedFromContext and
//     returns its token — the EXECUTION-TIME token.
//
// A test asserts permission-time token == execution-time token == ToolExecutionID,
// and that Prepare ran exactly once.
type preparerTool struct {
	name       string
	prepareErr error // non-nil → Prepare fails (fail-secure path)

	prepareCalls int32 // atomically incremented per Prepare invocation
	mu           sync.Mutex
	gotCallID    uuid.UUID // the callID Prepare was bound to (last call)
}

func (p *preparerTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: p.name}, nil
}

func (p *preparerTool) Prepare(ctx context.Context, callID uuid.UUID, argsJSON string) (tool.PreparedArtifact, error) {
	atomic.AddInt32(&p.prepareCalls, 1)
	p.mu.Lock()
	p.gotCallID = callID
	p.mu.Unlock()
	if p.prepareErr != nil {
		return nil, p.prepareErr
	}
	// The token IS the callID string: this is what binds the artifact to the call,
	// so permission-time and execution-time reads must agree on it.
	return tool.TokenArtifact{Token: callID.String()}, nil
}

func (p *preparerTool) BuildRequest(argsJSON string, prepared tool.PreparedArtifact) (tool.PermissionRequest, error) {
	ta, ok := prepared.(tool.TokenArtifact)
	if !ok {
		return nil, &prepareError{reason: "BuildRequest got no prepared artifact"}
	}
	// Summary carries the permission-time token; Description() surfaces it on the
	// emitted PermissionRequested so the test can read it off the wire.
	return tool.UnknownRequest{Tool: p.name, Summary: ta.Token}, nil
}

func (p *preparerTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	prepared, ok := PreparedFromContext(ctx)
	if !ok {
		return tool.TextResult("error: no prepared artifact in ctx"), nil
	}
	ta, ok := prepared.(tool.TokenArtifact)
	if !ok {
		return tool.TextResult("error: prepared artifact not a TokenArtifact"), nil
	}
	return tool.TextResult(ta.Token), nil
}

func (p *preparerTool) calls() int32 { return atomic.LoadInt32(&p.prepareCalls) }

func (p *preparerTool) boundCallID() uuid.UUID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gotCallID
}

// Compile-time assertions: preparerTool satisfies the base + the three optionals
// the seam exercises.
var (
	_ tool.InvokableTool      = (*preparerTool)(nil)
	_ tool.Preparer           = (*preparerTool)(nil)
	_ tool.PermissionPrompter = (*preparerTool)(nil)
)

// nonPreparerPromptTool is an InvokableTool + PermissionPrompter that is NOT a
// Preparer. It proves the non-Preparer row: no Prepare call, identical Ask-gate
// behavior, and PreparedFromContext reports absent in InvokableRun.
type nonPreparerPromptTool struct{ name string }

func (n *nonPreparerPromptTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: n.name}, nil
}
func (n *nonPreparerPromptTool) BuildRequest(argsJSON string, prepared tool.PreparedArtifact) (tool.PermissionRequest, error) {
	// A non-Preparer tool ignores prepared (it is nil); behavior is identical.
	return tool.UnknownRequest{Tool: n.name, Summary: "no-prepare"}, nil
}
func (n *nonPreparerPromptTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	if _, ok := PreparedFromContext(ctx); ok {
		return tool.TextResult("error: non-Preparer tool got a prepared artifact"), nil
	}
	return tool.TextResult("ran-no-prepare"), nil
}

var (
	_ tool.InvokableTool      = (*nonPreparerPromptTool)(nil)
	_ tool.PermissionPrompter = (*nonPreparerPromptTool)(nil)
)

// approveActor spawns a fake actor that acks then approves the first gate
// (ScopeOnce). It returns the gateReg channel to wire into RunBatch. The actor
// goroutine exits after one gate, so a fail-secure scenario (no gate opened)
// leaves it harmlessly parked on the receive until the test ends.
func approveActor() chan gateRegistration {
	gateReg := make(chan gateRegistration)
	go func() {
		reg, ok := <-gateReg
		if !ok {
			return
		}
		close(reg.ack)
		reg.reply <- command.ApproveToolCall{
			GateRoute: command.GateRoute{ToolExecutionID: reg.callID},
			Scope:     tool.ScopeOnce,
		}
	}()
	return gateReg
}

// permissionTokenOf returns the token embedded in the (single) emitted
// PermissionRequested event's request Description, and how many such events fired.
func permissionTokenOf(evs []event.Event) (token string, count int) {
	for _, ev := range evs {
		if pr, ok := ev.(event.PermissionRequested); ok {
			count++
			if pr.Request != nil {
				token = pr.Request.Description()
			}
		}
	}
	return token, count
}

// preparerSeamRow is one table row for TestRunBatch_PreparerSeam.
type preparerSeamRow struct {
	name              string
	makeTool          func() tool.InvokableTool
	prepareErr        bool  // the row drives a Prepare failure (skip bound-callID check)
	wantPrepareCalls  int32 // expected Prepare invocations (0 for non-Preparer)
	wantGate          bool  // a PermissionRequested must be emitted
	wantResultErr     bool  // the result is an error tool-result
	wantResultSubstr  string
	tokensMustBindCID bool // permission-time == execution-time == ToolExecutionID
}

// TestRunBatch_PreparerSeam drives a Preparer tool through an EffectAsk gate and
// asserts the seam's three invariants for the happy path, plus the non-Preparer
// row (no Prepare, identical behavior) and the fail-secure Prepare-error row (no
// gate, no exec, typed model-visible result).
func TestRunBatch_PreparerSeam(t *testing.T) {
	t.Parallel()

	tests := []preparerSeamRow{
		{
			name:              "preparer happy path: one Prepare, tokens bind to ToolExecutionID",
			makeTool:          func() tool.InvokableTool { return &preparerTool{name: "P"} },
			wantPrepareCalls:  1,
			wantGate:          true,
			tokensMustBindCID: true,
		},
		{
			name:             "non-preparer tool: no Prepare, identical Ask-gate behavior",
			makeTool:         func() tool.InvokableTool { return &nonPreparerPromptTool{name: "P"} },
			wantPrepareCalls: 0,
			wantGate:         true,
			wantResultSubstr: "ran-no-prepare",
		},
		{
			name: "prepare error: fail-secure — no gate, no exec, typed result",
			makeTool: func() tool.InvokableTool {
				return &preparerTool{name: "P", prepareErr: &prepareError{reason: "snapshot vanished"}}
			},
			prepareErr:       true,
			wantPrepareCalls: 1,
			wantGate:         false,
			wantResultErr:    true,
			wantResultSubstr: "tool preparation failed",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tl := tt.makeTool()
			gate := &fakePermissionGate{checkFn: func(name, args string) Effect { return EffectAsk }}
			ts := ToolSet{Permission: gate, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 4}
			emit, getEvents := collectEmit()
			gateReg := approveActor()

			results := withRunTimeout(t, func() []result {
				return RunBatch(context.Background(), []content.ToolUseBlock{call(t, "P", `{}`)}, ts, gateReg, uuid.New, emit)
			})

			if len(results) != 1 {
				t.Fatalf("len(results) = %d, want 1", len(results))
			}
			r := results[0]

			// Prepare call-count + binding (only the preparerTool tracks them).
			if pt, ok := tl.(*preparerTool); ok {
				if got := pt.calls(); got != tt.wantPrepareCalls {
					t.Errorf("Prepare called %d times, want %d", got, tt.wantPrepareCalls)
				}
				if tt.wantPrepareCalls > 0 && !tt.prepareErr && pt.boundCallID() != r.ToolExecutionID {
					t.Errorf("Prepare bound to callID %v, want ToolExecutionID %v", pt.boundCallID(), r.ToolExecutionID)
				}
			}

			permToken, nPerm := permissionTokenOf(getEvents())
			if tt.wantGate && nPerm != 1 {
				t.Errorf("PermissionRequested count = %d, want 1", nPerm)
			}
			if !tt.wantGate && nPerm != 0 {
				t.Errorf("PermissionRequested count = %d, want 0 (fail-secure: no gate)", nPerm)
			}

			if tt.wantResultErr != r.IsError {
				t.Errorf("result IsError = %v, want %v: %q", r.IsError, tt.wantResultErr, resultText(r))
			}
			if tt.wantResultSubstr != "" && !strings.Contains(resultText(r), tt.wantResultSubstr) {
				t.Errorf("result %q does not contain %q", resultText(r), tt.wantResultSubstr)
			}

			if tt.tokensMustBindCID {
				execToken := resultText(r)
				cid := r.ToolExecutionID.String()
				if permToken != cid {
					t.Errorf("permission-time token = %q, want ToolExecutionID %q", permToken, cid)
				}
				if execToken != cid {
					t.Errorf("execution-time token = %q, want ToolExecutionID %q", execToken, cid)
				}
				if permToken != execToken {
					t.Errorf("permission-time token %q != execution-time token %q", permToken, execToken)
				}
			}

			if tt.prepareErr {
				// Fail-secure: the result must carry the typed Prepare error verbatim
				// (prefix + reason) and NOTHING else — the tool body never ran, so no
				// artifact/success output leaked through.
				if got, want := resultText(r), errPreparePrefix+(&prepareError{reason: "snapshot vanished"}).Error(); got != want {
					t.Errorf("fail-secure result = %q, want exactly %q", got, want)
				}
			}
		})
	}
}

// withRunTimeout runs fn in a goroutine and fails the test if it does not return
// within a bounded window (guards against a wedged gate in the seam test).
func withRunTimeout(t *testing.T, fn func() []result) []result {
	t.Helper()
	done := make(chan []result, 1)
	go func() { done <- fn() }()
	select {
	case rs := <-done:
		return rs
	case <-time.After(2 * time.Second):
		t.Fatal("RunBatch wedged (Preparer seam test)")
		return nil
	}
}
