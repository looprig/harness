package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeRunTool is the minimal configurable InvokableTool for runner tests: it
// implements ONLY BaseTool + InvokableTool, NONE of the optional capability
// interfaces. Capabilities are added by the embedding wrapper types below
// (sequentialTool, auditTool, promptTool, writeKeyTool), so each test opts into
// exactly the capabilities it needs — exactly as the runner probes via type
// assertion. It records concurrency and ordering so tests can assert
// serialization/parallelism, and can be made to panic, error, or return empty.
type fakeRunTool struct {
	name string

	// run hooks
	panicMsg string        // non-empty → panic with this message
	runErr   error         // non-nil → InvokableRun returns (nil, runErr)
	empty    bool          // true → return a ToolResult with no Content
	output   string        // text content of the result on success
	delay    time.Duration // sleep inside run to widen the concurrency window

	// capability config consumed by the wrapper types (not by fakeRunTool itself).
	sequential bool
	auditFn    func(argsJSON string) string
	promptFn   func(argsJSON string) (tool.PermissionRequest, error)
	writeFn    func(argsJSON string) (string, bool, error)

	// observed state
	mu        sync.Mutex
	live      int32 // current concurrent runs
	maxLive   int32 // peak concurrent runs
	starts    []time.Time
	finishes  []time.Time
	totalRuns int32
}

func (f *fakeRunTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: f.name}, nil
}

func (f *fakeRunTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	atomic.AddInt32(&f.totalRuns, 1)
	cur := atomic.AddInt32(&f.live, 1)
	for {
		old := atomic.LoadInt32(&f.maxLive)
		if cur <= old || atomic.CompareAndSwapInt32(&f.maxLive, old, cur) {
			break
		}
	}
	f.mu.Lock()
	f.starts = append(f.starts, time.Now())
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.finishes = append(f.finishes, time.Now())
		f.mu.Unlock()
		atomic.AddInt32(&f.live, -1)
	}()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.empty {
		return &tool.ToolResult{}, nil
	}
	return tool.TextResult(f.output), nil
}

// peakLive returns the recorded peak concurrency.
func (f *fakeRunTool) peakLive() int32 { return atomic.LoadInt32(&f.maxLive) }

// sequentialTool adds the Sequential capability.
type sequentialTool struct{ *fakeRunTool }

func (s sequentialTool) Sequential() bool { return s.fakeRunTool.sequential }

// auditTool adds the Auditable capability.
type auditTool struct{ *fakeRunTool }

func (a auditTool) AuditSummary(argsJSON string) string { return a.fakeRunTool.auditFn(argsJSON) }

// promptTool adds the PermissionPrompter capability.
type promptTool struct{ *fakeRunTool }

func (p promptTool) BuildRequest(argsJSON string) (tool.PermissionRequest, error) {
	return p.fakeRunTool.promptFn(argsJSON)
}

// writeKeyTool adds the WriteTarget capability.
type writeKeyTool struct{ *fakeRunTool }

func (w writeKeyTool) WriteTarget(argsJSON string) (string, bool, error) {
	return w.fakeRunTool.writeFn(argsJSON)
}

// Compile-time assertions that fakeRunTool implements ONLY the base interface and
// the wrappers each add exactly one optional capability.
var (
	_ tool.InvokableTool      = (*fakeRunTool)(nil)
	_ tool.Sequential         = sequentialTool{}
	_ tool.Auditable          = auditTool{}
	_ tool.PermissionPrompter = promptTool{}
	_ tool.WriteTarget        = writeKeyTool{}
)

// fakePermissionGate is a configurable PermissionGate. checkFn returns the Effect
// per call; grantErr (if set) makes Grant fail. It records grant calls.
type fakePermissionGate struct {
	checkFn  func(name, argsJSON string) Effect
	grantErr error

	mu         sync.Mutex
	grants     []grantRecord
	checkCalls []string
	granted    map[string]bool // name → granted (for session-grant visibility)
}

type grantRecord struct {
	name  string
	args  string
	scope tool.ApprovalScope
}

func (g *fakePermissionGate) Check(ctx context.Context, t tool.InvokableTool, name, argsJSON string) Effect {
	g.mu.Lock()
	g.checkCalls = append(g.checkCalls, name)
	g.mu.Unlock()
	return g.checkFn(name, argsJSON)
}

func (g *fakePermissionGate) Grant(ctx context.Context, name, argsJSON string, scope tool.ApprovalScope) error {
	g.mu.Lock()
	g.grants = append(g.grants, grantRecord{name: name, args: argsJSON, scope: scope})
	if g.granted == nil {
		g.granted = map[string]bool{}
	}
	g.granted[name] = true
	g.mu.Unlock()
	return g.grantErr
}

// autoApproveGate approves everything (the common case for tests that don't
// exercise permission).
type autoApproveGate struct{}

func (autoApproveGate) Check(ctx context.Context, t tool.InvokableTool, name, argsJSON string) Effect {
	return EffectAutoApprove
}
func (autoApproveGate) Grant(ctx context.Context, name, argsJSON string, scope tool.ApprovalScope) error {
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// collectEmit returns an emit func backed by a synchronized slice and a getter.
func collectEmit() (func(event.Event), func() []event.Event) {
	var mu sync.Mutex
	var evs []event.Event
	emit := func(ev event.Event) {
		mu.Lock()
		evs = append(evs, ev)
		mu.Unlock()
	}
	get := func() []event.Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]event.Event, len(evs))
		copy(out, evs)
		return out
	}
	return emit, get
}

