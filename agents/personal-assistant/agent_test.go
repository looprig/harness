package personalassistant

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/agent/session"
)

func textOf(m *content.AIMessage) string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		if blk.Type == content.TypeText && blk.Text != nil {
			b.WriteString(blk.Text.Text)
		}
	}
	return b.String()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNewWithClientHappy(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	if a == nil {
		t.Fatal("newWithClient() returned nil assistant")
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
}

func TestNewWithClientPreCancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, err := newWithClient(ctx, &fakeLLM{}, testSpec())
	if a != nil {
		t.Errorf("expected nil assistant, got %v", a)
	}
	var se *session.SessionError
	if !errors.As(err, &se) || se.Kind != session.SessionContextDone {
		t.Fatalf("err = %v, want *session.SessionError{SessionContextDone}", err)
	}
}

func TestSendHappy(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{chunks: []content.Chunk{textChunk("hello")}}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ev, err := a.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	done, ok := ev.(loop.TurnDone)
	if !ok {
		t.Fatalf("event = %T, want loop.TurnDone", ev)
	}
	if got := textOf(done.Message); got != "hello" {
		t.Errorf("message text = %q, want hello", got)
	}
}

func TestSendProviderFailure(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{streamErr: errFakeProvider}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ev, err := a.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Send() Go error = %v, want nil", err)
	}
	failed, ok := ev.(loop.TurnFailed)
	if !ok {
		t.Fatalf("event = %T, want loop.TurnFailed", ev)
	}
	if !errors.Is(failed.Err, errFakeProvider) {
		t.Errorf("TurnFailed.Err = %v, want errors.Is errFakeProvider", failed.Err)
	}
}

func TestSendBlankInput(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	for _, in := range []string{"", "   ", "\t\n"} {
		ev, err := a.Send(context.Background(), in)
		if ev != nil {
			t.Errorf("Send(%q) event = %v, want nil", in, ev)
		}
		var ee *EmptyInputError
		if !errors.As(err, &ee) {
			t.Errorf("Send(%q) err = %v, want *EmptyInputError", in, err)
		}
	}
}

func TestStreamOrderedEvents(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{chunks: []content.Chunk{textChunk("a"), textChunk("b")}}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	sr, err := a.Stream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = sr.Close() }()

	var kinds []string
	for {
		ev, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		switch ev.(type) {
		case loop.TurnStarted:
			kinds = append(kinds, "started")
		case loop.TokenDelta:
			kinds = append(kinds, "delta")
		case loop.TurnDone:
			kinds = append(kinds, "done")
		default:
			t.Fatalf("unexpected event %T", ev)
		}
	}
	want := []string{"started", "delta", "delta", "done"}
	if !equalStrings(kinds, want) {
		t.Errorf("events = %v, want %v", kinds, want)
	}
}

func TestStreamBlankInput(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	sr, err := a.Stream(context.Background(), "  ")
	if sr != nil {
		t.Errorf("Stream() reader = %v, want nil", sr)
	}
	var ee *EmptyInputError
	if !errors.As(err, &ee) {
		t.Errorf("Stream() err = %v, want *EmptyInputError", err)
	}
}

// TestStreamCloseEventuallyReusable proves the contract: sr.Close() interrupts
// asynchronously, so a subsequent Send may briefly see *loop.TurnBusyError and
// must be retried; the session is eventually reusable.
func TestStreamCloseEventuallyReusable(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	a, err := newWithClient(context.Background(), &fakeLLM{chunks: []content.Chunk{textChunk("partial")}, hold: hold}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	sr, err := a.Stream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	// read at least one event so the turn is genuinely running before we close
	if _, err := sr.Next(); err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if err := sr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// allow any future turn to complete via EOF
	close(hold)

	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, err := a.Send(context.Background(), "again")
		if err == nil {
			if _, ok := ev.(loop.TurnDone); !ok {
				t.Fatalf("Send event = %T, want loop.TurnDone", ev)
			}
			return
		}
		var busy *loop.TurnBusyError
		if !errors.As(err, &busy) {
			t.Fatalf("Send err = %v, want nil or *loop.TurnBusyError", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("session not reusable within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCloseThenSend(t *testing.T) {
	t.Parallel()
	a, err := newWithClient(context.Background(), &fakeLLM{chunks: []content.Chunk{textChunk("x")}}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// Close is safe to call twice
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	ev, err := a.Send(context.Background(), "hi")
	if ev != nil {
		t.Errorf("Send event = %v, want nil", ev)
	}
	var se *session.SessionError
	if !errors.As(err, &se) || se.Kind != session.SessionLoopExited {
		t.Fatalf("Send err = %v, want *session.SessionError{SessionLoopExited}", err)
	}
}

// TestCtxIndependenceFromSession proves the session root is not the caller ctx:
// cancelling the construction ctx must not kill the session.
func TestCtxIndependenceFromSession(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := newWithClient(ctx, &fakeLLM{chunks: []content.Chunk{textChunk("ok")}}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	cancel() // cancel the construction ctx; the session must survive

	ev, err := a.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Send() error = %v, want nil (session should outlive ctx)", err)
	}
	if _, ok := ev.(loop.TurnDone); !ok {
		t.Fatalf("event = %T, want loop.TurnDone", ev)
	}
}

// TestSendCtxCancelInterrupts proves Send's documented interrupt contract:
// cancelling the ctx passed to Send while the turn is in flight returns
// loop.TurnInterrupted with a nil Go error. This is the Invoke cancel-while-
// running path, distinct from Stream's sr.Close().
func TestSendCtxCancelInterrupts(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	hold := make(chan struct{})
	defer close(hold) // belt-and-suspenders: release the fake if the turn outlives the test
	a, err := newWithClient(context.Background(), &fakeLLM{
		chunks:  []content.Chunk{textChunk("partial")},
		hold:    hold,
		entered: entered,
	}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		ev  loop.Event
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		ev, err := a.Send(ctx, "hi")
		resCh <- result{ev: ev, err: err}
	}()

	// wait until the turn is genuinely running (fake's Stream entered) before
	// cancelling — this exercises cancel-while-running, not cancel-before-start
	// (which would instead return a *session.SessionError).
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start within deadline")
	}
	cancel()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("Send() Go error = %v, want nil", res.err)
		}
		if _, ok := res.ev.(loop.TurnInterrupted); !ok {
			t.Fatalf("event = %T, want loop.TurnInterrupted", res.ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return after ctx cancel")
	}
}
