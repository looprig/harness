package hustleruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

// observingEvidenceTool wraps preparedEvidenceTool and additionally
// implements tool.EvidenceObservation (design §13.4, TOCTOU), so tests can
// exercise the evidence runtime's recording call site without needing a real
// target-sensitive evidence tool (classifiers-side follow-up work).
type observingEvidenceTool struct {
	*preparedEvidenceTool
	target       string
	token        string
	report       bool
	observePanic bool
	observed     []tool.Request
}

func (t *observingEvidenceTool) ObservedRequirement(request tool.Request, _ *tool.ToolResult) (string, string, bool) {
	t.observed = append(t.observed, request)
	if t.observePanic {
		panic("observe-secret")
	}
	return t.target, t.token, t.report
}

func TestEvidenceRunnerRecordsWellFormedObservationFromReportingTool(t *testing.T) {
	t.Parallel()
	reporting := &observingEvidenceTool{
		preparedEvidenceTool: newPreparedEvidenceTool("workspace_read", "ok"),
		target:               "/workspace/file", token: "hash-1", report: true,
	}
	runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{mustRuntimeTestID(t)})
	collector := NewObservationCollector()
	ctx := WithObservationCollector(context.Background(), collector)

	_, err := runner.run(ctx,
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, reporting)},
		[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
		testSecurityCeiling,
	)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	got := collector.Observations()
	want := gate.ObservationRequirement{Target: "/workspace/file", Token: "hash-1"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Observations() = %+v, want [%+v]", got, want)
	}
	if len(reporting.observed) != 1 || reporting.observed[0].ToolName != "workspace_read" {
		t.Fatalf("ObservedRequirement called with = %+v, want the executed call's own request", reporting.observed)
	}
}

func TestEvidenceRunnerSkipsObservationForNonReportingTool(t *testing.T) {
	t.Parallel()
	plain := newPreparedEvidenceTool("workspace_read", "ok")
	runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{mustRuntimeTestID(t)})
	collector := NewObservationCollector()
	ctx := WithObservationCollector(context.Background(), collector)

	_, err := runner.run(ctx,
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, plain)},
		[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
		testSecurityCeiling,
	)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got := collector.Observations(); len(got) != 0 {
		t.Fatalf("Observations() = %+v, want none (tool implements no EvidenceObservation capability)", got)
	}
}

func TestEvidenceRunnerSkipsObservationWhenToolReportsNone(t *testing.T) {
	t.Parallel()
	silent := &observingEvidenceTool{
		preparedEvidenceTool: newPreparedEvidenceTool("workspace_read", "ok"),
		target:               "/workspace/file", token: "hash-1", report: false,
	}
	runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{mustRuntimeTestID(t)})
	collector := NewObservationCollector()
	ctx := WithObservationCollector(context.Background(), collector)

	_, err := runner.run(ctx,
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, silent)},
		[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
		testSecurityCeiling,
	)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got := collector.Observations(); len(got) != 0 {
		t.Fatalf("Observations() = %+v, want none (ok=false means no target-sensitive observation)", got)
	}
}

func TestEvidenceRunnerDropsMalformedObservation(t *testing.T) {
	t.Parallel()
	malformed := &observingEvidenceTool{
		preparedEvidenceTool: newPreparedEvidenceTool("workspace_read", "ok"),
		target:               "", token: "hash-1", report: true, // empty target fails gate.ObservationRequirement.Valid()
	}
	runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{mustRuntimeTestID(t)})
	collector := NewObservationCollector()
	ctx := WithObservationCollector(context.Background(), collector)

	_, err := runner.run(ctx,
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, malformed)},
		[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
		testSecurityCeiling,
	)
	if err != nil {
		t.Fatalf("run() error = %v, want the evidence call to still succeed (recording is additive, never load-bearing)", err)
	}
	if got := collector.Observations(); len(got) != 0 {
		t.Fatalf("Observations() = %+v, want none (malformed observation dropped, not recorded)", got)
	}
}

// TestEvidenceRunnerObservationPanicIsRecoveredAndCallStillSucceeds proves a
// panicking ObservedRequirement (a trusted-but-fallible tool implementation,
// exactly like a permission classifier's Applies/MarshalInput/ValidateResult
// — pkg/gate/reviewer.go's PermissionClassifierPanicError doc comment) never
// aborts the evidence call that already succeeded: the observation is simply
// dropped.
func TestEvidenceRunnerObservationPanicIsRecoveredAndCallStillSucceeds(t *testing.T) {
	t.Parallel()
	panicking := &observingEvidenceTool{
		preparedEvidenceTool: newPreparedEvidenceTool("workspace_read", "ok"),
		observePanic:         true,
	}
	runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{mustRuntimeTestID(t)})
	collector := NewObservationCollector()
	ctx := WithObservationCollector(context.Background(), collector)

	results, err := runner.run(ctx,
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, panicking)},
		[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
		testSecurityCeiling,
	)
	if err != nil || len(results) != 1 {
		t.Fatalf("run() = (%d,%v), want the evidence call to succeed despite the observation panic", len(results), err)
	}
	if got := collector.Observations(); len(got) != 0 {
		t.Fatalf("Observations() = %+v, want none", got)
	}
}