// call builds a ToolUseBlock with a fresh ID.
func call(t *testing.T, name, args string) content.ToolUseBlock {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return content.ToolUseBlock{ID: id.String(), Name: name, Input: json.RawMessage(args)}
}

// runBatchNoGate runs RunBatch with a gateReg channel that is never used (no
// EffectAsk in the scenario) — a closed-on-cancel pattern is unnecessary. It uses
// the real uuid.New idGen seam (the production default).
func runBatchNoGate(ctx context.Context, calls []content.ToolUseBlock, ts ToolSet, emit func(event.Event)) []result {
	gateReg := make(chan gateRegistration)
	return RunBatch(ctx, calls, ts, gateReg, uuid.New, emit)
}

// resultText returns the flattened text of a result.
func resultText(r result) string { return flattenToText(r.Content) }

// countEvents counts ToolCallStarted and ToolCallCompleted in order, returning
// the index of the last Started and the first Completed.
func startedCompletedOrder(evs []event.Event) (lastStarted, firstCompleted, nStarted, nCompleted int) {
	lastStarted, firstCompleted = -1, -1
	for i, ev := range evs {
		switch ev.(type) {
		case event.ToolCallStarted:
			lastStarted = i
			nStarted++
		case event.ToolCallCompleted:
			if firstCompleted == -1 {
				firstCompleted = i
			}
			nCompleted++
		}
	}
	return
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRunBatch_UnknownTool: a call to a name not in the registry yields exactly
// one Started + one Completed{IsError} and an error tool-result; other calls run.
func TestRunBatch_UnknownTool(t *testing.T) {
	t.Parallel()
	good := &fakeRunTool{name: "Good", output: "did it"}
	ts := ToolSet{
		Permission:           autoApproveGate{},
		Registry:             []tool.InvokableTool{good},
		MaxParallelToolCalls: 4,
	}
	emit, getEvents := collectEmit()
	calls := []content.ToolUseBlock{
		call(t, "Nope", `{}`),
		call(t, "Good", `{}`),
	}
	results := runBatchNoGate(context.Background(), calls, ts, emit)

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if !results[0].IsError || !strings.Contains(resultText(results[0]), "unknown tool: Nope") {
		t.Errorf("unknown result = %+v / %q, want error mentioning unknown tool", results[0], resultText(results[0]))
	}
	if results[1].IsError || !strings.Contains(resultText(results[1]), "did it") {
		t.Errorf("good result = %+v / %q, want success", results[1], resultText(results[1]))
	}
	if atomic.LoadInt32(&good.totalRuns) != 1 {
		t.Errorf("good ran %d times, want 1 (batch must continue past unknown)", good.totalRuns)
	}
	_, _, nStarted, nCompleted := startedCompletedOrder(getEvents())
	if nStarted != 2 || nCompleted != 2 {
		t.Errorf("events: %d started / %d completed, want 2/2", nStarted, nCompleted)
	}
}

// TestRunBatch_ResultOrderMatchesCalls: results come back in call order and carry
// the originating ToolUseBlock.ID.
func TestRunBatch_ResultOrderMatchesCalls(t *testing.T) {
	t.Parallel()
	a := &fakeRunTool{name: "A", output: "ra"}
	b := &fakeRunTool{name: "B", output: "rb"}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{a, b}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	calls := []content.ToolUseBlock{call(t, "B", `{}`), call(t, "A", `{}`)}
	results := runBatchNoGate(context.Background(), calls, ts, emit)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].ToolExecutionID == (uuid.UUID{}) || results[0].ToolUseID != calls[0].ID {
		t.Errorf("results[0].ToolUseID = %q, want %q", results[0].ToolUseID, calls[0].ID)
	}
	if results[1].ToolUseID != calls[1].ID {
		t.Errorf("results[1].ToolUseID = %q, want %q", results[1].ToolUseID, calls[1].ID)
	}
	if !strings.Contains(resultText(results[0]), "rb") || !strings.Contains(resultText(results[1]), "ra") {
		t.Errorf("results out of order: %q, %q", resultText(results[0]), resultText(results[1]))
	}
}

