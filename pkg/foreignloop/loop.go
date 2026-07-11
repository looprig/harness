package foreignloop

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// Loop is the foreign-loop ACTOR: a goroutine that satisfies loop.Backend, drives a
// ForeignAgent one turn at a time, and publishes looprig events to the session
// fan-in. It mirrors the native loop's actor model — a single goroutine owns the
// committed state (msgs/turnIndex/hasSpawned), and every mutation flows through that
// goroutine, so no locks guard the conversation history.
type Loop struct {
	// Commands is the inbound command channel (unbuffered). Callers must never close
	// it; stop the actor with command.Shutdown. CommandSink exposes its send side.
	Commands chan command.Command
	// Done is closed when the actor has fully exited. DoneChan exposes its read side.
	Done chan struct{}

	// snapshots is the committed-state query seam: Snapshot sends a snapshotReq and
	// the actor (the sole owner of msgs/turnIndex) replies a defensive clone.
	snapshots chan snapshotReq

	// Identity + dependencies, set by New and then read by the actor. Late-bound loops
	// learn sid from the first ForeignInit.
	sessionID uuid.UUID
	loopID    uuid.UUID
	sid       string // the minted foreign session id (stamped onto LoopStarted by the caller)
	parent    loop.Provenance
	pub       EventPublisher
	cfg       loop.BoundDefinition
	spec      Spec
	idGen     func() (uuid.UUID, error)
	fac       *event.Factory

	// Actor-owned committed state — touched ONLY by the run goroutine.
	msgs       content.AgenticMessages
	turnIndex  event.TurnIndex
	hasSpawned bool
	sidBound   bool
}

// compile-time proof the foreign loop satisfies the engine-agnostic Backend.
var _ loop.Backend = (*Loop)(nil)

// New constructs a foreign loop and starts its actor goroutine. It validates the
// caller-supplied wiring (fail-secure, typed *ConfigError), prebinds the foreign
// session id when requested, and returns the loop plus that sid (the caller stamps
// it onto LoopStarted). loopCtx is the loop's lifetime; cancelling it tears the
// actor down.
func New(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
	pub EventPublisher, cfg loop.BoundDefinition, spec Spec,
	idGen func() (uuid.UUID, error), fac *event.Factory) (*Loop, string, error) {
	if err := validateWiring(cfg, spec, idGen, fac, pub); err != nil {
		return nil, "", err
	}
	sid := ""
	sidBound := false
	switch spec.SIDMode {
	case SIDPrebound:
		u, err := idGen()
		if err != nil {
			return nil, "", &SpawnError{Cause: err}
		}
		sid = u.String()
		sidBound = true
	case SIDLateBound:
	default:
		return nil, "", &ConfigError{Field: "Spec.SIDMode", Reason: "unknown"}
	}
	l := &Loop{
		Commands:  make(chan command.Command),
		Done:      make(chan struct{}),
		snapshots: make(chan snapshotReq),
		sessionID: sessionID,
		loopID:    loopID,
		sid:       sid,
		sidBound:  sidBound,
		parent:    parent,
		pub:       pub,
		cfg:       cfg,
		spec:      spec,
		idGen:     idGen,
		fac:       fac,
	}
	go l.run(loopCtx)
	return l, sid, nil
}

// validateWiring fail-secure validates the caller-supplied wiring shared by New and
// NewRestored, returning a typed *ConfigError on the first missing dependency. It does
// NOT validate the restore seed; that is the restored constructor's own concern.
func validateWiring(cfg loop.BoundDefinition, spec Spec, idGen func() (uuid.UUID, error), fac *event.Factory, pub EventPublisher) error {
	switch {
	case cfg == nil || cfg.EffectiveSystem() == "":
		return &ConfigError{Field: "System", Reason: "required"}
	case spec.Agent == nil:
		return &ConfigError{Field: "Spec.Agent", Reason: "required"}
	case idGen == nil:
		return &ConfigError{Field: "idGen", Reason: "required"}
	case fac == nil:
		return &ConfigError{Field: "fac", Reason: "required"}
	case pub == nil:
		return &ConfigError{Field: "pub", Reason: "required"}
	}
	return nil
}

// BuildWith adapts New to the foreignloop.Builder seam the composition root wires:
// it closes over the per-agent Spec (resolved at the root, NOT on loop.BoundDefinition) and
// returns a Builder that constructs the foreign loop as a loop.Backend. On a
// construction error it returns a NIL Backend (never a non-nil interface wrapping a
// nil *Loop), so the caller's nil check behaves.
func BuildWith(spec Spec) Builder {
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID,
		parent loop.Provenance, pub EventPublisher, cfg loop.BoundDefinition,
		idGen func() (uuid.UUID, error), fac *event.Factory) (loop.Backend, string, error) {
		l, sid, err := New(loopCtx, sessionID, loopID, parent, pub, cfg, spec, idGen, fac)
		if err != nil {
			return nil, "", err
		}
		return l, sid, nil
	}
}

// CommandSink exposes the command send-side for the Backend contract.
func (l *Loop) CommandSink() chan<- command.Command { return l.Commands }

// DoneChan exposes the completion channel for the Backend contract.
func (l *Loop) DoneChan() <-chan struct{} { return l.Done }

// run is the actor goroutine. It is the ONLY goroutine that reads or mutates the
// committed state (msgs/turnIndex/hasSpawned), serializing every command, snapshot,
// and turn so the foreign loop needs no locks. While a turn runs, runTurn takes over
// the select via an inner loop (awaitTurn); between turns this top-level loop is idle.
func (l *Loop) run(loopCtx context.Context) {
	defer close(l.Done)
	for {
		select {
		case <-loopCtx.Done():
			return
		case req := <-l.snapshots:
			req.reply <- snapshotResult{msgs: cloneMessages(l.msgs), turnIndex: l.turnIndex}
		case cmd := <-l.Commands:
			switch c := cmd.(type) {
			case command.UserInput:
				if l.runTurn(loopCtx, c) {
					return // a Shutdown arrived mid-turn; defer closes Done.
				}
			case command.Shutdown:
				c.Ack <- nil
				return // defer closes Done.
			case command.Interrupt:
				c.Ack <- false // idle: nothing to interrupt.
			default:
				slog.Warn("foreignloop: dropping un-honorable command while idle", "type", fmt.Sprintf("%T", cmd))
			}
		}
	}
}

// cloneMessages returns a copy of the message thread with its OWN backing array so a
// snapshot caller can never alias or race the live slice the actor keeps appending
// to. The Conversation values are treated as immutable, so a shallow header copy is
// enough. A nil/empty thread clones to nil.
func cloneMessages(msgs content.AgenticMessages) content.AgenticMessages {
	if len(msgs) == 0 {
		return nil
	}
	out := make(content.AgenticMessages, len(msgs))
	copy(out, msgs)
	return out
}
