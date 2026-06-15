package loop

import (
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// registerGate sends a gateRegistration through the actor's gateReg seam and waits
// for the ack (the actor closes it once the gate is installed). It returns the
// reply channel the actor will route a matching command to. The test acts as the
// runner here: it owns the buffered(1) reply channel and is its sole reader.
func registerGate(t *testing.T, l *Loop, callID uuid.UUID, kind gateKind) <-chan command.Command {
	t.Helper()
	reply := make(chan command.Command, 1)
	ack := make(chan struct{})
	select {
	case l.gateReg <- gateRegistration{callID: callID, reply: reply, kind: kind, ack: ack}:
	case <-time.After(2 * time.Second):
		t.Fatal("gateReg send wedged")
	}
	select {
	case <-ack:
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

// TestListenRoutesTwoConcurrentUserInputGates is the gate-concurrency core: two
// gateUserInput gates with distinct CallIDs are open at once; two ProvideUserInput
// commands (matching CallIDs) must each reach their OWN reply channel with the
// right answer. This is the case a single shared buffer-1 channel would corrupt.
func TestListenRoutesTwoConcurrentUserInputGates(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{})
	idA := newCallID(t)
	idB := newCallID(t)

	replyA := registerGate(t, l, idA, gateUserInput)
	replyB := registerGate(t, l, idB, gateUserInput)

	l.Commands <- command.ProvideUserInput{CallID: idA, Answer: "answerA"}
	l.Commands <- command.ProvideUserInput{CallID: idB, Answer: "answerB"}

	gotA, ok := recvReply(t, replyA, 2*time.Second)
	if !ok {
		t.Fatal("gate A received no reply")
	}
	if pui, _ := gotA.(command.ProvideUserInput); pui.Answer != "answerA" || pui.CallID != idA {
		t.Fatalf("gate A got %+v, want answerA for idA", gotA)
	}
	gotB, ok := recvReply(t, replyB, 2*time.Second)
	if !ok {
		t.Fatal("gate B received no reply")
	}
	if pui, _ := gotB.(command.ProvideUserInput); pui.Answer != "answerB" || pui.CallID != idB {
		t.Fatalf("gate B got %+v, want answerB for idB", gotB)
	}
}

// TestListenRoutesPermissionGate verifies an Approve reaches a gatePermission gate.
func TestListenRoutesPermissionGate(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)
	reply := registerGate(t, l, id, gatePermission)

	l.Commands <- command.ApproveToolCall{CallID: id}
	got, ok := recvReply(t, reply, 2*time.Second)
	if !ok {
		t.Fatal("permission gate received no reply")
	}
	if _, isApprove := got.(command.ApproveToolCall); !isApprove {
		t.Fatalf("permission gate got %T, want ApproveToolCall", got)
	}
}

// TestListenDropsWrongCallID: a command whose CallID matches no open gate is
// dropped (no delivery) and the actor stays alive (a subsequent valid command for
// a real gate is still routed).
func TestListenDropsWrongCallID(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)
	stray := newCallID(t)
	reply := registerGate(t, l, id, gateUserInput)

	// Stray CallID: no gate → dropped.
	l.Commands <- command.ProvideUserInput{CallID: stray, Answer: "ignored"}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("gate received a reply for a stray CallID")
	}
	// Actor still alive: the real command routes.
	l.Commands <- command.ProvideUserInput{CallID: id, Answer: "real"}
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
	l, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)
	reply := registerGate(t, l, id, gateUserInput)

	// Wrong kind: Approve cannot satisfy a user-input gate.
	l.Commands <- command.ApproveToolCall{CallID: id}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("user-input gate accepted an ApproveToolCall (wrong kind)")
	}
	// Right kind: still resolves.
	l.Commands <- command.ProvideUserInput{CallID: id, Answer: "ok"}
	if _, ok := recvReply(t, reply, 2*time.Second); !ok {
		t.Fatal("user-input gate did not resolve on ProvideUserInput")
	}
}

// TestListenDropsDuplicateAfterDelivery: once a gate is delivered+deleted, a
// duplicate command for the same CallID is dropped (no second delivery, no panic).
func TestListenDropsDuplicateAfterDelivery(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)
	reply := registerGate(t, l, id, gateUserInput)

	l.Commands <- command.ProvideUserInput{CallID: id, Answer: "first"}
	if _, ok := recvReply(t, reply, 2*time.Second); !ok {
		t.Fatal("first command not delivered")
	}
	// Duplicate for the now-deleted gate → dropped.
	l.Commands <- command.ProvideUserInput{CallID: id, Answer: "duplicate"}
	if _, ok := recvReply(t, reply, 200*time.Millisecond); ok {
		t.Fatal("duplicate delivered after gate was deleted")
	}
	// Prove the actor is still alive by registering+resolving a fresh gate.
	id2 := newCallID(t)
	reply2 := registerGate(t, l, id2, gateUserInput)
	l.Commands <- command.ProvideUserInput{CallID: id2, Answer: "alive"}
	if _, ok := recvReply(t, reply2, 2*time.Second); !ok {
		t.Fatal("actor wedged after duplicate command")
	}
}

// TestListenStaleCommandDoesNotDropLaterValidReply is the explicit regression for
// the shared-buffer bug §2c warns about: a stale/duplicate command sent BEFORE the
// real one must not consume a buffer slot that swallows the later valid reply. With
// per-gate channels and delete-on-delivery, the stale command (for an unregistered
// or already-deleted CallID) is dropped, and the subsequently-registered gate's
// real command is still delivered.
func TestListenStaleCommandDoesNotDropLaterValidReply(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{})
	id := newCallID(t)

	// A stale command arrives BEFORE any gate for this CallID is registered.
	l.Commands <- command.ProvideUserInput{CallID: id, Answer: "stale"}
	// Now the runner registers the gate for the same CallID...
	reply := registerGate(t, l, id, gateUserInput)
	// ...and the real command arrives. It must be delivered (the stale one did not
	// poison a shared buffer).
	l.Commands <- command.ProvideUserInput{CallID: id, Answer: "real"}
	got, ok := recvReply(t, reply, 2*time.Second)
	if !ok {
		t.Fatal("real reply dropped by a preceding stale command")
	}
	if pui, _ := got.(command.ProvideUserInput); pui.Answer != "real" {
		t.Fatalf("got %+v, want real (stale must not have been delivered)", got)
	}
}
