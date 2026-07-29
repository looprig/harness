package sessionruntime

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/storage/memstore"
)

// --- loopReviewContext unit tests -------------------------------------------------

// TestLoopReviewContextNilWithoutClassifiers is the regression proof for the
// default (pre-addendum) case: a session with no permission classifiers
// registered gets a nil loopReviewContext, so every Loop it builds keeps
// review-context capture OFF, byte-identical to before this addendum. This is
// checked directly on the field (not just "the turn didn't fail"), because a
// nil-by-luck outcome and a nil-by-construction guarantee look identical from
// the outside otherwise.
func TestLoopReviewContextNilWithoutClassifiers(t *testing.T) {
	t.Parallel()
	s := &Session{wsRoot: "/managed/workspace"}
	if got := s.loopReviewContext(); got != nil {
		t.Fatalf("loopReviewContext() = %+v, want nil (no classifiers configured)", got)
	}
}

// TestLoopReviewContextNilWithoutWorkspaceRoot proves the defense-in-depth
// half of loopReviewContext's guard: even with classifiers configured, an
// empty s.wsRoot must never produce a ReviewContext that would guarantee
// every future gated call's capture fails closed. In practice classifiers
// configured without a workspace root cannot reach a live session at all
// (every classifier's hustle.Definition requires evidence tools, and
// newHustleController's pre-existing fail-closed check already refuses
// construction without a populated ReadWorkspace) — this test exercises the
// guard directly, independent of that invariant.
func TestLoopReviewContextNilWithoutWorkspaceRoot(t *testing.T) {
	t.Parallel()
	classifier := newApplyProbeClassifier(t, "probe-classifier", "probe-rev-1")
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	s := &Session{permissionClassifiers: set, permissionReviewPolicy: validReviewPolicy(t)}
	if got := s.loopReviewContext(); got != nil {
		t.Fatalf("loopReviewContext() = %+v, want nil (no workspace root)", got)
	}
}

// TestLoopReviewContextAutoDerivedWhenClassifiersConfigured proves the
// auto-derive decision: once at least one permission classifier is
// registered, loopReviewContext returns a populated configuration with no
// further consumer input — WorkspaceRoot/WorkingDirectory sourced from
// s.wsRoot, GatePolicyRevision from the SAME s.permissionReviewPolicy.Revision
// the first addendum already wires, and a non-empty SecurityCeiling/Policy
// (the documented sentinel defaults — see loopReviewContext's doc comment).
func TestLoopReviewContextAutoDerivedWhenClassifiersConfigured(t *testing.T) {
	t.Parallel()
	classifier := newApplyProbeClassifier(t, "probe-classifier", "probe-rev-1")
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy := validReviewPolicy(t)
	s := &Session{permissionClassifiers: set, permissionReviewPolicy: policy, wsRoot: "/managed/workspace"}

	got := s.loopReviewContext()
	if got == nil {
		t.Fatal("loopReviewContext() = nil, want an auto-derived configuration")
	}
	if got.WorkspaceRoot != "/managed/workspace" || got.WorkingDirectory != "/managed/workspace" {
		t.Fatalf("WorkspaceRoot/WorkingDirectory = %q/%q, want both %q", got.WorkspaceRoot, got.WorkingDirectory, "/managed/workspace")
	}
	if got.GatePolicyRevision != policy.Revision {
		t.Fatalf("GatePolicyRevision = %q, want %q", got.GatePolicyRevision, policy.Revision)
	}
	if got.SecurityCeiling == "" {
		t.Fatal("SecurityCeiling is empty, want a documented sentinel value")
	}
	if got.Policy.Revision == "" || got.Policy.MaxBytes <= 0 || got.Policy.MaxEntries <= 0 {
		t.Fatalf("Policy = %+v, want a populated default gate.ReviewContextPolicy", got.Policy)
	}
}

// --- real Submit() end-to-end capture proof ---------------------------------------

// reviewContextCaptureProbeClassifier is a registered gate.PermissionClassifier
// whose Applies method captures the FULL gate.PermissionReviewSubject the
// session's real permissionReviewAdapter built for it — reached only via
// Session.StartPermissionReview, itself invoked by the loop actor only after
// a genuine permission gate opens during a genuine Submit()/runTurn. It
// always reports itself inapplicable (never schedules a Hustle run): this
// addendum is about proving live capture reaches the classifier with real
// content, not about classifier inference (Task 24's job — see
// TestWithLifecyclePermissionReviewThreadsToNewSessionAndRestoreSession's
// identical scoping note for applyProbeClassifier).
type reviewContextCaptureProbeClassifier struct {
	name       hustle.Name
	revision   string
	definition hustle.Definition
	captured   chan gate.PermissionReviewSubject
}

func (c *reviewContextCaptureProbeClassifier) Name() hustle.Name             { return c.name }
func (c *reviewContextCaptureProbeClassifier) Revision() string              { return c.revision }
func (c *reviewContextCaptureProbeClassifier) Definition() hustle.Definition { return c.definition }