// TestRunBatch_ToolExecutionIDBindsToProviderToolUseID locks the two-id tool model:
// RunBatch mints one internal ToolExecutionID per call (unique, non-zero, ours) and
// binds it 1:1 to the model's provider ToolUseID (the ToolUseBlock.ID). The result
// pairs back to the model ONLY by the provider ToolUseID — a ToolResultMessage built
// from a result carries that provider id, never our internal ToolExecutionID.
func TestRunBatch_ToolExecutionIDBindsToProviderToolUseID(t *testing.T) {
	t.Parallel()
	a := &fakeRunTool{name: "A", output: "ra"}
	b := &fakeRunTool{name: "B", output: "rb"}
	c := &fakeRunTool{name: "C", output: "rc"}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{a, b, c}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	calls := []content.ToolUseBlock{call(t, "A", `{}`), call(t, "B", `{}`), call(t, "C", `{}`)}
	results := runBatchNoGate(context.Background(), calls, ts, emit)
	if len(results) != len(calls) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(calls))
	}
	seenExec := make(map[uuid.UUID]bool)
	seenUse := make(map[string]bool)
	for i, r := range results {
		// Our minted handle: present, and unique across the batch (1:1, never reused).
		if r.ToolExecutionID.IsZero() {
			t.Errorf("results[%d]: ToolExecutionID is zero (must be minted)", i)
		}
		if seenExec[r.ToolExecutionID] {
			t.Errorf("results[%d]: ToolExecutionID %v reused — not 1:1", i, r.ToolExecutionID)
		}
		seenExec[r.ToolExecutionID] = true
		// Bound 1:1 to the model's provider ToolUseID for THIS call (call order).
		if r.ToolUseID != calls[i].ID {
			t.Errorf("results[%d]: ToolUseID = %q, want %q (provider id for this call)", i, r.ToolUseID, calls[i].ID)
		}
		if seenUse[r.ToolUseID] {
			t.Errorf("results[%d]: ToolUseID %q reused — not 1:1", i, r.ToolUseID)
		}
		seenUse[r.ToolUseID] = true
		// The result pairs back to the model by the provider ToolUseID only.
		if got := toolResultMessage(r).ToolUseID; got != calls[i].ID {
			t.Errorf("results[%d]: ToolResultMessage.ToolUseID = %q, want provider id %q", i, got, calls[i].ID)
		}
	}
}

// TestRunBatch_InvalidJSONArgs: a call whose Input is not valid JSON is a
// pre-execution failure (no panic, error result + event pair).
func TestRunBatch_InvalidJSONArgs(t *testing.T) {
	t.Parallel()
	good := &fakeRunTool{name: "Good", output: "ok"}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{good}, MaxParallelToolCalls: 4}
	emit, getEvents := collectEmit()
	calls := []content.ToolUseBlock{call(t, "Good", `{not json`)}
	results := runBatchNoGate(context.Background(), calls, ts, emit)
	if !results[0].IsError || !strings.Contains(resultText(results[0]), "invalid tool arguments") {
		t.Errorf("result = %+v / %q, want invalid-args error", results[0], resultText(results[0]))
	}
	if atomic.LoadInt32(&good.totalRuns) != 0 {
		t.Errorf("tool ran %d times, want 0 for invalid args", good.totalRuns)
	}
	_, _, nStarted, nCompleted := startedCompletedOrder(getEvents())
	if nStarted != 1 || nCompleted != 1 {
		t.Errorf("events: %d/%d, want 1/1", nStarted, nCompleted)
	}
}

// TestRunBatch_SequentialDrainsFirst: a Sequential tool must fully finish before
// any parallel tool starts.
func TestRunBatch_SequentialDrainsFirst(t *testing.T) {
	t.Parallel()
	seqInner := &fakeRunTool{name: "Seq", sequential: true, output: "s", delay: 30 * time.Millisecond}
	seq := sequentialTool{seqInner}
	par := &fakeRunTool{name: "Par", output: "p", delay: 5 * time.Millisecond}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{seq, par}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	calls := []content.ToolUseBlock{call(t, "Par", `{}`), call(t, "Seq", `{}`), call(t, "Par", `{}`)}
	runBatchNoGate(context.Background(), calls, ts, emit)

	seqInner.mu.Lock()
	seqFinish := seqInner.finishes[len(seqInner.finishes)-1]
	seqInner.mu.Unlock()
	par.mu.Lock()
	parStarts := append([]time.Time(nil), par.starts...)
	par.mu.Unlock()
	for i, ps := range parStarts {
		if ps.Before(seqFinish) {
			t.Errorf("parallel start[%d] %v began before sequential finished %v", i, ps, seqFinish)
		}
	}
}

// TestRunBatch_SessionGrantVisibility: a gate that asks for call 1 then (after a
// Grant) auto-approves call 2 — call 2 must not be prompted.
func TestRunBatch_SessionGrantVisibility(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	pt := promptTool{fakeRunTool: tl}
	tl.promptFn = func(argsJSON string) (tool.PermissionRequest, error) {
		return tool.UnknownRequest{Tool: "T", Summary: "do"}, nil
	}
	gate := &fakePermissionGate{}
	gate.checkFn = func(name, argsJSON string) Effect {
		gate.mu.Lock()
		granted := gate.granted["T"]
		gate.mu.Unlock()
		if granted {
			return EffectAutoApprove
		}
		return EffectAsk
	}
	ts := ToolSet{Permission: gate, Registry: []tool.InvokableTool{pt}, MaxParallelToolCalls: 4}
	emit, getEvents := collectEmit()

	gateReg := make(chan gateRegistration)
	// Fake actor: approve the single permission gate with ScopeSession.
	go func() {
		reg := <-gateReg
		close(reg.ack)
		reg.reply <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Scope: tool.ScopeSession}
	}()

	calls := []content.ToolUseBlock{call(t, "T", `{"n":1}`), call(t, "T", `{"n":2}`)}
	results := RunBatch(context.Background(), calls, ts, gateReg, uuid.New, emit)

	if len(results) != 2 || results[0].IsError || results[1].IsError {
		t.Fatalf("results = %+v, want 2 successes", results)
	}
	// Exactly one PermissionRequested event (only call 1 prompted).
	var nPerm int
	for _, ev := range getEvents() {
		if _, ok := ev.(event.PermissionRequested); ok {
			nPerm++
		}
	}
	if nPerm != 1 {
		t.Errorf("PermissionRequested count = %d, want 1 (call 2 must be auto-approved by the session grant)", nPerm)
	}
	gate.mu.Lock()
	nGrants := len(gate.grants)
	gate.mu.Unlock()
	if nGrants != 1 || gate.grants[0].scope != tool.ScopeSession {
		t.Errorf("grants = %+v, want 1 ScopeSession", gate.grants)
	}
}

