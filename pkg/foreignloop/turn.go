package foreignloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// turnOutcome is the turn goroutine's hand-back to the actor. The turn goroutine
// publishes the live Ephemeral events, the transcript-derived StepDones, and the
// non-interrupt terminal ITSELF; the actor only needs to commit the resulting
// messages and (on an interrupt) publish TurnInterrupted. committed is the
// transcript-derived (or soft-degraded) assistant history to append. success is true
// for a TurnDone terminal. interrupted means the turn ctx was cancelled and the
// goroutine published NO terminal (the actor owns TurnInterrupted). spawned is true
// once a prebound session spawned or a late-bound session id was learned, which
// advances hasSpawned so the next turn resumes. boundSID is the first late-bound
// foreign session id observed by the turn goroutine; the actor applies it.
type turnOutcome struct {
	committed   content.AgenticMessages
	success     bool
	interrupted bool
	spawned     bool
	boundSID    string
}

// drainedTurn is the stream drain's hand-back to driveTurn. bindErr is separate from
// termErr because a failed lock transition must stop before transcript commit, while
// a foreign terminal error still commits the transcript-derived assistant history.
type drainedTurn struct {
	assistant []*content.AIMessage
	boundSID  string
	termErr   error
	bindErr   error
	terminal  bool
}

type turnLock interface {
	release()
}

type turnLockOps struct {
	acquireTemporary func(loopID, cwd string) (turnLock, error)
	acquireDurable   func(sid, cwd string) (turnLock, error)
}

func productionTurnLockOps() turnLockOps {
	return turnLockOps{
		acquireTemporary: func(loopID, cwd string) (turnLock, error) {
			return acquireTemporaryForeignLock(loopID, cwd)
		},
		acquireDurable: func(sid, cwd string) (turnLock, error) {
			return acquireForeignLock(sid, cwd)
		},
	}
}

// runTurn drives one foreign turn from a UserInput submit. It runs ON the actor
// goroutine: it mints the turn/step ids, publishes TurnStarted BEFORE Spawn, launches
// the turn goroutine (driveTurn), and then takes over the actor's select via awaitTurn
// until the turn resolves. It returns whether the actor must EXIT (a Shutdown that
// arrived mid-turn). On an id-mint failure it fails secure: log and drop the submit
// (no partial turn, no zero-id events).
func (l *Loop) runTurn(loopCtx context.Context, c command.UserInput) (exit bool) {
	turnID, err := l.idGen()
	if err != nil {
		slog.Error("foreignloop: turn id mint failed; dropping submit (fail-secure)", "error", err)
		return false
	}
	stepID, err := l.idGen()
	if err != nil {
		slog.Error("foreignloop: step id mint failed; dropping submit (fail-secure)", "error", err)
		return false
	}
	cur := l.turnIndex + 1
	pub := l.publisher(loopCtx, turnID, stepID)

	user := &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: c.Blocks}}
	// TurnStarted is published BEFORE Spawn: a turn is announced the moment the actor
	// commits to it, independent of whether the agent process comes up. Its Cause
	// carries the submit command id so the session's drain (drainToFinalText) can
	// correlate the opening turn — RunSubagent over a foreign loop hangs without it.
	// The publish chokepoint only fills Coordinates (leaving Cause intact) and
	// Factory.Stamp adds only EventID+CreatedAt, so this Cause survives stamping.
	pub(event.TurnStarted{
		Header:    event.Header{Cause: identity.Cause{CommandID: c.Header.CommandID, Agency: c.Header.Agency}},
		TurnIndex: cur,
		Message:   user,
	})

	ft := ForeignTurn{
		SystemPrompt: l.cfg.System,
		ForeignSID:   l.sid,
		StartNew:     !l.hasSpawned,
		Input:        c.Blocks,
		Cwd:          l.spec.Cwd,
		Posture:      l.spec.Posture,
	}

	turnCtx, cancel := context.WithCancel(loopCtx)
	result := make(chan turnOutcome, 1)
	go l.driveTurn(turnCtx, cancel, ft, cur, l.sidBound, pub, result)
	return l.awaitTurn(loopCtx, cur, cancel, pub, result)
}

