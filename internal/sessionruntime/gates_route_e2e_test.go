package sessionruntime

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// gatedAccessSource reports Gated for every kind/scope, so each prepared
// requirement reaches the rule/approval stage.
type gatedAccessSource struct{}

func (gatedAccessSource) AccessVersion() uint16                   { return gatedomain.CurrentAccessVersion }
func (gatedAccessSource) AccessFor(string, string) (uint8, error) { return gatedomain.AccessGated, nil }

// orderedRuleWriter records persisted candidate batches with a shared sequence
// stamp so a test can prove persistence happened BEFORE execution.
type orderedRuleWriter struct {
	seq     *atomic.Int64
	mu      sync.Mutex
	batches [][]tool.RuleCandidate
	orders  []int64
}

func (w *orderedRuleWriter) WriteRules(_ context.Context, candidates []tool.RuleCandidate) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.batches = append(w.batches, append([]tool.RuleCandidate(nil), candidates...))
	w.orders = append(w.orders, w.seq.Add(1))
	return nil
}

func (w *orderedRuleWriter) snapshot() ([][]tool.RuleCandidate, []int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]tool.RuleCandidate(nil), w.batches...), append([]int64(nil), w.orders...)
}

// gatedE2ETool is an effectful CallPreparer tool: preparation yields one gated
// requirement carrying one reusable candidate; execution stamps the shared
// sequence so ordering against rule persistence is observable.
type gatedE2ETool struct {
	seq    *atomic.Int64
	mu     sync.Mutex
	runs   int
	orders []int64
}

func (g *gatedE2ETool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Gated", Desc: "d", Schema: []byte(`{"type":"object"}`)}, nil
}

func (g *gatedE2ETool) PrepareCall(_ context.Context, _ uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{
		ToolName: "Gated",
		Summary:  "do the gated thing",
		Requirements: []tool.Requirement{{
			Kind:        "tool.invoke",
			Scope:       "Gated",
			Match:       "do the gated thing",
			Description: "invoke Gated",
			Candidates: []tool.RuleCandidate{{
				Kind:        "tool.invoke",
				Match:       "Gated(*)",
				Description: "always allow Gated",
			}},
		}},
	}, nil, nil
}

func (g *gatedE2ETool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	g.mu.Lock()
	g.runs++
	g.orders = append(g.orders, g.seq.Add(1))
	g.mu.Unlock()
	return tool.TextResult("ran"), nil
}

func (g *gatedE2ETool) snapshot() (int, []int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runs, append([]int64(nil), g.orders...)
}

func gatedE2EDefinition(t *testing.T, gate loop.AccessGate, tl *gatedE2ETool) loop.Definition {
	t.Helper()
	return mustDefine(
		loop.WithName("agent"),
		loop.WithInference(&scriptedToolLLM{toolName: "Gated"}, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(tool.NewDefinition("Gated", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{tl}, nil
		})),
		loop.WithAccessGate(gate),
		loop.WithPolicyRevision("gated-e2e"),
		loop.WithDrainTimeout(200*time.Millisecond),
	)
}

// driveGateE2E submits one turn that calls the gated tool, answers the single
// opened gate with action (when non-empty), and drains to the turn terminal.
// It returns every enduring event observed.
func driveGateE2E(t *testing.T, s *Session, action string) []event.Event {
	t.Helper()
	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Submit(ctx, []content.Block{&content.TextBlock{Text: "go"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// The test session runs without a journal-backed hub tap for GateOpened
	// (the nop gate appender does not fan out), so the open gate is observed
	// through the public directory, exactly as a headless operator would.
	var seen []event.Event
	answered := false
	timeout := time.After(20 * time.Second)
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a terminal")
			}
			seen = append(seen, d.Event)
			switch d.Event.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return seen
			}
		case <-poll.C:
			open := s.ListGates(context.Background())
			if len(open) == 0 || answered {
				continue
			}
			if action == "" {
				t.Fatalf("gate opened unexpectedly: %+v", open[0])
			}
			answered = true
			if err := s.RespondGate(context.Background(), gatedomain.GateResponse{
				GateID: open[0].ID,
				Action: action,
				Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
			}); err != nil {
				t.Fatalf("RespondGate(%q): %v", action, err)
			}
		case <-timeout:
			t.Fatal("no terminal within deadline")
		}
	}
}

