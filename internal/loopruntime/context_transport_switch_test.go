package loopruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

// effectiveConfigCapture is a race-free observer for the test-only
// runtimeConfig.afterEffectiveConfigChange seam (config.go): the actor invokes it
// synchronously on the actor goroutine, strictly BEFORE the corresponding command's Ack send
// (same goroutine, sequential statements). A test that installs the capture before issuing a
// SetLoopMode/ChangeLoopInference and then blocks on that command's Ack (via
// sendSetMode/sendChange, change_test.go) therefore observes the captured value race-free,
// per Go's channel happens-before guarantee — no additional synchronization is required
// beyond the mutex guarding concurrent capture/read.
type effectiveConfigCapture struct {
	mu     sync.Mutex
	latest effectiveConfig
	seen   bool
}

func (c *effectiveConfigCapture) capture(cfg effectiveConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latest, c.seen = cfg, true
}

// last returns the most recently captured effectiveConfig, failing the test if the seam was
// never invoked.
func (c *effectiveConfigCapture) last(t *testing.T) effectiveConfig {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.seen {
		t.Fatal("afterEffectiveConfigChange was never invoked")
	}
	return c.latest
}

// newBoundLoopWithConfig starts a bound loop like newBoundLoop (change_test.go), but lets the
// caller mutate the runtimeConfig resolved from the bound definition before construction. This
// is the only way to install a test-only runtimeConfig seam (afterEffectiveConfigChange) on a
// loop that still validates SetLoopMode/ChangeLoopInference against REAL declared modes and
// ContextTransports: the public New/NewInMode constructors accept no runtimeConfig seam, and
// the raw-config test path (newWithConfig) carries no bound definition at all (every
// SetLoopMode there is refused with ChangeInvalidMode, and ChangeLoopInference never consults
// a bound definition's declared ContextTransport set).
//
// seed is optional (nil for a freshly-constructed loop). Pass a non-nil seed to exercise a
// path that requires a restored-like loop — e.g. startIdleCompactionPreparation (loop.go)
// requires a non-zero ContextBasis before it will run at all
// (basis.Revision != 0 && !basis.ThroughEventID.IsZero()), and a freshly-constructed loop's
// basis starts at the zero value.
func newBoundLoopWithConfig(t *testing.T, bound loop.BoundDefinition, seed *RestoredState, configure func(*runtimeConfig)) (*Loop, *recordingPublisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := &recordingPublisher{}
	cfg, err := configFromBound(bound, "")
	if err != nil {
		t.Fatalf("configFromBound: %v", err)
	}
	if configure != nil {
		configure(&cfg)
	}
	l, err := newLoopWithSeed(ctx, mustID(t), mustID(t), Provenance{}, rec, cfg, bound, bound.InitialMode(), seed)
	if err != nil {
		t.Fatalf("newLoopWithSeed: %v", err)
	}
	return l, rec
}

// idleCandidateCapturingSink is a compactionCandidateSink + contextCompactionAwaiter test
// double modeled on idleCandidateRecordingSink (safe_boundary_compaction_test.go), but it
// captures the FULL compactionExecutionCandidate handed to CoordinateCompactionCandidate —
// the idle-compaction preparation goroutine's output, i.e. call site 1 in loop.go — so a
// test can inspect candidate.Measurement.RequestFingerprint and candidate.Request directly.
// It never awaits a real compaction outcome: proving the pre-execution measurement is
// correct doesn't require driving a full compaction commit.
type idleCandidateCapturingSink struct {
	*recordingCompactionSink
	mu        sync.Mutex
	candidate *compactionExecutionCandidate
}

func (s *idleCandidateCapturingSink) CoordinateCompactionCandidate(_ context.Context, disposition compactionDisposition, candidate compactionExecutionCandidate) error {
	s.mu.Lock()
	captured := candidate
	captured.Transcript = cloneMessages(candidate.Transcript)
	s.candidate = &captured
	s.mu.Unlock()
	return s.CoordinateCompaction(context.Background(), disposition)
}