// awaitTurn is the actor's INNER select while a turn runs. It serves committed
// (pre-turn) snapshots, applies the turn outcome when it arrives, honors control
// commands (D4), and tears down on loop-ctx cancellation. It returns whether the
// actor must exit.
func (l *Loop) awaitTurn(loopCtx context.Context, cur event.TurnIndex,
	cancel context.CancelFunc, pub func(event.Event), result chan turnOutcome) (exit bool) {
	for {
		select {
		case out := <-result:
			l.applyOutcome(cur, out, pub)
			return false
		case req := <-l.snapshots:
			req.reply <- snapshotResult{msgs: cloneMessages(l.msgs), turnIndex: l.turnIndex}
		case cmd := <-l.Commands:
			if done, exit := l.handleTurnCommand(cmd, cur, cancel, pub, result); done {
				return exit
			}
		case <-loopCtx.Done():
			cancel()
			<-result // drain the cancelled turn goroutine before exiting.
			return true
		}
	}
}

// handleTurnCommand routes a command that arrives WHILE a turn runs. Interrupt
// cancels the turn ctx, waits for the goroutine to drain, and publishes
// TurnInterrupted (via applyOutcome). Shutdown cancels, drains, acks, and exits the
// actor. Every other command is an un-honorable mid-turn command — dropped with a
// warning, never blocking and never publishing. done reports whether the turn is
// resolved (the actor leaves awaitTurn); exit reports whether the whole actor stops.
func (l *Loop) handleTurnCommand(cmd command.Command, cur event.TurnIndex,
	cancel context.CancelFunc, pub func(event.Event), result chan turnOutcome) (done, exit bool) {
	switch c := cmd.(type) {
	case command.Interrupt:
		cancel()
		// applyOutcome publishes TurnInterrupted for the interrupted outcome; if the
		// turn raced to completion before cancel landed, it commits that outcome instead
		// (no double terminal — the goroutine already published its own).
		l.applyOutcome(cur, <-result, pub)
		c.Ack <- true
		return true, false
	case command.Shutdown:
		cancel()
		<-result // drain the cancelled turn goroutine before exiting.
		c.Ack <- nil
		return true, true
	default:
		slog.Warn("foreignloop: dropping un-honorable command during turn", "type", fmt.Sprintf("%T", cmd))
		return false, false
	}
}

// applyOutcome commits a resolved turn into the actor-owned state. On an interrupt it
// publishes TurnInterrupted and commits nothing. Otherwise it appends the committed
// assistant history, advances hasSpawned once a resumable session is known, and (on
// a successful terminal) advances turnIndex.
func (l *Loop) applyOutcome(cur event.TurnIndex, out turnOutcome, pub func(event.Event)) {
	l.applyBoundSID(out.boundSID)
	if out.interrupted {
		pub(event.TurnInterrupted{TurnIndex: cur})
		return
	}
	l.msgs = append(l.msgs, out.committed...)
	if out.spawned {
		// A prebound session is already resumable after Spawn; a late-bound session
		// becomes resumable only after ForeignInit supplies its id.
		l.hasSpawned = true
	}
	if out.success {
		l.turnIndex = cur
	}
}

func (l *Loop) applyBoundSID(boundSID string) {
	if boundSID == "" {
		return
	}
	l.sid = boundSID
	l.sidBound = true
	l.hasSpawned = true
}

// driveTurn is the per-turn goroutine. It spawns the agent, drains the live stream
// (publishing Ephemeral events via the mapper and collecting the authoritative
// assistant messages), then — if not interrupted — commits the transcript-derived
// history and publishes the terminal, handing the outcome back to the actor. It never
// touches actor-owned state; pub touches only immutable loop fields.
func (l *Loop) driveTurn(turnCtx context.Context, cancel context.CancelFunc, ft ForeignTurn,
	cur event.TurnIndex, sidBound bool, pub func(event.Event), result chan turnOutcome) {
	l.driveTurnWithLocks(turnCtx, cancel, ft, cur, sidBound, pub, result, productionTurnLockOps())
}

