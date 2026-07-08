package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	gatedomain "github.com/looprig/harness/pkg/gate"
)

type gateRegistrarPublisher struct {
	recordingPublisher
	gateID      gatedomain.ID
	prepareErr  error
	activateErr error
	prepared    []gatedomain.Gate
	activated   []gatedomain.Route
}

func (p *gateRegistrarPublisher) PrepareGateOpen(_ context.Context, _ uuid.UUID, g gatedomain.Gate, _ gatedomain.Payload) (gatedomain.ID, error) {
	if p.prepareErr != nil {
		return gatedomain.ID{}, p.prepareErr
	}
	gateID := p.gateID
	if gateID.IsZero() {
		gateID = g.Subject.ToolExecutionID
	}
	p.prepared = append(p.prepared, g)
	return gateID, nil
}

func (p *gateRegistrarPublisher) ActivateGate(_ context.Context, id gatedomain.ID, route gatedomain.Route) error {
	if p.activateErr != nil {
		return p.activateErr
	}
	route.GateID = id
	p.activated = append(p.activated, route)
	return nil
}

func newLoopWithGateRegistrar(t *testing.T, registrar *gateRegistrarPublisher) (*Loop, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sessionID := mustID(t)
	loopID := mustID(t)
	l, err := New(ctx, sessionID, loopID, Provenance{}, registrar, Config{Client: &fakeLLM{}, Model: testModel(), DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, cancel
}

// registerGate sends a gateRegistration through the actor's gateReg seam and waits
// for the ack (the actor closes it once the gate is installed). It returns the
// reply channel the actor will route a matching command to. The test acts as the
// runner here: it owns the buffered(1) reply channel and is its sole reader.
func registerGate(t *testing.T, l *Loop, callID uuid.UUID, kind gateKind) <-chan command.Command {
	t.Helper()
	reply := make(chan command.Command, 1)
	ack := make(chan gateInstallAck, 1)
	g := gatedomain.Gate{ID: callID, Subject: gatedomain.Subject{ToolExecutionID: callID}}
	select {
	case l.gateReg <- gateRegistration{gate: g, callID: callID, reply: reply, kind: kind, ack: ack}:
	case <-time.After(2 * time.Second):
		t.Fatal("gateReg send wedged")
	}
	select {
	case got := <-ack:
		if got.err != nil {
			t.Fatalf("gate registration err = %v", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate registration not acked")
	}
	return reply
}

// recvReply reads a routed command from a gate's reply channel with a timeout.
func recvReply(t *testing.T, reply <-chan command.Command, d time.Duration) (command.Command, bool) {
	t.Helper()
	select {
	case cmd := <-reply:
		return cmd, true
	case <-time.After(d):
		return nil, false
	}
}

func TestLoopGatePrepareFailureDoesNotInstallLocalBlocker(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	registrar := &gateRegistrarPublisher{prepareErr: errors.New("prepare failed")}
	l, _ := newLoopWithGateRegistrar(t, registrar)
	reply := make(chan command.Command, 1)
	ack := make(chan gateInstallAck, 1)

	l.gateReg <- gateRegistration{
		gate:   gatedomain.Gate{Subject: gatedomain.Subject{ToolExecutionID: callID}},
		callID: callID,
		reply:  reply,
		kind:   gateUserInput,
		ack:    ack,
	}
	got := <-ack
	if got.err == nil {
		t.Fatal("gate registration err = nil, want prepare failure")
	}

	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: callID}, Answer: "late"}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("gate reply delivered after prepare failure")
	}
}

func TestLoopGateActivationFailureRemovesLocalBlocker(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateID := newCallID(t)
	registrar := &gateRegistrarPublisher{gateID: gateID, activateErr: errors.New("activate failed")}
	l, _ := newLoopWithGateRegistrar(t, registrar)
	reply := make(chan command.Command, 1)
	ack := make(chan gateInstallAck, 1)

	l.gateReg <- gateRegistration{
		gate:   gatedomain.Gate{Subject: gatedomain.Subject{ToolExecutionID: callID}},
		callID: callID,
		reply:  reply,
		kind:   gateUserInput,
		ack:    ack,
	}
	got := <-ack
	if got.err == nil || got.gateID != gateID {
		t.Fatalf("gate registration ack = %+v, want activation failure with gateID %v", got, gateID)
	}

	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{GateID: gateID, ToolExecutionID: callID}, Answer: "late"}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("gate reply delivered after activation failure")
	}
}