func (*idleCandidateCapturingSink) AwaitCompaction(context.Context, event.CompactAttemptID) (contextCompactionAwaitResult, error) {
	return contextCompactionAwaitResult{}, errors.New("idleCandidateCapturingSink: await not supported")
}

func (s *idleCandidateCapturingSink) capturedCandidate(t *testing.T) compactionExecutionCandidate {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candidate == nil {
		t.Fatal("CoordinateCompactionCandidate was never invoked")
	}
	return *s.candidate
}

// secondTransportModel is a distinct, VALID model.Model whose transport identity
// (Provider/APIFormat/BaseURL) differs from testModel()'s. crossTransportDefinition declares
// it as a second member of the loop's ContextTransport set.
// Limits are set (unlike testModel(), which carries none) because this fixture's second
// transport is exercised through a REAL turn by TestContextMeasurementUsesEffectiveCapabilityAfterTransportSwitch,
// and loop.ResolveContextLimits fails closed (*loop.ContextLimitUnknownError) on a
// zero-value model.ContextLimits — a live turn on this model needs a resolvable limit to
// reach measureRequestContext at all.
func secondTransportModel() model.Model {
	return model.Model{
		Provider: "second-provider", APIFormat: model.APIFormatAnthropic, BaseURL: "https://second.example.test", Name: "second-model",
		Limits: testContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20},
	}
}

// thirdTransportModel is a distinct, VALID model.Model whose transport is deliberately NEVER
// declared by crossTransportDefinition, so a change to it exercises the existing
// ContextTransportNotDeclaredError refusal.
func thirdTransportModel() model.Model {
	return model.Model{Provider: "third-provider", APIFormat: model.APIFormatGemini, BaseURL: "https://third.example.test", Name: "third-model"}
}

// secondTransportCapability is a genuinely distinct contextcount.InferenceCapability from
// contextTestInferenceCapability() (context_test.go) — different Transport AND Provider — so a
// test can prove a mode/inference change actually RE-RESOLVED the capability rather than
// coincidentally keeping the same value.
func secondTransportCapability() contextcount.InferenceCapability {
	return contextcount.InferenceCapability{
		Provider: "second-provider", Transport: contextcount.InferenceTransportTLS,
		SecurityIdentity: contextcount.SecurityIdentity{0x02}, Retention: contextcount.RetentionNone,
	}
}

// crossTransportDefinition builds a bound definition declaring TWO loop.ContextTransport
// members: testModel()'s base transport (contextTestInferenceCapability) and
// secondTransportModel()'s transport (secondTransportCapability) — plus a mode ("second")
// whose model sits on the second transport. thirdTransportModel()'s transport is deliberately
// left undeclared.
func crossTransportDefinition(t *testing.T, client inference.Client) loop.BoundDefinition {
	t.Helper()
	base := testModel()
	second := secondTransportModel()
	baseCapability := contextTestInferenceCapability()
	counter := &loopContextCounter{capability: contextTestCapability(contextcount.CountQualityExactLocal), counts: []content.TokenCount{40}}
	d, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(client, base),
		loop.WithModes(
			loop.Mode{Name: "base"}, // zero Model: Bind resolves it to the base model/transport.
			loop.Mode{Name: "second", Model: second},
		),
		loop.WithInitialMode("base"),
		loop.WithContextCounter(counter),
		loop.WithInferenceCapability(baseCapability),
		loop.WithContextTransports(
			loop.ContextTransport{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: baseCapability},
			loop.ContextTransport{Provider: second.Provider, APIFormat: second.APIFormat, BaseURL: second.BaseURL, Capability: secondTransportCapability()},
		),
		loop.WithContextObservation(loop.ContextObservationPolicy{ReservedOutput: 20, CountTimeout: time.Second}),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	bound, err := d.Bind(context.Background(), tool.Bindings{SessionID: mustID(t), LoopID: mustID(t)})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return bound
}

