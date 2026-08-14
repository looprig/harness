package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// drainUUID builds a deterministic non-zero UUID from a seed for correlation
// fixtures. Mirrors identity.fixedUUID but local to this package's tests.
func drainUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

func cancelReasonPtr(reason event.CancelReason) *event.CancelReason { return &reason }

// fakeSubscription is a test double for event.Subscription: a buffered channel
// the test scripts events onto, a settable termination error, and a no-op Close.
// It is the seam that lets drainToFinalText be driven with a deterministic event
// sequence without a live hub/loop.
type fakeSubscription struct {
	events chan event.Delivery
	err    error
}

func newFakeSubscription(buf int) *fakeSubscription {
	return &fakeSubscription{events: make(chan event.Delivery, buf)}
}

func (f *fakeSubscription) Events() <-chan event.Delivery { return f.events }
func (f *fakeSubscription) Close() error                  { return nil }
func (f *fakeSubscription) Err() error                    { return f.err }

// feed wraps a scripted event in an event.Delivery (seq 0 — the drain helper ignores
// the sequence) and pushes it onto the fake's buffered channel.
func (f *fakeSubscription) feed(ev event.Event) { f.events <- event.Delivery{Event: ev} }

// turnStarted builds a TurnStarted whose Cause.CommandID is cmd and whose
// Coordinates.TurnID is turn — the opening resolution event drainToFinalText
// correlates on in phase 1. LoopID is left zero (matched by the also-zero LoopID
// on the zero-loop phase-2 helpers); use turnStartedOnLoop to pin a LoopID.
func turnStarted(cmd, turn uuid.UUID) event.TurnStarted {
	return turnStartedOnLoop(cmd, turn, uuid.UUID{})
}

// turnStartedOnLoop is turnStarted with an explicit Coordinates.LoopID, so phase 2
// can be driven with matching/mismatching loop ids for the fail-secure cross-check.
func turnStartedOnLoop(cmd, turn, loop uuid.UUID) event.TurnStarted {
	return event.TurnStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{LoopID: loop, TurnID: turn},
			Cause:       identity.Cause{CommandID: cmd},
		},
	}
}

// stepDone builds a StepDone for turn carrying a single AIMessage with text.
func stepDone(turn uuid.UUID, text string) event.StepDone {
	return event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{TurnID: turn}},
		Messages: content.AgenticMessages{aiMessage(text)},
	}
}

// aiMessage builds an *content.AIMessage with a single TextBlock.
func aiMessage(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

func TestAITextBoundsDelegatedOutput(t *testing.T) {
	got := aiText(aiMessage(strings.Repeat("x", maxDelegateOutputBytes+1)))
	if len(got) != maxDelegateOutputBytes {
		t.Fatalf("aiText length = %d, want %d", len(got), maxDelegateOutputBytes)
	}
}

// aiRefusalMessage builds an assistant message that declines on the provider's
// dedicated refusal channel — the shape core's *content.RefusalBlock exists to
// keep distinguishable from an empty successful answer.
func aiRefusalMessage(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.RefusalBlock{Text: text}},
	}}
}

