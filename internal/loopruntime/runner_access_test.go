package loopruntime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// ---------------------------------------------------------------------------
// Access fakes
// ---------------------------------------------------------------------------

// countingAccessGate is a configurable loop.AccessGate that records every
// authorized request.
type countingAccessGate struct {
	mu         sync.Mutex
	requests   []tool.Request
	resolution gatedomain.Resolution
	err        error
}

func (g *countingAccessGate) Authorize(_ context.Context, request tool.Request) (gatedomain.Resolution, error) {
	g.mu.Lock()
	g.requests = append(g.requests, request.Clone())
	g.mu.Unlock()
	if g.err != nil {
		return gatedomain.Resolution{}, g.err
	}
	return g.resolution, nil
}

func (g *countingAccessGate) authorized() []tool.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]tool.Request, len(g.requests))
	copy(out, g.requests)
	return out
}

// fixedAccessSource reports one fixed access state for every kind/scope.
type fixedAccessSource struct{ access uint8 }

func (fixedAccessSource) AccessVersion() uint16                  { return gatedomain.CurrentAccessVersion }
func (s fixedAccessSource) AccessFor(_, _ string) (uint8, error) { return s.access, nil }

// recordingRuleWriter records atomically persisted candidate batches. A
// non-nil err makes persistence fail (nothing is recorded).
type recordingRuleWriter struct {
	err    error
	mu     sync.Mutex
	writes [][]tool.RuleCandidate
}

func (w *recordingRuleWriter) WriteRules(_ context.Context, candidates []tool.RuleCandidate) error {
	if w.err != nil {
		return w.err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	batch := make([]tool.RuleCandidate, len(candidates))
	copy(batch, candidates)
	w.writes = append(w.writes, batch)
	return nil
}

func (w *recordingRuleWriter) batches() [][]tool.RuleCandidate {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][]tool.RuleCandidate, len(w.writes))
	copy(out, w.writes)
	return out
}

// recordingIssuer mints deterministic tokens and records every issuance.
type recordingIssuer struct {
	mu    sync.Mutex
	calls []string // target per issued grant
}

func (*recordingIssuer) GrantVersion() uint16 { return gatedomain.CurrentGrantVersion }

func (i *recordingIssuer) IssueGrant(_ context.Context, _, _, _, _, _, _, target string, _ int64) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, target)
	return "token:" + target, nil
}

func (i *recordingIssuer) targets() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.calls...)
}

// interactiveEvaluator builds a real interactive evaluator whose approver
// routes through the runner's per-call approval capability.
func interactiveEvaluator(t *testing.T, access uint8, writer gatedomain.RuleWriter, issuer gatedomain.GrantIssuer) *gatedomain.Evaluator {
	t.Helper()
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		accessBindings(access),
		nil,
		loop.GateApprover(),
		writer,
		issuer,
	)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}
	return evaluator
}

func accessBindings(access uint8) []gatedomain.AccessBinding {
	return []gatedomain.AccessBinding{
		{Kind: tool.CapabilityCommandExecute, Source: fixedAccessSource{access: access}},
		{Kind: "network", Source: fixedAccessSource{access: access}},
	}
}

// commandRequest builds a valid prepared command request bound to executionID,
// with an optional network requirement.
func commandRequest(executionID uuid.UUID, cmd string, network bool) tool.Request {
	request := tool.Request{
		ToolName:           "T",
		Summary:            "run " + cmd,
		ExecutionID:        executionID.String(),
		Command:            cmd,
		WorkingDirectory:   "/workspace",
		ExpiresAtUnixMilli: 1_800_000_000_000,
		Requirements: []tool.Requirement{{
			Kind:        tool.CapabilityCommandExecute,
			Match:       cmd,
			Description: "run command: " + cmd,
			GrantClass:  tool.GrantClassCommandStart,
			GrantTarget: cmd,
			Candidates: []tool.RuleCandidate{{
				Kind:        tool.CapabilityCommandExecute,
				Match:       "Bash(" + cmd + ")",
				Description: "Bash(" + cmd + ")",
				GrantClass:  tool.GrantClassCommandStart,
				GrantTarget: cmd,
			}},
		}},
	}
	if network {
		request.Requirements = append(request.Requirements, tool.Requirement{
			Kind:        "network",
			Match:       "tcp:github.com:443",
			Description: "connect to github.com:443",
			GrantClass:  "network.proxy-target.v1",
			GrantTarget: "tcp:github.com:443",
		})
	}
	return request
}

