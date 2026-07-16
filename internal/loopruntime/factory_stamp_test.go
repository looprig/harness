package loopruntime

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

type countingNoRunLLM struct{ calls atomic.Int32 }

func (c *countingNoRunLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("countingNoRunLLM.Invoke not used")
}

func (c *countingNoRunLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.calls.Add(1)
	return stream.NewStreamReader(func() (content.Chunk, error) { return nil, io.EOF }, nil), nil
}

type observedTurnStartCapability struct {
	reservation *hub.TurnStartReservation
	released    chan struct{}
	releaseOnce sync.Once
	publishes   atomic.Int32
}

func (c *observedTurnStartCapability) PublishTurnStarted(ctx context.Context, started event.TurnStarted) (bool, error) {
	c.publishes.Add(1)
	return c.reservation.PublishTurnStartedChecked(ctx, started)
}

func (c *observedTurnStartCapability) Release() {
	c.releaseOnce.Do(func() {
		close(c.released)
		c.reservation.Release()
	})
}

type capabilityHubPublisher struct {
	hub             *hub.Hub
	record          recordingPublisher
	capabilityReady chan *observedTurnStartCapability
}

func (p *capabilityHubPublisher) PublishEvent(ctx context.Context, ev event.Event) error {
	if err := p.hub.PublishEvent(ctx, ev); err != nil {
		return err
	}
	return p.record.PublishEvent(ctx, ev)
}

func (p *capabilityHubPublisher) PublishEventChecked(ctx context.Context, ev event.Event) error {
	if err := p.hub.PublishEventChecked(ctx, ev); err != nil {
		return err
	}
	return p.record.PublishEventChecked(ctx, ev)
}

func (p *capabilityHubPublisher) EnterExecution(context.Context, uuid.UUID) (func(), error) {
	return func() {}, nil
}

func (p *capabilityHubPublisher) EnterTurnStart(_ context.Context, loopID uuid.UUID) (TurnStartCapability, error) {
	reservation, err := p.hub.ReserveTurnStart(loopID)
	if err != nil {
		return nil, err
	}
	capability := &observedTurnStartCapability{reservation: reservation, released: make(chan struct{})}
	p.capabilityReady <- capability
	return capability, nil
}

// fixedClock returns a constant instant so a test can assert CreatedAt
// deterministically.
func fixedClock(ts time.Time) event.Clock { return func() time.Time { return ts } }

// seqIDGen mints a deterministic, distinct UUID per call (1, 2, 3, ...) so a test
// can assert minted EventIDs are non-zero without coupling to crypto/rand. It is
// safe for concurrent use (the actor and the turn goroutine both mint ids).
type seqIDGen struct {
	mu sync.Mutex
	n  byte
}

func (g *seqIDGen) gen() (uuid.UUID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return uuid.UUID{g.n}, nil
}

// workingFactory builds an event Factory whose EventID mint ALWAYS succeeds
// (uuid.New + time.Now). Tests that inject a FAILING idGen to exercise the
// loop's correlation-id failure branch use this so the Enduring OUTCOME event the
// loop publishes (TurnRejected/InputCancelled) is still stamped and delivered —
// otherwise the loop's own factory (sharing the failing idGen) would drop that
// outcome too (its EventID mint fails), and the test could never observe the
// reply. The EventID-mint failure branch itself is covered separately by
// TestEnduringMintErrorSkipsPublish.
func workingFactory() *event.Factory {
	return event.NewFactory(uuid.New, time.Now)
}

