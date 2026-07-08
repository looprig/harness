package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/transcript"
)

// pollDeadline / pollInterval bound the busy-wait used to observe the
// supervisor's goroutine effects. The assertion is the poll condition itself
// (recorded / dropped), never a fixed Sleep — the interval only paces the loop.
const (
	pollDeadline = 2 * time.Second
	pollInterval = 2 * time.Millisecond
)

// pollUntil spins cond on a bounded deadline, returning true as soon as cond
// holds (or a final check at expiry). It never asserts by sleeping a fixed
// duration — it waits only as long as the condition takes.
func pollUntil(t *testing.T, cond func() bool) bool {
	t.Helper()
	stop := time.Now().Add(pollDeadline)
	for time.Now().Before(stop) {
		if cond() {
			return true
		}
		time.Sleep(pollInterval)
	}
	return cond()
}

// mkID builds a distinct non-zero UUID from a single seed byte so table rows
// never collide (and a zero UUID never masquerades as a real id).
func mkID(b byte) uuid.UUID {
	var u uuid.UUID
	u[0] = b
	return u
}

// loopHeader stamps an event Header with just the producing loop id — the only
// coordinate the supervisor reads (ev.EventHeader().LoopID).
func loopHeader(loopID uuid.UUID) event.Header {
	return event.Header{Coordinates: identity.Coordinates{LoopID: loopID}}
}

// errSubscribe is a leaf sentinel for the Subscribe-failure path; errStreamLost
// stands in for a hub-forced subscription loss cause.
var (
	errSubscribe  = errors.New("api_test: subscribe boom")
	errStreamLost = errors.New("api_test: stream lost")
)

// fakeSub is a hand-fed event.Subscription: the test pushes events with feed and
// tears the stream down with Close (guarded against double-close by sync.Once).
// fail simulates a hub-forced loss — it sets the Err() cause and closes the
// channel WITHOUT an intentional stop(); Close reuses the same once so a later
// stop() is a safe no-op rather than a double-close panic.
type fakeSub struct {
	ch   chan event.Event
	once sync.Once

	mu  sync.Mutex
	err error
}

func newFakeSub() *fakeSub { return &fakeSub{ch: make(chan event.Event, 16)} }

func (s *fakeSub) Events() <-chan event.Event { return s.ch }

func (s *fakeSub) Close() error {
	s.once.Do(func() { close(s.ch) })
	return nil
}

func (s *fakeSub) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *fakeSub) feed(ev event.Event) { s.ch <- ev }

func (s *fakeSub) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.once.Do(func() { close(s.ch) })
}

// fakeAgent implements the full api.Agent interface. Subscribe carries behavior
// (hands back the injected sub or a forced error) for the supervisor; Interrupt
// and Close carry behavior for the session-lifecycle endpoints; Submit and
// RespondGate carry behavior for the input + gate-routing endpoints.
type fakeAgent struct {
	sub    *fakeSub
	subErr error

	gotFilter event.EventFilter

	// interruptResult/interruptErr are what Interrupt returns — the interrupt
	// endpoint surfaces the bool and maps a non-nil error to 500.
	interruptResult bool
	interruptErr    error

	// submitID/submitErr are what Submit returns: the input endpoint echoes the id
	// back as input_id and maps a non-nil error to 500. respondGateErr forces the
	// gate-dispatch 500 path. These are set at construction (before the server
	// handles a request), so they are read without the mutex.
	submitID       uuid.UUID
	submitErr      error
	respondGateErr error

	listGates []gate.Gate

	// exportSrc/exportPrompts/exportErr are what ExportSource returns for the export
	// endpoint: the source+resolver Reconstruct+Render fold into an HTML transcript,
	// or exportErr forces the 409 (ExportUnavailableError) / 500 (any other) paths.
	// Set at construction, read without the mutex.
	exportSrc     transcript.RecordSource
	exportPrompts transcript.SystemPromptResolver
	exportErr     error

	// mu guards the recorded-call fields below. The endpoints run in the server's
	// handler goroutine while the test asserts from its own goroutine, so every
	// recorded write must be synchronized to stay clean under -race.
	mu     sync.Mutex
	closed bool

	submittedBlocks []content.Block

	respondGateCalled bool
	respondGate       gate.GateResponse
}

func (a *fakeAgent) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	a.gotFilter = filter
	if a.subErr != nil {
		return nil, a.subErr
	}
	return a.sub, nil
}