func TestLoopGateRoutesByGateIDAfterActivation(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateID := newCallID(t)
	registrar := &gateRegistrarPublisher{gateID: gateID}
	l, _ := newLoopWithGateRegistrar(t, registrar)
	reply := make(chan command.Command, 1)
	ack := make(chan gateInstallAck, 1)

	l.gateReg <- gateRegistration{
		gate:   gatedomain.Gate{Subject: gatedomain.Subject{ToolExecutionID: callID}},
		callID: callID,
		reply:  reply,
		kind:   gateUserInput,
		ack:    ack,
	}
	if got := <-ack; got.err != nil || got.gateID != gateID {
		t.Fatalf("gate registration ack = %+v, want gateID %v and nil err", got, gateID)
	}
	if len(registrar.activated) != 1 {
		t.Fatalf("activated count = %d, want 1", len(registrar.activated))
	}
	if registrar.activated[0].GateID != gateID || registrar.activated[0].ToolExecutionID != callID {
		t.Fatalf("activated route = %+v, want gateID/toolExecutionID", registrar.activated[0])
	}

	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{GateID: gateID, ToolExecutionID: callID}, Answer: "answer"}
	got, ok := recvReply(t, reply, 2*time.Second)
	if !ok {
		t.Fatal("gate received no reply after activation")
	}
	if pui, ok := got.(command.ProvideUserInput); !ok || pui.Answer != "answer" {
		t.Fatalf("gate reply = %+v, want ProvideUserInput answer", got)
	}

	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{GateID: gateID, ToolExecutionID: callID}, Answer: "duplicate"}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("duplicate gate reply delivered")
	}
}

// TestListenRoutesTwoConcurrentUserInputGates is the gate-concurrency core: two
// gateUserInput gates with distinct CallIDs are open at once; two ProvideUserInput
// commands (matching CallIDs) must each reach their OWN reply channel with the
// right answer. This is the case a single shared buffer-1 channel would corrupt.
func TestListenRoutesTwoConcurrentUserInputGates(t *testing.T) {
	t.Parallel()
	l, _, _ := newLoop(t, &fakeLLM{})
	idA := newCallID(t)
	idB := newCallID(t)

	replyA := registerGate(t, l, idA, gateUserInput)
	replyB := registerGate(t, l, idB, gateUserInput)

	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: idA}, Answer: "answerA"}
	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: idB}, Answer: "answerB"}

	gotA, ok := recvReply(t, replyA, 2*time.Second)
	if !ok {
		t.Fatal("gate A received no reply")
	}
	if pui, _ := gotA.(command.ProvideUserInput); pui.Answer != "answerA" || pui.ToolExecutionID != idA {
		t.Fatalf("gate A got %+v, want answerA for idA", gotA)
	}
	gotB, ok := recvReply(t, replyB, 2*time.Second)
	if !ok {
		t.Fatal("gate B received no reply")
	}
	if pui, _ := gotB.(command.ProvideUserInput); pui.Answer != "answerB" || pui.ToolExecutionID != idB {
		t.Fatalf("gate B got %+v, want answerB for idB", gotB)
	}
}

// TestListenRoutesPermissionGate verifies an Approve reaches a gatePermission gate.
func TestListenRoutesPermissionGate(t *testing.T) {
	t.Parallel()
	l, _, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)
	reply := registerGate(t, l, id, gatePermission)

	l.Commands <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: id}}
	got, ok := recvReply(t, reply, 2*time.Second)
	if !ok {
		t.Fatal("permission gate received no reply")
	}
	if _, isApprove := got.(command.ApproveToolCall); !isApprove {
		t.Fatalf("permission gate got %T, want ApproveToolCall", got)
	}
}