// newLoopWithFactory starts a loop wired to a recordingPublisher AND a
// deterministic event Factory (fixed clock + sequential id-gen). The same id-gen
// feeds the loop's correlation ids (TurnID/StepID/ToolExecutionID) and the
// Factory's EventID mint, mirroring production where one generator backs both.
func newLoopWithFactory(t *testing.T, client inference.Client, ts time.Time) (*Loop, *recordingPublisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gen := &seqIDGen{}
	rec := &recordingPublisher{}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, rec, runtimeConfig{
		Client:       client,
		Model:        testModel(),
		DrainTimeout: 200 * time.Millisecond,
		idGen:        gen.gen,
		now:          fixedClock(ts),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, rec
}

func TestDelegateAcceptanceMintFailureReturnsExactErrorAndStartsNoWork(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("acceptance event id mint failed")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := &recordingPublisher{}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, rec, runtimeConfig{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("must not run")}},
		Model:        testModel(),
		DrainTimeout: 200 * time.Millisecond,
		idGen:        uuid.New,
		eventFactory: event.NewFactory(func() (uuid.UUID, error) { return uuid.UUID{}, sentinel }, time.Now),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t)
	accepted := make(chan error, 1)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, NoFold: true, TargetLoopID: mustID(t), Accepted: accepted}
	if got := <-accepted; got != sentinel {
		t.Fatalf("acceptance error = %T %v, want exact sentinel", got, got)
	}
	for _, ev := range rec.events() {
		if ev.EventHeader().Cause.CommandID == id {
			t.Fatalf("failed acceptance published or started work: %T", ev)
		}
	}
}

func TestDelegateAcceptanceAppendFailureReturnsExactErrorAndStartsNoWork(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("acceptance durable append failed")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := &recordingPublisher{checkedErr: sentinel}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, rec, runtimeConfig{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("must not run")}},
		Model:        testModel(),
		DrainTimeout: 200 * time.Millisecond,
		eventFactory: workingFactory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t)
	accepted := make(chan error, 1)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, NoFold: true, TargetLoopID: mustID(t), Accepted: accepted}
	if got := <-accepted; got != sentinel {
		t.Fatalf("acceptance error = %T %v, want exact sentinel", got, got)
	}
	for _, ev := range rec.events() {
		if ev.EventHeader().Cause.CommandID == id {
			t.Fatalf("failed acceptance published or started work: %T", ev)
		}
	}
}

// TestEnduringLoopEventsStamped drives a full turn through the loop and asserts
// every Enduring loop event published on the session fan-in carries a non-zero
// EventID and the factory's CreatedAt, while the Ephemeral TokenDelta is published
// WITHOUT requiring a mint (its EventID stays zero — Ephemeral events are never
// persisted, so they are not stamped, avoiding per-token crypto/rand). The single
// publish chokepoint stamps by Class(), so this covers TurnStarted/StepDone/
// TurnDone/LoopIdle alike.
func TestEnduringLoopEventsStamped(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	l, rec := newLoopWithFactory(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}}, ts)

	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: nil}
	if _, ok := awaitReply(t, rec, id).(event.TurnStarted); !ok {
		t.Fatal("submit did not start a turn")
	}
	// Drive to the terminal and the post-terminal LoopIdle so the full Enduring set
	// (TurnStarted, StepDone, TurnDone, LoopIdle) has been published.
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if _, ok := e.(event.LoopIdle); ok {
				return true
			}
		}
		return false
	})

	var sawEnduring, sawEphemeral bool
	for _, e := range rec.events() {
		h := e.EventHeader()
		switch e.Class() {
		case event.Enduring:
			sawEnduring = true
			if h.EventID.IsZero() {
				t.Errorf("Enduring %T published with zero EventID, want a minted id", e)
			}
			if !h.CreatedAt.Equal(ts) {
				t.Errorf("Enduring %T CreatedAt = %v, want %v (factory clock)", e, h.CreatedAt, ts)
			}
		case event.Ephemeral:
			sawEphemeral = true
			if !h.EventID.IsZero() {
				t.Errorf("Ephemeral %T published WITH EventID %v, want zero (never persisted, not stamped)", e, h.EventID)
			}
		}
	}
	if !sawEnduring {
		t.Fatal("no Enduring event captured")
	}
	if !sawEphemeral {
		t.Fatal("no Ephemeral event captured (expected at least one TokenDelta)")
	}
}

