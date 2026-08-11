//go:build integration

package rig

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/contextcount"
	"github.com/looprig/inference/stream"
)

type hookIntegrationStackKey struct{}

type hookIntegrationTrace struct {
	mu      sync.Mutex
	entries []hookIntegrationEntry
}

type hookIntegrationEntry struct {
	phase  string
	op     hook.Operation
	stack  []hook.Operation
	family hook.RecordFamily
	id     string
}

func (t *hookIntegrationTrace) around(operation hook.Operation) hook.Around {
	return hook.Around{
		Operation: operation,
		Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
			parent, _ := ctx.Value(hookIntegrationStackKey{}).([]hook.Operation)
			stack := append(append([]hook.Operation(nil), parent...), operation)
			entry := hookIntegrationEntry{phase: "begin", op: operation, stack: append([]hook.Operation(nil), stack...)}
			if call.JournalAppend != nil {
				entry.family, entry.id = call.JournalAppend.Family, call.JournalAppend.RecordID
			}
			t.add(entry)
			return context.WithValue(ctx, hookIntegrationStackKey{}, stack), func(result hook.Result) {
				t.add(hookIntegrationEntry{
					phase: "finish", op: operation,
					stack: append([]hook.Operation(nil), stack...),
				})
			}
		},
	}
}

func (t *hookIntegrationTrace) add(entry hookIntegrationEntry) {
	t.mu.Lock()
	t.entries = append(t.entries, entry)
	t.mu.Unlock()
}

func (t *hookIntegrationTrace) snapshot() []hookIntegrationEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]hookIntegrationEntry(nil), t.entries...)
}

type hookIntegrationLLM struct {
	mu    sync.Mutex
	calls int
}

func (*hookIntegrationLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("hook integration: native Invoke is unused")
}

func (l *hookIntegrationLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	l.mu.Lock()
	first := l.calls == 0
	l.calls++
	l.mu.Unlock()
	chunks := []content.Chunk{&content.TextChunk{Text: "done"}}
	if first {
		chunks = []content.Chunk{&content.ToolUseChunk{
			Index: 0, ID: "hook-use-1", Name: "Gated", InputJSON: `{}`,
		}}
	}
	index := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}, nil), nil
}

type hookIntegrationAccessSource struct{}

func (hookIntegrationAccessSource) AccessVersion() uint16 { return gate.CurrentAccessVersion }
func (hookIntegrationAccessSource) AccessFor(string, string) (uint8, error) {
	return gate.AccessGated, nil
}

type hookIntegrationRuleWriter struct{}

func (hookIntegrationRuleWriter) WriteRules(context.Context, []tool.RuleCandidate) error { return nil }

type hookIntegrationTool struct{ runs atomic.Int32 }

func (*hookIntegrationTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Gated", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (*hookIntegrationTool) PrepareCall(_ context.Context, _ uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{
		ToolName: "Gated",
		Summary:  "integration permission",
		Requirements: []tool.Requirement{{
			Kind: "tool.invoke", Scope: "Gated", Match: "Gated", Description: "run Gated",
		}},
	}, nil, nil
}

func (t *hookIntegrationTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	t.runs.Add(1)
	return tool.TextResult("approved"), nil
}

