package loop

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// Loop is the handle to a running agent loop for internal packages.
// Commands is unbuffered — sends block until the actor is ready. Callers must
// never close Commands; stop the actor with Shutdown. (Closing it would exit the
// actor through the `!ok` path, skipping terminal delivery and shutdown acks.)
// Done is closed when the actor has fully exited.
// Direct callers must honor the command contracts, including non-nil reply
// channels and non-nil Abandoned channels for StartTurn.
// Reply channels (the Ack on each command) must be buffered (capacity >= 1) or
// always read promptly: the actor sends acks synchronously, so a blocked ack
// send would stall the actor.
type Loop struct {
	Commands chan<- command.Command
	Done     <-chan struct{}
}

// idGenerator mints a fresh UUID. It defaults to uuid.New; tests inject a
// failing generator to exercise the crypto/rand failure branches.
type idGenerator func() (uuid.UUID, error)

const defaultDrainTimeout = 5 * time.Second

// resolveDrainTimeout applies the default when the caller leaves DrainTimeout unset.
func resolveDrainTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultDrainTimeout
	}
	return d
}

func New(ctx context.Context, sessionID uuid.UUID, cfg Config) (*Loop, error) {
	if cfg.Client == nil {
		return nil, &ConfigError{Kind: ConfigMissingClient}
	}
	if err := cfg.Model.Validate(); err != nil {
		return nil, &ConfigError{Kind: ConfigInvalidModel, Cause: err}
	}
	cfg.DrainTimeout = resolveDrainTimeout(cfg.DrainTimeout)
	cfg.Tools = resolveToolSetCaps(cfg.Tools)
	if cfg.idGen == nil {
		cfg.idGen = uuid.New
	}
	commands := make(chan command.Command)
	done := make(chan struct{})
	go listen(ctx, sessionID, cfg, commands, done)
	return &Loop{Commands: commands, Done: done}, nil
}

type loopStatus int

const (
	loopIdle loopStatus = iota
	loopRunning
	loopShuttingDown
)

type loopState struct {
	turnIndex     event.TurnIndex
	turnID        uuid.UUID // entity id for the active turn; zero when idle
	causationID   uuid.UUID // active StartTurn.Header.ID; zero when idle
	status        loopStatus
	cancelTurn    context.CancelFunc
	turnEvents    chan<- event.Event      // current turn's channel; actor closes it
	turnAbandoned <-chan struct{}         // always non-nil; closed when caller stops reading
	msgs          content.AgenticMessages // conversation history across turns
	shutdownAcks  []chan<- error
}

type turnResult struct {
	msgs     content.AgenticMessages
	terminal event.Event // TurnDone, TurnFailed, or TurnInterrupted
}