// TestRunBatch_MaxParallelCap: with MaxParallelToolCalls=2 and 6 calls, peak
// concurrency must never exceed 2.
func TestRunBatch_MaxParallelCap(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "P", output: "ok", delay: 20 * time.Millisecond}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, _ := collectEmit()
	var calls []content.ToolUseBlock
	for i := 0; i < 6; i++ {
		calls = append(calls, call(t, "P", `{}`))
	}
	runBatchNoGate(context.Background(), calls, ts, emit)
	if got := tl.peakLive(); got > 2 {
		t.Errorf("peak concurrency = %d, want <= 2", got)
	}
	if atomic.LoadInt32(&tl.totalRuns) != 6 {
		t.Errorf("totalRuns = %d, want 6", tl.totalRuns)
	}
}

// TestRunBatch_SameWriteTargetSerializes: two calls sharing a WriteTarget key run
// serially (no overlap); different keys overlap.
func TestRunBatch_SameWriteTargetSerializes(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "W", output: "ok", delay: 25 * time.Millisecond}
	wt := writeKeyTool{fakeRunTool: tl}
	tl.writeFn = func(argsJSON string) (string, bool, error) {
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", false, err
		}
		return a.Path, true, nil
	}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{wt}, MaxParallelToolCalls: 8}
	emit, _ := collectEmit()
	// Two calls to /same, two to different paths.
	calls := []content.ToolUseBlock{
		call(t, "W", `{"path":"/same"}`),
		call(t, "W", `{"path":"/same"}`),
	}
	runBatchNoGate(context.Background(), calls, ts, emit)

	// Same-key serialization → peak concurrency for /same must be 1.
	if got := tl.peakLive(); got > 1 {
		t.Errorf("same-WriteTarget peak concurrency = %d, want 1 (serialized)", got)
	}

	// Different keys → they can overlap. Fresh tool to reset counters.
	tl2 := &fakeRunTool{name: "W", output: "ok", delay: 25 * time.Millisecond}
	wt2 := writeKeyTool{fakeRunTool: tl2}
	tl2.writeFn = tl.writeFn
	ts2 := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{wt2}, MaxParallelToolCalls: 8}
	calls2 := []content.ToolUseBlock{
		call(t, "W", `{"path":"/a"}`),
		call(t, "W", `{"path":"/b"}`),
	}
	runBatchNoGate(context.Background(), calls2, ts2, emit)
	if got := tl2.peakLive(); got < 2 {
		t.Errorf("different-WriteTarget peak concurrency = %d, want 2 (parallel)", got)
	}
}

// TestRunBatch_WriteTargetError: a WriteTarget that errors is a pre-execution
// failure (not executed, error result + event pair).
func TestRunBatch_WriteTargetError(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "W", output: "ok"}
	wt := writeKeyTool{fakeRunTool: tl}
	tl.writeFn = func(argsJSON string) (string, bool, error) {
		return "", false, errors.New("bad target")
	}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{wt}, MaxParallelToolCalls: 4}
	emit, getEvents := collectEmit()
	calls := []content.ToolUseBlock{call(t, "W", `{}`)}
	results := runBatchNoGate(context.Background(), calls, ts, emit)
	if !results[0].IsError {
		t.Errorf("result = %+v, want IsError", results[0])
	}
	if atomic.LoadInt32(&tl.totalRuns) != 0 {
		t.Errorf("tool ran %d times, want 0 for WriteTarget error", tl.totalRuns)
	}
	_, _, nStarted, nCompleted := startedCompletedOrder(getEvents())
	if nStarted != 1 || nCompleted != 1 {
		t.Errorf("events: %d/%d, want 1/1", nStarted, nCompleted)
	}
}

// TestRunBatch_PanicRecovered: a panicking tool yields an error result, not a
// crash; sibling calls still complete.
func TestRunBatch_PanicRecovered(t *testing.T) {
	t.Parallel()
	boom := &fakeRunTool{name: "Boom", panicMsg: "kaboom"}
	ok := &fakeRunTool{name: "OK", output: "fine"}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{boom, ok}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	calls := []content.ToolUseBlock{call(t, "Boom", `{}`), call(t, "OK", `{}`)}
	results := runBatchNoGate(context.Background(), calls, ts, emit)
	if !results[0].IsError || !strings.Contains(resultText(results[0]), "panicked") {
		t.Errorf("panic result = %+v / %q, want panic error", results[0], resultText(results[0]))
	}
	if results[1].IsError || !strings.Contains(resultText(results[1]), "fine") {
		t.Errorf("sibling result = %+v, want success", results[1])
	}
}

