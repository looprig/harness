package loopruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

// These tests pin the loop actor's half of command.Admission: the durable
// acceptance (Commit) happens strictly before any effect, a refusal publishes
// nothing, and a taken input is carried over rather than cancelled when the loop
// goes away.

// admitInput sends one admitted input and returns the loop's answer. commit is
// called in Commit and may be nil (a restore re-offer).
func admitInput(t *testing.T, l *Loop, id uuid.UUID, commit func(context.Context) error) error {
	t.Helper()
	result := make(chan error, 1)
	if !sendCmd(t, l, command.UserInput{
		Header:    command.Header{CommandID: id, Agency: identity.AgencyUser},
		Blocks:    textBlocks("admitted"),
		Admission: &command.Admission{Commit: commit, Result: result},
	}) {
		t.Fatalf("loop exited before the admitted input was delivered")
	}
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("the loop never answered the admission")
		return nil
	}
}

func causedBy(events []event.Event, id uuid.UUID) []event.Event {
	var out []event.Event
	for _, ev := range events {
		if ev.EventHeader().Cause.CommandID == id {
			out = append(out, ev)
		}
	}
	return out
}

func TestRuntimeAdmissionCommitsBeforeAnyEffect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		running bool
	}{
		{name: "idle start"},
		{name: "running queue", running: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
			if tc.running {
				startTurn(t, l, rec, nil)
			}
			id := mustID(t)
			commits := 0
			var effectsAtCommit int
			err := admitInput(t, l, id, func(context.Context) error {
				commits++
				effectsAtCommit = len(causedBy(rec.events(), id))
				return nil
			})
			if err != nil {
				t.Fatalf("admission = %v, want taken", err)
			}
			if commits != 1 {
				t.Fatalf("Commit called %d times, want exactly once", commits)
			}
			if effectsAtCommit != 0 {
				t.Fatalf("%d events caused by the input were published BEFORE its acceptance was durable", effectsAtCommit)
			}
			want := event.Event(event.TurnStarted{})
			if tc.running {
				want = event.InputQueued{}
			}
			got := awaitReply(t, rec, id)
			if _, same := got.(event.TurnStarted); !tc.running && !same {
				t.Fatalf("outcome = %T, want %T", got, want)
			}
			if _, same := got.(event.InputQueued); tc.running && !same {
				t.Fatalf("outcome = %T, want %T", got, want)
			}
		})
	}
}

func TestRuntimeAdmissionCommitFailureLeavesNoEffect(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	sentinel := errors.New("disposition append failed")
	id := mustID(t)
	if err := admitInput(t, l, id, func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("admission = %v, want the Commit error unchanged", err)
	}
	// The actor is healthy and the input left no trace: a later input starts.
	next := mustID(t)
	if err := admitInput(t, l, next, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("next admission = %v", err)
	}
	if _, ok := awaitReply(t, rec, next).(event.TurnStarted); !ok {
		t.Fatalf("the next input did not start")
	}
	if got := causedBy(rec.events(), id); len(got) != 0 {
		t.Fatalf("an input whose acceptance failed published %d events: %+v", len(got), got)
	}
}

func TestRuntimeAdmissionRefusalPublishesNothingAndDoesNotCommit(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)
	for range loop.ManagedInputQueueCapacity {
		filler := mustID(t)
		if !sendCmd(t, l, command.UserInput{Header: command.Header{CommandID: filler}, Blocks: textBlocks("fill")}) {
			t.Fatalf("loop exited while filling its queue")
		}
		awaitReply(t, rec, filler)
	}
	id := mustID(t)
	committed := false
	err := admitInput(t, l, id, func(context.Context) error { committed = true; return nil })
	var rejected *loop.InputRejectedError
	if !errors.As(err, &rejected) || rejected.Reason != event.RejectQueueFull {
		t.Fatalf("admission = %v, want *loop.InputRejectedError{RejectQueueFull}", err)
	}
	if committed {
		t.Fatalf("a refused input's acceptance was committed")
	}
	if got := causedBy(rec.events(), id); len(got) != 0 {
		t.Fatalf("a refused admitted input published %d events (the disposition is its only answer): %+v", len(got), got)
	}
}

func TestRuntimeAdmissionConflictingHandshakesAreDeclined(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	id := mustID(t)
	result := make(chan error, 1)
	committed := false
	sendCmd(t, l, command.UserInput{
		Header:    command.Header{CommandID: id},
		Blocks:    textBlocks("both"),
		Accepted:  make(chan error, 1),
		Admission: &command.Admission{Commit: func(context.Context) error { committed = true; return nil }, Result: result},
	})
	var conflict *ConflictingAdmissionError
	if err := <-result; !errors.As(err, &conflict) {
		t.Fatalf("admission = %v, want *ConflictingAdmissionError", err)
	}
	if committed || len(causedBy(rec.events(), id)) != 0 {
		t.Fatalf("a conflicting input was committed or published")
	}
}