func (l *Loop) driveTurnWithLocks(turnCtx context.Context, cancel context.CancelFunc, ft ForeignTurn,
	cur event.TurnIndex, sidBound bool, pub func(event.Event), result chan turnOutcome, locks turnLockOps) {
	defer cancel()
	var (
		lk  turnLock
		err error
	)
	if sidBound {
		lk, err = locks.acquireDurable(ft.ForeignSID, l.spec.Cwd)
	} else {
		lk, err = locks.acquireTemporary(l.loopID.String(), l.spec.Cwd)
	}
	if err != nil {
		// A live process already drives this (sid,cwd) Claude session (or the lock I/O
		// failed): refuse to spawn a second driver that would corrupt the transcript.
		// TurnStarted was already published, so the turn is closed with TurnFailed; no
		// session was created, so hasSpawned stays false and nothing is committed.
		pub(event.TurnFailed{TurnIndex: cur, Err: err})
		result <- turnOutcome{}
		return
	}
	var outcome turnOutcome
	defer func() { result <- outcome }()
	defer func() { lk.release() }()
	stream, err := l.spec.Agent.Spawn(turnCtx, ft)
	if err != nil {
		// Spawn never came up: TurnStarted was already published, so the turn is closed
		// with TurnFailed. The agent never spawned, so hasSpawned stays false (the next
		// turn retries StartNew). Nothing is committed.
		pub(event.TurnFailed{TurnIndex: cur, Err: &SpawnError{Cause: err}})
		return
	}
	bindSID := func(sid string) error {
		boundLock, err := locks.acquireDurable(sid, l.spec.Cwd)
		if err != nil {
			return err
		}
		lk.release()
		lk = boundLock
		return nil
	}
	drained := l.drainStream(stream, cur, sidBound, ft.ForeignSID, bindSID, pub)
	closeErr := stream.Close()
	spawned := sidBound || drained.boundSID != ""
	if drained.bindErr != nil {
		pub(event.TurnFailed{TurnIndex: cur, Err: errors.Join(drained.bindErr, closeErr)})
		outcome = turnOutcome{spawned: spawned, boundSID: drained.boundSID}
		return
	}
	if turnCtx.Err() != nil {
		outcome = turnOutcome{interrupted: true, spawned: spawned, boundSID: drained.boundSID}
		return
	}
	committed := l.commitTurn(stream.TranscriptPath(), cur, drained.assistant, pub)
	// The stream terminal/protocol error is primary; Join retains any typed Close
	// cause for errors.As. Cancellation above deliberately preserves interruption.
	if turnErr := errors.Join(drained.termErr, closeErr); turnErr != nil {
		pub(event.TurnFailed{TurnIndex: cur, Err: turnErr})
		outcome = turnOutcome{committed: committed, spawned: spawned, boundSID: drained.boundSID}
		return
	}
	pub(event.TurnDone{TurnIndex: cur, Message: lastOf(drained.assistant)})
	outcome = turnOutcome{committed: committed, success: true, spawned: spawned, boundSID: drained.boundSID}
}