// TestRunBatch_SummaryHasNoSecret: ToolCallStarted.Summary comes from Auditable
// and must not contain the raw arg secret.
func TestRunBatch_SummaryHasNoSecret(t *testing.T) {
	t.Parallel()
	const secret = "SECRET_TOKEN_abc123"
	inner := &fakeRunTool{name: "Audit", output: "ok"}
	inner.auditFn = func(argsJSON string) string { return "Audit /safe/path" }
	tl := auditTool{inner}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 4}
	emit, getEvents := collectEmit()
	calls := []content.ToolUseBlock{call(t, "Audit", `{"token":"`+secret+`"}`)}
	runBatchNoGate(context.Background(), calls, ts, emit)
	var sawStarted bool
	for _, ev := range getEvents() {
		if s, ok := ev.(event.ToolCallStarted); ok {
			sawStarted = true
			if strings.Contains(s.Summary, secret) {
				t.Errorf("SECURITY: Summary %q leaks secret", s.Summary)
			}
			if s.Summary != "Audit /safe/path" {
				t.Errorf("Summary = %q, want audit summary", s.Summary)
			}
		}
	}
	if !sawStarted {
		t.Fatal("no ToolCallStarted emitted")
	}
}

// TestRunBatch_AuditTolratesInvalidJSON: when args are invalid JSON, the Summary
// path must still yield (at least) the tool name and never crash.
func TestRunBatch_AuditToleratesInvalidJSON(t *testing.T) {
	t.Parallel()
	inner := &fakeRunTool{name: "Audit", output: "ok"}
	inner.auditFn = func(argsJSON string) string {
		// A real Auditable tolerates invalid JSON by yielding just the name.
		if !json.Valid([]byte(argsJSON)) {
			return "Audit"
		}
		return "Audit summary"
	}
	tl := auditTool{inner}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 4}
	emit, getEvents := collectEmit()
	calls := []content.ToolUseBlock{call(t, "Audit", `{bad`)}
	runBatchNoGate(context.Background(), calls, ts, emit)
	var summary string
	for _, ev := range getEvents() {
		if s, ok := ev.(event.ToolCallStarted); ok {
			summary = s.Summary
		}
	}
	if summary != "Audit" {
		t.Errorf("Summary for invalid args = %q, want %q", summary, "Audit")
	}
}

// TestRunBatch_AllStartedBeforeAnyCompleted: every ToolCallStarted precedes every
// ToolCallCompleted, even for a batch larger than MaxParallelToolCalls.
func TestRunBatch_AllStartedBeforeAnyCompleted(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "P", output: "ok", delay: 5 * time.Millisecond}
	unknown := "Ghost"
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, getEvents := collectEmit()
	var calls []content.ToolUseBlock
	for i := 0; i < 5; i++ {
		calls = append(calls, call(t, "P", `{}`))
	}
	calls = append(calls, call(t, unknown, `{}`)) // include a pre-exec failure
	RunBatch(context.Background(), calls, ts, make(chan gateRegistration), uuid.New, emit)

	evs := getEvents()
	lastStarted, firstCompleted, nStarted, nCompleted := startedCompletedOrder(evs)
	if nStarted != 6 || nCompleted != 6 {
		t.Fatalf("events: %d started / %d completed, want 6/6", nStarted, nCompleted)
	}
	if lastStarted >= firstCompleted {
		t.Errorf("last Started index %d must be < first Completed index %d", lastStarted, firstCompleted)
	}
}

// TestRunBatch_FailureVisibility: every pre-execution failure mode yields exactly
// one Started + one Completed{IsError} and one error tool-result.
func TestRunBatch_FailureVisibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T) (ToolSet, content.ToolUseBlock, chan gateRegistration)
		wantInErr string
	}{
		{
			name: "unknown tool",
			setup: func(t *testing.T) (ToolSet, content.ToolUseBlock, chan gateRegistration) {
				ts := ToolSet{Permission: autoApproveGate{}, Registry: nil, MaxParallelToolCalls: 2}
				return ts, call(t, "Nope", `{}`), make(chan gateRegistration)
			},
			wantInErr: "unknown tool",
		},
		{
			name: "invalid json args",
			setup: func(t *testing.T) (ToolSet, content.ToolUseBlock, chan gateRegistration) {
				tl := &fakeRunTool{name: "T", output: "ok"}
				ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
				return ts, call(t, "T", `{bad`), make(chan gateRegistration)
			},
			wantInErr: "invalid tool arguments",
		},
		{
			name: "permission denied via Check",
			setup: func(t *testing.T) (ToolSet, content.ToolUseBlock, chan gateRegistration) {
				tl := &fakeRunTool{name: "T", output: "ok"}
				gate := &fakePermissionGate{checkFn: func(name, args string) Effect { return EffectDeny }}
				ts := ToolSet{Permission: gate, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
				return ts, call(t, "T", `{}`), make(chan gateRegistration)
			},
			wantInErr: "permission denied",
		},
		{
			name: "permission denied via gate Deny",
			setup: func(t *testing.T) (ToolSet, content.ToolUseBlock, chan gateRegistration) {
				tl := &fakeRunTool{name: "T", output: "ok"}
				pt := promptTool{fakeRunTool: tl}
				tl.promptFn = func(string) (tool.PermissionRequest, error) {
					return tool.UnknownRequest{Tool: "T", Summary: "x"}, nil
				}
				gate := &fakePermissionGate{checkFn: func(name, args string) Effect { return EffectAsk }}
				ts := ToolSet{Permission: gate, Registry: []tool.InvokableTool{pt}, MaxParallelToolCalls: 2}
				gateReg := make(chan gateRegistration)
				go func() {
					reg := <-gateReg
					close(reg.ack)
					reg.reply <- command.DenyToolCall{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}}
				}()
				return ts, call(t, "T", `{}`), gateReg
			},
			wantInErr: "permission denied",
		},
		{
			name: "WriteTarget error",
			setup: func(t *testing.T) (ToolSet, content.ToolUseBlock, chan gateRegistration) {
				tl := &fakeRunTool{name: "W", output: "ok"}
				wt := writeKeyTool{fakeRunTool: tl}
				tl.writeFn = func(string) (string, bool, error) { return "", false, errors.New("bad target") }
				ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{wt}, MaxParallelToolCalls: 2}
				return ts, call(t, "W", `{}`), make(chan gateRegistration)
			},
			// Assert the specific WriteTarget-failure message (prefix + cause), not
			// just an "error:" substring, so a regression in the message is caught.
			wantInErr: "invalid tool arguments: bad target",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts, c, gateReg := tt.setup(t)
			emit, getEvents := collectEmit()
			results := RunBatch(context.Background(), []content.ToolUseBlock{c}, ts, gateReg, uuid.New, emit)

			if len(results) != 1 {
				t.Fatalf("len(results) = %d, want 1", len(results))
			}
			if !results[0].IsError {
				t.Errorf("result.IsError = false, want true")
			}
			if !strings.Contains(resultText(results[0]), tt.wantInErr) {
				t.Errorf("result text %q does not contain %q", resultText(results[0]), tt.wantInErr)
			}
			var nStarted, nCompletedErr int
			for _, ev := range getEvents() {
				switch e := ev.(type) {
				case event.ToolCallStarted:
					nStarted++
				case event.ToolCallCompleted:
					if e.IsError {
						nCompletedErr++
					}
				}
			}
			if nStarted != 1 {
				t.Errorf("ToolCallStarted count = %d, want 1", nStarted)
			}
			if nCompletedErr != 1 {
				t.Errorf("ToolCallCompleted{IsError} count = %d, want 1", nCompletedErr)
			}
		})
	}
}