func (c *reviewContextCaptureProbeClassifier) Applies(subject gate.PermissionReviewSubject) bool {
	select {
	case c.captured <- subject:
	default:
	}
	return false
}

func (c *reviewContextCaptureProbeClassifier) MarshalInput(gate.PermissionReviewSubject) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func (c *reviewContextCaptureProbeClassifier) ValidateResult(subject gate.PermissionReviewSubject, _ hustle.Result) (gate.PermissionAssessment, error) {
	return gate.PermissionAssessment{Basis: subject.Basis}, nil
}

func newReviewContextCaptureProbeClassifier(t *testing.T, name hustle.Name, revision string) *reviewContextCaptureProbeClassifier {
	t.Helper()
	return &reviewContextCaptureProbeClassifier{
		name:       name,
		revision:   revision,
		definition: newValidReviewClassifierDefinition(t, &reviewClassifierClient{}, name, revision),
		captured:   make(chan gate.PermissionReviewSubject, 1),
	}
}

// TestSubmitCarriesRealReviewContextIntoRegisteredClassifier is THE test this
// addendum exists to make possible: it registers a real permission
// classifier, submits a real user turn through the genuine, unmodified
// Submit() path, lets a real gate.NewInteractiveEvaluator open a real
// permission gate on a real gated tool call, and proves the loop actor's own
// StartPermissionReview call (never invoked by the test — reached only
// through the real gate-open path) hands the registered classifier a
// gate.ReviewContext whose Entries genuinely reflect the submitted
// conversation. Before this addendum, cfg.reviewContext was nil for every
// real Submit() (Task 13's capture mechanism had zero non-test production
// callers), so this property was unprovable without a private-method
// shortcut.
func TestSubmitCarriesRealReviewContextIntoRegisteredClassifier(t *testing.T) {
	t.Parallel()

	seq := &atomic.Int64{}
	tl := &gatedE2ETool{seq: seq}
	writer := &orderedRuleWriter{seq: seq}
	evaluator, err := gate.NewInteractiveEvaluator(
		[]gate.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), writer, nil)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}

	classifier := newReviewContextCaptureProbeClassifier(t, "context-probe-classifier", "context-probe-rev-1")
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy := validReviewPolicy(t)

	ws := mustWorkspaceStore(t, memstore.New().Blobs)
	root := t.TempDir()

	s, err := newTestSession(context.Background(), gatedE2EDefinition(t, evaluator, tl),
		withSessionHustles([]hustle.Definition{testHustleDefinition(t, "unrelated-background-hustle")}, testHustleLimits()),
		withPermissionReview(set, policy),
		WithWorkspaceCheckpointing(ws, root),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if s.loopReviewContext() == nil {
		t.Fatal("loopReviewContext() = nil, want the auto-derived configuration reaching this session's loops")
	}

	marker := "capture-marker-" + mustUUID().String()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Submit(ctx, []content.Block{&content.TextBlock{Text: marker}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	var subject gate.PermissionReviewSubject
	select {
	case subject = <-classifier.captured:
	case <-time.After(20 * time.Second):
		t.Fatal("StartPermissionReview never reached the registered classifier through a real Submit()")
	}

	// Finding 1 (Phase 6 spec-compliance review): reviewOne's own
	// publishReviewCompleted(ReviewStatusNotApplicable) call for this
	// classifier already durably (checked) appends a PermissionReviewCompleted
	// audit event by the time Applies has run and returned — before this
	// addendum's fix, that append was ALWAYS rejected at the hub boundary
	// (Internal-visibility events cannot travel Session.PublishEventChecked's
	// public-only path), which faults the whole session on the next append.
	// This test previously reported PASS despite that firing silently
	// underneath it, because it never checked post-review session-fault
	// state. Poll briefly: the audit publish races this goroutine's receive
	// from classifier.captured (both happen inside the same reviewOne call,
	// synchronously, right after Applies returns).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := s.FaultErr(); err != nil {
			t.Fatalf("session faulted after a completed review: %v (permission-review audit publication must durably succeed, never fault the session)", err)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if subject.Context.ContextRevision == "" {
		t.Fatal("captured ReviewContext.ContextRevision is empty; capture was never genuinely attempted")
	}
	if subject.Context.WorkspaceRoot != root || subject.Context.WorkingDirectory != root {
		t.Fatalf("captured WorkspaceRoot/WorkingDirectory = %q/%q, want both %q", subject.Context.WorkspaceRoot, subject.Context.WorkingDirectory, root)
	}
	if len(subject.Context.Entries) == 0 {
		t.Fatal("captured ReviewContext has no entries")
	}
	found := false
	for _, e := range subject.Context.Entries {
		if strings.Contains(e.Content, marker) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("captured entries do not reflect the real submitted user message %q: %+v", marker, subject.Context.Entries)
	}

	// Drain the turn to a terminal so the test does not leave a permission
	// gate open (and its awaiting goroutines live) past the test's return.
	open := s.ListGates(context.Background())
	if len(open) == 1 {
		_ = s.RespondGate(context.Background(), gate.GateResponse{
			GateID: open[0].ID,
			Action: string(gate.ApprovalDeny),
			Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
		})
	}
}