func TestHooksIntegrationNativeOperationNestingAndPermission(t *testing.T) {
	trace := &hookIntegrationTrace{}
	operations := []hook.Operation{
		hook.OperationTurn,
		hook.OperationStep,
		hook.OperationInference,
		hook.OperationToolCall,
		hook.OperationGateWait,
		hook.OperationToolExecution,
		hook.OperationJournalAppend,
	}
	around := make([]hook.Around, 0, len(operations))
	for _, operation := range operations {
		around = append(around, trace.around(operation))
	}
	around = append(around, hook.Around{
		Operation: hook.OperationTurn,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			trace.add(hookIntegrationEntry{phase: "second_begin", op: hook.OperationTurn})
			return ctx, func(hook.Result) {
				trace.add(hookIntegrationEntry{phase: "second_finish", op: hook.OperationTurn})
			}
		},
	})
	evaluator, err := gate.NewInteractiveEvaluator(
		[]gate.AccessBinding{{Kind: "tool.invoke", Source: hookIntegrationAccessSource{}}},
		nil,
		loop.GateApprover(),
		hookIntegrationRuleWriter{},
		nil,
	)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}
	toolImpl := &hookIntegrationTool{}
	definition, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(&hookIntegrationLLM{}, validModel("hook-integration")),
		loop.WithTools(tool.NewDefinition("Gated", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{toolImpl}, nil
		})),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("permission-v1"),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	store := sessionStoreT(t)
	defined, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithHooks(hook.Set{Around: around}),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	live, err := defined.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = live.Shutdown(context.Background()) })
	sub, err := live.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()
	if _, err := live.Submit(ctx, []content.Block{&content.TextBlock{Text: "run it"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	answered := false
	var permissionEventID string
	for {
		select {
		case delivery := <-sub.Events():
			if _, ok := delivery.Event.(event.PermissionRequested); ok {
				permissionEventID = delivery.Event.EventHeader().EventID.String()
				lister, ok := live.(interface {
					ListGates(context.Context) []gate.Gate
				})
				if !ok {
					t.Fatal("session controller cannot list gates")
				}
				open := lister.ListGates(ctx)
				if len(open) != 1 {
					t.Fatalf("permission event has %d open gates, want 1", len(open))
				}
				if err := live.RespondGate(ctx, gate.GateResponse{
					GateID: open[0].ID,
					Action: string(gate.ApprovalApprove),
					Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
				}); err != nil {
					t.Fatalf("RespondGate: %v", err)
				}
				answered = true
			}
			if delivery.Event.EndsTurn() {
				if !answered {
					t.Fatal("turn ended before the real permission gate was answered")
				}
				goto terminal
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for permission-gated turn")
		}
	}

terminal:
	if got := toolImpl.runs.Load(); got != 1 {
		t.Fatalf("tool runs = %d, want 1", got)
	}
	entries := trace.snapshot()
	assertHookIntegrationRegistrationOrder(t, entries)
	assertHookIntegrationParent(t, entries, hook.OperationStep, hook.OperationTurn)
	assertHookIntegrationParent(t, entries, hook.OperationInference, hook.OperationStep)
	assertHookIntegrationParent(t, entries, hook.OperationToolCall, hook.OperationStep)
	assertHookIntegrationParent(t, entries, hook.OperationGateWait, hook.OperationToolCall)
	assertHookIntegrationParent(t, entries, hook.OperationToolExecution, hook.OperationToolCall)
	assertHookIntegrationJournalContext(t, entries)
	assertHookIntegrationJournalParent(t, entries, permissionEventID, hook.OperationToolCall)
}

func assertHookIntegrationJournalParent(
	t *testing.T,
	entries []hookIntegrationEntry,
	recordID string,
	parent hook.Operation,
) {
	t.Helper()
	if recordID == "" {
		t.Fatal("target durable event has no record id")
	}
	for _, entry := range entries {
		if entry.phase != "begin" || entry.op != hook.OperationJournalAppend || entry.id != recordID {
			continue
		}
		if len(entry.stack) >= 2 && entry.stack[len(entry.stack)-2] == parent {
			return
		}
		t.Fatalf("journal append %q stack = %v, want immediate parent %v", recordID, entry.stack, parent)
	}
	t.Fatalf("journal append %q was not observed", recordID)
}

func assertHookIntegrationRegistrationOrder(t *testing.T, entries []hookIntegrationEntry) {
	t.Helper()
	index := func(phase string) int {
		for i, entry := range entries {
			if entry.op == hook.OperationTurn && entry.phase == phase {
				return i
			}
		}
		return -1
	}
	firstBegin := index("begin")
	secondBegin := index("second_begin")
	secondFinish := index("second_finish")
	firstFinish := index("finish")
	if firstBegin < 0 || secondBegin <= firstBegin || secondFinish <= secondBegin || firstFinish <= secondFinish {
		t.Fatalf("turn callback order = first begin %d, second begin %d, second finish %d, first finish %d", firstBegin, secondBegin, secondFinish, firstFinish)
	}
}

func assertHookIntegrationParent(t *testing.T, entries []hookIntegrationEntry, operation, parent hook.Operation) {
	t.Helper()
	for _, entry := range entries {
		if entry.phase != "begin" || entry.op != operation {
			continue
		}
		if len(entry.stack) >= 2 && entry.stack[len(entry.stack)-2] == parent {
			return
		}
	}
	t.Fatalf("no %v begin inherited %v context: %+v", operation, parent, entries)
}

func assertHookIntegrationJournalContext(t *testing.T, entries []hookIntegrationEntry) {
	t.Helper()
	var normal int
	var nested int
	var fences int
	seen := make(map[string]struct{})
	nestedParents := make(map[hook.Operation]bool)
	for _, entry := range entries {
		if entry.phase != "begin" || entry.op != hook.OperationJournalAppend {
			continue
		}
		if _, exists := seen[entry.id]; exists {
			t.Fatalf("journal record %q observed more than once", entry.id)
		}
		seen[entry.id] = struct{}{}
		if entry.family == hook.RecordFence {
			fences++
			continue
		}
		normal++
		if len(entry.stack) >= 2 {
			nested++
			nestedParents[entry.stack[len(entry.stack)-2]] = true
		}
	}
	if normal == 0 {
		t.Fatal("no normal journal append was observed")
	}
	if nested == 0 {
		t.Fatal("no durable append inherited its active operation context")
	}
	for _, operation := range []hook.Operation{
		hook.OperationTurn,
		hook.OperationStep,
	} {
		if !nestedParents[operation] {
			t.Fatalf("no journal append inherited active %v context; parents=%v", operation, nestedParents)
		}
	}
	if fences != 1 {
		t.Fatalf("opening fence appends = %d, want exactly 1", fences)
	}
}

type hookIntegrationDecider struct {
	mu         sync.Mutex
	assessment event.DriftAssessment
}

func (d *hookIntegrationDecider) DecideRestore(_ context.Context, assessment event.DriftAssessment) (session.RestoreDecision, error) {
	d.mu.Lock()
	d.assessment = assessment
	d.mu.Unlock()
	return session.RestoreDecision{Accept: true, Source: event.DecisionSourcePolicy}, nil
}

func (d *hookIntegrationDecider) snapshot() event.DriftAssessment {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.assessment
}

func TestHooksIntegrationManifestDriftRestoreAndNoReplay(t *testing.T) {
	store := sessionStoreT(t)
	definition := mustDefine(
		loop.WithName("agent"),
		loop.WithInference(&hookIntegrationTextLLM{}, validModel("restore-hooks")),
	)
	guard := func(context.Context, hook.Call) error { return nil }
	var firstTurns atomic.Int32
	first, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithHooks(hook.Set{
			PolicyRevision: "guard-v1",
			Guards:         []hook.Guard{{Operation: hook.OperationTurn, Check: guard}},
			Around: []hook.Around{{
				Operation: hook.OperationTurn,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					firstTurns.Add(1)
					return ctx, nil
				},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("Define first: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	live, err := first.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := live.SessionID()
	waitForHookIntegrationTurn(t, ctx, live, "historical")
	if got := firstTurns.Load(); got != 1 {
		t.Fatalf("first runner turn count = %d, want 1", got)
	}
	if manifest := firstSessionManifest(t, store, id); manifest.HookPolicyRev != "guard-v1" {
		t.Fatalf("SessionStarted hook policy = %q, want guard-v1", manifest.HookPolicyRev)
	}
	if err := live.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown live: %v", err)
	}

	decider := &hookIntegrationDecider{}
	var restoredTurns atomic.Int32
	var restoredFences atomic.Int32
	var restoredAppends atomic.Int32
	second, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithRestoreDecider(decider),
		WithHooks(hook.Set{
			PolicyRevision: "guard-v2",
			Guards:         []hook.Guard{{Operation: hook.OperationTurn, Check: guard}},
			Around: []hook.Around{
				{
					Operation: hook.OperationTurn,
					Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
						restoredTurns.Add(1)
						return ctx, nil
					},
				},
				{
					Operation: hook.OperationJournalAppend,
					Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
						restoredAppends.Add(1)
						if call.JournalAppend.Family == hook.RecordFence {
							restoredFences.Add(1)
						}
						return ctx, nil
					},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("Define second: %v", err)
	}
	restored, err := second.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if got := restoredTurns.Load(); got != 0 {
		t.Fatalf("historical turns replayed through new hooks: %d", got)
	}
	if got := restoredFences.Load(); got != 1 {
		t.Fatalf("restore opening fences observed = %d, want exactly 1", got)
	}
	if restoredAppends.Load() == 0 {
		t.Fatal("restore produced no newly hooked journal appends")
	}
	assessment := decider.snapshot()
	var found bool
	for _, change := range assessment.Changes {
		if change.Category == event.DriftHookPolicy && change.Severity == event.DriftWarn {
			found = true
		}
	}
	if !found {
		t.Fatalf("restore drift = %+v, want hook-policy Warn", assessment)
	}
	waitForHookIntegrationTurn(t, ctx, restored, "new work")
	if got := restoredTurns.Load(); got != 1 {
		t.Fatalf("restored runner new-work count = %d, want 1", got)
	}
	if err := restored.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown changed-policy restore: %v", err)
	}

	var unchangedTurns atomic.Int32
	var unchangedFences atomic.Int32
	unchanged, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithHooks(hook.Set{
			PolicyRevision: "guard-v2",
			Guards:         []hook.Guard{{Operation: hook.OperationTurn, Check: guard}},
			Around: []hook.Around{
				{
					Operation: hook.OperationTurn,
					Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
						unchangedTurns.Add(1)
						return ctx, nil
					},
				},
				{
					Operation: hook.OperationJournalAppend,
					Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
						if call.JournalAppend.Family == hook.RecordFence {
							unchangedFences.Add(1)
						}
						return ctx, nil
					},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("Define unchanged policy: %v", err)
	}
	unchangedSession, err := unchanged.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession unchanged policy: %v", err)
	}
	t.Cleanup(func() { _ = unchangedSession.Shutdown(context.Background()) })
	if got := unchangedTurns.Load(); got != 0 {
		t.Fatalf("unchanged-policy restore replayed %d historical turns", got)
	}
	if got := unchangedFences.Load(); got != 1 {
		t.Fatalf("unchanged-policy restore opening fences = %d, want 1", got)
	}
	waitForHookIntegrationTurn(t, ctx, unchangedSession, "unchanged policy new work")
	if got := unchangedTurns.Load(); got != 1 {
		t.Fatalf("unchanged-policy new runner turn count = %d, want 1", got)
	}

	aroundOnly, aroundStore := defineHookRig(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationTurn,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, nil
		},
	}}})
	aroundSession, err := aroundOnly.NewSession(ctx)
	if err != nil {
		t.Fatalf("around-only NewSession: %v", err)
	}
	aroundID := aroundSession.SessionID()
	if err := aroundSession.Shutdown(ctx); err != nil {
		t.Fatalf("around-only Shutdown: %v", err)
	}
	if manifest := firstSessionManifest(t, aroundStore, aroundID); manifest.HookPolicyRev != "" {
		t.Fatalf("around-only hook policy = %q, want empty", manifest.HookPolicyRev)
	}
}

type hookIntegrationTextLLM struct{}

func (*hookIntegrationTextLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("hook integration: Invoke unused")
}

func (*hookIntegrationTextLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	done := false
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if done {
			return nil, io.EOF
		}
		done = true
		return &content.TextChunk{Text: "done"}, nil
	}, nil), nil
}

