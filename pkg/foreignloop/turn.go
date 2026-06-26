package foreignloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
)

// turnOutcome is the turn goroutine's hand-back to the actor. The turn goroutine
// publishes the live Ephemeral events, the transcript-derived StepDones, and the
// non-interrupt terminal ITSELF; the actor only needs to commit the resulting
// messages and (on an interrupt) publish TurnInterrupted. committed is the
// transcript-derived (or soft-degraded) assistant history to append. success is true
// for a TurnDone terminal. interrupted means the turn ctx was cancelled and the
// goroutine published NO terminal (the actor owns TurnInterrupted). spawned is true
// once Spawn succeeded, which advances hasSpawned so the next turn resumes the
// foreign session instead of starting a new one.
type turnOutcome struct {
	committed   content.AgenticMessages
	success     bool
	interrupted bool
	spawned     bool
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
	// commits to it, independent of whether the agent process comes up.
	pub(event.TurnStarted{TurnIndex: cur, Message: user})

	ft := ForeignTurn{
		SystemPrompt: l.cfg.Model.System,
		ForeignSID:   l.sid,
		StartNew:     !l.hasSpawned,
		Input:        c.Blocks,
		Cwd:          l.spec.Cwd,
		Posture:      l.spec.Posture,
	}

	turnCtx, cancel := context.WithCancel(loopCtx)
	result := make(chan turnOutcome, 1)
	go l.driveTurn(turnCtx, cancel, ft, cur, pub, result)
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

// handleTurnCommand routes a command that arrives WHILE a turn runs. D2 honors only
// the snapshot/result/loop-ctx paths (handled by awaitTurn); every command here is
// dropped with a warning. Interrupt and Shutdown handling land in D4. done reports
// whether the turn is resolved (the actor leaves awaitTurn); exit reports whether the
// whole actor must stop.
func (l *Loop) handleTurnCommand(cmd command.Command, _ event.TurnIndex,
	_ context.CancelFunc, _ func(event.Event), _ chan turnOutcome) (done, exit bool) {
	slog.Warn("foreignloop: dropping un-honorable command during turn", "type", fmt.Sprintf("%T", cmd))
	return false, false
}

// applyOutcome commits a resolved turn into the actor-owned state. On an interrupt it
// publishes TurnInterrupted and commits nothing. Otherwise it appends the committed
// assistant history, advances hasSpawned once the agent actually spawned, and (on a
// successful terminal) advances turnIndex.
func (l *Loop) applyOutcome(cur event.TurnIndex, out turnOutcome, pub func(event.Event)) {
	if out.interrupted {
		pub(event.TurnInterrupted{TurnIndex: cur})
		return
	}
	l.msgs = append(l.msgs, out.committed...)
	if out.spawned {
		// NOTE: hasSpawned tracks "the foreign session exists on disk", so it advances
		// whenever Spawn succeeded — even on a failed terminal — so the next turn
		// resumes rather than starting a new (and orphaning the prior) session.
		l.hasSpawned = true
	}
	if out.success {
		l.turnIndex = cur
	}
}

// driveTurn is the per-turn goroutine. It spawns the agent, drains the live stream
// (publishing Ephemeral events via the mapper and collecting the authoritative
// assistant messages), then — if not interrupted — commits the transcript-derived
// history and publishes the terminal, handing the outcome back to the actor. It never
// touches actor-owned state; pub touches only immutable loop fields.
func (l *Loop) driveTurn(turnCtx context.Context, cancel context.CancelFunc, ft ForeignTurn,
	cur event.TurnIndex, pub func(event.Event), result chan turnOutcome) {
	defer cancel()
	stream, err := l.spec.Agent.Spawn(turnCtx, ft)
	if err != nil {
		// Spawn never came up: TurnStarted was already published, so the turn is closed
		// with TurnFailed. The agent never spawned, so hasSpawned stays false (the next
		// turn retries StartNew). Nothing is committed.
		pub(event.TurnFailed{TurnIndex: cur, Err: &SpawnError{Cause: err}})
		result <- turnOutcome{}
		return
	}
	defer func() { _ = stream.Close() }()

	assistant, termErr := l.drainStream(stream, cur, pub)
	if turnCtx.Err() != nil {
		result <- turnOutcome{interrupted: true, spawned: true}
		return
	}
	committed := l.commitTurn(stream.TranscriptPath(), cur, assistant, pub)
	if termErr != nil {
		pub(event.TurnFailed{TurnIndex: cur, Err: termErr})
		result <- turnOutcome{committed: committed, spawned: true}
		return
	}
	pub(event.TurnDone{TurnIndex: cur, Message: lastOf(assistant)})
	result <- turnOutcome{committed: committed, success: true, spawned: true}
}

// drainStream consumes the live foreign stream. The mapper translates ONLY the live
// Ephemeral events (token/tool deltas), which are published immediately; the
// authoritative assistant rounds are collected for the commit phase (the transcript
// is authoritative, so a live StepDone is intentionally NOT published). It returns the
// collected assistant messages and any terminal-error cause reported on the stream.
func (l *Loop) drainStream(stream ForeignStream, cur event.TurnIndex, pub func(event.Event)) ([]*content.AIMessage, error) {
	m := newMapper(cur, l.idGen)
	var assistant []*content.AIMessage
	var termErr error
	for fe := range stream.Events() {
		switch fe.Kind {
		case ForeignInit:
			if fe.SessionID != "" && fe.SessionID != l.sid {
				slog.Warn("foreignloop: foreign session id mismatch", "want", l.sid, "got", fe.SessionID)
			}
		case ForeignStepComplete:
			if fe.Message != nil {
				assistant = append(assistant, fe.Message)
			}
		case ForeignTerminalOK:
			if fe.Message != nil {
				assistant = append(assistant, fe.Message)
			}
		case ForeignTerminalError:
			termErr = &ForeignResultError{Detail: fe.ErrText}
		default:
			l.publishMapped(m, fe, pub)
		}
	}
	return assistant, termErr
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