// unpreparedRunTool implements ONLY BaseTool + InvokableTool: no CallPreparer.
type unpreparedRunTool struct{ name string }

func (u unpreparedRunTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: u.name}, nil
}

func (u unpreparedRunTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ran"), nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// An effectful tool without a preparation step fails closed: no evaluation, no
// gate, no execution — only a paired error tool-result.
func TestRunBatch_UnpreparedToolFailsClosed(t *testing.T) {
	t.Parallel()
	access := &countingAccessGate{resolution: gatedomain.Resolution{Approved: true}}
	ts := ToolSet{Access: access, Registry: []tool.InvokableTool{unpreparedRunTool{name: "T"}}, MaxParallelToolCalls: 2}
	emit, _ := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want one error result", results)
	}
	if !strings.Contains(resultText(results[0]), "preparation") {
		t.Errorf("result = %q, want an unprepared-tool failure", resultText(results[0]))
	}
	if len(access.authorized()) != 0 {
		t.Errorf("Authorize calls = %d, want 0 for an unprepared tool", len(access.authorized()))
	}
}

// A pure tool returning an empty request executes without any gate.
func TestRunBatch_PureEmptyRequestSkipsGate(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	access := &countingAccessGate{resolution: gatedomain.Resolution{Approved: true}}
	ts := ToolSet{Access: access, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, getEvents := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want one success", results)
	}
	for _, ev := range getEvents() {
		if _, ok := ev.(event.PermissionRequested); ok {
			t.Fatal("PermissionRequested emitted for a pure empty request")
		}
	}
	if len(access.authorized()) != 1 {
		t.Errorf("Authorize calls = %d, want exactly 1", len(access.authorized()))
	}
}

// The runner mints the execution ID once, prepares once, evaluates once, and
// the same execution ID travels to preparation, the started event, and the
// prepared execution contract the tool reads back.
func TestRunBatch_MintsPreparesEvaluatesOnce(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	var mu sync.Mutex
	var preparedIDs []uuid.UUID
	tl.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		mu.Lock()
		preparedIDs = append(preparedIDs, executionID)
		mu.Unlock()
		return tool.Request{}, tool.TokenArtifact{Token: "artifact"}, nil
	}
	var seen tool.PreparedCall
	var seenOK bool
	tl.onRun = func(ctx context.Context) {
		mu.Lock()
		seen, seenOK = PreparedCallFromContext(ctx)
		mu.Unlock()
	}
	access := &countingAccessGate{resolution: gatedomain.Resolution{Approved: true}}
	ts := ToolSet{Access: access, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, getEvents := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want one success", results)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(preparedIDs) != 1 {
		t.Fatalf("PrepareCall count = %d, want exactly 1", len(preparedIDs))
	}
	if len(access.authorized()) != 1 {
		t.Fatalf("Authorize count = %d, want exactly 1", len(access.authorized()))
	}
	if !seenOK {
		t.Fatal("PreparedCallFromContext ok = false inside InvokableRun")
	}
	if seen.ExecutionID != preparedIDs[0] {
		t.Errorf("PreparedCall.ExecutionID = %s, want the minted %s", seen.ExecutionID, preparedIDs[0])
	}
	if artifact, ok := seen.Artifact.(tool.TokenArtifact); !ok || artifact.Token != "artifact" {
		t.Errorf("PreparedCall.Artifact = %#v, want the prepared TokenArtifact", seen.Artifact)
	}
	for _, ev := range getEvents() {
		if started, ok := ev.(event.ToolCallStarted); ok && started.ToolExecutionID != preparedIDs[0] {
			t.Errorf("ToolCallStarted.ToolExecutionID = %s, want %s", started.ToolExecutionID, preparedIDs[0])
		}
	}
}

