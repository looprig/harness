package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
)

// TestCrossProviderModelSwitchPreservesConversationAndCapability is Task 4.1's Scenario
// A: a LIVE cross-provider switch mid-conversation, driven entirely through the real
// production path (crossTransportDefinition's bound definition + the real
// ChangeLoopInference command + a real compactionExecutor, not a hand-assembled
// fixture). It proves four things in one flow:
//
//  1. The live switch to a second DECLARED transport is accepted.
//  2. A compaction summary established BEFORE the switch survives it — the design's
//     explicit "conversation is preserved across a transport switch" decision (Phase 2).
//  3. The measurement taken on the FIRST turn after the switch reflects the NEWLY
//     re-resolved InferenceCapability, not the stale base-transport one (the same
//     RequestFingerprint proof TestContextMeasurementUsesEffectiveCapabilityAfterTransportSwitch
//     uses, since it is the simplest reliable way to observe which capability a live
//     measurement actually used).
//  4. A further switch to an UNDECLARED transport is refused, and the loop's effective
//     capability is proven unchanged by it (still the SECOND transport's, not silently
//     reverted or corrupted) via the same "effort-only change reports current capability"
//     idiom TestChangeInferenceUndeclaredTransportRefusedAndCapabilityUnchanged uses.
//
// The compaction summary is established via a MANUAL command.Compact against a real
// compactionExecutor (echoExecutorCompactor), not automatic token-budget-triggered
// compaction: the essential claim under test is "a summary that existed before the
// switch is still present after," which a manual trigger proves exactly as well as an
// automatic one, without the fragility of tuning a synthetic token budget.
func TestCrossProviderModelSwitchPreservesConversationAndCapability(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := crossTransportDefinition(t, llm)

	var counter *loopContextCounter
	var capture effectiveConfigCapture
	summary := validFinalizationSummary()
	seed := &RestoredState{
		Msgs:      content.AgenticMessages{replacementTestMessage("prior history"), replacementTestMessage("committed history")},
		TurnIndex: 1,
		Basis:     event.ContextBasis{Revision: 3, ThroughEventID: uuid.UUID{0x10}}, HasBasis: true,
	}
	l, rec := newBoundLoopWithConfig(t, bound, seed, func(cfg *runtimeConfig) {
		counter = cfg.ContextCounter.(*loopContextCounter)
		cfg.afterEffectiveConfigChange = capture.capture
		cfg.Compaction = &loop.CompactionPolicy{KeepRecentSegments: 1, KeepRecentTokens: 10000, MaxSummaryTokens: 10, ReservedOutput: 20, CountTimeout: time.Second, Hustle: "context.compact"}
		executor, err := newCompactionExecutor(context.Background(), compactionExecutorConfig{
			Compactor: &echoExecutorCompactor{summary: summary},
			Counter:   cfg.ContextCounter, CounterCapability: cfg.CounterCapability,
			Settings:         contextAdmissionSettings{ReservedOutput: 20, CountTimeout: time.Second},
			MaxSummaryTokens: 10,
		})
		if err != nil {
			t.Fatalf("newCompactionExecutor: %v", err)
		}
		cfg.compactionSink = executor
	})

	// 1. Real turn on the BASE transport establishes conversation history and, via the
	// live measurement, a non-zero ContextBasis (the precondition startIdleCompactionPreparation
	// requires before it will run at all).
	startTurn(t, l, rec, textBlocks("hello on the base transport"))
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
		t.Fatal("first (base-transport) turn did not reach TurnDone")
	}
	// awaitTerminalAfter (not another drainToTerminal, which always scans from index 0
	// and would re-match this turn's already-recorded terminal) is required to find the
	// SECOND turn's terminal below — the established multi-turn idiom documented on
	// drainToTerminal's own doc comment.
	afterFirstTurn := terminalIndex(rec, 0)

	var sessionID, loopID = mustID(t), mustID(t)
	for _, e := range rec.events() {
		if ts, ok := e.(event.TurnStarted); ok {
			sessionID, loopID = ts.Coordinates.SessionID, ts.Coordinates.LoopID
			break
		}
	}

	// 2. Manually compact the base-transport conversation into `summary` while idle.
	preCompactionEventCount := len(rec.events())
	sendCompact(t, l, sessionID, loopID, mustID(t), identity.AgencyUser)
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for i := preCompactionEventCount; i < len(evs); i++ {
			if _, ok := evs[i].(event.CompactionCommitted); ok {
				return true
			}
		}
		return false
	})

	// 3. Live-switch to the SECOND declared transport.
	if res := sendChange(t, l, command.ChangeLoopInference{Model: secondTransportModel(), SetModel: true}); res.Err != nil {
		t.Fatalf("ChangeLoopInference(second) err = %v, want accepted switch", res.Err)
	}

	// 4. Run the FIRST turn after the switch.
	startTurn(t, l, rec, textBlocks("hello after the switch"))
	if _, ok := awaitTerminalAfter(t, rec, afterFirstTurn).(event.TurnDone); !ok {
		t.Fatal("post-switch turn did not reach TurnDone")
	}

	// Assertion (2): the pre-switch summary is present in the post-switch turn's
	// request base — conversation was preserved across the transport switch.
	postSwitchReq := llm.lastReq()
	if len(postSwitchReq.Messages) < 1 || !reflect.DeepEqual(postSwitchReq.Messages[0], summary) {
		t.Fatalf("post-switch request messages[0] = %#v, want the pre-switch compaction summary %#v",
			firstMessageOrNil(postSwitchReq.Messages), summary)
	}
	second := secondTransportModel()
	if postSwitchReq.Model.Provider != second.Provider || postSwitchReq.Model.APIFormat != second.APIFormat || postSwitchReq.Model.BaseURL != second.BaseURL {
		t.Fatalf("post-switch request model transport = %+v, want %s/%s/%s",
			postSwitchReq.Model, second.Provider, second.APIFormat, second.BaseURL)
	}

	// Assertion (3): the post-switch measurement's fingerprint was computed with the
	// NEWLY re-resolved (second-transport) InferenceCapability, not the stale base one.
	measured, _ := contextEvents(rec.events())
	if measured == nil {
		t.Fatal("ContextMeasured was not published for the post-switch turn")
	}
	counter.mu.Lock()
	if len(counter.requests) < 1 {
		counter.mu.Unlock()
		t.Fatal("counted requests = 0, want at least 1")
	}
	gotRequest := counter.requests[len(counter.requests)-1]
	counter.mu.Unlock()

	wantFingerprint, err := contextRequestFingerprint(
		gotRequest, measured.Measurement.Basis, revisionDigest(nil), counter.capability, secondTransportCapability(),
	)
	if err != nil {
		t.Fatalf("contextRequestFingerprint(post-switch capability) error = %v", err)
	}
	if measured.Measurement.RequestFingerprint != wantFingerprint {
		t.Fatalf("measured fingerprint = %x, want fingerprint computed with the post-switch capability %x", measured.Measurement.RequestFingerprint, wantFingerprint)
	}
	staleFingerprint, err := contextRequestFingerprint(
		gotRequest, measured.Measurement.Basis, revisionDigest(nil), counter.capability, contextTestInferenceCapability(),
	)
	if err != nil {
		t.Fatalf("contextRequestFingerprint(frozen base capability) error = %v", err)
	}
	if measured.Measurement.RequestFingerprint == staleFingerprint {
		t.Fatal("measured fingerprint matches the frozen base-transport capability; the switch did not re-resolve it")
	}

	// Assertion (4): a further switch to an UNDECLARED transport is refused, and the
	// effective capability is provably UNCHANGED by it — still the second transport's,
	// proven by a subsequent successful effort-only change (which never itself
	// re-resolves capability) reporting the same value that was already current.
	res := sendChange(t, l, command.ChangeLoopInference{Model: thirdTransportModel(), SetModel: true})
	var ce *loop.ChangeError
	if !errors.As(res.Err, &ce) || ce.Kind != loop.ChangeInvalidModel {
		t.Fatalf("ChangeLoopInference(third, undeclared) err = %v, want ChangeInvalidModel", res.Err)
	}
	var notDeclared *loop.ContextTransportNotDeclaredError
	if !errors.As(res.Err, &notDeclared) {
		t.Fatalf("ChangeLoopInference(third, undeclared) cause = %v, want *loop.ContextTransportNotDeclaredError", res.Err)
	}
	if res := sendChange(t, l, command.ChangeLoopInference{Effort: testEffortHigh, SetEffort: true}); res.Err != nil {
		t.Fatalf("post-refusal effort-only change err = %v", res.Err)
	}
	wantCapability := secondTransportCapability()
	if got := capture.last(t); got.inferenceCapability != wantCapability {
		t.Fatalf("effective capability after refused undeclared-transport change = %+v, want unchanged %+v (the second transport's)", got.inferenceCapability, wantCapability)
	}
}

