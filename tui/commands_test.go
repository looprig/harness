package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

func TestReadNext(t *testing.T) {
	t.Parallel()

	started := event.TurnStarted{}
	done := event.TurnDone{Message: &content.AIMessage{}}
	r := scriptedReader(started, done)

	// First call yields the TurnStarted event.
	msg := readNext(r)()
	ev, ok := msg.(eventMsg)
	if !ok {
		t.Fatalf("first msg = %T, want eventMsg", msg)
	}
	if _, ok := ev.ev.(event.TurnStarted); !ok {
		t.Errorf("first event = %T, want event.TurnStarted", ev.ev)
	}

	// Second call yields the TurnDone event.
	msg = readNext(r)()
	ev, ok = msg.(eventMsg)
	if !ok {
		t.Fatalf("second msg = %T, want eventMsg", msg)
	}
	if _, ok := ev.ev.(event.TurnDone); !ok {
		t.Errorf("second event = %T, want event.TurnDone", ev.ev)
	}

	// Third call yields EOF.
	msg = readNext(r)()
	if _, ok := msg.(streamEOFMsg); !ok {
		t.Fatalf("third msg = %T, want streamEOFMsg", msg)
	}
}

// errStreamReader is a StreamReader whose Next returns a non-EOF error.
func errStreamReader(err error) *llm.StreamReader[event.Event] {
	return llm.NewStreamReader(func() (event.Event, error) { return nil, err }, nil)
}

func TestReadNextError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("stream broke")
	r := errStreamReader(wantErr)

	msg := readNext(r)()
	em, ok := msg.(streamErrMsg)
	if !ok {
		t.Fatalf("msg = %T, want streamErrMsg", msg)
	}
	if !errors.Is(em.err, wantErr) {
		t.Errorf("err = %v, want %v", em.err, wantErr)
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
		callID     uuid.UUID
		scope      tool.ApprovalScope
		approveErr error
		wantErr    bool
	}{
		{name: "once succeeds", callID: callID(1), scope: tool.ScopeOnce},
		{name: "session succeeds", callID: callID(2), scope: tool.ScopeSession},
		{name: "workspace succeeds", callID: callID(3), scope: tool.ScopeWorkspace},
		{name: "error surfaced", callID: callID(4), scope: tool.ScopeOnce, approveErr: errApprove, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{approveErr: tt.approveErr}
			msg := approveCmd(context.Background(), agent, tt.callID, tt.scope)()

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
		callID  uuid.UUID
		denyErr error
		wantErr bool
	}{
		{name: "deny succeeds", callID: callID(1)},
		{name: "error surfaced", callID: callID(2), denyErr: errDeny, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{denyErr: tt.denyErr}
			msg := denyCmd(context.Background(), agent, tt.callID)()

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
		callID    uuid.UUID
		answer    string
		answerErr error
		wantErr   bool
	}{
		{name: "answer succeeds", callID: callID(1), answer: "yes, proceed"},
		{name: "empty answer forwarded", callID: callID(2), answer: ""},
		{name: "error surfaced", callID: callID(3), answer: "x", answerErr: errAnswer, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{answerErr: tt.answerErr}
			msg := provideAnswerCmd(context.Background(), agent, tt.callID, tt.answer)()

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
	go func() { done <- approveCmd(ctx, &fakeAgent{}, callID(1), tool.ScopeOnce)() }()
	go func() { done <- denyCmd(ctx, &fakeAgent{}, callID(2))() }()
	go func() { done <- provideAnswerCmd(ctx, &fakeAgent{}, callID(3), "a")() }()

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
