package loop

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// gateKind distinguishes the two kinds of parked-runner gate so listen can refuse
// to satisfy a user-input gate with an approval (or vice versa). Routing matches
// on CallID AND kind: a stray approve/deny can never answer an AskUser gate.
type gateKind uint8

const (
	// gatePermission is opened by the runner when a tool call needs interactive
	// approval; it accepts ApproveToolCall / DenyToolCall.
	gatePermission gateKind = iota
	// gateUserInput is opened by RequestUserInput (AskUser); it accepts
	// ProvideUserInput.
	gateUserInput
)

// gate is the actor-owned record of an open gate: the dedicated reply channel for
// the parked runner and the kind of command it will accept. Stored in
// loopState.pendingGates, keyed by CallID, and touched ONLY by listen/the actor.
type gate struct {
	reply chan<- command.Command
	kind  gateKind
}

// gateRegistration is the request a parked runner sends to the actor to install a
// gate. The actor records {reply, kind} under callID, then closes ack to signal
// install-before-emit: the runner may emit its request event only after the gate
// is installed, so no routed reply can be dropped on a race.
type gateRegistration struct {
	callID uuid.UUID
	reply  chan<- command.Command
	kind   gateKind
	ack    chan<- struct{}
}

// accepts reports whether a control command may satisfy a gate of the given kind.
// gatePermission ↔ ApproveToolCall/DenyToolCall; gateUserInput ↔ ProvideUserInput.
// Any other pairing is rejected (fail-safe): listen drops a mismatched command
// rather than delivering it to the wrong parked runner.
func accepts(kind gateKind, cmd command.Command) bool {
	switch cmd.(type) {
	case command.ApproveToolCall, command.DenyToolCall:
		return kind == gatePermission
	case command.ProvideUserInput:
		return kind == gateUserInput
	default:
		return false
	}
}

// Unexported context-key types. Each is a distinct zero-size struct so values
// never collide across packages (the idiomatic Go ctx-key pattern) and cannot be
// constructed by an outside package.
type emitKey struct{}
type callIDKey struct{}
type gateRegKey struct{}

// withEmit returns a child ctx carrying the per-turn emit func. The runner injects
// it per tool call; EmitFromContext / RequestUserInput read it back.
func withEmit(ctx context.Context, emit func(event.Event)) context.Context {
	return context.WithValue(ctx, emitKey{}, emit)
}

// withCallID returns a child ctx carrying the active tool call's CallID.
func withCallID(ctx context.Context, callID uuid.UUID) context.Context {
	return context.WithValue(ctx, callIDKey{}, callID)
}

// withGateReg returns a child ctx carrying the actor's gate-registration handle.
// Only the loop wires this; RequestUserInput reads it to open a gateUserInput gate.
func withGateReg(ctx context.Context, gateReg chan<- gateRegistration) context.Context {
	return context.WithValue(ctx, gateRegKey{}, gateReg)
}

// callIDFromContext reads the active CallID, false when absent.
func callIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(callIDKey{}).(uuid.UUID)
	return v, ok
}

// gateRegFromContext reads the gate-registration handle, false when absent.
func gateRegFromContext(ctx context.Context) (chan<- gateRegistration, bool) {
	v, ok := ctx.Value(gateRegKey{}).(chan<- gateRegistration)
	return v, ok
}

// EmitFromContext returns the per-turn event-emit func the runner injected, and
// false when none is present (the tool is being run outside a turn). Event-emitting
// tools call this; it is the only sanctioned way for a tool in tools/ to emit an
// event without depending on the loop internals.
func EmitFromContext(ctx context.Context) (func(event.Event), bool) {
	v, ok := ctx.Value(emitKey{}).(func(event.Event))
	return v, ok
}

// GateContextMissing identifies which injected ctx value RequestUserInput could
// not find. It is a fail-secure signal: a tool that calls RequestUserInput outside
// a turn (no emit / CallID / gateReg in ctx) is a bug, so it errors rather than
// silently proceeding.
type GateContextMissing string

const (
	GateContextEmit    GateContextMissing = "emit"
	GateContextCallID  GateContextMissing = "callID"
	GateContextGateReg GateContextMissing = "gateReg"
)

// GateContextError is returned by RequestUserInput when the ctx is missing one of
// the runner-injected values. Callers can errors.As to inspect which value.
type GateContextError struct{ Missing GateContextMissing }

func (e *GateContextError) Error() string {
	return "loop: RequestUserInput called without ctx value: " + string(e.Missing)
}

// RequestUserInput is the loop-provided helper AskUser calls to open a user-input
// gate. It encapsulates all the gate plumbing so a tool never touches gateReg
// directly:
//
//  1. Read emit, CallID, gateReg from ctx — any missing → *GateContextError
//     (fail-secure; calling this outside a turn is a bug).
//  2. Register a gateUserInput gate synchronously and ctx-aware: send the
//     registration, then wait for the ack (install-before-emit). Both selects
//     escape on ctx.Done so a cancelled turn / departed actor never wedges.
//  3. Emit UserInputRequested AFTER the ack — the gate is installed, so the
//     matching ProvideUserInput cannot be dropped on a race.
//  4. Block on the dedicated reply channel (buffered(1), runner is sole reader)
//     or ctx.Done. CallID is re-validated on receipt as cheap defence.
//
// Returns the raw answer; AskUser validates it against its choices.
func RequestUserInput(ctx context.Context, question string, choices []string) (string, error) {
	emit, ok := EmitFromContext(ctx)
	if !ok {
		return "", &GateContextError{Missing: GateContextEmit}
	}
	callID, ok := callIDFromContext(ctx)
	if !ok {
		return "", &GateContextError{Missing: GateContextCallID}
	}
	gateReg, ok := gateRegFromContext(ctx)
	if !ok {
		return "", &GateContextError{Missing: GateContextGateReg}
	}

	// reply is buffered(1) so the actor's routed send never blocks (runner is the
	// sole reader). ack is unbuffered: the actor closes it to signal installation.
	reply := make(chan command.Command, 1)
	ack := make(chan struct{})

	// Register synchronously, ctx-aware: no wedge if the actor is gone or the turn
	// is cancelled.
	select {
	case gateReg <- gateRegistration{callID: callID, reply: reply, kind: gateUserInput, ack: ack}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-ack:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Install-before-emit: only now is the gate guaranteed installed.
	emit(event.UserInputRequested{CallID: callID, Question: question, Choices: choices})

	select {
	case cmd := <-reply:
		// listen already matched by CallID + kind; re-validate the CallID as cheap
		// defence in depth, and narrow to the concrete command for the answer.
		pui, ok := cmd.(command.ProvideUserInput)
		if !ok || pui.GateCallID() != callID {
			return "", &GateReplyMismatchError{CallID: callID}
		}
		return pui.Answer, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// GateReplyMismatchError is returned if the command delivered on a gateUserInput
// reply channel is not a ProvideUserInput for the expected CallID. listen routes
// by CallID + kind, so this is a defence-in-depth guard that should never fire in
// normal operation.
type GateReplyMismatchError struct{ CallID uuid.UUID }

func (e *GateReplyMismatchError) Error() string {
	return "loop: gate reply did not match expected ProvideUserInput for call " + e.CallID.String()
}
