package sessionruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

// captureSlogDefault redirects the process-wide slog default logger to an
// in-memory buffer for the calling test's duration, restoring the previous
// default in cleanup. review_adapter.go/gates.go log through the top-level
// slog.Warn(Context)/Error(Context) funcs — there is no injected *slog.Logger
// seam — so swapping the default is the only way a test can observe exactly
// what those call sites wrote.
func captureSlogDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// This file exercises Task 17 (design §16/§17): permissionReviewAdapter must
// publish secret-free PermissionReviewStarted/PermissionReviewCompleted audit
// events for every terminal status it can produce, never leak review content
// (context, command, evidence/model output, prompt, rationale) into any
// durably-appended event, and route an audit-APPEND failure through the same
// session fault path Task 14's ordinary Hustle faults use — while an ordinary
// EXPECTED classifier outcome (needs_human/failed/timed_out/cancelled/stale/
// not_applicable) never touches that fault path at all.

// reviewAuditPublisherStub is a permissionReviewAuditPublisher fake that
// validates and marshals every event exactly as hub.PublishInternalEventChecked
// does (event.ValidateEvent then event.MarshalEvent), so a test can inspect
// the REAL wire bytes a durable append would have produced — not just Go
// struct fields. failWhen, when set, makes matching publishes fail with
// failErr instead of recording the event, so a test can simulate a selective
// durable append failure.
//
// This stub deliberately does NOT exercise Hub.validateInternalPublication's
// own boundary rules (visibility/class/session/type allowlist) — it is a
// content/behavior fake for review_adapter's OWN logic, not a substitute for
// the real-Hub boundary coverage in pkg/hub/permission_review_publish_test.go
// and TestSubmitCarriesRealReviewContextIntoRegisteredClassifier's FaultErr
// assertion: that is precisely the gap that let the
// PermissionReviewStarted/Completed allowlist omission ship undetected.
type reviewAuditPublisherStub struct {
	mu        sync.Mutex
	events    []event.Event
	marshaled [][]byte
	failWhen  func(event.Event) bool
	failErr   error
}

func (s *reviewAuditPublisherStub) PublishInternalEventChecked(_ context.Context, ev event.Event) error {
	if err := event.ValidateEvent(ev); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWhen != nil && s.failWhen(ev) {
		return s.failErr
	}
	data, err := event.MarshalEvent(ev)
	if err != nil {
		return err
	}
	s.events = append(s.events, ev)
	s.marshaled = append(s.marshaled, data)
	return nil
}

func (s *reviewAuditPublisherStub) snapshot() ([]event.Event, [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event.Event(nil), s.events...), append([][]byte(nil), s.marshaled...)
}

func (s *reviewAuditPublisherStub) started() []event.PermissionReviewStarted {
	events, _ := s.snapshot()
	var out []event.PermissionReviewStarted
	for _, ev := range events {
		if started, ok := ev.(event.PermissionReviewStarted); ok {
			out = append(out, started)
		}
	}
	return out
}

func (s *reviewAuditPublisherStub) completed() []event.PermissionReviewCompleted {
	events, _ := s.snapshot()
	var out []event.PermissionReviewCompleted
	for _, ev := range events {
		if completed, ok := ev.(event.PermissionReviewCompleted); ok {
			out = append(out, completed)
		}
	}
	return out
}

// reviewAuditFaultReporterStub is a permissionReviewFaultReporter fake that
// records every fault it is asked to report.
type reviewAuditFaultReporterStub struct {
	mu     sync.Mutex
	faults []error
}

func (s *reviewAuditFaultReporterStub) ReportFault(_ context.Context, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, cause)
}

func (s *reviewAuditFaultReporterStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.faults)
}

// newAuditTestAdapter builds a valid permissionReviewAdapter and wires it
// with a REAL *event.Factory stamper (mirroring Session.StartPermissionReview's
// production wiring exactly) plus fresh publisher/fault-reporter fakes.
func newAuditTestAdapter(
	t *testing.T,
	runner permissionReviewHustleRunner,
	classifiers gate.PermissionClassifierSet,
	policy gate.PermissionReviewPolicy,
	responder permissionReviewResponder,
) (*permissionReviewAdapter, *reviewAuditPublisherStub, *reviewAuditFaultReporterStub) {
	t.Helper()
	adapter, err := newPermissionReviewAdapter(runner, classifiers, policy, responder)
	if err != nil {
		t.Fatalf("newPermissionReviewAdapter: %v", err)
	}
	pub := &reviewAuditPublisherStub{}
	faults := &reviewAuditFaultReporterStub{}
	adapter.publisher = pub
	adapter.stamper = event.NewFactory(func() (uuid.UUID, error) { return mustUUID(), nil }, func() time.Time { return time.Now() })
	adapter.faults = faults
	adapter.auditTimeout = 5 * time.Second
	return adapter, pub, faults
}