// TestSetModeReResolvesCapabilityAcrossTransport proves a SetLoopMode into a mode whose model
// sits on a DIFFERENT declared ContextTransport re-resolves the loop's effective
// InferenceCapability to that transport's declared value — not the frozen base-transport value
// resolveMode's InferenceCapability field would (wrongly) report for a non-base mode.
func TestSetModeReResolvesCapabilityAcrossTransport(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := crossTransportDefinition(t, llm)
	var capture effectiveConfigCapture
	l, _ := newBoundLoopWithConfig(t, bound, nil, func(cfg *runtimeConfig) {
		cfg.afterEffectiveConfigChange = capture.capture
	})

	res := sendSetMode(t, l, "second")
	if res.Err != nil {
		t.Fatalf("SetLoopMode(second) err = %v", res.Err)
	}
	want := secondTransportCapability()
	if got := capture.last(t); got.inferenceCapability != want {
		t.Fatalf("effective capability after SetMode(second) = %+v, want %+v", got.inferenceCapability, want)
	}
}

// TestChangeInferenceReResolvesCapabilityAcrossTransport proves a direct ChangeLoopInference
// (staying in the base mode) to a model on a DIFFERENT declared ContextTransport likewise
// re-resolves the effective InferenceCapability.
func TestChangeInferenceReResolvesCapabilityAcrossTransport(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := crossTransportDefinition(t, llm)
	var capture effectiveConfigCapture
	l, _ := newBoundLoopWithConfig(t, bound, nil, func(cfg *runtimeConfig) {
		cfg.afterEffectiveConfigChange = capture.capture
	})

	res := sendChange(t, l, command.ChangeLoopInference{Model: secondTransportModel(), SetModel: true})
	if res.Err != nil {
		t.Fatalf("ChangeLoopInference(second) err = %v", res.Err)
	}
	if res.Mode != "base" {
		t.Fatalf("committed mode = %q, want unchanged base (a direct inference change never touches mode)", res.Mode)
	}
	want := secondTransportCapability()
	if got := capture.last(t); got.inferenceCapability != want {
		t.Fatalf("effective capability after ChangeLoopInference(second) = %+v, want %+v", got.inferenceCapability, want)
	}
}

// TestChangeInferenceUndeclaredTransportRefusedAndCapabilityUnchanged is a REGRESSION check
// (not new behavior) that a ChangeLoopInference to a model on an UNDECLARED transport is still
// refused with ChangeInvalidModel wrapping a *loop.ContextTransportNotDeclaredError (already
// enforced by ValidateContextModel), AND proves the all-or-nothing contract this task must
// preserve: the refusal touches NOTHING, including the effective capability — a subsequent
// successful effort-only change (which never re-resolves capability, since it never sets a
// model) still reports the loop's ORIGINAL base-transport capability, proving the refused
// change did not silently corrupt state.effective.
func TestChangeInferenceUndeclaredTransportRefusedAndCapabilityUnchanged(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := crossTransportDefinition(t, llm)
	var capture effectiveConfigCapture
	l, _ := newBoundLoopWithConfig(t, bound, nil, func(cfg *runtimeConfig) {
		cfg.afterEffectiveConfigChange = capture.capture
	})

	res := sendChange(t, l, command.ChangeLoopInference{Model: thirdTransportModel(), SetModel: true})
	var ce *loop.ChangeError
	if !errors.As(res.Err, &ce) || ce.Kind != loop.ChangeInvalidModel {
		t.Fatalf("ChangeLoopInference(third, undeclared) err = %v, want ChangeInvalidModel", res.Err)
	}
	var notDeclared *loop.ContextTransportNotDeclaredError
	if !errors.As(res.Err, &notDeclared) {
		t.Fatalf("ChangeLoopInference(third, undeclared) cause = %v, want *loop.ContextTransportNotDeclaredError", res.Err)
	}

	// The refusal returned before touching state.effective at all, so the ORIGINAL
	// construction-time (base-transport) capability must still be in effect. Prove it via a
	// subsequent successful effort-only change, which never re-resolves capability itself
	// (SetModel is false) and so simply reports whatever capability was already current.
	if res := sendChange(t, l, command.ChangeLoopInference{Effort: testEffortHigh, SetEffort: true}); res.Err != nil {
		t.Fatalf("post-refusal effort-only change err = %v", res.Err)
	}
	want := contextTestInferenceCapability()
	if got := capture.last(t); got.inferenceCapability != want {
		t.Fatalf("effective capability after refused undeclared-transport change = %+v, want unchanged %+v", got.inferenceCapability, want)
	}
}