// TestListenDropsWrongCallID: a command whose ToolExecutionID matches no open gate is
// dropped (no delivery) and the actor stays alive (a subsequent valid command for
// a real gate is still routed).
func TestListenDropsWrongCallID(t *testing.T) {
	t.Parallel()
	l, _, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)
	stray := newCallID(t)
	reply := registerGate(t, l, id, gateUserInput)

	// Stray ToolExecutionID: no gate → dropped.
	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: stray}, Answer: "ignored"}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("gate received a reply for a stray ToolExecutionID")
	}
	// Actor still alive: the real command routes.
	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: id}, Answer: "real"}
	got, ok := recvReply(t, reply, 2*time.Second)
	if !ok {
		t.Fatal("actor wedged: real command not routed after stray")
	}
	if pui, _ := got.(command.ProvideUserInput); pui.Answer != "real" {
		t.Fatalf("got %+v, want real", got)
	}
}

// TestListenDropsWrongKind: an ApproveToolCall sent to a gateUserInput gate is
// dropped (kind mismatch), and the gate still resolves on the right command kind.
func TestListenDropsWrongKind(t *testing.T) {
	t.Parallel()
	l, _, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)
	reply := registerGate(t, l, id, gateUserInput)

	// Wrong kind: Approve cannot satisfy a user-input gate.
	l.Commands <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: id}}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("user-input gate accepted an ApproveToolCall (wrong kind)")
	}
	// Right kind: still resolves.
	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: id}, Answer: "ok"}
	if _, ok := recvReply(t, reply, 2*time.Second); !ok {
		t.Fatal("user-input gate did not resolve on ProvideUserInput")
	}
}

// TestListenDropsDuplicateAfterDelivery: once a gate is delivered+deleted, a
// duplicate command for the same ToolExecutionID is dropped (no second delivery, no panic).
func TestListenDropsDuplicateAfterDelivery(t *testing.T) {
	t.Parallel()
	l, _, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)
	reply := registerGate(t, l, id, gateUserInput)

	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: id}, Answer: "first"}
	if _, ok := recvReply(t, reply, 2*time.Second); !ok {
		t.Fatal("first command not delivered")
	}
	// Duplicate for the now-deleted gate → dropped.
	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: id}, Answer: "duplicate"}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("duplicate delivered after gate was deleted")
	}
	// Prove the actor is still alive by registering+resolving a fresh gate.
	id2 := newCallID(t)
	reply2 := registerGate(t, l, id2, gateUserInput)
	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: id2}, Answer: "alive"}
	if _, ok := recvReply(t, reply2, 2*time.Second); !ok {
		t.Fatal("actor wedged after duplicate command")
	}
}

// TestListenStaleCommandDoesNotDropLaterValidReply is the explicit regression for
// the shared-buffer bug §2c warns about: a stale/duplicate command sent BEFORE the
// real one must not consume a buffer slot that swallows the later valid reply. With
// per-gate channels and delete-on-delivery, the stale command (for an unregistered
// or already-deleted ToolExecutionID) is dropped, and the subsequently-registered gate's
// real command is still delivered.
func TestListenStaleCommandDoesNotDropLaterValidReply(t *testing.T) {
	t.Parallel()
	l, _, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)

	// A stale command arrives BEFORE any gate for this ToolExecutionID is registered.
	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: id}, Answer: "stale"}
	// Now the runner registers the gate for the same ToolExecutionID...
	reply := registerGate(t, l, id, gateUserInput)
	// ...and the real command arrives. It must be delivered (the stale one did not
	// poison a shared buffer).
	l.Commands <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: id}, Answer: "real"}
	got, ok := recvReply(t, reply, 2*time.Second)
	if !ok {
		t.Fatal("real reply dropped by a preceding stale command")
	}
	if pui, _ := got.(command.ProvideUserInput); pui.Answer != "real" {
		t.Fatalf("got %+v, want real (stale must not have been delivered)", got)
	}
}