// assertCompletedIdentity checks the fields every PermissionReviewCompleted
// must carry regardless of status: GateID/ToolExecutionID/Classifier/
// ClassifierRevision, and Internal visibility.
func assertCompletedIdentity(t *testing.T, got event.PermissionReviewCompleted, gateID gate.ID, toolExecutionID uuid.UUID, classifier hustle.Name, revision string) {
	t.Helper()
	if got.GateID != gateID {
		t.Errorf("GateID = %v, want %v", got.GateID, gateID)
	}
	if got.ToolExecutionID != toolExecutionID {
		t.Errorf("ToolExecutionID = %v, want %v", got.ToolExecutionID, toolExecutionID)
	}
	if got.Classifier != classifier {
		t.Errorf("Classifier = %q, want %q", got.Classifier, classifier)
	}
	if got.ClassifierRevision != revision {
		t.Errorf("ClassifierRevision = %q, want %q", got.ClassifierRevision, revision)
	}
	if got.Visibility() != event.Internal {
		t.Errorf("Visibility() = %v, want Internal", got.Visibility())
	}
	if err := event.ValidateEvent(got); err != nil {
		t.Errorf("ValidateEvent(completed) error = %v", err)
	}
}

// --- one test per terminal status (design §16.2's exact validation rules) ---