// TestContextMeasurementUsesEffectiveCapabilityAfterTransportSwitch proves that a LIVE
// context measurement taken during a turn started AFTER a transport-crossing
// ChangeLoopInference reflects state.effective.inferenceCapability (the newly re-resolved
// transport's capability), not the frozen construction-time runtimeConfig.InferenceCapability
// call site 2 (the `case req := <-contextRequests:` goroutine in loop.go) used to read. Every
// live turn's context measurement goes through this exact call site, so it is the
// simplest real behavioral drive available — no extra plumbing is needed to reach it.
//
// The proof works through the RequestFingerprint: measureRequestContext folds
// InferenceCapability into the returned event.ContextMeasurement.RequestFingerprint (see
// contextRequestFingerprint / contextFingerprintTemplateForRequest in context.go). This
// fixture configures no RuntimeContextProvider, so runtimeContextTail is always nil and
// runtimeContextRevision is therefore the FIXED constant revisionDigest(nil) — the one
// otherwise-nondeterministic fingerprint input becomes reproducible, so the test can
// recompute the EXACT expected fingerprint from the actually-counted request (captured by
// loopContextCounter) and the actually-published Basis, once with the correct (new,
// post-switch) capability and once with the WRONG (frozen, base-transport) capability the
// bug would have used — and show the measured fingerprint matches only the former.
func TestContextMeasurementUsesEffectiveCapabilityAfterTransportSwitch(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := crossTransportDefinition(t, llm)
	var counter *loopContextCounter
	l, rec := newBoundLoopWithConfig(t, bound, nil, func(cfg *runtimeConfig) {
		counter = cfg.ContextCounter.(*loopContextCounter)
	})

	if res := sendChange(t, l, command.ChangeLoopInference{Model: secondTransportModel(), SetModel: true}); res.Err != nil {
		t.Fatalf("ChangeLoopInference(second) err = %v", res.Err)
	}

	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "hello"}})
	terminal := drainToTerminal(t, rec)
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("terminal = %T %+v, want TurnDone", terminal, terminal)
	}

	measured, _ := contextEvents(rec.events())
	if measured == nil {
		t.Fatal("ContextMeasured was not published for the post-switch turn")
	}

	// A single turn measures context TWICE (turn.go: once before the primary inference
	// candidate, once more after the final response completes), each admission a
	// distinct Basis, so contextEvents' LAST-wins scan (context_loop_test.go) surfaces the
	// SECOND publish. The measurements run strictly sequentially on the turn goroutine
	// (each measure() call blocks for its reply before the turn proceeds), so the LAST
	// recorded request corresponds exactly to the LAST published ContextMeasured.
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
		t.Fatal("measured fingerprint matches the frozen base-transport capability; call site did not read the re-resolved effective capability")
	}
}

