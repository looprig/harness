package sessionruntime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/storage/memstore"
)

// This file is the Finding 1 (Phase 6 spec-compliance review) end-to-end
// regression proof: a REAL *hub.Hub, wired with a REAL (recording, not
// nop) durable eventAppender via WithEventAppender — the exact composition
// the production journal uses (session.go's injectedEventAppender) — must
// actually receive PermissionReviewStarted/PermissionReviewCompleted audit
// events from a genuine Submit() → gate open → StartPermissionReview flow,
// and the session must never fault doing it. review_audit_test.go's
// reviewAuditPublisherStub is a content/behavior fake for review_adapter's
// OWN logic; it never exercised Hub.validateInternalPublication's boundary
// rules at all, which is exactly how the PermissionReviewStarted/Completed
// allowlist omission shipped undetected. pkg/hub/permission_review_publish_test.go
// covers the boundary mechanism directly; this test proves the full
// production wiring (StartPermissionReview → adapter.publisher = s.hub →
// Hub.PublishInternalEventChecked → the real eventAppender) end to end.

func TestPermissionReviewAuditDurablyPublishesThroughRealHubWithoutFaultingSession(t *testing.T) {
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

	classifier := newReviewContextCaptureProbeClassifier(t, "hub-integration-classifier", "hub-integration-rev-1")
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	policy := validReviewPolicy(t)

	ws := mustWorkspaceStore(t, memstore.New().Blobs)
	root := t.TempDir()
	appender := &recordingEventAppender{}

	s, err := newTestSession(context.Background(), gatedE2EDefinition(t, evaluator, tl),
		withSessionHustles([]hustle.Definition{testHustleDefinition(t, "unrelated-background-hustle")}, testHustleLimits()),
		withPermissionReview(set, policy),
		WithWorkspaceCheckpointing(ws, root),
		WithEventAppender(appender),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	marker := "hub-integration-marker-" + mustUUID().String()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Submit(ctx, []content.Block{&content.TextBlock{Text: marker}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	select {
	case <-classifier.captured:
	case <-time.After(20 * time.Second):
		t.Fatal("StartPermissionReview never reached the registered classifier through a real Submit()")
	}

	// The classifier reports itself not-applicable, so reviewOne durably
	// (checked) appends exactly one PermissionReviewCompleted for it —
	// through the REAL hub, the REAL appender, and the production wiring
	// (adapter.publisher = s.hub). Poll briefly: the append races this
	// goroutine's receive from classifier.captured.
	var completed *event.PermissionReviewCompleted
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range appender.snapshot() {
			if c, ok := ev.(event.PermissionReviewCompleted); ok {
				found := c
				completed = &found
				break
			}
		}
		if completed != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if completed == nil {
		t.Fatal("no PermissionReviewCompleted event reached the real durable appender; the review audit append never committed")
	}
	if completed.Classifier != "hub-integration-classifier" || completed.ClassifierRevision != "hub-integration-rev-1" {
		t.Fatalf("completed.Classifier/ClassifierRevision = %q/%q, want hub-integration-classifier/hub-integration-rev-1",
			completed.Classifier, completed.ClassifierRevision)
	}
	if completed.Status != gate.ReviewStatusNotApplicable {
		t.Fatalf("completed.Status = %q, want not_applicable", completed.Status)
	}
	if completed.Visibility() != event.Internal {
		t.Fatalf("completed.Visibility() = %v, want Internal", completed.Visibility())
	}
	if err := event.ValidateEvent(*completed); err != nil {
		t.Fatalf("durably appended PermissionReviewCompleted failed ValidateEvent: %v", err)
	}

	// The single most safety-critical assertion: durable audit publication
	// for a real review must never fault the session.
	if err := s.FaultErr(); err != nil {
		t.Fatalf("session faulted after a completed review published through the real hub: %v", err)
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