// TestAITextCarriesRefusal pins the projection aiText applies to the blocks of a
// delegate's terminal message. A refusal is the child's answer — it declined —
// and dropping it hands the parent "" for a turn that completed, which is the
// zero-block-success failure RefusalBlock was added to prevent.
func TestAITextCarriesRefusal(t *testing.T) {
	t.Parallel()

	const refusal = "I'm sorry, I can't help with that."

	tests := []struct {
		name string
		msg  *content.AIMessage
		want string
	}{
		{
			name: "refusal-only reply is the delegate answer",
			msg:  aiRefusalMessage(refusal),
			want: refusal,
		},
		{
			name: "prose then refusal keeps both in order",
			msg: &content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{
					&content.TextBlock{Text: "Here is what I can say. "},
					&content.RefusalBlock{Text: refusal},
				},
			}},
			want: "Here is what I can say. " + refusal,
		},
		{
			name: "refusal then prose keeps both in order",
			msg: &content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{
					&content.RefusalBlock{Text: refusal},
					&content.TextBlock{Text: " Try a safer phrasing."},
				},
			}},
			want: refusal + " Try a safer phrasing.",
		},
		{
			name: "unexplained refusal contributes no text and yields the empty string",
			msg:  aiRefusalMessage(""),
			want: "",
		},
		{
			name: "thinking and tool-use blocks still contribute nothing",
			msg: &content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{
					&content.ThinkingBlock{Thinking: "internal"},
					&content.ToolUseBlock{ID: "t1", Name: "Bash"},
					&content.RefusalBlock{Text: refusal},
				},
			}},
			want: refusal,
		},
		{
			name: "nil message is the empty string",
			msg:  nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := aiText(tt.msg); got != tt.want {
				t.Fatalf("aiText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAITextBoundsRefusalOutput proves the delegate output cap applies to a
// refusal exactly as it does to prose: a hostile provider cannot bypass the
// bound by moving its payload onto the refusal channel.
func TestAITextBoundsRefusalOutput(t *testing.T) {
	t.Parallel()
	got := aiText(aiRefusalMessage(strings.Repeat("x", maxDelegateOutputBytes+1)))
	if len(got) != maxDelegateOutputBytes {
		t.Fatalf("aiText length = %d, want %d", len(got), maxDelegateOutputBytes)
	}
}

// TestDrainToFinalTextReturnsRefusalTerminal drives the drain helper itself with
// a scripted refusal terminal, so the defect is pinned at the helper boundary as
// well as at the projection.
func TestDrainToFinalTextReturnsRefusalTerminal(t *testing.T) {
	t.Parallel()
	const refusal = "I'm sorry, I can't help with that."
	cmd, turn := drainUUID(0x21), drainUUID(0x22)
	sub := newFakeSubscription(4)
	sub.feed(turnStarted(cmd, turn))
	sub.feed(turnDone(turn, aiRefusalMessage(refusal)))

	got, err := drainDelegateAnswer(context.Background(), sub, cmd, nil)
	if err != nil {
		t.Fatalf("drainDelegateAnswer() error = %v", err)
	}
	if got != refusal {
		t.Fatalf("drainDelegateAnswer() = %q, want %q", got, refusal)
	}
}

// delegateRefusingChild is a child agent whose provider declines on the
// dedicated refusal channel: the stream carries a *content.RefusalChunk and no
// text at all, exactly as an OpenAI `refusal` delta arrives. The loop runtime
// materializes it into a *content.RefusalBlock on the terminal AIMessage.
func delegateRefusingChild(name, refusal string) loop.Definition {
	return mustDefine(
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{&content.RefusalChunk{Text: refusal}}}, validModel(name)),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

// TestDelegateStartSyncReturnsChildRefusal is the decisive end-to-end case: a
// real refusing subagent is started synchronously and its answer travels the
// whole managed-delegation path — provider stream, loop runtime, TurnDone,
// drain, delegate tool result. The parent must receive the refusal. Receiving ""
// would tell the parent model the child completed and produced nothing, which is
// the zero-block-success confusion *content.RefusalBlock exists to prevent.
func TestDelegateStartSyncReturnsChildRefusal(t *testing.T) {
	t.Parallel()
	const refusal = "I'm sorry, I can't help with that."
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateRefusingChild("child", refusal))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: "child", Name: "api_planner", Message: "go", WaitForResponse: true,
	})
	if err != nil {
		t.Fatalf("Execute(start) error = %v", err)
	}
	if res.ResponseStatus != tool.DelegateResponseCompleted || res.State != tool.AgentStateIdle {
		t.Fatalf("result state/status = %v/%v, want idle/completed", res.State, res.ResponseStatus)
	}
	if res.Response != refusal {
		t.Fatalf("delegate response = %q, want the child's refusal %q", res.Response, refusal)
	}
}