// TestRunBatch_IDGenFailure: a call whose ToolExecutionID cannot be minted (idGen returns
// an error) is a fail-secure pre-execution failure — it is NOT executed, NO gate
// is opened for it, yet it still gets exactly one Started + one Completed{IsError}
// + one error tool-result. Sibling calls with a working idGen still run and pair.
func TestRunBatch_IDGenFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failMints map[int]bool // 0-based mint index that should error
		nCalls    int
		wantRuns  int32 // expected total tool runs across the batch
	}{
		{
			name:      "first call id-gen fails, sibling runs",
			failMints: map[int]bool{0: true},
			nCalls:    2,
			wantRuns:  1, // only the second (working) call executes
		},
		{
			name:      "all id-gen fails, nothing runs",
			failMints: map[int]bool{0: true, 1: true},
			nCalls:    2,
			wantRuns:  0,
		},
		{
			name:      "middle call id-gen fails, siblings run",
			failMints: map[int]bool{1: true},
			nCalls:    3,
			wantRuns:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tl := &fakeRunTool{name: "T", output: "ran"}
			// A gate that records whether it is ever consulted: a failed-mint call
			// must never reach Check (no gate opened for a call with no ToolExecutionID).
			gate := &fakePermissionGate{checkFn: func(name, args string) Effect { return EffectAutoApprove }}
			ts := ToolSet{Permission: gate, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 4}

			emit, getEvents := collectEmit()

			var mintCalls int32
			idGen := func() (uuid.UUID, error) {
				i := int(atomic.AddInt32(&mintCalls, 1)) - 1
				if tt.failMints[i] {
					return uuid.UUID{}, errors.New("rand source exhausted")
				}
				return uuid.New()
			}

			var calls []content.ToolUseBlock
			for i := 0; i < tt.nCalls; i++ {
				calls = append(calls, call(t, "T", `{}`))
			}
			results := RunBatch(context.Background(), calls, ts, make(chan gateRegistration), idGen, emit)

			if len(results) != tt.nCalls {
				t.Fatalf("len(results) = %d, want %d", len(results), tt.nCalls)
			}
			// Each failed-mint call is an error result with the internal id message
			// and pairs with its originating ToolUseBlock by index; working calls
			// succeed. Results stay in call order regardless of mint outcome.
			for i := range results {
				if tt.failMints[i] {
					if !results[i].IsError {
						t.Errorf("results[%d].IsError = false, want true (id-gen failed)", i)
					}
					if !strings.Contains(resultText(results[i]), "could not generate call id") {
						t.Errorf("results[%d] text = %q, want internal id-gen error", i, resultText(results[i]))
					}
				} else {
					if results[i].IsError || !strings.Contains(resultText(results[i]), "ran") {
						t.Errorf("results[%d] = %+v / %q, want success", i, results[i], resultText(results[i]))
					}
				}
				if results[i].ToolUseID != calls[i].ID {
					t.Errorf("results[%d].ToolUseID = %q, want %q (pairs by index)", i, results[i].ToolUseID, calls[i].ID)
				}
			}

			if got := atomic.LoadInt32(&tl.totalRuns); got != tt.wantRuns {
				t.Errorf("tool ran %d times, want %d (failed-mint calls must not execute)", got, tt.wantRuns)
			}

			// No gate (Check) consulted for a failed-mint call: Check is only reached
			// by executable calls, so the consult count equals the number of working
			// calls.
			gate.mu.Lock()
			nChecks := len(gate.checkCalls)
			gate.mu.Unlock()
			if want := tt.nCalls - len(tt.failMints); nChecks != want {
				t.Errorf("Check consulted %d times, want %d (no gate for failed-mint call)", nChecks, want)
			}

			// Every requested call still gets exactly one Started + one Completed.
			_, _, nStarted, nCompleted := startedCompletedOrder(getEvents())
			if nStarted != tt.nCalls || nCompleted != tt.nCalls {
				t.Errorf("events: %d started / %d completed, want %d/%d", nStarted, nCompleted, tt.nCalls, tt.nCalls)
			}
		})
	}
}