func TestTurnStartedMintFailureCancelsCapabilityBeforeInference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "opening stamp failure rejects input and releases reserved activity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			sessionID := mustID(t)
			loopID := mustID(t)
			h := hub.New(sessionID)
			publisher := &capabilityHubPublisher{hub: h, capabilityReady: make(chan *observedTurnStartCapability, 1)}
			client := &countingNoRunLLM{}
			mintErr := errors.New("TurnStarted event id mint failed")
			var mintCalls atomic.Int32
			factory := event.NewFactory(func() (uuid.UUID, error) {
				if mintCalls.Add(1) == 1 {
					return uuid.UUID{}, mintErr
				}
				return uuid.New()
			}, time.Now)
			l, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, publisher, runtimeConfig{
				Client:       client,
				Model:        testModel(),
				DrainTimeout: 200 * time.Millisecond,
				idGen:        uuid.New,
				eventFactory: factory,
			})
			if err != nil {
				t.Fatal(err)
			}
			inputID := mustID(t)
			l.Commands <- command.UserInput{Header: command.Header{CommandID: inputID, CreatedAt: time.Now()}}
			var capability *observedTurnStartCapability
			select {
			case capability = <-publisher.capabilityReady:
			case <-time.After(time.Second):
				t.Fatal("turn-start capability was not acquired")
			}

			deadline := time.Now().Add(200 * time.Millisecond)
			var rejected event.TurnRejected
			for rejected.Cause.CommandID != inputID {
				for _, ev := range publisher.record.events() {
					candidate, ok := ev.(event.TurnRejected)
					if ok && candidate.Cause.CommandID == inputID {
						rejected = candidate
						break
					}
				}
				if rejected.Cause.CommandID == inputID {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("TurnStarted stamp failure did not resolve input as TurnRejected")
				}
				time.Sleep(time.Millisecond)
			}
			if rejected.Reason != event.RejectInternal {
				t.Fatalf("TurnRejected reason = %v, want RejectInternal", rejected.Reason)
			}
			if client.calls.Load() != 0 {
				t.Fatalf("inference calls = %d, want 0 after TurnStarted stamp failure", client.calls.Load())
			}
			if capability.publishes.Load() != 0 {
				t.Fatalf("capability publish calls = %d, want 0 when stamping failed", capability.publishes.Load())
			}
			select {
			case <-capability.released:
			case <-time.After(100 * time.Millisecond):
				t.Fatal("failed turn-start capability was not released promptly")
			}

			later := make(chan error, 1)
			laterLoopID := mustID(t)
			go func() {
				later <- h.PublishEventChecked(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{
					SessionID: sessionID,
					LoopID:    laterLoopID,
				}}})
			}()
			select {
			case err := <-later:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatal("later Hub activity blocked behind failed turn-start capability")
			}
		})
	}
}

// TestEnduringMintErrorSkipsPublish asserts the loop's fail-secure mint-error
// handling: when the Factory cannot mint an EventID for an Enduring event, that
// event is NOT published (no zero-EventID Enduring event ever reaches the fan-in),
// and the loop does not wedge. The Ephemeral TokenDeltas still publish unstamped.
//
// The id-gen succeeds for the correlation ids the actor/turn need (TurnID, StepID,
// ToolExecutionID) but fails the EventID mint, so the turn runs yet its Enduring
// events are skipped. We force this by injecting a Factory whose IDGen always
// errors while the loop's own idGen succeeds — the two seams are independent.
func TestEnduringMintErrorSkipsPublish(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	mintErr := errors.New("event id mint failed")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gen := &seqIDGen{} // correlation ids succeed
	rec := &recordingPublisher{}
	// Factory IDGen always fails: every Enduring stamp errors, so every Enduring
	// event is skipped, but the turn (using gen for TurnID/StepID) still runs.
	failingFactory := event.NewFactory(func() (uuid.UUID, error) { return uuid.UUID{}, mintErr }, fixedClock(ts))
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, rec, runtimeConfig{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("hi")}},
		Model:        testModel(),
		DrainTimeout: 200 * time.Millisecond,
		idGen:        gen.gen,
		eventFactory: failingFactory,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: nil}

	// The loop must not wedge: it keeps running and remains usable. Give it time to
	// process the submit (whose Enduring outcomes are all skipped), then prove no
	// zero-EventID Enduring event was published and the actor still accepts work.
	time.Sleep(100 * time.Millisecond)
	for _, e := range rec.events() {
		if e.Class() == event.Enduring {
			t.Fatalf("Enduring %T was published despite a mint failure; it must be skipped, never published with a zero EventID", e)
		}
	}

	// The actor is not wedged: a Shutdown still completes.
	ack := make(chan error, 1)
	if !sendCmd(t, l, command.Shutdown{Ack: ack}) {
		t.Fatal("actor wedged after mint-error skips (Shutdown send did not land)")
	}
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor did not exit after Shutdown; mint-error handling wedged the loop")
	}
}
