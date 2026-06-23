package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// TestSubNext covers the continuous reader: each staged event is delivered as an
// eventMsg in FIFO order, and a closed channel yields a subClosedMsg carrying the
// subscription's typed termination cause.
func TestSubNext(t *testing.T) {
	t.Parallel()

	sub := newFakeSubscription()
	sub.push(event.TurnStarted{})
	sub.push(event.TurnDone{Message: &content.AIMessage{}})

	// First receive yields the TurnStarted event.
	msg := subNext(sub)()
	ev, ok := msg.(eventMsg)
	if !ok {
		t.Fatalf("first msg = %T, want eventMsg", msg)
	}
	if _, ok := ev.ev.(event.TurnStarted); !ok {
		t.Errorf("first event = %T, want event.TurnStarted", ev.ev)
	}

	// Second receive yields the TurnDone event.
	msg = subNext(sub)()
	ev, ok = msg.(eventMsg)
	if !ok {
		t.Fatalf("second msg = %T, want eventMsg", msg)
	}
	if _, ok := ev.ev.(event.TurnDone); !ok {
		t.Errorf("second event = %T, want event.TurnDone", ev.ev)
	}
}

// TestSubNextClosed covers the reader's terminal: a closed channel yields a
// subClosedMsg carrying Err() — nil for an intentional Close, the typed loss error
// for a hub-forced drop.
func TestSubNextClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		closeErr error
		wantErr  bool
	}{
		{name: "intentional close yields nil err", closeErr: nil},
		{name: "loss yields the typed err", closeErr: errors.New("lost"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sub := newFakeSubscription()
			sub.closeErr = tt.closeErr
			_ = sub.Close()

			msg := subNext(sub)()
			cm, ok := msg.(subClosedMsg)
			if !ok {
				t.Fatalf("msg = %T, want subClosedMsg", msg)
			}
			if (cm.err != nil) != tt.wantErr {
				t.Errorf("err != nil = %v, want %v", cm.err != nil, tt.wantErr)
			}
		})
	}
}

// TestSubNextNilIsNoop guards the /clear re-subscribe window: a re-arm built from a
// transiently-nil m.sub must be a no-op (nil msg), never a nil-deref panic.
func TestSubNextNilIsNoop(t *testing.T) {
	t.Parallel()
	if msg := subNext(nil)(); msg != nil {
		t.Fatalf("subNext(nil)() = %v, want nil (no-op, no panic)", msg)
	}
}

// TestSubscribeCmd covers the startup subscribe: it forwards the single-loop
// DefaultEventFilter (built from PrimaryLoopID) and reports the outcome. On success
// the stream is carried on the msg; on error the err is carried and the stream is nil.
func TestSubscribeCmd(t *testing.T) {
	t.Parallel()

	t.Run("success carries the stream and the default filter", func(t *testing.T) {
		t.Parallel()
		primary := callID(0x5A)
		sub := newFakeSubscription()
		agent := &fakeAgent{primaryLoopID: primary, subStream: sub}

		msg := subscribeCmd(agent)()
		res, ok := msg.(subscribedMsg)
		if !ok {
			t.Fatalf("msg = %T, want subscribedMsg", msg)
		}
		if res.err != nil {
			t.Errorf("err = %v, want nil", res.err)
		}
		if res.sub != sub {
			t.Errorf("sub = %p, want %p", res.sub, sub)
		}
		// The forwarded filter is the single-loop default for the primary loop.
		if _, ok := agent.subFilter.Ephemeral.Loops[primary]; !ok {
			t.Errorf("subscribe filter Ephemeral did not scope to the primary loop %v", primary)
		}
		if !agent.subFilter.Enduring.All {
			t.Error("subscribe filter Enduring.All = false, want true (every loop's enduring events)")
		}
	})

	t.Run("error carries the err and a nil stream", func(t *testing.T) {
		t.Parallel()
		agent := &fakeAgent{subErr: errors.New("no hub")}

		msg := subscribeCmd(agent)()
		res, ok := msg.(subscribedMsg)
		if !ok {
			t.Fatalf("msg = %T, want subscribedMsg", msg)
		}
		if res.err == nil {
			t.Error("err = nil, want non-nil")
		}
		if res.sub != nil {
			t.Errorf("sub = %p, want nil on error", res.sub)
		}
	})
}