// A denied evaluation never executes.
func TestRunBatch_DeniedEvaluationNeverExecutes(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	access := &countingAccessGate{resolution: gatedomain.Resolution{Approved: false}}
	ts := ToolSet{Access: access, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, _ := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || !results[0].IsError || !strings.Contains(resultText(results[0]), "permission denied") {
		t.Fatalf("results = %+v, want one permission-denied error", results)
	}
	if tl.totalRuns != 0 {
		t.Errorf("totalRuns = %d, want 0 for a denied call", tl.totalRuns)
	}
}

// An access-gate failure (including a headless approval-required denial) fails closed.
func TestRunBatch_AuthorizeErrorFailsClosed(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	access := &countingAccessGate{err: errors.New("evaluator unavailable")}
	ts := ToolSet{Access: access, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, _ := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || !results[0].IsError || !strings.Contains(resultText(results[0]), "permission denied") {
		t.Fatalf("results = %+v, want one permission-denied error", results)
	}
	if tl.totalRuns != 0 {
		t.Errorf("totalRuns = %d, want 0 after an authorize failure", tl.totalRuns)
	}
}

// A loop with no access gate wired denies every tool call (fail closed).
func TestRunBatch_NilAccessGateDenies(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	ts := ToolSet{Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, _ := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || !results[0].IsError || !strings.Contains(resultText(results[0]), "permission denied") {
		t.Fatalf("results = %+v, want one permission-denied error", results)
	}
	if tl.totalRuns != 0 {
		t.Errorf("totalRuns = %d, want 0 without an access gate", tl.totalRuns)
	}
}

// Issued tokens travel in the prepared execution contract, never in the old
// ambient grant context.
func TestRunBatch_GrantsTravelInPreparedCall(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	var mu sync.Mutex
	var gotGrants []string
	var ambient []string
	tl.onRun = func(ctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		if prepared, ok := PreparedCallFromContext(ctx); ok {
			gotGrants = prepared.Grants
		}
		ambient = tool.GrantsFromContext(ctx)
	}
	access := &countingAccessGate{resolution: gatedomain.Resolution{Approved: true, Grants: []string{"tok-1", "tok-2"}}}
	ts := ToolSet{Access: access, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, _ := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want one success", results)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotGrants) != 2 || gotGrants[0] != "tok-1" || gotGrants[1] != "tok-2" {
		t.Errorf("PreparedCall.Grants = %v, want the issued tokens", gotGrants)
	}
	if ambient != nil {
		t.Errorf("ambient GrantsFromContext = %v, want nil (tokens travel only in the prepared contract)", ambient)
	}
}

// A prepared request bound to a different execution ID fails closed before the gate.
func TestRunBatch_PreparedExecutionIDMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	tl.prepareFn = func(_ uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		other, err := uuid.New()
		if err != nil {
			t.Errorf("uuid.New: %v", err)
		}
		return commandRequest(other, "git status", false), nil, nil
	}
	access := &countingAccessGate{resolution: gatedomain.Resolution{Approved: true}}
	ts := ToolSet{Access: access, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, _ := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want one error result", results)
	}
	if len(access.authorized()) != 0 {
		t.Errorf("Authorize calls = %d, want 0 for a mis-bound request", len(access.authorized()))
	}
	if tl.totalRuns != 0 {
		t.Errorf("totalRuns = %d, want 0", tl.totalRuns)
	}
}

// A gated call through a REAL interactive evaluator opens exactly one combined
// gate; approving once mints fresh grants for every command-backed requirement
// and delivers them in the prepared execution contract.
func TestRunBatch_InteractiveGateOpensOnceApproveOnce(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	tl.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git push", true), nil, nil
	}
	var mu sync.Mutex
	var gotGrants []string
	tl.onRun = func(ctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		if prepared, ok := PreparedCallFromContext(ctx); ok {
			gotGrants = prepared.Grants
		}
	}
	writer := &recordingRuleWriter{}
	issuer := &recordingIssuer{}
	ts := ToolSet{
		Access:               interactiveEvaluator(t, gatedomain.AccessGated, writer, issuer),
		Registry:             []tool.InvokableTool{tl},
		MaxParallelToolCalls: 2,
	}
	emit, getEvents := collectEmit()

	gateReg := make(chan gateRegistration, 2)
	var registrations int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for reg := range gateReg {
			registrations++
			reg.ack <- gateInstallAck{gateID: reg.gate.Subject.ToolExecutionID}
			reg.reply <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Scope: tool.ScopeOnce}
		}
	}()

	results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, gateReg, uuid.New, emit)
	close(gateReg)
	<-done

	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want one success", results)
	}
	if registrations != 1 {
		t.Fatalf("gate registrations = %d, want exactly one combined gate", registrations)
	}
	targets := issuer.targets()
	if len(targets) != 2 || targets[0] != "git push" || targets[1] != "tcp:github.com:443" {
		t.Fatalf("issued grant targets = %v, want exact command and network targets", targets)
	}
	mu.Lock()
	if len(gotGrants) != 2 {
		t.Errorf("PreparedCall.Grants = %v, want two fresh tokens", gotGrants)
	}
	mu.Unlock()
	if len(writer.batches()) != 0 {
		t.Errorf("writer batches = %d, want 0 for a once approval", len(writer.batches()))
	}
	var nRequested int
	for _, ev := range getEvents() {
		if _, ok := ev.(event.PermissionRequested); ok {
			nRequested++
		}
	}
	if nRequested != 1 {
		t.Errorf("PermissionRequested count = %d, want 1", nRequested)
	}
}