func turnDone(turn uuid.UUID, msg *content.AIMessage) event.TurnDone {
	return event.TurnDone{
		Header:  event.Header{Coordinates: identity.Coordinates{TurnID: turn}},
		Message: msg,
	}
}

// turnDoneOnLoop is turnDone with an explicit Coordinates.LoopID, for exercising
// the phase-2 LoopID cross-check (right TurnID on a wrong loop must be ignored).
func turnDoneOnLoop(turn, loop uuid.UUID, msg *content.AIMessage) event.TurnDone {
	return event.TurnDone{
		Header:  event.Header{Coordinates: identity.Coordinates{LoopID: loop, TurnID: turn}},
		Message: msg,
	}
}

func turnFailed(turn uuid.UUID, err error) event.TurnFailed {
	return event.TurnFailed{
		Header: event.Header{Coordinates: identity.Coordinates{TurnID: turn}},
		Err:    err,
	}
}

func turnInterrupted(turn uuid.UUID) event.TurnInterrupted {
	return event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{TurnID: turn}}}
}

func turnRejected(cmd uuid.UUID, reason event.RejectReason) event.TurnRejected {
	return event.TurnRejected{
		Header: event.Header{Cause: identity.Cause{CommandID: cmd}},
		Reason: reason,
	}
}

// errProvider is a sentinel leaf error used to assert TurnFailed.Err and
// subscription-loss wrapping reach the caller via errors.Is.
var errProvider = errors.New("provider exploded")