// drainStream consumes the live foreign stream. The mapper translates ONLY the live
// Ephemeral events (token/tool deltas), which are published immediately; the
// authoritative assistant rounds are collected for the commit phase (the transcript
// is authoritative, so a live StepDone is intentionally NOT published). A newly
// learned sid is transitioned to its durable lock before its bound event is visible.
func (l *Loop) drainStream(stream ForeignStream, cur event.TurnIndex, sidBound bool,
	expectedSID string, bindSID func(string) error, pub func(event.Event)) drainedTurn {
	m := newMapper(cur, l.idGen)
	var out drainedTurn
	for fe := range stream.Events() {
		switch fe.Kind {
		case ForeignInit:
			if out.terminal {
				continue
			}
			if fe.SessionID != "" && !sidBound && out.boundSID == "" {
				out.boundSID = fe.SessionID
				expectedSID = fe.SessionID
				if err := bindSID(fe.SessionID); err != nil {
					pub(event.ForeignSessionBound{ForeignSID: fe.SessionID})
					out.bindErr = err
					return out
				}
				pub(event.ForeignSessionBound{ForeignSID: fe.SessionID})
			} else if fe.SessionID != "" && fe.SessionID != expectedSID {
				slog.Warn("foreignloop: foreign session id mismatch", "want", expectedSID, "got", fe.SessionID)
			}
		case ForeignStepComplete:
			if fe.Message != nil {
				out.assistant = append(out.assistant, fe.Message)
			}
		case ForeignTerminalOK:
			out.terminal = true
			if fe.Message != nil {
				out.assistant = append(out.assistant, fe.Message)
			}
		case ForeignTerminalError:
			out.terminal = true
			out.termErr = &ForeignResultError{Detail: fe.ErrText}
		default:
			l.publishMapped(m, fe, pub)
		}
	}
	if out.bindErr == nil {
		switch {
		case !sidBound && out.boundSID == "":
			out.termErr = errors.Join(
				out.termErr,
				&ForeignProtocolError{Reason: "late-bound stream ended without init event"},
			)
		case !out.terminal:
			out.termErr = &ForeignProtocolError{Reason: "stream ended without terminal event"}
		}
	}
	return out
}

// publishMapped maps one live foreign event to its Ephemeral looprig events and
// publishes them. A mapping error is fail-secure: log and skip (never emit an
// uncorrelated event).
func (l *Loop) publishMapped(m *mapper, fe ForeignEvent, pub func(event.Event)) {
	evs, err := m.toEvents(fe)
	if err != nil {
		slog.Error("foreignloop: mapping foreign event failed; skipping", "error", err)
		return
	}
	for _, ev := range evs {
		pub(ev)
	}
}

// commitTurn reads the authoritative on-disk transcript and publishes one StepDone
// per assistant round, accumulating the committed history. If the transcript is
// unavailable it SOFT-DEGRADES to the assistant messages seen on the live stream —
// emitting a synthetic StepDone per message so restore still keeps the reply rather
// than losing the turn's output entirely.
func (l *Loop) commitTurn(path string, cur event.TurnIndex, assistant []*content.AIMessage, pub func(event.Event)) content.AgenticMessages {
	groups, err := decodeTranscriptTail(path, int(cur)-1)
	if err != nil {
		var unavailable *TranscriptUnavailableError
		if !errors.As(err, &unavailable) {
			slog.Warn("foreignloop: transcript decode failed; degrading to stream assistant", "error", err)
		}
		return commitFromAssistant(assistant, pub)
	}
	var committed content.AgenticMessages
	for _, group := range groups {
		pub(event.StepDone{Messages: group})
		committed = append(committed, group...)
	}
	return committed
}

// commitFromAssistant is the transcript-loss fallback: it publishes one synthetic
// StepDone per assistant message collected from the live stream and returns them as
// the committed history, so a turn whose transcript could not be read still commits
// the assistant reply rather than dropping it.
func commitFromAssistant(assistant []*content.AIMessage, pub func(event.Event)) content.AgenticMessages {
	var committed content.AgenticMessages
	for _, am := range assistant {
		pub(event.StepDone{Messages: content.AgenticMessages{am}})
		committed = append(committed, am)
	}
	return committed
}

// lastOf returns the last collected assistant message (the complete AI response for
// the turn terminal), or nil if the stream carried none.
func lastOf(assistant []*content.AIMessage) *content.AIMessage {
	if len(assistant) == 0 {
		return nil
	}
	return assistant[len(assistant)-1]
}