// TestSessionRouteApproveOnceExecutesAndWritesNothing drives the full session
// route: prepared request → interactive evaluator → one combined gate →
// RespondGate(Approve) → execution, with no rule persisted.
func TestSessionRouteApproveOnceExecutesAndWritesNothing(t *testing.T) {
	t.Parallel()
	seq := &atomic.Int64{}
	tl := &gatedE2ETool{seq: seq}
	writer := &orderedRuleWriter{seq: seq}
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), writer, nil)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}
	s, err := newTestSession(context.Background(), gatedE2EDefinition(t, evaluator, tl))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	driveGateE2E(t, s, string(gatedomain.ApprovalApprove))

	runs, _ := tl.snapshot()
	if runs != 1 {
		t.Errorf("tool runs = %d, want 1 (once approval executes)", runs)
	}
	if batches, _ := writer.snapshot(); len(batches) != 0 {
		t.Errorf("writer batches = %d, want 0 (once approval writes nothing)", len(batches))
	}
}

// TestSessionRouteApproveAlwaysPersistsCandidatesBeforeExecution proves the
// workspace approval persists the DISPLAYED candidates and does so BEFORE the
// tool executes.
func TestSessionRouteApproveAlwaysPersistsCandidatesBeforeExecution(t *testing.T) {
	t.Parallel()
	seq := &atomic.Int64{}
	tl := &gatedE2ETool{seq: seq}
	writer := &orderedRuleWriter{seq: seq}
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), writer, nil)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}
	s, err := newTestSession(context.Background(), gatedE2EDefinition(t, evaluator, tl))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	driveGateE2E(t, s, string(gatedomain.ApprovalApproveAlwaysWorkspace))

	runs, runOrders := tl.snapshot()
	batches, writeOrders := writer.snapshot()
	if runs != 1 {
		t.Fatalf("tool runs = %d, want 1", runs)
	}
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].Match != "Gated(*)" {
		t.Fatalf("writer batches = %+v, want the one displayed candidate", batches)
	}
	if len(writeOrders) != 1 || len(runOrders) != 1 || writeOrders[0] >= runOrders[0] {
		t.Errorf("persistence order = %v, execution order = %v: candidates must persist before execution", writeOrders, runOrders)
	}
}

// TestSessionRouteDenyRejectsWithoutExecution proves the Deny action fails the
// call closed: the tool never runs and nothing persists.
func TestSessionRouteDenyRejectsWithoutExecution(t *testing.T) {
	t.Parallel()
	seq := &atomic.Int64{}
	tl := &gatedE2ETool{seq: seq}
	writer := &orderedRuleWriter{seq: seq}
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), writer, nil)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}
	s, err := newTestSession(context.Background(), gatedE2EDefinition(t, evaluator, tl))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	seen := driveGateE2E(t, s, string(gatedomain.ApprovalDeny))

	runs, _ := tl.snapshot()
	if runs != 0 {
		t.Errorf("tool runs = %d, want 0 (deny rejects)", runs)
	}
	if batches, _ := writer.snapshot(); len(batches) != 0 {
		t.Errorf("writer batches = %d, want 0 (deny writes nothing)", len(batches))
	}
	requested := false
	for _, ev := range seen {
		if _, ok := ev.(event.PermissionRequested); ok {
			requested = true
		}
	}
	if !requested {
		t.Error("no PermissionRequested observed: the denied call never reached the interactive gate")
	}
	if open := s.ListGates(context.Background()); len(open) != 0 {
		t.Errorf("ListGates() after deny = %d gates, want 0 (gate resolved)", len(open))
	}
}

// TestSessionRouteHeadlessDeniesWithTypedDenial proves a headless session (no
// approver, no writer) never opens a gate: the unmet gated requirement resolves
// to the evaluator's typed approval-required denial and the call fails closed.
func TestSessionRouteHeadlessDeniesWithTypedDenial(t *testing.T) {
	t.Parallel()
	seq := &atomic.Int64{}
	tl := &gatedE2ETool{seq: seq}
	evaluator, err := gatedomain.NewHeadlessEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}}, nil, nil)
	if err != nil {
		t.Fatalf("NewHeadlessEvaluator: %v", err)
	}
	s, err := newTestSession(context.Background(), gatedE2EDefinition(t, evaluator, tl))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	seen := driveGateE2E(t, s, "")

	runs, _ := tl.snapshot()
	if runs != 0 {
		t.Errorf("tool runs = %d, want 0 (headless denies)", runs)
	}
	denied := false
	for _, ev := range seen {
		if decided, ok := ev.(event.PermissionDecided); ok {
			if decided.Effect == event.PermissionEffectDeny {
				denied = true
				if !strings.Contains(decided.Reason, "access") {
					t.Errorf("PermissionDecided.Reason = %q, want an access denial reason", decided.Reason)
				}
			}
		}
	}
	if !denied {
		t.Error("no deny PermissionDecided observed for the headless call")
	}
}