func listen(ctx context.Context, sessionID uuid.UUID, cfg Config, commands <-chan command.Command, done chan struct{}) {
	defer close(done)

	internal := make(chan turnResult, 1)
	var state loopState

	publish := func(ev event.Event) {
		// EventID is fresh per emitted event. A crypto/rand failure here is a
		// system-level fault; it must not abort turn execution (the bare per-turn
		// event is delivered separately), so log it and emit the envelope with a
		// zero EventID rather than dropping the sink copy. CallID stays zero: no
		// event in v1 pertains to a tool call.
		eventID, err := cfg.idGen()
		if err != nil {
			slog.Error("event id generation failed; emitting envelope with zero EventID", "error", err)
		}
		env := event.EventEnvelope{
			SessionID:   sessionID, // sessionID is uuid.UUID
			TurnID:      state.turnID,
			EventID:     eventID,
			CausationID: state.causationID,
			Event:       ev,
		}
		switch e := ev.(type) {
		case event.TurnStarted:
			env.TurnIndex = e.TurnIndex
		case event.TokenDelta:
			env.TurnIndex = e.TurnIndex
		case event.TurnDone:
			env.TurnIndex = e.TurnIndex
		case event.TurnFailed:
			env.TurnIndex = e.TurnIndex
		case event.TurnInterrupted:
			env.TurnIndex = e.TurnIndex
		}
		for _, sink := range cfg.Sinks {
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Warn("event sink panicked", "panic", r)
					}
				}()
				sink.OnEvent(ctx, env)
			}()
		}
	}

	publish(event.SessionStarted{SessionID: sessionID})

	// deliverAndClose publishes the terminal event, sends it to the per-turn
	// channel unless the caller abandoned the stream, and closes the channel.
	// Always called by the actor, never by the turn goroutine, and only after the
	// turn goroutine has sent its result on `internal` (so closing turnEvents can
	// never race a concurrent emit).
	//
	// Three escapes, so the actor can never wedge here:
	//   - turnEvents <- terminal: the normal path, caller reads the terminal.
	//   - turnAbandoned: Invoke closes it via defer after receiving the terminal;
	//     Stream.Close closes it explicitly.
	//   - ctx.Done: a buggy caller that never reads and never closes Abandoned
	//     (e.g. a leaked Stream reader) must not pin the actor forever. A root-ctx
	//     cancel always frees it. Without this case such a caller would wedge the
	//     actor outside its select loop, where neither Shutdown nor root-ctx
	//     cancel could reach it.
	deliverAndClose := func(terminal event.Event) {
		publish(terminal)
		select {
		case state.turnEvents <- terminal:
		case <-state.turnAbandoned: // caller abandoned; terminal already in sinks
		case <-ctx.Done(): // hard loop kill; terminal already in sinks
		}
		close(state.turnEvents)
		state.turnEvents = nil
		state.turnAbandoned = nil
	}

	forceAbandon := func() {
		abandoned := make(chan struct{})
		close(abandoned)
		state.turnAbandoned = abandoned
	}

	ackShutdowns := func(err error) {
		for _, ack := range state.shutdownAcks {
			ack <- err
		}
		state.shutdownAcks = nil
	}

	for {
		select {
		case cmd, ok := <-commands:
			if !ok {
				return
			}
			switch c := cmd.(type) {

			case command.StartTurn:
				if err := c.Validate(); err != nil {
					slog.Warn("invalid StartTurn command", "error", err)
					if c.Ack != nil {
						c.Ack <- err
					}
					if c.Events != nil {
						close(c.Events)
					}
					continue
				}
				if state.status != loopIdle {
					reason := command.TurnAlreadyRunning
					if state.status == loopShuttingDown {
						reason = command.SessionShuttingDown
					}
					c.Ack <- &command.TurnBusyError{Reason: reason}
					close(c.Events)
					continue
				}
				turnID, err := cfg.idGen()
				if err != nil {
					// Cannot mint a TurnID; reject the turn at the gate (the turn
					// never starts) rather than running an unidentifiable turn.
					slog.Error("turn id generation failed; rejecting StartTurn", "error", err)
					c.Ack <- &IDGenerationError{Cause: err}
					close(c.Events)
					continue
				}
				state.turnIndex++
				state.turnID = turnID
				state.causationID = c.CommandHeader().ID
				state.status = loopRunning
				state.turnEvents = c.Events
				state.turnAbandoned = c.Abandoned
				turnCtx, cancel := context.WithCancel(c.Ctx)
				state.cancelTurn = cancel
				idx, preMsgs := state.turnIndex, state.msgs
				go func() {
					defer cancel()
					defer func() {
						if r := recover(); r != nil {
							slog.Error("turn goroutine panicked", "panic", r)
							// preMsgs excludes the user message (runTurn appends it
							// internally), so a panic rolls back exactly like a
							// normal failure: history holds only completed pairs.
							internal <- turnResult{
								msgs:     preMsgs,
								terminal: event.TurnFailed{TurnIndex: idx, Err: &event.TurnPanicError{Detail: fmt.Sprintf("%v", r)}},
							}
						}
					}()
					// Non-terminal events apply backpressure rather than drop:
					// a slow Stream consumer slows token production for its own
					// turn (never the actor). Escapes on Abandoned (caller gone)
					// and turnCtx.Done (interrupt/shutdown) keep emit from pinning
					// the turn goroutine when the consumer stops reading.
					emit := func(ev event.Event) {
						publish(ev)
						select {
						case c.Events <- ev:
						case <-c.Abandoned:
						case <-turnCtx.Done():
						}
					}
					updated, terminal := runTurn(turnCtx, c.Input, idx, preMsgs, cfg, cfg.Client, emit)
					internal <- turnResult{msgs: updated, terminal: terminal}
				}()
				c.Ack <- nil

			case command.Interrupt:
				if err := c.Validate(); err != nil {
					slog.Warn("invalid Interrupt command", "error", err)
					continue
				}
				if state.cancelTurn != nil {
					state.cancelTurn()
					state.cancelTurn = nil
					c.Ack <- true
				} else {
					c.Ack <- false
				}

			case command.Shutdown:
				if err := c.Validate(); err != nil {
					slog.Warn("invalid Shutdown command", "error", err)
				} else {
					state.shutdownAcks = append(state.shutdownAcks, c.Ack)
				}
				if state.status == loopShuttingDown {
					continue
				}
				wasRunning := state.status == loopRunning
				state.status = loopShuttingDown
				if state.cancelTurn != nil {
					state.cancelTurn()
					state.cancelTurn = nil
				}
				if !wasRunning {
					ackShutdowns(nil)
					return
				}
				// Turn goroutine is winding down; wait for internal below.
			}

		case result := <-internal:
			state.msgs = result.msgs
			state.cancelTurn = nil
			shuttingDown := state.status == loopShuttingDown
			if !shuttingDown {
				state.status = loopIdle
			}
			// deliverAndClose publishes the terminal envelope, which must still
			// carry this turn's correlation IDs, so clear them only afterward.
			deliverAndClose(result.terminal)
			state.turnID = uuid.UUID{}
			state.causationID = uuid.UUID{}
			if shuttingDown {
				ackShutdowns(nil)
				return
			}

		case <-ctx.Done():
			if state.cancelTurn != nil {
				state.cancelTurn()
				state.cancelTurn = nil
			}
			if state.status == loopRunning || state.status == loopShuttingDown {
				// Hard loop kill. Wait for the cancelled turn goroutine to drain
				// and deliver its terminal, but bound the wait: a provider that
				// ignores ctx must not hold the actor (and Loop.Done) hostage.
				// forceAbandon lets deliverAndClose skip a caller that is already
				// gone; the timeout detaches a goroutine still blocked in the
				// provider. We do NOT close turnEvents on the timeout path — the
				// detached goroutine may still hold it and would panic on a send
				// to a closed channel; it is wedged in the provider and would
				// never have produced a terminal anyway.
				forceAbandon()
				select {
				case result := <-internal:
					deliverAndClose(result.terminal)
				case <-time.After(cfg.DrainTimeout):
					slog.Error("turn goroutine did not drain after ctx cancel; detaching",
						"timeout", cfg.DrainTimeout)
					state.turnEvents = nil
					state.turnAbandoned = nil
				}
			}
			ackShutdowns(&command.LoopTerminatedError{Cause: ctx.Err()})
			return
		}
	}
}