func firstMessageOrNil(msgs content.AgenticMessages) content.Conversation {
	if len(msgs) == 0 {
		return nil
	}
	return msgs[0]
}

// TestRestoreAfterLiveTransportSwitch is Task 4.1's Scenario B: a REAL switch-then-
// restore round trip. It drives a genuine live ChangeLoopInference, captures the
// event.ModelRuntime the loop ACTUALLY durably emitted (event.LoopInferenceChanged.Runtime
// — not a hand-constructed RestoredState, which is exactly the shortcut that let Task
// 3.2's write-side bug hide from every other test), and feeds that captured Runtime into
// the REAL NewRestoredWithRuntime entry point. It proves the restored loop's live model
// genuinely reflects the switched-to transport — the assertion that would have caught
// Task 3.2's Critical bug (modelRuntime()/runtimeForModel() never populating
// APIFormat/BaseURL) had it existed at the time, since it drives the real write path
// rather than a fixture assembled to look like it.
func TestRestoreAfterLiveTransportSwitch(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := crossTransportDefinition(t, llm)

	sessionID, loopID := mustID(t), mustID(t)
	rec := &recordingPublisher{}
	l, err := New(context.Background(), sessionID, loopID, Provenance{}, rec, bound)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	second := secondTransportModel()
	if res := sendChange(t, l, command.ChangeLoopInference{Model: second, SetModel: true}); res.Err != nil {
		t.Fatalf("ChangeLoopInference(second) err = %v, want accepted live switch", res.Err)
	}

	// Capture what was ACTUALLY durably emitted for the switch — the real write path,
	// not a fixture standing in for it.
	var captured event.ModelRuntime
	var found bool
	for _, e := range rec.events() {
		if ic, ok := e.(event.LoopInferenceChanged); ok && ic.Runtime.Key == second.Key() {
			captured = ic.Runtime
			found = true
		}
	}
	if !found {
		t.Fatal("no LoopInferenceChanged(second) was published; nothing to restore from")
	}
	if captured.APIFormat != second.APIFormat || captured.BaseURL != second.BaseURL {
		t.Fatalf("captured durable Runtime APIFormat/BaseURL = %q/%q, want %q/%q (the write-side bug this scenario guards against)",
			captured.APIFormat, captured.BaseURL, second.APIFormat, second.BaseURL)
	}

	seed := RestoredState{HasRuntime: true, Runtime: captured}
	restoredRec := &recordingPublisher{}
	l2, err := NewRestoredWithRuntime(context.Background(), sessionID, loopID, Provenance{}, restoredRec, bound, seed, RuntimeDependencies{})
	if err != nil {
		t.Fatalf("NewRestoredWithRuntime: %v", err)
	}
	if l2 == nil {
		t.Fatal("NewRestoredWithRuntime returned a nil loop")
	}

	runOneTurn(t, l2, restoredRec, "post-restore turn")
	got := llm.lastReq().Model
	if got.Provider != second.Provider || got.APIFormat != second.APIFormat || got.BaseURL != second.BaseURL || got.Name != second.Name {
		t.Fatalf("restored loop's live model = %+v, want the switched-to transport %s/%s/%s/%s",
			got, second.Provider, second.APIFormat, second.BaseURL, second.Name)
	}
}