// TestIdleCompactionPreparationUsesEffectiveCapabilityAfterTransportSwitch is the companion
// to TestContextMeasurementUsesEffectiveCapabilityAfterTransportSwitch above: it proves call
// site 1 (the idle-compaction preparation goroutine spawned from startIdleCompactionPreparation
// in loop.go) ALSO reads the per-turn state.effective.inferenceCapability snapshotted into
// idleCompactionPreparation.inferenceCapability (context.go), rather than the frozen
// runtimeConfig.InferenceCapability, once a transport-crossing ChangeLoopInference has run.
// Nothing exercised call site 1 directly before this test — the reviewer flagged that
// confidence in the fix there rested purely on structural symmetry with call site 2, not on
// an executed proof.
//
// The loop must already carry a non-zero ContextBasis for startIdleCompactionPreparation to
// run at all, so this passes a non-nil seed to newBoundLoopWithConfig (a restored-like seed)
// rather than the nil seed the other tests in this file use. A command.Compact sent
// while idle drives compaction admission straight into startIdleCompactionPreparation
// (loop.go: `if state.status == loopIdle { ... startIdleCompactionPreparation(...) }`), and
// idleCandidateCapturingSink (a compactionCandidateSink + contextCompactionAwaiter, the two
// interfaces startIdleCompactionPreparation requires of the sink) captures the resulting
// compactionExecutionCandidate — the direct output of the measureRequestContext call this
// task's fix touches — without needing a real compaction executor.
//
// As in the call-site-2 test, the proof is via RequestFingerprint: this fixture configures
// no RuntimeContextProvider, so runtimeContextRevision is the fixed constant
// revisionDigest(nil), letting the test recompute the exact expected fingerprint from the
// captured candidate's Request and Measurement.Basis for both the correct (new, post-switch)
// capability and the wrong (frozen, base-transport) capability the bug would have used.
func TestIdleCompactionPreparationUsesEffectiveCapabilityAfterTransportSwitch(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := crossTransportDefinition(t, llm)
	sink := &idleCandidateCapturingSink{recordingCompactionSink: newRecordingCompactionSink()}
	seed := RestoredState{
		Msgs:      content.AgenticMessages{replacementTestMessage("history")},
		TurnIndex: 1,
		Basis:     event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{0xd0}},
		HasBasis:  true,
	}
	l, _ := newBoundLoopWithConfig(t, bound, &seed, func(cfg *runtimeConfig) {
		cfg.compactionSink = sink
	})

	if res := sendChange(t, l, command.ChangeLoopInference{Model: secondTransportModel(), SetModel: true}); res.Err != nil {
		t.Fatalf("ChangeLoopInference(second) err = %v", res.Err)
	}

	sendCompact(t, l, mustID(t), mustID(t), mustID(t), identity.AgencyUser)
	select {
	case <-sink.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("idle compaction candidate was never coordinated")
	}

	candidate := sink.capturedCandidate(t)
	counterCapability := contextTestCapability(contextcount.CountQualityExactLocal)

	wantFingerprint, err := contextRequestFingerprint(
		candidate.Request, candidate.Measurement.Basis, revisionDigest(nil), counterCapability, secondTransportCapability(),
	)
	if err != nil {
		t.Fatalf("contextRequestFingerprint(post-switch capability) error = %v", err)
	}
	if candidate.Measurement.RequestFingerprint != wantFingerprint {
		t.Fatalf("idle-compaction measured fingerprint = %x, want fingerprint computed with the post-switch capability %x", candidate.Measurement.RequestFingerprint, wantFingerprint)
	}

	staleFingerprint, err := contextRequestFingerprint(
		candidate.Request, candidate.Measurement.Basis, revisionDigest(nil), counterCapability, contextTestInferenceCapability(),
	)
	if err != nil {
		t.Fatalf("contextRequestFingerprint(frozen base capability) error = %v", err)
	}
	if candidate.Measurement.RequestFingerprint == staleFingerprint {
		t.Fatal("idle-compaction measured fingerprint matches the frozen base-transport capability; call site 1 did not read the re-resolved effective capability")
	}
}

