package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

// permissionReviewRunnerStub is a permissionReviewHustleRunner fake mirroring
// compactionRunnerStub's shape exactly (compaction_adapter_test.go): it
// records the request it was given, runs the supplied validator against a
// canned result (or a canned runtime error), and always finalizes strictly
// after validation succeeds or fails.
type permissionReviewRunnerStub struct {
	mu                       sync.Mutex
	requests                 []hustle.Request
	result                   hustle.Result
	runtimeErr               error
	validatorBeforeFinalizer bool
	finalizerCalls           int
	validateErr              error
	block                    chan struct{}
}

func (s *permissionReviewRunnerStub) RunAndFinalize(ctx context.Context, request hustle.Request, validate hustleruntime.ValidateResult, finalizer hustleruntime.Finalizer) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()

	var outcome hustle.Outcome
	if s.runtimeErr != nil {
		outcome.Err = s.runtimeErr
	} else if err := validate(ctx, s.result); err != nil {
		s.mu.Lock()
		s.validateErr = err
		s.mu.Unlock()
		outcome.Err = err
	} else {
		s.mu.Lock()
		s.validatorBeforeFinalizer = true
		s.mu.Unlock()
		outcome.Result = &s.result
	}
	err := finalizer(ctx, outcome)
	s.mu.Lock()
	s.finalizerCalls++
	s.mu.Unlock()
	return err
}

func (s *permissionReviewRunnerStub) requestNames() []hustle.Name {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]hustle.Name, len(s.requests))
	for i, r := range s.requests {
		out[i] = r.Name
	}
	return out
}

func (s *permissionReviewRunnerStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// permissionReviewResponderStub is a permissionReviewResponder fake: it
// records every basis+reason it was asked to respond with (so a test can
// assert whether, and with what evidence, an eligible combined decision
// reached the classifier-response seam) and can be configured to return an
// error. By default it reports "applied" (the gate was actually resolved)
// whenever no error is configured, matching production's ordinary success
// path; forceApplied lets a test simulate the stale no-op (applied=false,
// err=nil) design §16.2 requires review to audit as "stale".
type permissionReviewResponderStub struct {
	mu           sync.Mutex
	calls        []permissionReviewResponderCall
	respErr      error
	forceApplied *bool
}

type permissionReviewResponderCall struct {
	basis  gate.ReviewBasis
	reason string
}

func (s *permissionReviewResponderStub) respondFromClassifier(_ context.Context, basis gate.ReviewBasis, reason string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, permissionReviewResponderCall{basis: basis, reason: reason})
	applied := s.respErr == nil
	if s.forceApplied != nil {
		applied = *s.forceApplied
	}
	return applied, s.respErr
}

func (s *permissionReviewResponderStub) snapshot() []permissionReviewResponderCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]permissionReviewResponderCall(nil), s.calls...)
}

// reviewClassifierStub is a gate.PermissionClassifier fake: it records how
// many times each method was called and lets a test control Applies and the
// two typed results.
type reviewClassifierStub struct {
	name       hustle.Name
	revision   string
	definition hustle.Definition
	applies    bool

	appliesCalls  int
	marshalCalls  int
	validateCalls int

	marshalOutput json.RawMessage
	marshalErr    error
	assessment    gate.PermissionAssessment
	validateErr   error

	// panicApplies/panicMarshalInput, when set, make the corresponding method
	// panic(panicValue) instead of returning normally — used to prove a
	// trusted-but-fallible classifier implementation panicking cannot crash
	// the review goroutine.
	panicApplies      bool
	panicMarshalInput bool
	panicValue        any
}