func TestEvidenceRunnerNoCollectorAttachedIsANoOp(t *testing.T) {
	t.Parallel()
	reporting := &observingEvidenceTool{
		preparedEvidenceTool: newPreparedEvidenceTool("workspace_read", "ok"),
		target:               "/workspace/file", token: "hash-1", report: true,
	}
	runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{mustRuntimeTestID(t)})

	// No WithObservationCollector call at all — the ordinary shape for every
	// non-review Hustle run today.
	_, err := runner.run(context.Background(),
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, reporting)},
		[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
		testSecurityCeiling,
	)
	if err != nil {
		t.Fatalf("run() error = %v, want no collector attached to still succeed", err)
	}
}

// TestRunAndFinalizeThreadsObservationCollectorThroughRealEvidenceExecution
// is a regression test for a genuine cross-cutting gap this addendum's own
// Carbon-side end-to-end test (carbon-permission-classifier's
// TestPermissionReviewObservationSymlinkSwapBlocksAutoApprovalEndToEnd)
// discovered: every OTHER test in this file attaches the
// ObservationCollector directly to the ctx passed straight into
// evidenceRunner.run (bypassing Controller.RunAndFinalize/executeWithEvidence
// entirely), which is not how a real classifier review attaches one
// (internal/sessionruntime/review_adapter.go's reviewOne calls
// WithObservationCollector on the ctx it hands to RunAndFinalize, several
// call frames above evidenceRunner.run). executeWithEvidence
// (execution.go) derives its per-attempt context via
// newEvidenceAttemptContext, which deliberately strips every ambient
// context value from its caller (see evidence_context.go's own doc comment,
// and TestEvidenceExecutionStripsAmbientContextAuthority above proving that
// isolation for an arbitrary key) — including, before execution.go's fix,
// the ObservationCollector itself, silently discarding every real
// target-sensitive evidence tool's recorded observation. This test drives
// the REAL RunAndFinalize -> executeWithEvidence -> evidenceCtx path (no
// shortcut) and proves the collector attached at the OUTER ctx, exactly
// where reviewOne attaches it, still receives the observation a real
// evidence call records.
func TestRunAndFinalizeThreadsObservationCollectorThroughRealEvidenceExecution(t *testing.T) {
	t.Parallel()

	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	reporting := &observingEvidenceTool{
		preparedEvidenceTool: newPreparedEvidenceTool("workspace_read", "ok"),
		target:               "/workspace/file", token: "hash-real-path", report: true,
	}
	invocation := 0
	client := &runtimeTestClient{invoke: func(_ context.Context, _ inference.Request) (*inference.Response, error) {
		invocation++
		if invocation == 1 {
			return oneEvidenceCallResponse("call-real-path"), nil
		}
		return terminalEvidenceResponse(`{"summary":"allow"}`, nil), nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(_ context.Context, _ tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{reporting}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	request := runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID)

	// Attach the collector to the OUTER ctx passed to RunAndFinalize — the
	// exact shape review_adapter.go's reviewOne uses in production — never
	// directly to evidenceRunner.run the way every other test in this file
	// does.
	collector := NewObservationCollector()
	runCtx := WithObservationCollector(context.Background(), collector)
	err := controller.RunAndFinalize(runCtx, request, func(_ context.Context, _ hustle.Result) error {
		return nil
	}, noOpFinalizer)
	if err != nil {
		t.Fatal(err)
	}

	got := collector.Observations()
	want := gate.ObservationRequirement{Target: "/workspace/file", Token: "hash-real-path"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Observations() = %+v, want [%+v] (collector attached at RunAndFinalize's own ctx, exactly like review_adapter.go, must survive the real evidence-execution path)", got, want)
	}
}

func TestObservationCollectorBoundsCount(t *testing.T) {
	t.Parallel()
	collector := NewObservationCollector()
	for i := 0; i < MaxObservationsPerRun+5; i++ {
		collector.record(gate.ObservationRequirement{Target: "/workspace/file", Token: "tok"})
	}
	if got := len(collector.Observations()); got != MaxObservationsPerRun {
		t.Fatalf("Observations() length = %d, want capped at %d", got, MaxObservationsPerRun)
	}
}

func TestObservationCollectorNilReceiverIsSafe(t *testing.T) {
	t.Parallel()
	var collector *ObservationCollector
	collector.record(gate.ObservationRequirement{Target: "/workspace/file", Token: "tok"})
	if got := collector.Observations(); got != nil {
		t.Fatalf("Observations() on nil collector = %+v, want nil", got)
	}
}

func TestWithObservationCollectorNilIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := WithObservationCollector(context.Background(), nil)
	if got := observationCollectorFromContext(ctx); got != nil {
		t.Fatalf("observationCollectorFromContext() = %v, want nil", got)
	}
}