// TestSubmitCmd covers the fire-and-forget submit: blocks are forwarded to Submit
// and the result msg carries only the error (a nil err is a silent success).
func TestSubmitCmd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		submitErr error
		wantErr   bool
	}{
		{name: "success is silent", submitErr: nil},
		{name: "error surfaced", submitErr: errors.New("send failed"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{submitErr: tt.submitErr}
			blocks := []content.Block{&content.TextBlock{Text: "hi"}}
			msg := submitCmd(context.Background(), agent, blocks)()

			res, ok := msg.(submitResultMsg)
			if !ok {
				t.Fatalf("msg = %T, want submitResultMsg", msg)
			}
			if (res.err != nil) != tt.wantErr {
				t.Errorf("err != nil = %v, want %v", res.err != nil, tt.wantErr)
			}
			if !agent.submitCalled {
				t.Error("Submit not called")
			}
		})
	}
}

func TestInterruptTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		agent         *fakeAgent
		wantCancelled bool
		wantErr       bool
	}{
		{
			name:          "cancelled true no error",
			agent:         &fakeAgent{interruptCancelled: true},
			wantCancelled: true,
			wantErr:       false,
		},
		{
			name:    "error surfaced",
			agent:   &fakeAgent{interruptErr: errors.New("interrupt failed")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := interruptTurn(context.Background(), tt.agent)()
			res, ok := msg.(interruptResultMsg)
			if !ok {
				t.Fatalf("msg = %T, want interruptResultMsg", msg)
			}
			if res.cancelled != tt.wantCancelled {
				t.Errorf("cancelled = %v, want %v", res.cancelled, tt.wantCancelled)
			}
			if (res.err != nil) != tt.wantErr {
				t.Errorf("err != nil = %v, want %v", res.err != nil, tt.wantErr)
			}
		})
	}
}

func TestReopenAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		open      OpenAgent
		wantAgent bool
		wantErr   bool
	}{
		{
			name:      "success returns agent",
			open:      fakeOpen(&fakeAgent{}),
			wantAgent: true,
		},
		{
			name:    "error surfaced, nil agent",
			open:    func(context.Context) (Agent, error) { return nil, errors.New("open failed") },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := reopenAgent(context.Background(), tt.open)()
			res, ok := msg.(reopenResultMsg)
			if !ok {
				t.Fatalf("msg = %T, want reopenResultMsg", msg)
			}
			if (res.agent != nil) != tt.wantAgent {
				t.Errorf("agent != nil = %v, want %v", res.agent != nil, tt.wantAgent)
			}
			if (res.err != nil) != tt.wantErr {
				t.Errorf("err != nil = %v, want %v", res.err != nil, tt.wantErr)
			}
		})
	}
}

func TestCloseAgent(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	msg := closeAgent(agent)()
	if msg != nil {
		t.Errorf("closeAgent msg = %v, want nil", msg)
	}
	if !agent.closeCalled {
		t.Error("closeAgent did not call agent.Close")
	}
}

func TestPrintPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		actions []printAction
		want    string
	}{
		{
			name:    "empty slice yields empty string",
			actions: nil,
			want:    "",
		},
		{
			name:    "single action",
			actions: []printAction{{Lines: []string{"a", ""}}},
			want:    "a\n",
		},
		{
			name:    "two actions",
			actions: []printAction{{Lines: []string{"a", ""}}, {Lines: []string{"b", ""}}},
			want:    "a\n\nb\n",
		},
		{
			name:    "action with multiple content lines",
			actions: []printAction{{Lines: []string{"a", "b", "c", ""}}},
			want:    "a\nb\nc\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := printPayload(tt.actions)
			if got != tt.want {
				t.Errorf("printPayload = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPrintPayloadReadOnly asserts printPayload never mutates the caller's
// actions or their Lines slices (no append-aliasing into a caller's slice).
func TestPrintPayloadReadOnly(t *testing.T) {
	t.Parallel()

	lines := []string{"a", ""}
	actions := []printAction{{EntryID: 1, Lines: lines}}
	_ = printPayload(actions)

	if got, want := len(actions[0].Lines), 2; got != want {
		t.Errorf("Lines length mutated: got %d, want %d", got, want)
	}
	if actions[0].Lines[0] != "a" || actions[0].Lines[1] != "" {
		t.Errorf("Lines content mutated: %q", actions[0].Lines)
	}
}

func TestPrintToScrollback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		actions []printAction
		wantNil bool
	}{
		{name: "empty is no-op (nil cmd)", actions: nil, wantNil: true},
		{name: "non-empty returns a command", actions: []printAction{{Lines: []string{"a", ""}}}, wantNil: false},
		// Defensive: an action carrying no renderable content (Lines that join to "")
		// must NOT dispatch tea.Println("") — see printToScrollback's blank-payload guard.
		{name: "action with nil lines is no-op", actions: []printAction{{EntryID: 1, Lines: nil}}, wantNil: true},
		{name: "action with a single empty line is no-op", actions: []printAction{{EntryID: 1, Lines: []string{""}}}, wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := printToScrollback(tt.actions)
			if (cmd == nil) != tt.wantNil {
				t.Errorf("printToScrollback nil = %v, want %v", cmd == nil, tt.wantNil)
			}
		})
	}
}

// TestApproveCmd covers the bounded approve dispatch: every allowed scope is
// forwarded verbatim with the call ID, and a configured error surfaces on the
// result msg (non-fatal). errScope is a sentinel for the error path.
func TestApproveCmd(t *testing.T) {
	t.Parallel()

	errApprove := errors.New("approve failed")

	tests := []struct {
		name       string
		loopID     uuid.UUID
		callID     uuid.UUID
		scope      tool.ApprovalScope
		approveErr error
		wantErr    bool
	}{
		{name: "once succeeds", loopID: callID(10), callID: callID(1), scope: tool.ScopeOnce},
		{name: "session succeeds", loopID: callID(20), callID: callID(2), scope: tool.ScopeSession},
		{name: "workspace succeeds", loopID: callID(30), callID: callID(3), scope: tool.ScopeWorkspace},
		{name: "error surfaced", loopID: callID(40), callID: callID(4), scope: tool.ScopeOnce, approveErr: errApprove, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{approveErr: tt.approveErr}
			msg := approveCmd(context.Background(), agent, tt.loopID, tt.callID, tt.scope)()

			res, ok := msg.(promptResultMsg)
			if !ok {
				t.Fatalf("msg = %T, want promptResultMsg", msg)
			}
			if (res.err != nil) != tt.wantErr {
				t.Errorf("err != nil = %v, want %v", res.err != nil, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(res.err, errApprove) {
				t.Errorf("err = %v, want %v", res.err, errApprove)
			}
			if agent.lastLoopID != tt.loopID {
				t.Errorf("recorded loopID = %v, want %v", agent.lastLoopID, tt.loopID)
			}
			if agent.lastCallID != tt.callID {
				t.Errorf("recorded callID = %v, want %v", agent.lastCallID, tt.callID)
			}
			if agent.lastScope != tt.scope {
				t.Errorf("recorded scope = %v, want %v", agent.lastScope, tt.scope)
			}
		})
	}
}

// TestDenyCmd covers the bounded deny dispatch: the call ID is forwarded and a
// configured error surfaces on the result msg (non-fatal).
func TestDenyCmd(t *testing.T) {
	t.Parallel()

	errDeny := errors.New("deny failed")

	tests := []struct {
		name    string
		loopID  uuid.UUID
		callID  uuid.UUID
		denyErr error
		wantErr bool
	}{
		{name: "deny succeeds", loopID: callID(10), callID: callID(1)},
		{name: "error surfaced", loopID: callID(20), callID: callID(2), denyErr: errDeny, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{denyErr: tt.denyErr}
			msg := denyCmd(context.Background(), agent, tt.loopID, tt.callID)()

			res, ok := msg.(promptResultMsg)
			if !ok {
				t.Fatalf("msg = %T, want promptResultMsg", msg)
			}
			if (res.err != nil) != tt.wantErr {
				t.Errorf("err != nil = %v, want %v", res.err != nil, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(res.err, errDeny) {
				t.Errorf("err = %v, want %v", res.err, errDeny)
			}
			if agent.lastLoopID != tt.loopID {
				t.Errorf("recorded loopID = %v, want %v", agent.lastLoopID, tt.loopID)
			}
			if agent.lastCallID != tt.callID {
				t.Errorf("recorded callID = %v, want %v", agent.lastCallID, tt.callID)
			}
		})
	}
}

// TestProvideAnswerCmd covers the bounded answer dispatch: the call ID and answer
// are forwarded verbatim and a configured error surfaces on the result msg.
func TestProvideAnswerCmd(t *testing.T) {
	t.Parallel()

	errAnswer := errors.New("answer failed")

	tests := []struct {
		name      string
		loopID    uuid.UUID
		callID    uuid.UUID
		answer    string
		answerErr error
		wantErr   bool
	}{
		{name: "answer succeeds", loopID: callID(10), callID: callID(1), answer: "yes, proceed"},
		{name: "empty answer forwarded", loopID: callID(20), callID: callID(2), answer: ""},
		{name: "error surfaced", loopID: callID(30), callID: callID(3), answer: "x", answerErr: errAnswer, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{answerErr: tt.answerErr}
			msg := provideAnswerCmd(context.Background(), agent, tt.loopID, tt.callID, tt.answer)()

			res, ok := msg.(promptResultMsg)
			if !ok {
				t.Fatalf("msg = %T, want promptResultMsg", msg)
			}
			if (res.err != nil) != tt.wantErr {
				t.Errorf("err != nil = %v, want %v", res.err != nil, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(res.err, errAnswer) {
				t.Errorf("err = %v, want %v", res.err, errAnswer)
			}
			if agent.lastLoopID != tt.loopID {
				t.Errorf("recorded loopID = %v, want %v", agent.lastLoopID, tt.loopID)
			}
			if agent.lastCallID != tt.callID {
				t.Errorf("recorded callID = %v, want %v", agent.lastCallID, tt.callID)
			}
			if agent.lastAnswer != tt.answer {
				t.Errorf("recorded answer = %q, want %q", agent.lastAnswer, tt.answer)
			}
		})
	}
}

// TestPromptDispatchBounded asserts the dispatch cmds honor a cancelled parent
// context by returning promptly rather than blocking — the bounded-ctx guarantee
// that keeps the Update loop from wedging on a stuck send. Each cmd still returns
// a promptResultMsg (never panics).
func TestPromptDispatchBounded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled parent: the bounded child inherits cancellation

	// Each goroutine gets its own agent: this test asserts only that the cmds
	// return promptly, and a shared fake's recorder fields would race.
	done := make(chan tea.Msg, 3)
	go func() { done <- approveCmd(ctx, &fakeAgent{}, callID(10), callID(1), tool.ScopeOnce)() }()
	go func() { done <- denyCmd(ctx, &fakeAgent{}, callID(20), callID(2))() }()
	go func() { done <- provideAnswerCmd(ctx, &fakeAgent{}, callID(30), callID(3), "a")() }()

	for i := 0; i < 3; i++ {
		select {
		case msg := <-done:
			if _, ok := msg.(promptResultMsg); !ok {
				t.Errorf("msg = %T, want promptResultMsg", msg)
			}
		case <-time.After(time.Second):
			t.Fatal("dispatch cmd did not return promptly under a cancelled context")
		}
	}
}