// TestRestoreLegacyPartialTransportRecord is Task 4.1's Scenario C: an exploratory
// regression check for a legacy/partial durable record — a RestoredState whose Runtime
// carries a changed Provider but EMPTY APIFormat/BaseURL, simulating either a
// pre-Task-3.1 journal record or an incompletely-populated one. Restored against
// crossTransportDefinition (whose base transport's Provider differs from the seed's),
// NewRestoredWithRuntime's zero-value graft semantics
// (`if seed.Runtime.APIFormat != "" { override }`) leave APIFormat/BaseURL at the BASE
// mode's transport while the Provider comes from the seed — producing
// {Provider: "second-provider", APIFormat: <base's "openai">, BaseURL: <base's
// "http://localhost:1234">}, a combination that is NOT a member of either declared
// ContextTransport (lookupTransport matches on the exact Provider+APIFormat+BaseURL
// triple; see pkg/loop/context_transport.go). This test proves Task 3.3's hard
// restore-time check (RestoreTransportMismatchError) correctly rejects that incoherent
// combination rather than silently restoring into a nonsensical transport identity.
//
// This was run and observed, not assumed: the incoherent combination IS correctly
// rejected. No production bug was found here.
func TestRestoreLegacyPartialTransportRecord(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := crossTransportDefinition(t, llm)
	base := testModel()

	seed := RestoredState{HasRuntime: true, Runtime: event.ModelRuntime{
		Key: model.ModelKey{Provider: "second-provider", Model: "second-model"},
		// APIFormat and BaseURL are deliberately left zero: the legacy/partial shape
		// under test.
	}}
	rec := &recordingPublisher{}
	l, err := NewRestoredWithRuntime(context.Background(), mustID(t), mustID(t), Provenance{}, rec, bound, seed, RuntimeDependencies{})
	if l != nil {
		t.Fatalf("NewRestoredWithRuntime returned a non-nil loop for an incoherent Provider+APIFormat+BaseURL combination: %+v", l)
	}
	var mismatch *RestoreTransportMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("NewRestoredWithRuntime err = %v, want *RestoreTransportMismatchError (fail-secure rejection of the incoherent grafted transport)", err)
	}
	// The rejected identity is exactly the incoherent combination the graft's
	// zero-value semantics would have produced: the seed's Provider, but the BASE
	// mode's (untouched) APIFormat/BaseURL — never any validly declared transport.
	if mismatch.Provider != "second-provider" || mismatch.APIFormat != base.APIFormat || mismatch.BaseURL != base.BaseURL {
		t.Fatalf("mismatch = %+v, want Provider \"second-provider\" with the BASE transport's APIFormat/BaseURL %q/%q",
			mismatch, base.APIFormat, base.BaseURL)
	}
}