// TestRunBatch_ResultPreviewCapped: an oversized result is truncated + marked; a
// small one is not.
func TestRunBatch_ResultPreviewCapped(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", previewMaxBytes*2)
	tl := &fakeRunTool{name: "Big", output: big}
	small := &fakeRunTool{name: "Small", output: "tiny"}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl, small}, MaxParallelToolCalls: 4}
	emit, getEvents := collectEmit()
	calls := []content.ToolUseBlock{call(t, "Big", `{}`), call(t, "Small", `{}`)}
	RunBatch(context.Background(), calls, ts, make(chan gateRegistration), uuid.New, emit)

	var bigPreview, smallPreview string
	for _, ev := range getEvents() {
		if c, ok := ev.(event.ToolCallCompleted); ok {
			if strings.HasPrefix(c.ResultPreview, "x") {
				bigPreview = c.ResultPreview
			}
			if strings.HasPrefix(c.ResultPreview, "tiny") {
				smallPreview = c.ResultPreview
			}
		}
	}
	if len(bigPreview) > previewMaxBytes+len(truncationMarker)+1 {
		t.Errorf("big preview length %d exceeds cap %d", len(bigPreview), previewMaxBytes)
	}
	if !strings.Contains(bigPreview, truncationMarker) {
		t.Errorf("big preview %q missing truncation marker", bigPreview[:min(60, len(bigPreview))])
	}
	if smallPreview != "tiny" {
		t.Errorf("small preview = %q, want %q (not truncated)", smallPreview, "tiny")
	}
}

// TestRunBatch_PreviewLineCap: a result with many lines is capped by line count.
func TestRunBatch_PreviewLineCap(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	for i := 0; i < previewMaxLines*2; i++ {
		sb.WriteString("line\n")
	}
	tl := &fakeRunTool{name: "Lines", output: sb.String()}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 4}
	emit, getEvents := collectEmit()
	RunBatch(context.Background(), []content.ToolUseBlock{call(t, "Lines", `{}`)}, ts, make(chan gateRegistration), uuid.New, emit)
	var preview string
	for _, ev := range getEvents() {
		if c, ok := ev.(event.ToolCallCompleted); ok {
			preview = c.ResultPreview
		}
	}
	if !strings.Contains(preview, truncationMarker) {
		t.Errorf("preview missing truncation marker: %q", preview)
	}
	lines := strings.Count(preview, "\n")
	if lines > previewMaxLines+1 {
		t.Errorf("preview has %d lines, want <= %d", lines, previewMaxLines)
	}
}

// TestRunBatch_GrantErrorStillExecutes: a Grant failure must not fail the call —
// it still executes (the user approved it); Grant error is only logged.
func TestRunBatch_GrantErrorStillExecutes(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "executed"}
	pt := promptTool{fakeRunTool: tl}
	tl.promptFn = func(string) (tool.PermissionRequest, error) {
		return tool.UnknownRequest{Tool: "T", Summary: "x"}, nil
	}
	gate := &fakePermissionGate{
		checkFn:  func(name, args string) Effect { return EffectAsk },
		grantErr: errors.New("disk full"),
	}
	ts := ToolSet{Permission: gate, Registry: []tool.InvokableTool{pt}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	gateReg := make(chan gateRegistration)
	go func() {
		reg := <-gateReg
		close(reg.ack)
		reg.reply <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Scope: tool.ScopeWorkspace}
	}()
	results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, gateReg, uuid.New, emit)
	if results[0].IsError {
		t.Errorf("result = %+v, want success despite Grant error", results[0])
	}
	if !strings.Contains(resultText(results[0]), "executed") {
		t.Errorf("result text = %q, want execution to have happened", resultText(results[0]))
	}
	if atomic.LoadInt32(&tl.totalRuns) != 1 {
		t.Errorf("tool ran %d times, want 1", tl.totalRuns)
	}
}

// TestRunBatch_ScopeOnceNoGrant: approving with ScopeOnce must not call Grant.
func TestRunBatch_ScopeOnceNoGrant(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	pt := promptTool{fakeRunTool: tl}
	tl.promptFn = func(string) (tool.PermissionRequest, error) {
		return tool.UnknownRequest{Tool: "T", Summary: "x"}, nil
	}
	gate := &fakePermissionGate{checkFn: func(name, args string) Effect { return EffectAsk }}
	ts := ToolSet{Permission: gate, Registry: []tool.InvokableTool{pt}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	gateReg := make(chan gateRegistration)
	go func() {
		reg := <-gateReg
		close(reg.ack)
		reg.reply <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Scope: tool.ScopeOnce}
	}()
	results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, gateReg, uuid.New, emit)
	if results[0].IsError {
		t.Fatalf("result = %+v, want success", results[0])
	}
	gate.mu.Lock()
	nGrants := len(gate.grants)
	gate.mu.Unlock()
	if nGrants != 0 {
		t.Errorf("Grant called %d times for ScopeOnce, want 0", nGrants)
	}
}