// TestDrainToFinalText drives the shared collect helper with scripted event
// sequences over a fake subscription, covering every §5 exit and the noise-
// rejection (phase-1 correlation) and ctx-interrupt fail-safe paths.
func TestDrainToFinalText(t *testing.T) {
	t.Parallel()

	cmd := drainUUID(0x01)
	turn := drainUUID(0x02)
	otherCmd := drainUUID(0x03)
	otherTurn := drainUUID(0x04)
	loop := drainUUID(0x05)
	otherLoop := drainUUID(0x06)

	tests := []struct {
		name string
		// script is fed onto the fake's buffered channel before draining (the
		// subscribe-before-submit ordering means every event is already buffered).
		script []event.Event
		// closeAfter closes the channel after the script (subscription-loss path).
		closeAfter bool
		// subErr is set on the fake before draining (the hub-forced loss cause).
		subErr error

		wantText         string
		wantErr          bool
		wantFailed       bool // *drainFailedError wrapping errProvider
		wantTurnRejected bool // *TurnRejectedError (the package's existing typed reject)
		wantCancelled    *event.CancelReason
		wantStatus       tool.DelegateStatusValue
		wantLost         bool // *drainLostError
		wantInterrupts   int32
	}{
		{
			name:     "clean TurnDone returns its message text",
			script:   []event.Event{turnStarted(cmd, turn), stepDone(turn, "partial"), turnDone(turn, aiMessage("final"))},
			wantText: "final",
		},
		{
			name:     "fallback to last StepDone when TurnDone.Message is nil",
			script:   []event.Event{turnStarted(cmd, turn), stepDone(turn, "step text"), turnDone(turn, nil)},
			wantText: "step text",
		},
		{
			name:       "TurnFailed yields a typed failed error wrapping the cause",
			script:     []event.Event{turnStarted(cmd, turn), turnFailed(turn, errProvider)},
			wantErr:    true,
			wantFailed: true,
		},
		{
			name:             "TurnRejected before any turn yields a typed TurnRejectedError",
			script:           []event.Event{turnRejected(cmd, event.RejectShuttingDown)},
			wantErr:          true,
			wantTurnRejected: true,
		},
		{
			name:          "queued interrupt cancellation resolves before TurnStarted",
			script:        []event.Event{event.LoopIdle{}},
			closeAfter:    true,
			wantErr:       true,
			wantCancelled: cancelReasonPtr(event.CancelTurnInterrupted),
			wantStatus:    tool.DelegateStatusInterrupted,
		},
		{
			name:          "queued failed-turn cancellation resolves failed",
			script:        []event.Event{event.LoopIdle{}},
			closeAfter:    true,
			wantErr:       true,
			wantCancelled: cancelReasonPtr(event.CancelTurnFailed),
			wantStatus:    tool.DelegateStatusFailed,
		},
		{
			name:          "client-retracted cancellation maps fail-securely to interrupted",
			script:        []event.Event{event.LoopIdle{}},
			closeAfter:    true,
			wantErr:       true,
			wantCancelled: cancelReasonPtr(event.CancelClientRetracted),
			wantStatus:    tool.DelegateStatusInterrupted,
		},
		{
			name:       "subscription loss before terminal wraps Err()",
			script:     []event.Event{turnStarted(cmd, turn), stepDone(turn, "partial")},
			closeAfter: true,
			subErr:     errProvider,
			wantErr:    true,
			wantLost:   true,
		},
		{
			name:       "subscription loss with no Err yields a no-terminal lost error",
			script:     []event.Event{turnStarted(cmd, turn)},
			closeAfter: true,
			wantErr:    true,
			wantLost:   true,
		},
		{
			name: "noise for other command/turn is ignored, real terminal resolves",
			script: []event.Event{
				turnStarted(otherCmd, otherTurn),
				stepDone(otherTurn, "OTHER step"),
				turnStarted(cmd, turn),
				stepDone(turn, "partial"),
				turnDone(otherTurn, aiMessage("OTHER final")),
				turnDone(turn, aiMessage("final")),
			},
			wantText: "final",
		},
		{
			// Fail-secure LoopID cross-check: a phase-2 TurnDone carrying the right
			// TurnID but a WRONG LoopID must be ignored; only the matching-loop
			// terminal resolves.
			name: "phase-2 event with right TurnID but wrong LoopID is ignored",
			script: []event.Event{
				turnStartedOnLoop(cmd, turn, loop),
				turnDoneOnLoop(turn, otherLoop, aiMessage("WRONG loop final")),
				turnDoneOnLoop(turn, loop, aiMessage("final")),
			},
			wantText: "final",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sub := newFakeSubscription(len(tt.script) + 1)
			sub.err = tt.subErr
			for _, ev := range tt.script {
				if tt.wantCancelled != nil {
					ev = event.InputCancelled{Header: event.Header{Cause: identity.Cause{CommandID: cmd}}, Reason: *tt.wantCancelled}
				}
				sub.feed(ev)
			}
			if tt.closeAfter {
				close(sub.events)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var interrupts atomic.Int32
			interrupt := func() { interrupts.Add(1) }

			got, err := drainToFinalText(ctx, sub, cmd, interrupt)

			if (err != nil) != tt.wantErr {
				t.Fatalf("drainToFinalText() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if got != tt.wantText {
					t.Errorf("drainToFinalText() text = %q, want %q", got, tt.wantText)
				}
			}
			if tt.wantFailed {
				var fe *drainFailedError
				if !errors.As(err, &fe) {
					t.Fatalf("err = %v, want *drainFailedError", err)
				}
				if !errors.Is(err, errProvider) {
					t.Errorf("err = %v, want it to wrap errProvider", err)
				}
			}
			if tt.wantTurnRejected {
				var re *TurnRejectedError
				if !errors.As(err, &re) {
					t.Fatalf("err = %v, want *TurnRejectedError", err)
				}
				if re.Reason != event.RejectShuttingDown {
					t.Errorf("rejected reason = %v, want RejectShuttingDown", re.Reason)
				}
			}
			if tt.wantCancelled != nil {
				var ce *drainCancelledError
				if !errors.As(err, &ce) || ce.Reason != *tt.wantCancelled {
					t.Fatalf("err = %T %v, want drainCancelledError reason %v", err, err, *tt.wantCancelled)
				}
				if got := statusFromDrain(err); got != tt.wantStatus {
					t.Fatalf("statusFromDrain = %v, want %v", got, tt.wantStatus)
				}
			}
			if tt.wantLost {
				var le *drainLostError
				if !errors.As(err, &le) {
					t.Fatalf("err = %v, want *drainLostError", err)
				}
				if tt.subErr != nil && !errors.Is(err, tt.subErr) {
					t.Errorf("err = %v, want it to wrap subErr %v", err, tt.subErr)
				}
			}
			if got := interrupts.Load(); got != tt.wantInterrupts {
				t.Errorf("interrupt called %d times, want %d", got, tt.wantInterrupts)
			}
		})
	}
}