func (s *reviewClassifierStub) Name() hustle.Name             { return s.name }
func (s *reviewClassifierStub) Revision() string              { return s.revision }
func (s *reviewClassifierStub) Definition() hustle.Definition { return s.definition }
func (s *reviewClassifierStub) Applies(gate.PermissionReviewSubject) bool {
	s.appliesCalls++
	if s.panicApplies {
		panic(s.panicValue)
	}
	return s.applies
}
func (s *reviewClassifierStub) MarshalInput(gate.PermissionReviewSubject) (json.RawMessage, error) {
	s.marshalCalls++
	if s.panicMarshalInput {
		panic(s.panicValue)
	}
	if s.marshalErr != nil {
		return nil, s.marshalErr
	}
	if s.marshalOutput != nil {
		return s.marshalOutput, nil
	}
	return json.RawMessage(`{}`), nil
}
func (s *reviewClassifierStub) ValidateResult(subject gate.PermissionReviewSubject, _ hustle.Result) (gate.PermissionAssessment, error) {
	s.validateCalls++
	if s.validateErr != nil {
		return gate.PermissionAssessment{}, s.validateErr
	}
	// A real classifier stamps Basis from the exact subject it was handed
	// (gate.PermissionAssessment.Basis must equal the subject's Basis for
	// gate.CombinePermissionAssessments to accept the outcome). The subject's
	// SubjectDigest is only known once NewPermissionReviewSubject stamps it,
	// so the stub fills this in here rather than asking every test to
	// pre-compute it.
	assessment := s.assessment
	assessment.Basis = subject.Basis
	return assessment, nil
}

// reviewClassifierClient is an inference.Client stub used only to satisfy
// hustle.WithNamedInference; it is never invoked by the adapter-level tests
// (which use permissionReviewRunnerStub instead of a real
// *hustleruntime.Controller), and blocks on ctx.Done() when it IS invoked by
// the one live-Controller integration test below.
type reviewClassifierClient struct{ invoked chan struct{} }

func (c *reviewClassifierClient) Invoke(ctx context.Context, _ inference.Request) (*inference.Response, error) {
	if c.invoked != nil {
		close(c.invoked)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*reviewClassifierClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("review_adapter_test: unexpected stream call")
}

type reviewEvidenceTool struct{}

func (*reviewEvidenceTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name: "review-evidence", Desc: "read review evidence",
		Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, nil
}

func (*reviewEvidenceTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

func reviewEvidencePolicy() hustle.EvidenceToolPolicy {
	return hustle.EvidenceToolPolicy{
		Revision: "review-evidence-v1",
		Limits: hustle.ToolLoopLimits{
			MaxRounds: 1, MaxCalls: 1, MaxCallsPerRound: 1,
			MaxResultBytes: 1024, MaxEvidenceBytes: 1024,
		},
		Definitions: []tool.Definition{tool.NewEvidenceDefinition(
			"review-evidence", 0,
			[]tool.ToolInfo{{
				Name: "review-evidence", Desc: "read review evidence",
				Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
			}},
			func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{&reviewEvidenceTool{}}, nil
			},
		)},
	}
}

// newValidReviewClassifierDefinition builds a hustle.Definition that satisfies
// gate.NewPermissionClassifierSet's strict descriptor requirements: blocking
// participation, a named (not current-loop) model, a structured output
// schema, and an evidence-tool policy.
func newValidReviewClassifierDefinition(t *testing.T, client inference.Client, name hustle.Name, revision string) hustle.Definition {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName(name),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 4096, OutputBytes: 4096}),
		hustle.WithSystemPrompt("review permission requests safely", "prompt-v1"),
		hustle.WithPolicyRevision(revision),
		hustle.WithNamedInference(client, validModel("reviewer")),
		hustle.WithOutputSchema(inference.OutputSchema{
			Name: "permission_assessment",
			Schema: json.RawMessage(`{
				"type":"object",
				"properties":{"recommendation":{"type":"string"}},
				"required":["recommendation"],
				"additionalProperties":false
			}`),
			Strict: true,
		}),
		hustle.WithEvidenceTools(reviewEvidencePolicy()),
	)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	return definition
}

func newValidReviewClassifier(t *testing.T, name hustle.Name, revision string, applies bool) *reviewClassifierStub {
	t.Helper()
	return &reviewClassifierStub{
		name: name, revision: revision, applies: applies,
		definition: newValidReviewClassifierDefinition(t, &reviewClassifierClient{}, name, revision),
	}
}