func waitForHookIntegrationTurn(t *testing.T, ctx context.Context, controller session.SessionController, text string) {
	t.Helper()
	sub, err := controller.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()
	if _, err := controller.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	for {
		select {
		case delivery := <-sub.Events():
			if delivery.Event.EndsTurn() {
				return
			}
		case <-ctx.Done():
			t.Fatalf("turn %q timed out: %v", text, ctx.Err())
		}
	}
}

func firstSessionManifest(t *testing.T, store *sessionstore.Store, id uuid.UUID) event.ConfigManifest {
	t.Helper()
	for _, value := range replayRigEvents(t, store, id) {
		if started, ok := value.(event.SessionStarted); ok {
			return started.Manifest
		}
	}
	t.Fatal("journal has no SessionStarted")
	return event.ConfigManifest{}
}

type hookIntegrationForeignBackend struct {
	commands chan command.Command
	done     chan struct{}
	once     sync.Once
}

func newHookIntegrationForeignBackend() *hookIntegrationForeignBackend {
	backend := &hookIntegrationForeignBackend{
		commands: make(chan command.Command),
		done:     make(chan struct{}),
	}
	go backend.serve()
	return backend
}

func (b *hookIntegrationForeignBackend) serve() {
	for value := range b.commands {
		switch value := value.(type) {
		case command.Shutdown:
			value.Ack <- nil
			b.once.Do(func() { close(b.done) })
			return
		case command.Interrupt:
			value.Ack <- false
		}
	}
}