func TestDrainDelegateAnswerUsesExactTurnDoneMessage(t *testing.T) {
	t.Parallel()
	cmd, turn := drainUUID(0x31), drainUUID(0x32)
	sub := newFakeSubscription(3)
	sub.feed(turnStarted(cmd, turn))
	sub.feed(stepDone(turn, "progress"))
	sub.feed(turnDone(turn, nil))
	text, err := drainDelegateAnswer(context.Background(), sub, cmd, func() {})
	if err != nil {
		t.Fatalf("drainDelegateAnswer: %v", err)
	}
	if text != "" {
		t.Fatalf("answer = %q, want exact empty TurnDone message", text)
	}
}

func TestDrainDelegateAnswerReturnsAtDeadlineWithoutTerminal(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	sub := newFakeSubscription(0)
	var interrupts atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := drainDelegateAnswer(ctx, sub, drainUUID(0x41), func() { interrupts.Add(1) })
		done <- err
	}()
	select {
	case err := <-done:
		var interrupted *drainInterruptedError
		if !errors.As(err, &interrupted) {
			t.Fatalf("error = %T %v, want drainInterruptedError", err, err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain remained blocked after its deadline")
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for interrupts.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := interrupts.Load(); got != 1 {
		t.Fatalf("interrupt calls = %d, want 1", got)
	}
}

// TestDrainToFinalTextInterruptOnCtxCancel asserts the ctx-cancel fail-safe in
// isolation (it needs interleaved goroutine timing the table cannot express):
// after the opening TurnStarted, cancelling ctx calls interrupt() exactly once,
// the helper keeps draining, and the sub-loop's TurnInterrupted terminal yields a
// typed interrupted error. The helper must not return on ctx.Done() alone, and
// must not busy-loop calling interrupt repeatedly.
func TestDrainToFinalTextInterruptOnCtxCancel(t *testing.T) {
	t.Parallel()

	cmd := drainUUID(0x01)
	turn := drainUUID(0x02)

	sub := newFakeSubscription(4)
	sub.feed(turnStarted(cmd, turn))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var interrupts atomic.Int32
	interrupt := func() { interrupts.Add(1) }

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := drainToFinalText(ctx, sub, cmd, interrupt)
		done <- result{text, err}
	}()

	// Give the drain time to consume TurnStarted and block in its select, then
	// cancel ctx to trip the fail-safe.
	deadline := time.After(2 * time.Second)
	for interrupts.Load() == 0 {
		cancel()
		select {
		case <-deadline:
			t.Fatal("interrupt() was never called after ctx cancel")
		case <-time.After(time.Millisecond):
		}
	}

	// Feed the terminal the sub-loop produces once interrupted.
	sub.feed(turnInterrupted(turn))

	var res result
	select {
	case res = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainToFinalText did not return after TurnInterrupted")
	}

	if got := interrupts.Load(); got != 1 {
		t.Fatalf("interrupt called %d times, want exactly 1", got)
	}
	var ie *drainInterruptedError
	if !errors.As(res.err, &ie) {
		t.Fatalf("err = %v, want *drainInterruptedError", res.err)
	}
	if res.text != "" {
		t.Errorf("text = %q, want empty (no partial on interrupt)", res.text)
	}
}