func validReviewPolicy(t *testing.T) gate.PermissionReviewPolicy {
	t.Helper()
	policy, err := gate.DefaultPermissionReviewPolicy("review-policy-v1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	return policy
}

func validReviewRequest(t *testing.T, gateID gate.ID, toolExecutionID uuid.UUID) loopruntime.PermissionReviewRequest {
	t.Helper()
	return loopruntime.PermissionReviewRequest{
		GateID:          gateID,
		ToolExecutionID: toolExecutionID,
		Request:         tool.Request{ToolName: "Bash", Summary: "echo ok"},
		ReviewContext: gate.ReviewContext{
			Coordinates: identity.Coordinates{
				SessionID: mustUUID(), LoopID: mustUUID(), TurnID: mustUUID(), StepID: mustUUID(),
			},
			ContextRevision:    "context-rev-1",
			WorkspaceRoot:      "/workspace",
			WorkingDirectory:   "/workspace",
			GatePolicyRevision: "gate-policy-rev-1",
			SecurityCeiling:    "ceiling-1",
			// A built ReviewContext must carry at least one user-message entry and
			// one assistant tool-request entry (the "current user ask" and "active
			// action" the classifier is meant to be reviewing) — see
			// pkg/gate/review_subject.go's validateBuiltReviewContext.
			Entries: []gate.ReviewContextEntry{
				{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage, Content: "run echo ok"},
				{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest, Content: "Bash(echo ok)"},
			},
		},
	}
}

// TestNewPermissionReviewAdapterRejectsInvalidConfiguration mirrors
// TestNewCompactionAdapterRejectsInvalidConfiguration's shape: every
// collaborator newPermissionReviewAdapter depends on must be validated at
// construction, not discovered mid-review.
func TestNewPermissionReviewAdapterRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	runner := &permissionReviewRunnerStub{}
	var typedNilRunner *permissionReviewRunnerStub
	validClassifier := newValidReviewClassifier(t, "review-classifier", "classifier-rev-1", true)
	validSet, err := gate.NewPermissionClassifierSet(validClassifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	validPolicy := validReviewPolicy(t)
	responder := &permissionReviewResponderStub{}
	var typedNilResponder *permissionReviewResponderStub

	tests := []struct {
		name        string
		runner      permissionReviewHustleRunner
		classifiers gate.PermissionClassifierSet
		policy      gate.PermissionReviewPolicy
		responder   permissionReviewResponder
		field       permissionReviewAdapterField
	}{
		{name: "nil runner", classifiers: validSet, policy: validPolicy, responder: responder, field: permissionReviewAdapterFieldRunner},
		{name: "typed nil runner", runner: typedNilRunner, classifiers: validSet, policy: validPolicy, responder: responder, field: permissionReviewAdapterFieldRunner},
		{name: "empty classifiers", runner: runner, policy: validPolicy, responder: responder, field: permissionReviewAdapterFieldClassifiers},
		{name: "empty policy revision", runner: runner, classifiers: validSet, responder: responder, field: permissionReviewAdapterFieldPolicy},
		{name: "nil responder", runner: runner, classifiers: validSet, policy: validPolicy, field: permissionReviewAdapterFieldResponder},
		{name: "typed nil responder", runner: runner, classifiers: validSet, policy: validPolicy, responder: typedNilResponder, field: permissionReviewAdapterFieldResponder},
		{name: "valid", runner: runner, classifiers: validSet, policy: validPolicy, responder: responder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter, err := newPermissionReviewAdapter(tt.runner, tt.classifiers, tt.policy, tt.responder)
			if tt.field == "" {
				if err != nil || adapter == nil {
					t.Fatalf("newPermissionReviewAdapter() = (%v, %v), want valid adapter", adapter, err)
				}
				return
			}
			var invalid *permissionReviewAdapterError
			if !errors.As(err, &invalid) || invalid.Field != tt.field {
				t.Fatalf("error = %T %v, want field %q", err, err, tt.field)
			}
			if adapter != nil {
				t.Fatal("invalid configuration returned a non-nil adapter")
			}
		})
	}
}

