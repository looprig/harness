package loopruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// reviewLifecyclePublisher is a gateRegistrar + permissionReviewStarter fake
// that records the ORDER in which the actor calls each seam via a shared
// monotonic sequence counter, so a test can assert
// prepare < activate < review-start without relying on wall-clock timing.
type reviewLifecyclePublisher struct {
	recordingPublisher

	gateID      gatedomain.ID
	activateErr error

	// releaseReview, if non-nil, is what a spawned "inference" goroutine blocks
	// on inside StartPermissionReview. StartPermissionReview itself never blocks
	// on it directly (that would defeat the point) — it hands the wait off to its
	// own goroutine and returns immediately, mirroring the contract a real
	// session-side adapter must honor.
	releaseReview chan struct{}

	mu          sync.Mutex
	nextSeq     int
	prepareSeq  int
	activateSeq int
	reviewSeq   int
	reviewCalls int
	lastReview  PermissionReviewRequest
}

func (p *reviewLifecyclePublisher) nextSequence() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextSeq++
	return p.nextSeq
}

func (p *reviewLifecyclePublisher) PrepareGateOpen(_ context.Context, _ uuid.UUID, g gatedomain.Gate, _ gatedomain.Payload) (gatedomain.ID, error) {
	seq := p.nextSequence()
	p.mu.Lock()
	p.prepareSeq = seq
	p.mu.Unlock()
	gateID := p.gateID
	if gateID.IsZero() {
		gateID = g.Subject.ToolExecutionID
	}
	return gateID, nil
}

func (p *reviewLifecyclePublisher) ActivateGate(_ context.Context, _ gatedomain.ID, _ gatedomain.Route) error {
	if p.activateErr != nil {
		return p.activateErr
	}
	seq := p.nextSequence()
	p.mu.Lock()
	p.activateSeq = seq
	p.mu.Unlock()
	return nil
}

func (p *reviewLifecyclePublisher) CloseGate(context.Context, gatedomain.ID, gatedomain.CloseReason) error {
	return nil
}

func (p *reviewLifecyclePublisher) StartPermissionReview(_ context.Context, req PermissionReviewRequest) {
	seq := p.nextSequence()
	p.mu.Lock()
	p.reviewSeq = seq
	p.reviewCalls++
	p.lastReview = req
	release := p.releaseReview
	p.mu.Unlock()
	if release != nil {
		// Simulate kicking off asynchronous classifier inference on a goroutine
		// the caller does not wait on.
		go func() { <-release }()
	}
}

func (p *reviewLifecyclePublisher) snapshot() (calls, prepareSeq, activateSeq, reviewSeq int, lastReview PermissionReviewRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reviewCalls, p.prepareSeq, p.activateSeq, p.reviewSeq, p.lastReview
}