func (b *hookIntegrationForeignBackend) CommandSink() chan<- command.Command { return b.commands }
func (b *hookIntegrationForeignBackend) DoneChan() <-chan struct{}           { return b.done }
func (*hookIntegrationForeignBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

func TestHooksIntegrationNativeAndForeignEngineBoundary(t *testing.T) {
	var nativeTurns atomic.Int32
	turnObserver := func(counter *atomic.Int32) hook.Set {
		return hook.Set{Around: []hook.Around{{
			Operation: hook.OperationTurn,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				counter.Add(1)
				return ctx, nil
			},
		}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nativeDefinition := mustDefine(
		loop.WithName("native"),
		loop.WithInference(&hookIntegrationTextLLM{}, validModel("native")),
	)
	nativeRig, err := Define(
		WithLoops(nativeDefinition),
		WithPrimers("native"),
		WithSessionStore(sessionStoreT(t)),
		WithHooks(turnObserver(&nativeTurns)),
	)
	if err != nil {
		t.Fatalf("Define native: %v", err)
	}
	native, err := nativeRig.NewSession(ctx)
	if err != nil {
		t.Fatalf("native NewSession: %v", err)
	}
	waitForHookIntegrationTurn(t, ctx, native, "native")
	if got := nativeTurns.Load(); got != 1 {
		t.Fatalf("native turn hook count = %d, want 1", got)
	}
	if err := native.Shutdown(ctx); err != nil {
		t.Fatalf("native Shutdown: %v", err)
	}

	var foreignTurns atomic.Int32
	foreignDefinition := mustDefine(
		loop.WithName("foreign"),
		loop.WithInference(&hookIntegrationTextLLM{}, validModel("foreign")),
		loop.WithEngine(loop.EngineForeignClaude),
	)
	liveBuilder := foreign.Builder(func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		loop.Provenance,
		foreign.EventPublisher,
		loop.BoundDefinition,
		func() (uuid.UUID, error),
		*event.Factory,
	) (loop.Backend, string, error) {
		return newHookIntegrationForeignBackend(), "hook-integration-foreign", nil
	})
	restoredBuilder := foreign.RestoredBuilder(func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		loop.Provenance,
		foreign.EventPublisher,
		loop.BoundDefinition,
		func() (uuid.UUID, error),
		*event.Factory,
		foreign.RestoredForeign,
	) (loop.Backend, error) {
		return newHookIntegrationForeignBackend(), nil
	})
	foreignRig, err := Define(
		WithLoops(foreignDefinition),
		WithPrimers("foreign"),
		WithSessionStore(sessionStoreT(t)),
		WithForeignBuilders(liveBuilder, restoredBuilder),
		WithHooks(turnObserver(&foreignTurns)),
	)
	if err != nil {
		t.Fatalf("Define foreign: %v", err)
	}
	foreignSession, err := foreignRig.NewSession(ctx)
	if err != nil {
		t.Fatalf("foreign NewSession: %v", err)
	}
	if _, err := foreignSession.Submit(ctx, []content.Block{&content.TextBlock{Text: "foreign"}}); err != nil {
		t.Fatalf("foreign Submit: %v", err)
	}
	if got := foreignTurns.Load(); got != 0 {
		t.Fatalf("foreign engine triggered %d native turn hooks", got)
	}
	if err := foreignSession.Shutdown(ctx); err != nil {
		t.Fatalf("foreign Shutdown: %v", err)
	}
}

type hookIntegrationCounter struct {
	capability contextcount.CounterCapability
}

func (*hookIntegrationCounter) CountContext(context.Context, inference.Request) (contextcount.ContextCount, error) {
	return contextcount.ContextCount{
		Model:       validModel("compact").Key(),
		InputTokens: 12,
		Quality:     contextcount.CountQualityExactLocal,
	}, nil
}
func (c *hookIntegrationCounter) CounterCapability() contextcount.CounterCapability {
	return c.capability
}

type hookIntegrationCompactionLLM struct {
	mu          sync.Mutex
	streamCalls int
}

func (l *hookIntegrationCompactionLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	l.mu.Lock()
	l.streamCalls++
	l.mu.Unlock()
	return (&hookIntegrationTextLLM{}).Stream(context.Background(), inference.Request{})
}

func (*hookIntegrationCompactionLLM) Invoke(_ context.Context, request inference.Request) (*inference.Response, error) {
	var input struct {
		Basis struct {
			Revision       event.ContextRevision `json:"revision"`
			ThroughEventID uuid.UUID             `json:"through_event_id"`
		} `json:"basis"`
		Model struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"model"`
		RequestFingerprint string `json:"request_fingerprint"`
	}
	text := ""
	if len(request.Messages) > 0 {
		if message, ok := request.Messages[len(request.Messages)-1].(*content.UserMessage); ok && len(message.Blocks) > 0 {
			if block, ok := message.Blocks[0].(*content.TextBlock); ok {
				text = block.Text
			}
		}
	}
	if err := json.Unmarshal([]byte(text), &input); err != nil {
		return nil, err
	}
	output, err := json.Marshal(map[string]any{
		"version": loop.CompactionWireV1,
		"basis": map[string]any{
			"revision":         input.Basis.Revision,
			"through_event_id": input.Basis.ThroughEventID,
		},
		"model": map[string]string{
			"provider": input.Model.Provider,
			"model":    input.Model.Model,
		},
		"request_fingerprint": input.RequestFingerprint,
		"summary":             "<conversation_summary><goal>done</goal><constraints></constraints><decisions></decisions><state>ready</state><open_items></open_items></conversation_summary>",
	})
	if err != nil {
		return nil, err
	}
	return &inference.Response{
		Message: &content.AIMessage{Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: string(output)}},
		}},
		Usage:        &content.Usage{OutputTokens: 2},
		FinishReason: stream.FinishReasonStop,
	}, nil
}

func TestHooksIntegrationHustleInferenceIsNotNativeInference(t *testing.T) {
	client := &hookIntegrationCompactionLLM{}
	compactionModel := validModel("compact")
	compactionModel.Limits.WindowTokens = 100
	compactionModel.Limits.MaxInputTokens = 80
	compactionModel.Limits.MaxOutputTokens = 20
	capability := contextcount.CounterCapability{
		Transport:    contextcount.CounterTransportLocal,
		Retention:    contextcount.RetentionNone,
		TokenizerRev: "hook-integration-v1",
		Quality:      contextcount.CountQualityExactLocal,
	}
	definition := mustDefine(
		loop.WithName("agent"),
		loop.WithInference(client, compactionModel),
		loop.WithContextCounter(&hookIntegrationCounter{capability: capability}),
		loop.WithInferenceCapability(contextcount.InferenceCapability{
			Transport: contextcount.InferenceTransportLocal,
			Retention: contextcount.RetentionNone,
		}),
		loop.WithCompaction(loop.CompactionPolicy{
			CounterPolicy:      loop.CounterPolicyRequireExact,
			KeepRecentSegments: 1, KeepRecentTokens: 10000,
			ReservedOutput:   20,
			MaxSummaryTokens: 10,
			CountTimeout:     time.Second,
			Hustle:           "context.compact",
		}),
	)
	compactor, err := hustle.Define(
		hustle.WithName("context.compact"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(5*time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024 * 1024, OutputBytes: 1024 * 1024}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("compact", "v1"),
		hustle.WithPolicyRevision("v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	var nativeInferences atomic.Int32
	var compactions atomic.Int32
	store := sessionStoreT(t)
	defined, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithHustles(compactor),
		WithHustleLimits(validHustleLimits()),
		WithHooks(hook.Set{Around: []hook.Around{
			{
				Operation: hook.OperationInference,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					nativeInferences.Add(1)
					return ctx, nil
				},
			},
			{
				Operation: hook.OperationCompaction,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					compactions.Add(1)
					return ctx, nil
				},
			},
		}}),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	live, err := defined.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = live.Shutdown(context.Background()) })
	waitForHookIntegrationTurn(t, ctx, live, "seed")
	nativeInferences.Store(0)
	sub, err := live.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()
	if _, err := live.Compact(ctx); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	for {
		select {
		case delivery := <-sub.Events():
			switch delivery.Event.(type) {
			case event.CompactionCommitted:
				if got := compactions.Load(); got != 1 {
					t.Fatalf("compaction hooks = %d, want 1", got)
				}
				if got := nativeInferences.Load(); got != 0 {
					t.Fatalf("hustle execution triggered %d native inference hooks", got)
				}
				return
			case event.CompactionRejected:
				t.Fatalf("compaction failed: %#v", delivery.Event)
			}
		case <-ctx.Done():
			t.Fatalf("Compact timed out: %v", ctx.Err())
		}
	}
}