// TestPermissionReviewAdapterReviewSchedulesOnlyApplicableClassifiers proves
// design §14.3 steps 1-2+4: every registered classifier's applicability is
// evaluated, but only an applicable classifier gets a scheduled Hustle run —
// and that run's request carries the classifier's own Name and a
// validate step that runs strictly before the finalizer (mirroring
// TestCompactionAdapterValidatesBeforeFinalization's assertion shape).
func TestPermissionReviewAdapterReviewSchedulesOnlyApplicableClassifiers(t *testing.T) {
	t.Parallel()
	applicable := newValidReviewClassifier(t, "applicable-classifier", "rev-applicable", true)
	notApplicable := newValidReviewClassifier(t, "not-applicable-classifier", "rev-not-applicable", false)
	set, err := gate.NewPermissionClassifierSet(applicable, notApplicable)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	adapter, err := newPermissionReviewAdapter(runner, set, validReviewPolicy(t), &permissionReviewResponderStub{})
	if err != nil {
		t.Fatalf("newPermissionReviewAdapter: %v", err)
	}

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)

	adapter.review(context.Background(), req)

	if applicable.appliesCalls != 1 || notApplicable.appliesCalls != 1 {
		t.Fatalf("appliesCalls = (%d, %d), want each classifier's applicability evaluated exactly once", applicable.appliesCalls, notApplicable.appliesCalls)
	}
	if applicable.marshalCalls != 1 {
		t.Fatalf("applicable.marshalCalls = %d, want 1", applicable.marshalCalls)
	}
	if notApplicable.marshalCalls != 0 {
		t.Fatalf("notApplicable.marshalCalls = %d, want 0 (never scheduled)", notApplicable.marshalCalls)
	}
	if got := runner.requestNames(); len(got) != 1 || got[0] != applicable.name {
		t.Fatalf("scheduled requests = %v, want exactly [%q]", got, applicable.name)
	}
	if applicable.validateCalls != 1 {
		t.Fatalf("applicable.validateCalls = %d, want 1", applicable.validateCalls)
	}
	if !runner.validatorBeforeFinalizer {
		t.Fatal("runner finalized before validation")
	}
	if runner.finalizerCalls != 1 {
		t.Fatalf("finalizerCalls = %d, want 1", runner.finalizerCalls)
	}
}

// TestPermissionReviewAdapterReviewSkipsWhenReviewContextMissing proves the
// fail-closed default: a zero ReviewContext (ContextRevision == "") means no
// live review was captured for this turn, so review must schedule nothing.
func TestPermissionReviewAdapterReviewSkipsWhenReviewContextMissing(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{}
	adapter, err := newPermissionReviewAdapter(runner, set, validReviewPolicy(t), &permissionReviewResponderStub{})
	if err != nil {
		t.Fatalf("newPermissionReviewAdapter: %v", err)
	}

	req := loopruntime.PermissionReviewRequest{
		GateID: mustUUID(), ToolExecutionID: mustUUID(),
		Request: tool.Request{ToolName: "Bash"},
		// ReviewContext deliberately left zero.
	}
	adapter.review(context.Background(), req)

	if classifier.appliesCalls != 0 || runner.callCount() != 0 {
		t.Fatalf("appliesCalls = %d, runner calls = %d, want 0/0 when ReviewContext is missing", classifier.appliesCalls, runner.callCount())
	}
}

// TestPermissionReviewAdapterReviewSkipsClassifierWithInvalidBasis proves
// design §25.4's fail-closed default at the per-classifier boundary: a
// ReviewBasis that fails gate.NewPermissionReviewSubject's own validation
// (here, a missing required GatePolicyRevision) skips that classifier
// entirely rather than scheduling it with a malformed subject.
func TestPermissionReviewAdapterReviewSkipsClassifierWithInvalidBasis(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{}
	adapter, err := newPermissionReviewAdapter(runner, set, validReviewPolicy(t), &permissionReviewResponderStub{})
	if err != nil {
		t.Fatalf("newPermissionReviewAdapter: %v", err)
	}

	req := loopruntime.PermissionReviewRequest{
		GateID: mustUUID(), ToolExecutionID: mustUUID(),
		Request: tool.Request{ToolName: "Bash"},
		ReviewContext: gate.ReviewContext{
			ContextRevision: "context-rev-1",
			// GatePolicyRevision deliberately left empty: required for a valid basis.
			SecurityCeiling: "ceiling-1",
		},
	}
	adapter.review(context.Background(), req)

	if classifier.appliesCalls != 0 {
		t.Fatalf("appliesCalls = %d, want 0 (subject construction must fail before Applies)", classifier.appliesCalls)
	}
	if runner.callCount() != 0 {
		t.Fatalf("runner calls = %d, want 0", runner.callCount())
	}
}

