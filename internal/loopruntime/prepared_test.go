package loopruntime

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// prepareError is the typed error a CallPreparer fake returns to exercise the
// fail-secure path (call not executed, no gate opened, model-visible result).
type prepareError struct{ reason string }

func (e *prepareError) Error() string { return "prepare: " + e.reason }

// preparerTool is a fake InvokableTool + CallPreparer used to prove the
// runner's preparation seam end-to-end:
//
//   - PrepareCall records its call-count and the executionID it was bound to,
//     then returns a gated command request bound to that executionID plus a
//     tool.TokenArtifact whose Token IS the executionID string. A configured
//     prepareErr makes PrepareCall fail.
//   - InvokableRun reads the prepared execution contract back from ctx via
//     PreparedCallFromContext and returns the artifact's token — the
//     EXECUTION-TIME token.
//
// A test asserts execution-time token == ToolExecutionID and that PrepareCall
// ran exactly once.
type preparerTool struct {
	name       string
	prepareErr error // non-nil → PrepareCall fails (fail-secure path)

	prepareCalls int32 // atomically incremented per PrepareCall invocation
	mu           sync.Mutex
	gotCallID    uuid.UUID // the executionID PrepareCall was bound to (last call)
}

func (p *preparerTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: p.name}, nil
}

func (p *preparerTool) PrepareCall(_ context.Context, executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
	atomic.AddInt32(&p.prepareCalls, 1)
	p.mu.Lock()
	p.gotCallID = executionID
	p.mu.Unlock()
	if p.prepareErr != nil {
		return tool.Request{}, nil, p.prepareErr
	}
	// The token IS the executionID string: this is what binds the artifact to
	// the call, so preparation-time and execution-time reads must agree on it.
	return commandRequest(executionID, "git status", false), tool.TokenArtifact{Token: executionID.String()}, nil
}

func (p *preparerTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	prepared, ok := PreparedCallFromContext(ctx)
	if !ok {
		return tool.TextResult("error: no prepared execution contract in ctx"), nil
	}
	ta, ok := prepared.Artifact.(tool.TokenArtifact)
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

// Compile-time assertions: preparerTool satisfies the base + the mandatory
// preparation capability the seam exercises.
var (
	_ tool.InvokableTool = (*preparerTool)(nil)
	_ tool.CallPreparer  = (*preparerTool)(nil)
)

// approveActor spawns a fake actor that acks then approves the first gate
// (once-approval action). It returns the gateReg channel to wire into RunBatch. The actor
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
			Action:    gatedomain.ApprovalApprove,
		}
	}()
	return gateReg
}

// permissionEventCount returns how many PermissionRequested events fired.
func permissionEventCount(evs []event.Event) int {
	count := 0
	for _, ev := range evs {
		if _, ok := ev.(event.PermissionRequested); ok {
			count++
		}
	}
	return count
}

// preparerSeamRow is one table row for TestRunBatch_PreparerSeam.
type preparerSeamRow struct {
	name              string
	makeTool          func() tool.InvokableTool
	prepareErr        bool  // the row drives a PrepareCall failure (skip bound-callID check)
	wantPrepareCalls  int32 // expected PrepareCall invocations (0 for an unprepared tool)
	wantGate          bool  // a PermissionRequested must be emitted
	wantResultErr     bool  // the result is an error tool-result
	wantResultSubstr  string
	tokensMustBindCID bool // execution-time artifact token == ToolExecutionID
}

// TestRunBatch_PreparerSeam drives a CallPreparer tool through the combined
// interactive gate and asserts the seam's invariants for the happy path, plus
// the unprepared-tool row (fail closed, no gate) and the fail-secure
// prepare-error row (no gate, no exec, typed model-visible result).
func TestRunBatch_PreparerSeam(t *testing.T) {
	t.Parallel()

	tests := []preparerSeamRow{
		{
			name:              "preparer happy path: one PrepareCall, token binds to ToolExecutionID",
			makeTool:          func() tool.InvokableTool { return &preparerTool{name: "P"} },
			wantPrepareCalls:  1,
			wantGate:          true,
			tokensMustBindCID: true,
		},
		{
			name:             "unprepared tool: fail closed, no gate",
			makeTool:         func() tool.InvokableTool { return unpreparedRunTool{name: "P"} },
			wantPrepareCalls: 0,
			wantGate:         false,
			wantResultErr:    true,
			wantResultSubstr: "tool has no call preparation",
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
			ts := ToolSet{
				Access:               interactiveEvaluator(t, gatedomain.AccessGated, &recordingRuleWriter{}, &recordingIssuer{}),
				Registry:             []tool.InvokableTool{tl},
				MaxParallelToolCalls: 4,
			}
			emit, getEvents := collectEmit()
			gateReg := approveActor()

			results := withRunTimeout(t, func() []result {
				return RunBatch(context.Background(), []content.ToolUseBlock{call(t, "P", `{}`)}, ts, gateReg, uuid.New, emit)
			})

			if len(results) != 1 {
				t.Fatalf("len(results) = %d, want 1", len(results))
			}
			r := results[0]

			// PrepareCall call-count + binding (only the preparerTool tracks them).
			if pt, ok := tl.(*preparerTool); ok {
				if got := pt.calls(); got != tt.wantPrepareCalls {
					t.Errorf("PrepareCall called %d times, want %d", got, tt.wantPrepareCalls)
				}
				if tt.wantPrepareCalls > 0 && !tt.prepareErr && pt.boundCallID() != r.ToolExecutionID {
					t.Errorf("PrepareCall bound to callID %v, want ToolExecutionID %v", pt.boundCallID(), r.ToolExecutionID)
				}
			}

			nPerm := permissionEventCount(getEvents())
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
				if cid := r.ToolExecutionID.String(); execToken != cid {
					t.Errorf("execution-time token = %q, want ToolExecutionID %q", execToken, cid)
				}
			}

			if tt.prepareErr {
				// Fail-secure: the result must carry the typed PrepareCall error
				// verbatim (prefix + reason) and NOTHING else — the tool body never
				// ran, so no artifact/success output leaked through.
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
