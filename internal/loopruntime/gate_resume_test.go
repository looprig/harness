package loopruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// serveGateCloses answers every abandon request on gateReg with closeErr and
// records the gate ids it was asked to close.
func serveGateCloses(ctx context.Context, gateReg chan gateRegistration, closeErr error) <-chan gatedomain.ID {
	closed := make(chan gatedomain.ID, 8)
	go func() {
		for {
			select {
			case reg := <-gateReg:
				if !reg.abandonID.IsZero() {
					closed <- reg.abandonID
					reg.ack <- gateInstallAck{gateID: reg.abandonID, err: closeErr}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return closed
}

func restoredPermissionAdoption(callID uuid.UUID, gateID gatedomain.ID) *gateAdoption {
	request := tool.Request{ToolName: "A"}
	return &gateAdoption{gates: map[uuid.UUID]adoptableGate{
		callID: {id: gateID, kind: gatePermission, reply: make(chan command.Command, 1), request: &request},
	}}
}

// TestRunBatchClosesAnUnadoptedRestoredGateBeforeExecuting: a restored permission
// gate the access pass decides WITHOUT asking is durably closed before any call runs.
func TestRunBatchClosesAnUnadoptedRestoredGateBeforeExecuting(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &fakeRunTool{name: "A", output: "ra"}
	gateReg := make(chan gateRegistration)
	closed := serveGateCloses(ctx, gateReg, nil)
	callID, gateID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174001"), gatedomain.ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174002"))
	calls := []content.ToolUseBlock{call(t, "A", `{}`)}
	fault := &batchGateFault{}
	batchCtx := withBatchGateFault(withGateAdoption(ctx, restoredPermissionAdoption(callID, gateID)), fault)
	results := RunBatch(batchCtx, calls, ToolSet{Access: autoApproveGate{}, Registry: []tool.InvokableTool{a}, MaxParallelToolCalls: 1},
		BatchRuntime{GateRegistrations: gateReg, IDGen: uuid.New, executionIDs: map[string]uuid.UUID{calls[0].ID: callID}})
	select {
	case got := <-closed:
		if got != gateID {
			t.Fatalf("closed gate %v, want %v", got, gateID)
		}
	default:
		t.Fatal("the unadopted restored gate was not closed")
	}
	if atomic.LoadInt32(&a.totalRuns) != 1 || len(results) != 1 || results[0].IsError {
		t.Fatalf("runs=%d results=%+v, want the approved call to run once after the close", atomic.LoadInt32(&a.totalRuns), results)
	}
	if err := fault.failure(); err != nil {
		t.Fatalf("fault = %v, want none", err)
	}
}

// TestRunBatchExecutesNothingWhenARestoredGateCannotBeClosed: a gate that cannot be
// durably closed would be resumed by the next restore, so the batch runs nothing
// and the failure is recorded for the turn.
func TestRunBatchExecutesNothingWhenARestoredGateCannotBeClosed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &fakeRunTool{name: "A", output: "ra"}
	gateReg := make(chan gateRegistration)
	closeErr := errors.New("journal append failed")
	serveGateCloses(ctx, gateReg, closeErr)
	callID, gateID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174003"), gatedomain.ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174004"))
	calls := []content.ToolUseBlock{call(t, "A", `{}`)}
	fault := &batchGateFault{}
	batchCtx := withBatchGateFault(withGateAdoption(ctx, restoredPermissionAdoption(callID, gateID)), fault)
	RunBatch(batchCtx, calls, ToolSet{Access: autoApproveGate{}, Registry: []tool.InvokableTool{a}, MaxParallelToolCalls: 1},
		BatchRuntime{GateRegistrations: gateReg, IDGen: uuid.New, executionIDs: map[string]uuid.UUID{calls[0].ID: callID}})
	if atomic.LoadInt32(&a.totalRuns) != 0 {
		t.Fatalf("a call ran %d times while a restored gate stayed open", atomic.LoadInt32(&a.totalRuns))
	}
	var gateErr *GateCloseError
	if err := fault.failure(); !errors.As(err, &gateErr) || gateErr.GateID != gateID || !errors.Is(err, closeErr) {
		t.Fatalf("fault = %v, want the GateCloseError for %v", err, gateID)
	}
}