// A workspace-scope approval maps to Approve-always: the displayed candidates
// persist atomically before grants are minted.
func TestRunBatch_WorkspaceScopePersistsCandidates(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	tl.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git status", false), nil, nil
	}
	writer := &recordingRuleWriter{}
	issuer := &recordingIssuer{}
	ts := ToolSet{
		Access:               interactiveEvaluator(t, gatedomain.AccessGated, writer, issuer),
		Registry:             []tool.InvokableTool{tl},
		MaxParallelToolCalls: 2,
	}
	emit, _ := collectEmit()

	gateReg := make(chan gateRegistration, 1)
	go func() {
		reg := <-gateReg
		reg.ack <- gateInstallAck{}
		reg.reply <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Scope: tool.ScopeWorkspace}
	}()

	results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, gateReg, uuid.New, emit)

	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want one success", results)
	}
	batches := writer.batches()
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].Match != "Bash(git status)" {
		t.Fatalf("writer batches = %+v, want one batch with the displayed candidate", batches)
	}
	if len(issuer.targets()) != 1 {
		t.Errorf("issued grants = %v, want one", issuer.targets())
	}
}

// The retired session scope fails closed: it maps to no new approval action.
func TestRunBatch_SessionScopeFailsClosed(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	tl.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git status", false), nil, nil
	}
	ts := ToolSet{
		Access:               interactiveEvaluator(t, gatedomain.AccessGated, &recordingRuleWriter{}, &recordingIssuer{}),
		Registry:             []tool.InvokableTool{tl},
		MaxParallelToolCalls: 2,
	}
	emit, _ := collectEmit()

	gateReg := make(chan gateRegistration, 1)
	go func() {
		reg := <-gateReg
		reg.ack <- gateInstallAck{}
		reg.reply <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Scope: tool.ScopeSession}
	}()

	results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, gateReg, uuid.New, emit)

	if len(results) != 1 || !results[0].IsError || !strings.Contains(resultText(results[0]), "permission denied") {
		t.Fatalf("results = %+v, want one permission-denied error", results)
	}
	if tl.totalRuns != 0 {
		t.Errorf("totalRuns = %d, want 0", tl.totalRuns)
	}
}

// A headless evaluator never opens a gate: an unmet gated requirement is a
// typed approval-required denial and the call fails closed.
func TestRunBatch_HeadlessUnmetDeniesWithoutGate(t *testing.T) {
	t.Parallel()
	tl := &fakeRunTool{name: "T", output: "ok"}
	tl.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git status", false), nil, nil
	}
	evaluator, err := gatedomain.NewHeadlessEvaluator(accessBindings(gatedomain.AccessGated), nil, &recordingIssuer{})
	if err != nil {
		t.Fatalf("NewHeadlessEvaluator() error = %v", err)
	}
	ts := ToolSet{Access: evaluator, Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, getEvents := collectEmit()

	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "T", `{}`)}, ts, emit)

	if len(results) != 1 || !results[0].IsError || !strings.Contains(resultText(results[0]), "permission denied") {
		t.Fatalf("results = %+v, want one permission-denied error", results)
	}
	for _, ev := range getEvents() {
		if _, ok := ev.(event.PermissionRequested); ok {
			t.Fatal("PermissionRequested emitted by a headless gate")
		}
	}
	if tl.totalRuns != 0 {
		t.Errorf("totalRuns = %d, want 0", tl.totalRuns)
	}
}
