package hustleruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
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