func (a *fakeAgent) Submit(_ context.Context, blocks []content.Block) (uuid.UUID, error) {
	a.mu.Lock()
	a.submittedBlocks = blocks
	a.mu.Unlock()
	return a.submitID, a.submitErr
}
func (a *fakeAgent) PrimaryLoopID() uuid.UUID { return uuid.UUID{} }
func (a *fakeAgent) RespondGate(_ context.Context, response gate.GateResponse) error {
	a.mu.Lock()
	a.respondGateCalled, a.respondGate = true, response
	a.mu.Unlock()
	return a.respondGateErr
}
func (a *fakeAgent) ListGates(context.Context) []gate.Gate { return a.listGates }
func (a *fakeAgent) Interrupt(_ context.Context) (bool, error) {
	return a.interruptResult, a.interruptErr
}
func (a *fakeAgent) Close(_ context.Context) error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	return nil
}
func (a *fakeAgent) ExportSource(_ context.Context) (transcript.RecordSource, transcript.SystemPromptResolver, error) {
	return a.exportSrc, a.exportPrompts, a.exportErr
}

// wasClosed reports whether Close has been called (mutex-guarded so the delete
// handler's off-lock Close synchronizes cleanly with the test assertion under
// -race).
func (a *fakeAgent) wasClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

// submittedBlocksSnapshot returns the blocks the last Submit was handed (mutex-
// guarded so the input handler's write synchronizes with the test read).
func (a *fakeAgent) submittedBlocksSnapshot() []content.Block {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.submittedBlocks
}

func (a *fakeAgent) respondGateArgs() (called bool, response gate.GateResponse) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.respondGateCalled, a.respondGate
}

// TestSupervisor_DrainsUntilStop proves the supervisor still owns a whole-session
// subscription lifecycle but no longer records gate state. It drains incoming
// events until stop(), joins the goroutine, and tolerates repeated stop calls.
func TestSupervisor_DrainsUntilStop(t *testing.T) {
	t.Parallel()

	fs := newFakeSub()
	fa := &fakeAgent{sub: fs}
	sup, err := newSupervisor(fa)
	if err != nil {
		t.Fatalf("newSupervisor() unexpected error = %v", err)
	}
	if !fa.gotFilter.Ephemeral.All || !fa.gotFilter.Enduring.All {
		t.Fatalf("supervisor subscribed with %+v, want whole-session (Ephemeral.All && Enduring.All)", fa.gotFilter)
	}

	for i := 0; i < cap(fs.ch); i++ {
		fs.feed(event.TokenDelta{Header: loopHeader(mkID(0xA1)), TurnIndex: event.TurnIndex(i)})
	}
	if !pollUntil(t, func() bool { return len(fs.ch) == 0 }) {
		t.Fatalf("supervisor did not drain subscription within %v", pollDeadline)
	}

	if err := sup.stop(); err != nil {
		t.Errorf("stop() error = %v", err)
	}
	if err := sup.stop(); err != nil {
		t.Errorf("second stop() error = %v", err)
	}
	select {
	case <-sup.done:
	default:
		t.Fatal("run goroutine did not exit after stop()")
	}
	if got := sup.exitError(); got != nil {
		t.Errorf("exitError() after intentional stop = %v, want nil", got)
	}
}

// TestSupervisor_SubscribeError proves newSupervisor surfaces the Subscribe
// failure and starts no goroutine (nothing to leak) when the agent refuses.
func TestSupervisor_SubscribeError(t *testing.T) {
	t.Parallel()
	fa := &fakeAgent{subErr: errSubscribe}
	sup, err := newSupervisor(fa)
	if !errors.Is(err, errSubscribe) {
		t.Fatalf("newSupervisor() error = %v, want %v", err, errSubscribe)
	}
	if sup != nil {
		t.Fatalf("newSupervisor() = %v on error, want nil supervisor", sup)
	}
}

// TestSupervisor_ExitErrorOnLoss proves a hub-forced subscription loss (channel
// closes WITHOUT an intentional stop) surfaces the loss cause via exitError.
func TestSupervisor_ExitErrorOnLoss(t *testing.T) {
	t.Parallel()

	fs := newFakeSub()
	fa := &fakeAgent{sub: fs}
	sup, err := newSupervisor(fa)
	if err != nil {
		t.Fatalf("newSupervisor() unexpected error = %v", err)
	}

	// Simulate the hub dropping this subscriber: Err() now reports the cause and
	// the channel closes, without any stop() call.
	fs.fail(errStreamLost)

	select {
	case <-sup.done:
	case <-time.After(pollDeadline):
		t.Fatalf("run goroutine did not exit after simulated loss within %v", pollDeadline)
	}
	if got := sup.exitError(); !errors.Is(got, errStreamLost) {
		t.Fatalf("exitError() after loss = %v, want %v", got, errStreamLost)
	}
	// stop() after a loss is a safe no-op (fail reused the close-once).
	if err := sup.stop(); err != nil {
		t.Errorf("stop() after loss error = %v", err)
	}
}