// TestRunBatch_MiddlewareOutermostFirst: the first-listed middleware is the
// outermost wrapper of InvokableRun.
func TestRunBatch_MiddlewareOutermostFirst(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	var mu sync.Mutex
	var order []string
	mw := func(tag string) tool.ToolMiddleware {
		return func(ctx context.Context, tt tool.InvokableTool, args string, next tool.ToolExecuteFunc) (*tool.ToolResult, error) {
			mu.Lock()
			order = append(order, tag+":before")
			mu.Unlock()
			res, err := next(ctx, args)
			mu.Lock()
			order = append(order, tag+":after")
			mu.Unlock()
			return res, err
		}
	}
	ts := ToolSet{
		Permission:           autoApproveGate{},
		Registry:             []tool.InvokableTool{tl},
		Middlewares:          []tool.ToolMiddleware{mw("outer"), mw("inner")},
		MaxParallelToolCalls: 4,
	}
	emit, _ := collectEmit()
	RunBatch(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, make(chan gateRegistration), uuid.New, emit)
	want := []string{"outer:before", "inner:before", "inner:after", "outer:after"}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestRunBatch_EmptyResultInjected: a tool returning an empty Content slice gets
// an "error: empty result" injected (the ToolResult contract).
func TestRunBatch_EmptyResultInjected(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "E", empty: true}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "E", `{}`)}, ts, emit)
	if !results[0].IsError || !strings.Contains(resultText(results[0]), "empty result") {
		t.Errorf("result = %+v / %q, want empty-result error", results[0], resultText(results[0]))
	}
}

// TestRunBatch_ToolErrorBecomesResult: a tool returning (nil, err) yields an
// error tool-result, not a thrown error.
func TestRunBatch_ToolErrorBecomesResult(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "Err", runErr: errors.New("boom inside")}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "Err", `{}`)}, ts, emit)
	if !results[0].IsError || !strings.Contains(resultText(results[0]), "boom inside") {
		t.Errorf("result = %+v / %q, want error result", results[0], resultText(results[0]))
	}
}

// TestRunBatch_CtxInjectedPerCall: the per-call ctx must carry ToolExecutionID + emit +
// gateReg so an event-emitting / input-requesting tool works. We use a tool that
// calls RequestUserInput.
func TestRunBatch_CtxInjectedPerCall(t *testing.T) {
	t.Parallel()
	asker := &ctxProbeTool{name: "Ask"}
	ts := ToolSet{Permission: autoApproveGate{}, Registry: []tool.InvokableTool{asker}, MaxParallelToolCalls: 4}
	emit, getEvents := collectEmit()
	gateReg := make(chan gateRegistration)
	go func() {
		reg := <-gateReg
		close(reg.ack)
		reg.reply <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Answer: "green"}
	}()
	results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "Ask", `{}`)}, ts, gateReg, uuid.New, emit)
	if results[0].IsError {
		t.Fatalf("result = %+v, want success", results[0])
	}
	if !strings.Contains(resultText(results[0]), "green") {
		t.Errorf("result = %q, want the injected answer 'green'", resultText(results[0]))
	}
	var sawUserInput bool
	for _, ev := range getEvents() {
		if _, ok := ev.(event.UserInputRequested); ok {
			sawUserInput = true
		}
	}
	if !sawUserInput {
		t.Error("expected UserInputRequested event from the ctx-aware tool")
	}
}

// ctxProbeTool calls RequestUserInput to prove the runner injected emit/ToolExecutionID/
// gateReg into the per-call ctx.
type ctxProbeTool struct{ name string }

func (c *ctxProbeTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: c.name}, nil
}
func (c *ctxProbeTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	ans, err := RequestUserInput(ctx, "color?", []string{"green"})
	if err != nil {
		return nil, err
	}
	return tool.TextResult("answer=" + ans), nil
}

// TestRunBatch_CtxCancelNoLeak: cancelling ctx while a gate is open returns
// without wedging.
func TestRunBatch_CtxCancelDuringGate(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	pt := promptTool{fakeRunTool: tl}
	tl.promptFn = func(string) (tool.PermissionRequest, error) {
		return tool.UnknownRequest{Tool: "T", Summary: "x"}, nil
	}
	gate := &fakePermissionGate{checkFn: func(name, args string) Effect { return EffectAsk }}
	ts := ToolSet{Permission: gate, Registry: []tool.InvokableTool{pt}, MaxParallelToolCalls: 4}
	emit, _ := collectEmit()
	ctx, cancel := context.WithCancel(context.Background())
	gateReg := make(chan gateRegistration)
	// Actor acks but never replies; we cancel instead.
	go func() {
		reg := <-gateReg
		close(reg.ack)
		// no reply
	}()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	done := make(chan []result, 1)
	go func() {
		done <- RunBatch(ctx, []content.ToolUseBlock{call(t, "T", `{}`)}, ts, gateReg, uuid.New, emit)
	}()
	select {
	case <-done:
		// returned without wedging — pass
	case <-time.After(2 * time.Second):
		t.Fatal("RunBatch wedged on ctx cancel during gate wait")
	}
}
