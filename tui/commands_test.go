package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
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