// TestCompactionExecutorPreparesWithPerCandidateInferenceCapability is the executor-level
// companion to the two call-site tests above: it proves compactionExecutor.prepare() derives
// its post-compaction measurement fingerprint from InferenceCapability carried on the
// compactionExecutionCandidate itself, not a value frozen on compactionExecutorConfig at
// construction time (that field is removed entirely by this task — capability is not known
// until a candidate is dispatched, since it can change between executor construction and any
// given compaction attempt via ChangeLoopInference/SetLoopMode).
//
// The proof drives CoordinateCompactionCandidate twice against the SAME executor instance,
// varying only candidate.InferenceCapability between runs (same request, same transcript, same
// counter capability, same settings) and shows the resulting PostCount.Fingerprint template
// produces two DIFFERENT [32]byte digests for an identical basis — a difference that can only
// be explained by prepare() reading candidate.InferenceCapability per call.
func TestCompactionExecutorPreparesWithPerCandidateInferenceCapability(t *testing.T) {
	t.Parallel()
	compactor := &echoExecutorCompactor{summary: validFinalizationSummary()}
	counter := &loopContextCounter{
		capability: contextTestCapability(contextcount.CountQualityExactLocal),
		counts:     []content.TokenCount{40},
	}
	settings := contextAdmissionSettings{ReservedOutput: 20, CountTimeout: time.Second}
	executor, err := newCompactionExecutor(context.Background(), compactionExecutorConfig{
		Compactor: compactor, Counter: counter, CounterCapability: counter.capability,
		Settings: settings, MaxSummaryTokens: 10,
	})
	if err != nil {
		t.Fatalf("newCompactionExecutor() error = %v", err)
	}

	requestModel := testModel()
	requestModel.Limits = testContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
	basis := event.ContextBasis{Revision: 7, ThroughEventID: uuid.UUID{7}}
	fingerprint := [32]byte{8}

	runOnce := func(attemptID event.CompactAttemptID, capability contextcount.InferenceCapability) compactionPostCount {
		t.Helper()
		attempt := validFinalizationAttempt()
		attempt.AttemptID = attemptID
		attempt.Basis = basis
		candidate := compactionExecutionCandidate{
			Measurement: event.ContextMeasurement{
				Basis: basis, Model: requestModel.Key(), RequestFingerprint: fingerprint,
				InputTokens: 40, InputLimit: 80, Quality: contextcount.CountQualityExactLocal,
			},
			Request: inference.Request{
				Model: requestModel, System: "system",
				Messages: content.AgenticMessages{replacementTestMessage("old transcript")},
			},
			RuntimeRevision:     revisionDigest(nil),
			Transcript:          content.AgenticMessages{replacementTestMessage("old transcript")},
			InferenceCapability: capability,
		}
		if err := executor.CoordinateCompactionCandidate(context.Background(), compactionDisposition{
			Kind: compactionDispositionStart, Attempt: &attempt,
		}, candidate); err != nil {
			t.Fatalf("CoordinateCompactionCandidate() error = %v", err)
		}
		result, err := executor.AwaitCompaction(context.Background(), attempt.AttemptID)
		if err != nil {
			t.Fatalf("AwaitCompaction() error = %v", err)
		}
		if result.Disposition != contextCompactionAwaitCommitted || result.Proposal.Success == nil {
			t.Fatalf("result = %+v, want prepared success", result)
		}
		return result.Proposal.Success.PostCount
	}

	basePost := runOnce(event.CompactAttemptID(uuid.UUID{0xa1}), contextTestInferenceCapability())
	switchedPost := runOnce(event.CompactAttemptID(uuid.UUID{0xa2}), secondTransportCapability())

	compareBasis := event.ContextBasis{Revision: 9, ThroughEventID: uuid.UUID{9}}
	baseFingerprint, err := basePost.Fingerprint.Fingerprint(compareBasis)
	if err != nil {
		t.Fatalf("basePost.Fingerprint.Fingerprint() error = %v", err)
	}
	switchedFingerprint, err := switchedPost.Fingerprint.Fingerprint(compareBasis)
	if err != nil {
		t.Fatalf("switchedPost.Fingerprint.Fingerprint() error = %v", err)
	}
	if baseFingerprint == switchedFingerprint {
		t.Fatal("PostCount fingerprint unchanged across different candidate.InferenceCapability values; prepare() did not use the per-candidate capability")
	}
}