func newLoopWithReviewPublisher(t *testing.T, pub *reviewLifecyclePublisher) (*Loop, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sessionID := mustID(t)
	loopID := mustID(t)
	l, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, pub, runtimeConfig{Client: &fakeLLM{}, Model: testModel(), DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, cancel
}

// awaitReviewCall polls the publisher until at least one StartPermissionReview
// call has been recorded, or fails the test after ~2s.
func awaitReviewCall(t *testing.T, pub *reviewLifecyclePublisher) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if calls, _, _, _, _ := pub.snapshot(); calls > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("permission review never started")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestPermissionReviewStartsAfterActivationOrdering proves the design §14.2/
// §14.3 lifecycle: PrepareGateOpen, then ActivateGate, then — only after the
// runner has been acked — exactly one StartPermissionReview call carrying the
// minted GateID, the registration's ToolExecutionID, and the live-only
// ReviewContext snapshot the registration carried.
func TestPermissionReviewStartsAfterActivationOrdering(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateID := newCallID(t)
	pub := &reviewLifecyclePublisher{gateID: gateID}
	l, _ := newLoopWithReviewPublisher(t, pub)

	reviewContext := gatedomain.ReviewContext{
		ContextRevision:    "context-rev-1",
		GatePolicyRevision: "policy-rev-1",
		SecurityCeiling:    "ceiling-1",
	}
	displayed := tool.Request{ToolName: "Bash", Summary: "echo ok"}
	reply := make(chan command.Command, 1)
	ack := make(chan gateInstallAck, 1)

	l.gateReg <- gateRegistration{
		gate:          gatedomain.Gate{Subject: gatedomain.Subject{ToolExecutionID: callID}},
		payload:       gatedomain.PermissionPayload{Request: displayed},
		reviewContext: reviewContext,
		callID:        callID,
		reply:         reply,
		kind:          gatePermission,
		ack:           ack,
	}

	got := <-ack
	if got.err != nil || got.gateID != gateID {
		t.Fatalf("gate registration ack = %+v, want gateID %v and nil err", got, gateID)
	}

	awaitReviewCall(t, pub)
	calls, prepareSeq, activateSeq, reviewSeq, lastReview := pub.snapshot()
	if calls != 1 {
		t.Fatalf("review calls = %d, want 1", calls)
	}
	if !(prepareSeq < activateSeq && activateSeq < reviewSeq) {
		t.Fatalf("lifecycle order violated: prepare=%d activate=%d review=%d, want prepare < activate < review", prepareSeq, activateSeq, reviewSeq)
	}
	if lastReview.GateID != gateID {
		t.Fatalf("review GateID = %v, want %v", lastReview.GateID, gateID)
	}
	if lastReview.ToolExecutionID != callID {
		t.Fatalf("review ToolExecutionID = %v, want %v", lastReview.ToolExecutionID, callID)
	}
	if lastReview.Request.ToolName != "Bash" || lastReview.Request.Summary != "echo ok" {
		t.Fatalf("review Request = %+v, want the displayed prepared request", lastReview.Request)
	}
	if lastReview.ReviewContext.ContextRevision != "context-rev-1" ||
		lastReview.ReviewContext.GatePolicyRevision != "policy-rev-1" ||
		lastReview.ReviewContext.SecurityCeiling != "ceiling-1" {
		t.Fatalf("review ReviewContext = %+v, want the registration's snapshot", lastReview.ReviewContext)
	}
}

// TestPermissionReviewActivationFailureStartsNoReview proves design §14.2's
// "activation failure cannot produce an auto-approval": when ActivateGate
// fails, the actor must never call StartPermissionReview.
func TestPermissionReviewActivationFailureStartsNoReview(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateID := newCallID(t)
	pub := &reviewLifecyclePublisher{gateID: gateID, activateErr: errors.New("activate failed")}
	l, _ := newLoopWithReviewPublisher(t, pub)

	reply := make(chan command.Command, 1)
	ack := make(chan gateInstallAck, 1)
	l.gateReg <- gateRegistration{
		gate:          gatedomain.Gate{Subject: gatedomain.Subject{ToolExecutionID: callID}},
		payload:       gatedomain.PermissionPayload{Request: tool.Request{ToolName: "Bash"}},
		reviewContext: gatedomain.ReviewContext{ContextRevision: "rev"},
		callID:        callID,
		reply:         reply,
		kind:          gatePermission,
		ack:           ack,
	}

	got := <-ack
	if got.err == nil {
		t.Fatal("gate registration err = nil, want activation failure")
	}

	// Give the actor a bounded window to (incorrectly) start a review; none
	// should ever arrive.
	<-time.After(200 * time.Millisecond)
	if calls, _, _, _, _ := pub.snapshot(); calls != 0 {
		t.Fatalf("review calls = %d after activation failure, want 0", calls)
	}
}

// TestPermissionReviewDoesNotDelayHumanResponse proves design §14.2's "the
// human can answer immediately" and the actor's fire-and-forget contract for
// review start: even while a (simulated) classifier review is still running,
// an ApproveToolCall sent right after the ack must be routed to the parked
// runner promptly — the actor is never stuck waiting on review inference.
func TestPermissionReviewDoesNotDelayHumanResponse(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateID := newCallID(t)
	release := make(chan struct{})
	defer close(release)
	pub := &reviewLifecyclePublisher{gateID: gateID, releaseReview: release}
	l, _ := newLoopWithReviewPublisher(t, pub)

	reply := make(chan command.Command, 1)
	ack := make(chan gateInstallAck, 1)
	l.gateReg <- gateRegistration{
		gate:          gatedomain.Gate{Subject: gatedomain.Subject{ToolExecutionID: callID}},
		payload:       gatedomain.PermissionPayload{Request: tool.Request{ToolName: "Bash"}},
		reviewContext: gatedomain.ReviewContext{ContextRevision: "rev"},
		callID:        callID,
		reply:         reply,
		kind:          gatePermission,
		ack:           ack,
	}
	got := <-ack
	if got.err != nil || got.gateID != gateID {
		t.Fatalf("gate registration ack = %+v, want gateID %v and nil err", got, gateID)
	}
	awaitReviewCall(t, pub)

	l.Commands <- command.ApproveToolCall{GateRoute: command.GateRoute{GateID: gateID, ToolExecutionID: callID}}
	replyCmd, ok := recvReply(t, reply, 500*time.Millisecond)
	if !ok {
		t.Fatal("human approval was not delivered promptly while review was still in flight")
	}
	if _, isApprove := replyCmd.(command.ApproveToolCall); !isApprove {
		t.Fatalf("routed command = %T, want ApproveToolCall", replyCmd)
	}
}