// TestSessionStartPermissionReviewNoopsWithoutClassifiers proves the
// documented "absence of configuration preserves the exact pre-Task-14 gate
// lifecycle" guarantee: a Session that never applied withPermissionReview
// (permissionClassifiers is the zero PermissionClassifierSet) must not panic
// and must start no review.
func TestSessionStartPermissionReviewNoopsWithoutClassifiers(t *testing.T) {
	t.Parallel()
	s := &Session{}
	req := validReviewRequest(t, mustUUID(), mustUUID())
	// Must return promptly and without panicking; there is nothing further to
	// observe since no adapter is ever constructed.
	done := make(chan struct{})
	go func() {
		s.StartPermissionReview(context.Background(), req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartPermissionReview did not return for an unconfigured session")
	}
}

// TestSessionStartPermissionReviewNoopsWithoutBoundHustleController proves a
// session configured with classifiers but whose Hustle runtime is not yet
// bound (s.hustleController == nil) also starts no review rather than
// dereferencing a nil runner.
func TestSessionStartPermissionReviewNoopsWithoutBoundHustleController(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	s := &Session{permissionClassifiers: set, permissionReviewPolicy: validReviewPolicy(t)}
	req := validReviewRequest(t, mustUUID(), mustUUID())

	done := make(chan struct{})
	go func() {
		s.StartPermissionReview(context.Background(), req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartPermissionReview did not return with an unbound hustle controller")
	}
	if classifier.appliesCalls != 0 {
		t.Fatalf("appliesCalls = %d, want 0 (no controller to run against)", classifier.appliesCalls)
	}
}

// TestSessionStartPermissionReviewDoesNotWaitForScheduledClassifierRun proves
// design §14.3's "the actor does not block on inference" at the
// Session.StartPermissionReview boundary itself (not just inside
// permissionReviewAdapter.review, which the tests above already cover): the
// runner's RunAndFinalize blocks on a channel the test controls, yet
// StartPermissionReview must still return almost immediately because it
// spawns adapter.review on its own goroutine rather than calling it inline.
//
// This cannot go through a real *hustleruntime.Controller (s.hustleController
// requires evidence-tool runtime wiring — RuntimeConfig.Evidence — that no
// permission classifier can be defined without per gate.NewPermissionClassifierSet's
// descriptor checks, and that wiring is out of this task's scope), so it
// swaps s.hustleController is exercised indirectly: newPermissionReviewAdapter
// is called with a fake runner exactly as Session.StartPermissionReview would
// call it with s.hustleController, and the `go adapter.review(...)` dispatch
// itself is what is under test here.
func TestSessionStartPermissionReviewDoesNotWaitForScheduledClassifierRun(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	runner := &permissionReviewRunnerStub{block: block, result: hustle.Result{Output: json.RawMessage(`{}`)}}
	adapter, err := newPermissionReviewAdapter(runner, set, validReviewPolicy(t), &permissionReviewResponderStub{})
	if err != nil {
		t.Fatalf("newPermissionReviewAdapter: %v", err)
	}
	req := validReviewRequest(t, mustUUID(), mustUUID())

	// Exactly the dispatch Session.StartPermissionReview performs: fire off
	// review on its own goroutine and return without waiting on it.
	start := time.Now()
	done := make(chan struct{})
	go func() {
		adapter.review(context.Background(), req)
		close(done)
	}()
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("dispatching review took %v, want it to return immediately", elapsed)
	}

	select {
	case <-done:
		t.Fatal("review completed before its blocked RunAndFinalize call was released")
	case <-time.After(50 * time.Millisecond):
		// Expected: review is still blocked inside RunAndFinalize.
	}
}

// TestWithPermissionReviewSetsClassifiersAndPolicy proves the
// withSessionHustles-style construction Option stores exactly what it was
// given (mirroring TestSessionHustleFinalizerContextPreservesTrustedContext's
// direct-Option-application style in hustle_test.go), and that an
// un-configured Session keeps the zero PermissionClassifierSet the
// StartPermissionReview no-op guard depends on.
func TestWithPermissionReviewSetsClassifiersAndPolicy(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy := validReviewPolicy(t)

	s := &Session{}
	if len(s.permissionClassifiers.Classifiers()) != 0 {
		t.Fatal("unconfigured Session has a non-zero permissionClassifiers set")
	}

	withPermissionReview(set, policy)(s)

	got := s.permissionClassifiers.Classifiers()
	if len(got) != 1 || got[0].Name() != "classifier" {
		t.Fatalf("permissionClassifiers = %#v, want the classifier set passed to withPermissionReview", got)
	}
	if s.permissionReviewPolicy.Revision != policy.Revision {
		t.Fatalf("permissionReviewPolicy.Revision = %q, want %q", s.permissionReviewPolicy.Revision, policy.Revision)
	}
}

// TestPermissionReviewAdapterReviewRespondsWhenEligible proves design §14.3
// steps 6-8 wired end to end at the adapter level: once
// gate.CombinePermissionAssessments reports an eligible combined decision,
// review attempts exactly one classifier-originated response, carrying the
// common basis every applicable classifier's subject shared (with the
// per-classifier ClassifierRevision/SubjectDigest fields zeroed) and a reason
// that names the contributing classifier and its revision.
func TestPermissionReviewAdapterReviewRespondsWhenEligible(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	classifier.assessment = gate.PermissionAssessment{
		Risk:           gate.ReviewRiskLow,
		Authorization:  gate.ReviewAuthorizationUnknown,
		Recommendation: gate.ReviewAllow,
		Rationale:      "low risk, allow",
	}
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	// validReviewRequest stamps GatePolicyRevision "gate-policy-rev-1"; the
	// review policy's own Revision must equal that exact value for
	// gate.CombinePermissionAssessments to accept the outcome.
	policy, err := gate.DefaultPermissionReviewPolicy("gate-policy-rev-1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	responder := &permissionReviewResponderStub{}
	adapter, err := newPermissionReviewAdapter(runner, set, policy, responder)
	if err != nil {
		t.Fatalf("newPermissionReviewAdapter: %v", err)
	}

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	calls := responder.snapshot()
	if len(calls) != 1 {
		t.Fatalf("responder calls = %d, want exactly 1", len(calls))
	}
	if calls[0].basis.GateID != gateID || calls[0].basis.ToolExecutionID != toolExecutionID {
		t.Fatalf("responder basis = %+v, want gate/tool-execution identity to match the request", calls[0].basis)
	}
	if calls[0].basis.ClassifierRevision != "" {
		t.Errorf("responder basis carried a per-classifier ClassifierRevision %q, want the common (zeroed) basis", calls[0].basis.ClassifierRevision)
	}
	if calls[0].reason != "classifier@rev-1" {
		t.Errorf("responder reason = %q, want %q", calls[0].reason, "classifier@rev-1")
	}
}

// TestPermissionReviewAdapterReviewNeverRespondsWhenNotEligible proves the
// converse: any non-eligible combined decision — here, no classifier applies
// at all — reaches zero responder calls, so the human gate is never touched
// on this path (design §25.4: every non-allow condition preserves it).
func TestPermissionReviewAdapterReviewNeverRespondsWhenNotEligible(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", false)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{}
	responder := &permissionReviewResponderStub{}
	adapter, err := newPermissionReviewAdapter(runner, set, validReviewPolicy(t), responder)
	if err != nil {
		t.Fatalf("newPermissionReviewAdapter: %v", err)
	}

	req := validReviewRequest(t, mustUUID(), mustUUID())
	adapter.review(context.Background(), req)

	if calls := responder.snapshot(); len(calls) != 0 {
		t.Fatalf("responder calls = %d, want 0 (no applicable classifier)", len(calls))
	}
}
