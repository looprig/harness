package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// newCallID mints a UUID or fails the test.
func newCallID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return u
}

// turnCtx builds a fully-injected per-call ctx (emit + ToolExecutionID + gateReg) as the
// runner would. It returns the ctx, the gateReg channel a fake actor reads, and a
// slice-collecting emit recorder.
func injectedCtx(t *testing.T, callID uuid.UUID, gateReg chan<- gateRegistration, emit func(event.Event)) context.Context {
	t.Helper()
	ctx := context.Background()
	ctx = withEmit(ctx, emit)
	ctx = withCallID(ctx, callID)
	ctx = withGateReg(ctx, gateReg)
	return ctx
}

// TestRequestUserInput_DeliversAnswer drives the happy path: a fake actor reads
// the registration, closes the ack (install-before-emit), then sends a matching
// ProvideUserInput on the gate's reply channel. RequestUserInput must return the
// delivered answer.
func TestRequestUserInput_DeliversAnswer(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateReg := make(chan gateRegistration)
	var emitted []event.Event
	emitCh := make(chan event.Event, 4)
	emit := func(ev event.Event) { emitCh <- ev }
	ctx := injectedCtx(t, callID, gateReg, emit)

	// Fake actor: ack the registration, then reply with the answer.
	go func() {
		reg := <-gateReg
		if reg.callID != callID {
			t.Errorf("reg.callID = %v, want %v", reg.callID, callID)
		}
		if reg.kind != gateUserInput {
			t.Errorf("reg.kind = %v, want gateUserInput", reg.kind)
		}
		close(reg.ack)
		reg.reply <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: callID}, Answer: "blue"}
	}()

	done := make(chan struct{})
	var got string
	var gotErr error
	go func() {
		got, gotErr = RequestUserInput(ctx, "favorite color?", []string{"red", "blue"})
		close(done)
	}()

	select {
	case ev := <-emitCh:
		emitted = append(emitted, ev)
	case <-time.After(2 * time.Second):
		t.Fatal("RequestUserInput did not emit UserInputRequested")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestUserInput did not return")
	}

	if gotErr != nil {
		t.Fatalf("RequestUserInput err = %v, want nil", gotErr)
	}
	if got != "blue" {
		t.Fatalf("answer = %q, want %q", got, "blue")
	}
	// The emitted event must be UserInputRequested with the question and choices.
	uir, ok := emitted[0].(event.UserInputRequested)
	if !ok {
		t.Fatalf("emitted[0] = %T, want event.UserInputRequested", emitted[0])
	}
	if uir.ToolExecutionID != callID || uir.Question != "favorite color?" || len(uir.Choices) != 2 {
		t.Fatalf("UserInputRequested = %+v, want ToolExecutionID/Question/2 choices", uir)
	}
}

// TestRequestUserInput_EmitsAfterAck verifies install-before-emit: the event must
// not be emitted until the actor has acked the registration. The fake actor reads
// the registration but withholds the ack; RequestUserInput must NOT have emitted
// yet. After the ack closes, the emit appears.
func TestRequestUserInput_EmitsAfterAck(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateReg := make(chan gateRegistration)
	emitCh := make(chan event.Event, 4)
	emit := func(ev event.Event) { emitCh <- ev }
	ctx := injectedCtx(t, callID, gateReg, emit)

	done := make(chan struct{})
	go func() {
		_, _ = RequestUserInput(ctx, "q", nil)
		close(done)
	}()

	reg := <-gateReg
	// Before ack: no emit must have happened.
	select {
	case ev := <-emitCh:
		t.Fatalf("emitted %T before ack; install-before-emit violated", ev)
	case <-time.After(50 * time.Millisecond):
	}
	close(reg.ack)
	// After ack: emit appears.
	select {
	case ev := <-emitCh:
		if _, ok := ev.(event.UserInputRequested); !ok {
			t.Fatalf("emitted %T, want UserInputRequested", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no emit after ack")
	}
	// Reply so the goroutine returns (no leak).
	reg.reply <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: callID}, Answer: "x"}
	<-done
}

// TestRequestUserInput_CancelBeforeRegister: ctx is already cancelled and no actor
// reads gateReg. RequestUserInput must return ctx.Err() without wedging.
func TestRequestUserInput_CancelBeforeRegister(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateReg := make(chan gateRegistration) // no reader
	emit := func(event.Event) {}
	ctx := injectedCtx(t, callID, gateReg, emit)
	cctx, cancel := context.WithCancel(ctx)
	cancel() // cancel before the call

	done := make(chan struct{})
	var gotErr error
	go func() {
		_, gotErr = RequestUserInput(cctx, "q", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestUserInput wedged on cancelled ctx during register")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", gotErr)
	}
}