func TestPermissionReviewAdapterAuditsAllowed(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	classifier.assessment = gate.PermissionAssessment{
		Risk: gate.ReviewRiskLow, Authorization: gate.ReviewAuthorizationUnknown,
		Categories:     []gate.ReviewRiskCategory{gate.ReviewCategoryMutableNetwork},
		Recommendation: gate.ReviewAllow, Rationale: "low risk, allow",
	}
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy, err := gate.DefaultPermissionReviewPolicy("gate-policy-rev-1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, policy, responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	got := completedEvents[0]
	assertCompletedIdentity(t, got, gateID, toolExecutionID, "classifier", "rev-1")
	if got.Status != gate.ReviewStatusAllowed {
		t.Errorf("Status = %q, want allowed", got.Status)
	}
	if !got.AutoApproved {
		t.Error("AutoApproved = false, want true")
	}
	if got.Risk != gate.ReviewRiskLow || got.Authorization != gate.ReviewAuthorizationUnknown {
		t.Errorf("Risk/Authorization = %q/%q, want low/unknown", got.Risk, got.Authorization)
	}
	if len(got.Categories) != 1 || got.Categories[0] != gate.ReviewCategoryMutableNetwork {
		t.Errorf("Categories = %v, want [mutable_network]", got.Categories)
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0", faults.count())
	}
}

func TestPermissionReviewAdapterAuditsNeedsHuman(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	classifier.assessment = gate.PermissionAssessment{
		Risk: gate.ReviewRiskLow, Authorization: gate.ReviewAuthorizationUnknown,
		Categories: []gate.ReviewRiskCategory{gate.ReviewCategoryDestructiveLocal},
		// A non-"allow" recommendation makes the combined decision ineligible
		// (ReviewDecisionRecommendation) even though THIS classifier's own
		// Hustle validated cleanly (Status stays Allowed).
		Recommendation: gate.ReviewNeedsHuman, Rationale: "needs a human look",
	}
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy, err := gate.DefaultPermissionReviewPolicy("gate-policy-rev-1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, policy, responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	got := completedEvents[0]
	assertCompletedIdentity(t, got, gateID, toolExecutionID, "classifier", "rev-1")
	if got.Status != gate.ReviewStatusNeedsHuman {
		t.Errorf("Status = %q, want needs_human", got.Status)
	}
	if got.AutoApproved {
		t.Error("AutoApproved = true, want false")
	}
	if got.Risk != gate.ReviewRiskLow || got.Authorization != gate.ReviewAuthorizationUnknown {
		t.Errorf("Risk/Authorization = %q/%q, want low/unknown", got.Risk, got.Authorization)
	}
	if len(got.Categories) != 1 || got.Categories[0] != gate.ReviewCategoryDestructiveLocal {
		t.Errorf("Categories = %v, want [destructive_local]", got.Categories)
	}
	if len(responder.snapshot()) != 0 {
		t.Error("responder called, want 0 (ineligible decision must never touch the gate)")
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0", faults.count())
	}
}

func TestPermissionReviewAdapterAuditsNotApplicable(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", false)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	if len(pub.started()) != 0 {
		t.Errorf("started events = %d, want 0 (a non-applicable classifier's Hustle is never scheduled)", len(pub.started()))
	}
	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	got := completedEvents[0]
	assertCompletedIdentity(t, got, gateID, toolExecutionID, "classifier", "rev-1")
	if got.Status != gate.ReviewStatusNotApplicable {
		t.Errorf("Status = %q, want not_applicable", got.Status)
	}
	if got.AutoApproved || got.Risk != "" || got.Authorization != "" || len(got.Categories) != 0 {
		t.Errorf("not_applicable carried assessment data: %#v", got)
	}
	if runner.callCount() != 0 {
		t.Errorf("runner calls = %d, want 0", runner.callCount())
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0", faults.count())
	}
}

func TestPermissionReviewAdapterAuditsStale(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	classifier.assessment = gate.PermissionAssessment{
		Risk: gate.ReviewRiskLow, Authorization: gate.ReviewAuthorizationUnknown,
		Recommendation: gate.ReviewAllow, Rationale: "low risk, allow",
	}
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy, err := gate.DefaultPermissionReviewPolicy("gate-policy-rev-1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	notApplied := false
	// The combined decision IS eligible (this classifier's own assessment
	// passes policy), but the responder reports the gate response was
	// silently dropped as stale (design §16.2: the GATE's fate decides
	// AutoApproved, never the classifier's own opinion).
	responder := &permissionReviewResponderStub{forceApplied: &notApplied}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, policy, responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	if len(responder.snapshot()) != 1 {
		t.Fatalf("responder calls = %d, want 1 (eligible decision must still attempt a response)", len(responder.snapshot()))
	}
	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	got := completedEvents[0]
	assertCompletedIdentity(t, got, gateID, toolExecutionID, "classifier", "rev-1")
	if got.Status != gate.ReviewStatusStale {
		t.Errorf("Status = %q, want stale", got.Status)
	}
	if got.AutoApproved || got.Risk != "" || got.Authorization != "" || len(got.Categories) != 0 {
		t.Errorf("stale carried assessment/auto-approval data: %#v", got)
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0", faults.count())
	}
}

func TestPermissionReviewAdapterAuditsFailed(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	classifier.validateErr = errors.New("malformed classifier output")
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	if len(pub.started()) != 1 {
		t.Fatalf("started events = %d, want 1", len(pub.started()))
	}
	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	got := completedEvents[0]
	assertCompletedIdentity(t, got, gateID, toolExecutionID, "classifier", "rev-1")
	if got.Status != gate.ReviewStatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.AutoApproved || got.Risk != "" || got.Authorization != "" || len(got.Categories) != 0 {
		t.Errorf("failed carried assessment data: %#v", got)
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0 (an ordinary classifier failure is not an integrity fault)", faults.count())
	}
}

func TestPermissionReviewAdapterAuditsTimedOut(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{runtimeErr: &hustleruntime.RunError{
		Name: "classifier", Stage: hustle.StageInference, ReasonCode: hustle.ReasonTimeout,
	}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	got := completedEvents[0]
	assertCompletedIdentity(t, got, gateID, toolExecutionID, "classifier", "rev-1")
	if got.Status != gate.ReviewStatusTimedOut {
		t.Errorf("Status = %q, want timed_out", got.Status)
	}
	if got.AutoApproved || got.Risk != "" || got.Authorization != "" || len(got.Categories) != 0 {
		t.Errorf("timed_out carried assessment data: %#v", got)
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0", faults.count())
	}
}

func TestPermissionReviewAdapterAuditsCancelled(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{runtimeErr: &hustleruntime.RunError{
		Name: "classifier", Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled,
	}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	got := completedEvents[0]
	assertCompletedIdentity(t, got, gateID, toolExecutionID, "classifier", "rev-1")
	if got.Status != gate.ReviewStatusCancelled {
		t.Errorf("Status = %q, want cancelled", got.Status)
	}
	if got.AutoApproved || got.Risk != "" || got.Authorization != "" || len(got.Categories) != 0 {
		t.Errorf("cancelled carried assessment data: %#v", got)
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0", faults.count())
	}
}

// TestPermissionReviewAdapterCancelledReviewContextIsAudited proves the OTHER
// source of "cancelled" (design §15's outer review-lifecycle cancellation —
// gate resolve/owner close/loop interrupt/session shutdown/policy timeout —
// as opposed to the Hustle runtime's own typed ReasonCanceled) also
// classifies and audits as cancelled, using an actually-cancelled ctx rather
// than a typed runner error.
func TestPermissionReviewAdapterCancelledReviewContextIsAudited(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	// The stub itself does not check ctx, so it "succeeds" mechanically, but
	// classifyReviewFailureStatus consults ctx.Err() directly — outcome.Err
	// must still be non-nil for the !allowed branch to run at all, so pair
	// the cancelled ctx with a plain runtime error (as a real cancelled
	// RunAndFinalize call would also return one).
	runner := &permissionReviewRunnerStub{runtimeErr: errors.New("context canceled mid-run")}
	responder := &permissionReviewResponderStub{}
	adapter, pub, _ := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	adapter.review(ctx, req)

	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	if got := completedEvents[0].Status; got != gate.ReviewStatusCancelled {
		t.Errorf("Status = %q, want cancelled", got)
	}
}

// --- Started/Completed pairing ---

func TestPermissionReviewAdapterStartedCompletedPairing(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "pairing-classifier", "pairing-rev-1", true)
	classifier.assessment = gate.PermissionAssessment{
		Risk: gate.ReviewRiskLow, Authorization: gate.ReviewAuthorizationUnknown,
		Recommendation: gate.ReviewAllow, Rationale: "ok",
	}
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy, err := gate.DefaultPermissionReviewPolicy("gate-policy-rev-1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	adapter, pub, _ := newAuditTestAdapter(t, runner, set, policy, &permissionReviewResponderStub{})

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)
	adapter.review(context.Background(), req)

	startedEvents := pub.started()
	completedEvents := pub.completed()
	if len(startedEvents) != 1 || len(completedEvents) != 1 {
		t.Fatalf("started = %d, completed = %d, want 1/1", len(startedEvents), len(completedEvents))
	}
	started, completed := startedEvents[0], completedEvents[0]
	if started.GateID != completed.GateID {
		t.Errorf("GateID mismatch: started=%v completed=%v", started.GateID, completed.GateID)
	}
	if started.ToolExecutionID != completed.ToolExecutionID {
		t.Errorf("ToolExecutionID mismatch: started=%v completed=%v", started.ToolExecutionID, completed.ToolExecutionID)
	}
	if started.Classifier != completed.Classifier {
		t.Errorf("Classifier mismatch: started=%q completed=%q", started.Classifier, completed.Classifier)
	}
	if started.ClassifierRevision != completed.ClassifierRevision {
		t.Errorf("ClassifierRevision mismatch: started=%q completed=%q", started.ClassifierRevision, completed.ClassifierRevision)
	}
	if started.GateID != gateID || started.ToolExecutionID != toolExecutionID {
		t.Errorf("started identity = %+v, want gate=%v tool_execution=%v", started, gateID, toolExecutionID)
	}
	if started.Classifier != "pairing-classifier" || started.ClassifierRevision != "pairing-rev-1" {
		t.Errorf("started classifier identity = %q@%q, want pairing-classifier@pairing-rev-1", started.Classifier, started.ClassifierRevision)
	}
}

// --- secret scan ---

// TestPermissionReviewAdapterNeverLeaksSecretsIntoDurableEvents runs a
// realistic full review — a real gate.PermissionClassifierSet built from a
// real hustle.Definition (evidence tools, structured-output-with-tools, a
// real system prompt), real ReviewContext content, a real command/requirement
// description, a real classifier-produced PermissionAssessment carrying a
// rationale, and a "model output" payload — with a distinctive marker seeded
// into every one of those content channels: context, command, prompt/model
// input (MarshalInput's rendered payload — literally what becomes the
// classifier's inference request), evidence/model output (hustle.Result.Output
// — the only reachable analog at this layer for evidence-derived content;
// evidence-tool execution itself lives entirely inside hustleruntime, never
// surfacing to review_adapter), and rationale. A second classifier fails
// validation with an error that WRAPS a distinct marker, exercising the
// specific risk the task calls out: an error-wrapping path (e.g. `%v` of
// arbitrary content) that could accidentally route secret text into a
// durably-appended event.
//
// It then marshals every event this run durably appended (via
// reviewAuditPublisherStub, which validates+marshals exactly as
// hub.PublishEventChecked does) PLUS a realistic HustleStarted/HustleCompleted
// pair built from the SAME classifier's real Definition().Descriptor() and
// RunID (representing exactly what a real *hustleruntime.Controller run for
// this same invocation would durably append via
// internal/hustleruntime/audit.go's publishStarted/publishCompleted — those
// types structurally carry only Definition/RunID/Runtime/Duration/Usage/
// Stage/ReasonCode, no free-text field exists to leak through), and asserts
// NONE of the seeded markers appear anywhere in the combined marshaled bytes.
func TestPermissionReviewAdapterNeverLeaksSecretsIntoDurableEvents(t *testing.T) {
	t.Parallel()

	const (
		markerContext   = "SECRET-CONTEXT-MARKER-7b2e44"
		markerCommand   = "SECRET-COMMAND-MARKER-9f3d1a"
		markerPrompt    = "SECRET-PROMPT-MARKER-3e19aa"
		markerOutput    = "SECRET-OUTPUT-MARKER-4c81ff"
		markerRationale = "SECRET-RATIONALE-MARKER-1a9e02"
		markerFailWrap  = "SECRET-FAILWRAP-MARKER-0dcab8"
	)

	allowedClassifier := newValidReviewClassifier(t, "allowed-classifier", "rev-allowed-1", true)
	allowedClassifier.marshalOutput = json.RawMessage(fmt.Sprintf(`{"prompt":%q}`, markerPrompt))
	allowedClassifier.assessment = gate.PermissionAssessment{
		Risk: gate.ReviewRiskLow, Authorization: gate.ReviewAuthorizationUnknown,
		Categories:     []gate.ReviewRiskCategory{gate.ReviewCategoryMutableNetwork},
		Recommendation: gate.ReviewAllow,
		Rationale:      "classifier reasoning: " + markerRationale,
	}

	failingClassifier := newValidReviewClassifier(t, "failing-classifier", "rev-failing-1", true)
	failingClassifier.validateErr = fmt.Errorf("malformed output near %q: %s", "field", markerFailWrap)

	set, err := gate.NewPermissionClassifierSet(allowedClassifier, failingClassifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy, err := gate.DefaultPermissionReviewPolicy("gate-policy-rev-1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{
		Output: json.RawMessage(fmt.Sprintf(`{"evidence_output":%q}`, markerOutput)),
	}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, policy, responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := loopruntime.PermissionReviewRequest{
		GateID: gateID, ToolExecutionID: toolExecutionID,
		Request: tool.Request{
			ToolName: "Bash",
			Summary:  "run a command: " + markerCommand,
			Requirements: []tool.Requirement{{
				Kind: "tool.invoke", Scope: "Bash", Match: markerCommand,
				Description: "shell command containing " + markerCommand,
			}},
		},
		ReviewContext: gate.ReviewContext{
			Coordinates: identity.Coordinates{
				SessionID: mustUUID(), LoopID: mustUUID(), TurnID: mustUUID(), StepID: mustUUID(),
			},
			ContextRevision:    "context-rev-1",
			WorkspaceRoot:      "/workspace",
			WorkingDirectory:   "/workspace",
			GatePolicyRevision: "gate-policy-rev-1",
			SecurityCeiling:    "ceiling-1",
			Entries: []gate.ReviewContextEntry{
				{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage, Content: "please run this: " + markerContext},
				{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest, Content: "Bash(" + markerContext + ")"},
			},
		},
	}
	adapter.review(context.Background(), req)

	if faults.count() != 0 {
		t.Fatalf("faults = %d, want 0", faults.count())
	}

	_, marshaled := pub.snapshot()
	if len(marshaled) == 0 {
		t.Fatal("no events were durably appended by this review")
	}

	// A realistic companion HustleStarted/HustleCompleted pair for the SAME
	// classifier run, structurally faithful to
	// internal/hustleruntime/audit.go's publishStarted/publishCompleted.
	hustleHeader := event.Header{
		// HustleStarted/HustleCompleted are session-scoped, exactly like
		// internal/hustleruntime/audit.go's own auditHeader — only SessionID,
		// never LoopID/TurnID/StepID.
		Coordinates:     identity.Coordinates{SessionID: req.ReviewContext.Coordinates.SessionID},
		Cause:           identity.Cause{Coordinates: req.ReviewContext.Coordinates},
		EventID:         mustUUID(),
		CreatedAt:       time.Now(),
		EventVisibility: event.Internal,
	}
	startDescriptor := event.HustleRunDescriptor{
		Definition: allowedClassifier.Definition().Descriptor(),
		RunID:      hustle.RunID(mustUUID()),
	}
	completedDescriptor := startDescriptor
	completedDescriptor.Runtime = event.ModelRuntime{
		Key:    model.ModelKey{Provider: "provider", Model: "model"},
		Limits: model.ContextLimits{WindowTokens: 100},
	}
	hustleStarted := event.HustleStarted{Header: hustleHeader, Run: startDescriptor}
	hustleCompleted := event.HustleCompleted{
		Header: hustleHeader, Run: completedDescriptor, Duration: time.Second,
		Usage: &content.Usage{InputTokens: 10, OutputTokens: 5},
	}
	for _, hev := range []event.Event{hustleStarted, hustleCompleted} {
		data, err := event.MarshalEvent(hev)
		if err != nil {
			t.Fatalf("MarshalEvent(%T) error = %v", hev, err)
		}
		marshaled = append(marshaled, data)
	}

	markers := []string{markerContext, markerCommand, markerPrompt, markerOutput, markerRationale, markerFailWrap}
	for index, data := range marshaled {
		for _, marker := range markers {
			if bytes.Contains(data, []byte(marker)) {
				t.Errorf("event[%d] leaks secret marker %q: %s", index, marker, data)
			}
		}
	}
}

// TestPermissionReviewAdapterMarshalInputFailureNeverLeaksErrorText proves a
// classifier-controlled MarshalInput failure — an unconstrained error owned
// entirely by the classifier implementation (unlike
// gate.NewPermissionReviewSubject's bounded, content-free typed error, or
// classifyReviewFailureStatus's typed-reason-only classification) — never
// reaches operational logs verbatim. A classifier could embed raw
// subject/command/context content in its MarshalInput error text; reviewOne's
// slog.Warn call for this path must not repeat that text.
func TestPermissionReviewAdapterMarshalInputFailureNeverLeaksErrorText(t *testing.T) {
	// Deliberately NOT t.Parallel(): captureSlogDefault swaps the process-wide
	// slog default for the duration of this test; running concurrently with
	// another test that also redirects the default risks this test's own log
	// line landing in a different test's buffer instead of this one's.
	const markerMarshalErr = "SECRET-MARSHALINPUT-MARKER-6af0e1"

	classifier := newValidReviewClassifier(t, "marshal-fail-classifier", "rev-1", true)
	classifier.marshalErr = fmt.Errorf("encode failed near payload %q", markerMarshalErr)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy, err := gate.DefaultPermissionReviewPolicy("gate-policy-rev-1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	responder := &permissionReviewResponderStub{}
	adapter, _, _ := newAuditTestAdapter(t, runner, set, policy, responder)

	logs := captureSlogDefault(t)
	req := validReviewRequest(t, mustUUID(), mustUUID())
	adapter.review(context.Background(), req)

	if strings.Contains(logs.String(), markerMarshalErr) {
		t.Fatalf("classifier MarshalInput error text leaked into logs: %s", logs.String())
	}
}

// --- unrecovered classifier panics must not crash the review goroutine ---
//
// Session.StartPermissionReview runs adapter.review on its own goroutine with
// no recover anywhere above it; an unrecovered panic there would terminate
// the ENTIRE process, taking down every concurrent session, not just this
// gate's review. classifier.Applies and classifier.MarshalInput are called
// directly on that goroutine (reviewOne), so a trusted-but-fallible
// classifier implementation that panics in either must be recovered and
// turned into a bounded, content-free ReviewStatusFailed outcome instead.

// TestPermissionReviewAdapterAppliesPanicNeverCrashesAndFailsClassifierClosed
// proves a panic inside classifier.Applies does not propagate out of
// adapter.review (if it did, this test's process would crash rather than
// merely fail), that the panicking classifier's Hustle is never scheduled,
// that no gate response is ever attempted (the gate must stay human, not be
// auto-approved by a classifier that just panicked), and that the classifier
// is audited as failed (not not_applicable — an ambiguous applicability
// determination is never treated as a confident "does not apply").
func TestPermissionReviewAdapterAppliesPanicNeverCrashesAndFailsClassifierClosed(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	classifier.panicApplies = true
	classifier.panicValue = errors.New("boom: applicability check exploded")
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)

	// If Applies' panic were not recovered, this call would crash the whole
	// test process rather than return normally.
	adapter.review(context.Background(), req)

	if classifier.appliesCalls != 1 {
		t.Fatalf("appliesCalls = %d, want 1", classifier.appliesCalls)
	}
	if runner.callCount() != 0 {
		t.Fatalf("runner calls = %d, want 0 (a classifier whose applicability check panicked must never be scheduled)", runner.callCount())
	}
	if len(responder.snapshot()) != 0 {
		t.Fatalf("responder calls = %d, want 0 (a panic must never let the gate auto-approve)", len(responder.snapshot()))
	}
	if len(pub.started()) != 0 {
		t.Errorf("started events = %d, want 0 (applicability was never confirmed)", len(pub.started()))
	}
	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	got := completedEvents[0]
	assertCompletedIdentity(t, got, gateID, toolExecutionID, "classifier", "rev-1")
	if got.Status != gate.ReviewStatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.AutoApproved || got.Risk != "" || got.Authorization != "" || len(got.Categories) != 0 {
		t.Errorf("failed carried assessment data: %#v", got)
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0 (a recovered classifier panic is an ordinary expected failure, not an integrity fault)", faults.count())
	}
}

// TestPermissionReviewAdapterMarshalInputPanicNeverCrashesOrLeaksMarker is the
// panic-path companion to TestPermissionReviewAdapterMarshalInputFailureNeverLeaksErrorText:
// classifier.MarshalInput panicking with a value that embeds raw
// classifier-controlled content must not crash the review goroutine, and the
// recovered panic value must never reach a log line or any durably-appended
// event — matching the exact content-free discipline the ordinary
// (non-panic) MarshalInput error path already follows.
func TestPermissionReviewAdapterMarshalInputPanicNeverCrashesOrLeaksMarker(t *testing.T) {
	// Deliberately NOT t.Parallel(): captureSlogDefault swaps the process-wide
	// slog default for the duration of this test.
	const marker = "secret-marker-xyz-marshalinput-panic-7c3e9a"

	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	classifier.panicMarshalInput = true
	classifier.panicValue = "secret-marker-xyz-marshalinput-panic-7c3e9a: rm -rf /some/path"
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	logs := captureSlogDefault(t)
	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)

	// If MarshalInput's panic were not recovered at the pkg/gate registry
	// boundary, this call would crash the whole test process.
	adapter.review(context.Background(), req)

	if strings.Contains(logs.String(), marker) {
		t.Fatalf("classifier MarshalInput panic value leaked into logs: %s", logs.String())
	}
	if runner.callCount() != 0 {
		t.Fatalf("runner calls = %d, want 0 (a classifier whose input marshal panicked must never be scheduled)", runner.callCount())
	}
	if len(responder.snapshot()) != 0 {
		t.Fatalf("responder calls = %d, want 0 (a panic must never let the gate auto-approve)", len(responder.snapshot()))
	}
	_, marshaled := pub.snapshot()
	for index, data := range marshaled {
		if bytes.Contains(data, []byte(marker)) {
			t.Errorf("event[%d] leaks recovered panic marker: %s", index, data)
		}
	}
	completedEvents := pub.completed()
	if len(completedEvents) != 1 {
		t.Fatalf("completed events = %d, want 1: %#v", len(completedEvents), completedEvents)
	}
	if got := completedEvents[0].Status; got != gate.ReviewStatusFailed {
		t.Errorf("Status = %q, want failed", got)
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0 (a recovered classifier panic is an ordinary expected failure, not an integrity fault)", faults.count())
	}
}

// TestPermissionReviewAdapterOnePanickingClassifierLeavesOtherClassifierCorrectlyCombined
// registers two classifiers for the same gate: one panics in Applies, the
// other applies normally and would, on its own, be eligible to auto-approve.
// Per design §11's conjunctive combination ("every applicable classifier
// must produce a locally eligible allow"), a classifier whose own
// applicability could not be confidently determined must be treated as
// applicable-but-failed — never as silently not-applicable — so it cannot be
// excluded from the conjunction and let the OTHER classifier's allow decide
// the whole gate. The well-behaved classifier must still run to completion
// (proving one classifier's panic does not take down review of the other),
// but the combined decision must stay ineligible and the gate must stay
// human: no classifier-originated response is ever attempted.
func TestPermissionReviewAdapterOnePanickingClassifierLeavesOtherClassifierCorrectlyCombined(t *testing.T) {
	t.Parallel()
	panicking := newValidReviewClassifier(t, "panicking-classifier", "rev-panicking", true)
	panicking.panicApplies = true
	panicking.panicValue = "boom"

	wellBehaved := newValidReviewClassifier(t, "well-behaved-classifier", "rev-well-behaved", true)
	wellBehaved.assessment = gate.PermissionAssessment{
		Risk: gate.ReviewRiskLow, Authorization: gate.ReviewAuthorizationUnknown,
		Categories:     []gate.ReviewRiskCategory{gate.ReviewCategoryMutableNetwork},
		Recommendation: gate.ReviewAllow, Rationale: "low risk, allow",
	}

	set, err := gate.NewPermissionClassifierSet(panicking, wellBehaved)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{result: hustle.Result{Output: json.RawMessage(`{}`)}}
	responder := &permissionReviewResponderStub{}
	adapter, pub, faults := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	gateID := mustUUID()
	toolExecutionID := mustUUID()
	req := validReviewRequest(t, gateID, toolExecutionID)

	// If the panicking classifier's Applies panic were not recovered, this
	// call would crash the whole test process before the well-behaved
	// classifier ever got a chance to run.
	adapter.review(context.Background(), req)

	if panicking.appliesCalls != 1 {
		t.Fatalf("panicking.appliesCalls = %d, want 1", panicking.appliesCalls)
	}
	// The well-behaved classifier must still be fully reviewed: one
	// classifier panicking must not take down review of the other.
	if wellBehaved.appliesCalls != 1 || wellBehaved.marshalCalls != 1 || wellBehaved.validateCalls != 1 {
		t.Fatalf(
			"wellBehaved calls = applies:%d marshal:%d validate:%d, want 1/1/1 (unaffected by the other classifier's panic)",
			wellBehaved.appliesCalls, wellBehaved.marshalCalls, wellBehaved.validateCalls,
		)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1 (only the well-behaved classifier is scheduled)", runner.callCount())
	}
	// The panicking classifier makes the WHOLE conjunctive decision
	// ineligible, so the well-behaved classifier's own allow must never be
	// allowed to auto-approve the gate on its own.
	if len(responder.snapshot()) != 0 {
		t.Fatalf("responder calls = %d, want 0 (gate must stay human: one classifier failed closed)", len(responder.snapshot()))
	}

	completedEvents := pub.completed()
	if len(completedEvents) != 2 {
		t.Fatalf("completed events = %d, want 2: %#v", len(completedEvents), completedEvents)
	}
	statuses := map[hustle.Name]gate.ReviewStatus{}
	autoApproved := map[hustle.Name]bool{}
	for _, ev := range completedEvents {
		statuses[ev.Classifier] = ev.Status
		autoApproved[ev.Classifier] = ev.AutoApproved
	}
	if got := statuses["panicking-classifier"]; got != gate.ReviewStatusFailed {
		t.Errorf("panicking-classifier Status = %q, want failed", got)
	}
	// The well-behaved classifier's OWN Hustle validated cleanly (Status stays
	// Allowed), but design §16.2: the combined decision — not this
	// classifier's own opinion — decides AutoApproved. Since the panicking
	// classifier made the whole conjunction ineligible, this must never be
	// audited as an auto-approval.
	if got := statuses["well-behaved-classifier"]; got != gate.ReviewStatusNeedsHuman {
		t.Errorf("well-behaved-classifier Status = %q, want needs_human (the combined decision, not this classifier's own allow, decides the outcome)", got)
	}
	if autoApproved["panicking-classifier"] || autoApproved["well-behaved-classifier"] {
		t.Errorf("AutoApproved = %#v, want both false", autoApproved)
	}
	if faults.count() != 0 {
		t.Errorf("faults = %d, want 0", faults.count())
	}
}

// --- audit-append failure vs ordinary expected classifier failure ---

// TestPermissionReviewAdapterAuditAppendFailureFaultsSession proves an audit
// APPEND failure (the durable publish path itself failing — an I/O/integrity
// problem, design §17's "durable-event append failure") reaches the exact
// session fault path Task 14's ordinary Hustle faults already use
// (sessionHustleFaultReporter), reused rather than duplicated.
func TestPermissionReviewAdapterAuditAppendFailureFaultsSession(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", false)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{}
	adapter, pub, _ := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), &permissionReviewResponderStub{})

	s := &Session{sessionID: mustUUID(), hub: hub.New(mustUUID())}
	adapter.faults = sessionHustleFaultReporter{session: s}
	appendFailure := errors.New("durable append failed")
	pub.failWhen = func(ev event.Event) bool {
		_, ok := ev.(event.PermissionReviewCompleted)
		return ok
	}
	pub.failErr = appendFailure

	req := validReviewRequest(t, mustUUID(), mustUUID())
	adapter.review(context.Background(), req)

	if err := s.faultIfFaulted(); err == nil {
		t.Fatal("session did not fault after an audit append failure")
	}
}

// TestPermissionReviewAdapterExpectedClassifierFailureDoesNotFaultSession
// proves the converse: an ORDINARY expected classifier outcome (here,
// not_applicable — equally true for needs_human/failed/timed_out/cancelled/
// stale, all covered by the dedicated status tests above showing zero
// fault-reporter calls) never faults the session, and never touches the
// gate response seam either — the human gate stays exactly as it was.
func TestPermissionReviewAdapterExpectedClassifierFailureDoesNotFaultSession(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", false)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	runner := &permissionReviewRunnerStub{}
	responder := &permissionReviewResponderStub{}
	adapter, pub, _ := newAuditTestAdapter(t, runner, set, validReviewPolicy(t), responder)

	s := &Session{sessionID: mustUUID(), hub: hub.New(mustUUID())}
	adapter.faults = sessionHustleFaultReporter{session: s}

	req := validReviewRequest(t, mustUUID(), mustUUID())
	adapter.review(context.Background(), req)

	if len(pub.completed()) != 1 || pub.completed()[0].Status != gate.ReviewStatusNotApplicable {
		t.Fatalf("completed events = %#v, want exactly one not_applicable", pub.completed())
	}
	if err := s.faultIfFaulted(); err != nil {
		t.Fatalf("session faulted on an ordinary expected classifier outcome: %v", err)
	}
	if len(responder.snapshot()) != 0 {
		t.Error("responder called, want 0 (the human gate must stay untouched)")
	}
}