// TestCarriedOverInputIsNotCancelledWhenTheLoopGoesAway covers both exits — a
// graceful Shutdown and a cancelled loop context — against a control: an ordinary
// queued input IS still returned, so the exception is exactly the carry-over mark.
func TestCarriedOverInputIsNotCancelledWhenTheLoopGoesAway(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		shutdown bool
	}{
		{name: "graceful shutdown", shutdown: true},
		{name: "cancelled loop context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, rec, cancel := newLoop(t, &fakeLLM{blockUntilCancel: true})
			startTurn(t, l, rec, nil)
			ordinary := mustID(t)
			if !sendCmd(t, l, command.UserInput{Header: command.Header{CommandID: ordinary}, Blocks: textBlocks("ordinary")}) {
				t.Fatalf("loop exited")
			}
			awaitReply(t, rec, ordinary)
			admitted := mustID(t)
			if err := admitInput(t, l, admitted, func(context.Context) error { return nil }); err != nil {
				t.Fatalf("admission = %v", err)
			}
			if tc.shutdown {
				ack := make(chan error, 1)
				if !sendCmd(t, l, command.Shutdown{Ack: ack}) {
					t.Fatalf("loop exited before Shutdown")
				}
			} else {
				cancel()
			}
			select {
			case <-l.Done:
			case <-time.After(5 * time.Second):
				t.Fatalf("loop did not exit")
			}
			var ordinaryCancelled bool
			for _, ev := range rec.events() {
				cancelled, ok := ev.(event.InputCancelled)
				if !ok {
					continue
				}
				switch cancelled.Cause.CommandID {
				case admitted:
					t.Fatalf("a carried-over input was returned as InputCancelled on exit")
				case ordinary:
					ordinaryCancelled = true
				}
			}
			if !ordinaryCancelled {
				t.Fatalf("control: the ordinary queued input was not returned on exit")
			}
		})
	}
}

// TestCarriedOverInputIsStillRetainedByAnInterrupt keeps the exception narrow: an
// ordinary interrupt is not the loop going away, and a carry-over input is human
// input, so it is retained and starts its own turn once the interrupted one ends.
func TestCarriedOverInputIsStillRetainedByAnInterrupt(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)
	admitted := mustID(t)
	if err := admitInput(t, l, admitted, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("admission = %v", err)
	}
	ack := make(chan bool, 1)
	if !sendCmd(t, l, command.Interrupt{Ack: ack}) {
		t.Fatalf("loop exited")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, ev := range causedBy(rec.events(), admitted) {
			switch ev.(type) {
			case event.TurnStarted:
				return
			case event.InputCancelled:
				t.Fatalf("an interrupt cancelled a carried-over human input")
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retained input never started after the interrupt")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gatedFailLLM streams nothing until release is closed, then fails the turn.
type gatedFailLLM struct{ release chan struct{} }

func (g *gatedFailLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("gatedFailLLM.Invoke not used")
}

func (g *gatedFailLLM) Stream(ctx context.Context, _ inference.Request) (*stream.StreamReader[content.Chunk], error) {
	next := func() (content.Chunk, error) {
		select {
		case <-g.release:
			return nil, errors.New("provider failed")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return stream.NewStreamReader(next, nil), nil
}

// TestCarriedOverInputIsStillReturnedByAFailedTurn keeps the carry-over exception
// narrow: a failed turn ahead of it, with the loop staying alive, resolves it
// VISIBLY (InputCancelled{TurnFailed}) like any queued input. Skipping it here
// would strand it silently in a live loop — nothing would ever replay it.
func TestCarriedOverInputIsStillReturnedByAFailedTurn(t *testing.T) {
	t.Parallel()
	llm := &gatedFailLLM{release: make(chan struct{})}
	l, rec, _ := newLoop(t, llm)
	startTurn(t, l, rec, nil)
	admitted := mustID(t)
	if err := admitInput(t, l, admitted, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("admission = %v", err)
	}
	close(llm.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, ev := range causedBy(rec.events(), admitted) {
			if cancelled, ok := ev.(event.InputCancelled); ok {
				if cancelled.Reason != event.CancelTurnFailed {
					t.Fatalf("returned with %v, want CancelTurnFailed", cancelled.Reason)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("a carried-over input behind a failed turn was never resolved")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCommitRunsUnderTheLoopContext is F3's loop half: a hard kill (the loop's
// context ending) preempts a Commit wedged in storage, so the actor can exit.
func TestCommitRunsUnderTheLoopContext(t *testing.T) {
	t.Parallel()
	l, _, cancel := newLoop(t, &fakeLLM{blockUntilCancel: true})
	result := make(chan error, 1)
	entered := make(chan struct{})
	if !sendCmd(t, l, command.UserInput{
		Header: command.Header{CommandID: mustID(t), Agency: identity.AgencyUser},
		Blocks: textBlocks("wedged"),
		Admission: &command.Admission{Result: result, Commit: func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}},
	}) {
		t.Fatalf("loop exited")
	}
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("admission = %v, want the cancelled Commit's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("a Commit wedged in storage was not preempted by the loop's context")
	}
	select {
	case <-l.Done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the loop did not exit after its context ended")
	}
}