// TestRequestUserInput_CancelBeforeAck: the actor reads the registration but never
// acks; the ctx is cancelled. RequestUserInput must return ctx.Err() without
// wedging and without emitting.
func TestRequestUserInput_CancelBeforeAck(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateReg := make(chan gateRegistration)
	emitCh := make(chan event.Event, 4)
	emit := func(ev event.Event) { emitCh <- ev }
	ctx := injectedCtx(t, callID, gateReg, emit)
	cctx, cancel := context.WithCancel(ctx)

	done := make(chan struct{})
	var gotErr error
	go func() {
		_, gotErr = RequestUserInput(cctx, "q", nil)
		close(done)
	}()
	<-gateReg // accept the registration but withhold the ack
	cancel()  // cancel while waiting on ack
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestUserInput wedged waiting for ack after cancel")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", gotErr)
	}
	// No event must have been emitted (emit is after ack).
	select {
	case ev := <-emitCh:
		t.Fatalf("emitted %T after cancel-before-ack; emit must follow ack", ev)
	default:
	}
}

// TestRequestUserInput_CancelWhileBlockedOnReply: registration + ack succeed and
// the event is emitted, but no reply is ever sent; cancelling the ctx must
// unblock the call with ctx.Err().
func TestRequestUserInput_CancelWhileBlockedOnReply(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateReg := make(chan gateRegistration)
	emitCh := make(chan event.Event, 4)
	emit := func(ev event.Event) { emitCh <- ev }
	ctx := injectedCtx(t, callID, gateReg, emit)
	cctx, cancel := context.WithCancel(ctx)

	go func() {
		reg := <-gateReg
		close(reg.ack)
		// never reply
	}()
	done := make(chan struct{})
	var gotErr error
	go func() {
		_, gotErr = RequestUserInput(cctx, "q", nil)
		close(done)
	}()
	<-emitCh // wait until it has emitted and is blocked on reply
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestUserInput wedged blocked on reply after cancel")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", gotErr)
	}
}

// TestRequestUserInput_MissingCtxValues: each of emit/ToolExecutionID/gateReg missing from
// ctx yields a typed GateContextError (fail-secure), without touching gateReg.
func TestRequestUserInput_MissingCtxValues(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateReg := make(chan gateRegistration)
	emit := func(event.Event) {}

	tests := []struct {
		name string
		ctx  func() context.Context
		want GateContextMissing
	}{
		{
			name: "missing emit",
			ctx: func() context.Context {
				c := withCallID(context.Background(), callID)
				return withGateReg(c, gateReg)
			},
			want: GateContextEmit,
		},
		{
			name: "missing callID",
			ctx: func() context.Context {
				c := withEmit(context.Background(), emit)
				return withGateReg(c, gateReg)
			},
			want: GateContextCallID,
		},
		{
			name: "missing gateReg",
			ctx: func() context.Context {
				c := withEmit(context.Background(), emit)
				return withCallID(c, callID)
			},
			want: GateContextGateReg,
		},
		{
			name: "empty ctx",
			ctx:  func() context.Context { return context.Background() },
			want: GateContextEmit,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := RequestUserInput(tt.ctx(), "q", nil)
			var gce *GateContextError
			if !errors.As(err, &gce) {
				t.Fatalf("err = %v, want *GateContextError", err)
			}
			if gce.Missing != tt.want {
				t.Fatalf("Missing = %v, want %v", gce.Missing, tt.want)
			}
		})
	}
}

// TestEmitFromContext covers present/absent.
func TestEmitFromContext(t *testing.T) {
	t.Parallel()
	t.Run("present", func(t *testing.T) {
		t.Parallel()
		var gotEv event.Event
		emit := func(ev event.Event) { gotEv = ev }
		ctx := withEmit(context.Background(), emit)
		got, ok := EmitFromContext(ctx)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		got(event.SessionStarted{})
		if _, isSession := gotEv.(event.SessionStarted); !isSession {
			t.Fatalf("emit forwarded %T, want SessionStarted", gotEv)
		}
	})
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		if _, ok := EmitFromContext(context.Background()); ok {
			t.Fatal("ok = true on empty ctx, want false")
		}
	})
}

// TestAccepts is the full kind×cmd matrix.
func TestAccepts(t *testing.T) {
	t.Parallel()
	approve := command.ApproveToolCall{}
	deny := command.DenyToolCall{}
	provide := command.ProvideUserInput{}

	tests := []struct {
		name string
		kind gateKind
		cmd  command.Command
		want bool
	}{
		{"permission accepts approve", gatePermission, approve, true},
		{"permission accepts deny", gatePermission, deny, true},
		{"permission rejects provide", gatePermission, provide, false},
		{"userInput accepts provide", gateUserInput, provide, true},
		{"userInput rejects approve", gateUserInput, approve, false},
		{"userInput rejects deny", gateUserInput, deny, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := accepts(tt.kind, tt.cmd); got != tt.want {
				t.Errorf("accepts(%v, %T) = %v, want %v", tt.kind, tt.cmd, got, tt.want)
			}
		})
	}
}
