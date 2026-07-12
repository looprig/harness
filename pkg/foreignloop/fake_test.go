package foreignloop

import (
	"context"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// workingFac builds an event Factory whose EventID mint always succeeds. The
// foreign loop uses a SEPARATE generator for EventIDs (crypto/rand) from the
// correlation idGen (the deterministic seqIDGen) so the correlation sequence
// (sid=1, turnID=2, stepID=3, toolExecID=4, ...) stays clean while Enduring events
// still receive valid non-zero EventIDs.
func workingFac() *event.Factory {
	return event.NewFactory(uuid.New, time.Now)
}

// mustID mints a non-zero UUID for test identity, failing the test on error.
func mustID(t interface{ Fatal(args ...any) }) uuid.UUID {
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// fakePublisher implements EventPublisher, recording every published event under a
// mutex. Publish is called from both the actor and the turn goroutine across a turn
// (never concurrently — the mutex only keeps -race happy).
type fakePublisher struct {
	mu         sync.Mutex
	events     []event.Event
	checkedErr error
}

func (p *fakePublisher) PublishEvent(_ context.Context, ev event.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

func (p *fakePublisher) PublishEventChecked(ctx context.Context, ev event.Event) error {
	p.mu.Lock()
	err := p.checkedErr
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return p.PublishEvent(ctx, ev)
}

// snapshot returns a defensive copy of the events recorded so far.
func (p *fakePublisher) snapshot() []event.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]event.Event, len(p.events))
	copy(out, p.events)
	return out
}

// count returns how many events have been recorded so far. The D2 happy-path test
// captures this at Spawn time to prove TurnStarted was published BEFORE Spawn.
func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

// fakeStream is a scripted ForeignStream. It feeds its scripted events on Events(),
// reports a fixed transcript path, and (in block mode) holds open until the turn ctx
// is cancelled or Close is called — the D4 interrupt path.
type fakeStream struct {
	events     []ForeignEvent
	transcript string
	block      bool            // after the scripted events, hold open until ctx/Close
	ctx        context.Context // the Spawn ctx (turn ctx); feed honors its cancellation

	ch      chan ForeignEvent
	stop    chan struct{}
	once    sync.Once // start feed exactly once
	closeCh sync.Once // Close is idempotent
}

func (s *fakeStream) Events() <-chan ForeignEvent {
	s.once.Do(func() {
		s.ch = make(chan ForeignEvent)
		go s.feed()
	})
	return s.ch
}

func (s *fakeStream) feed() {
	defer close(s.ch)
	for _, fe := range s.events {
		select {
		case s.ch <- fe:
		case <-s.stop:
			return
		case <-s.ctx.Done():
			return
		}
	}
	if s.block {
		select {
		case <-s.stop:
		case <-s.ctx.Done():
		}
	}
}

func (s *fakeStream) TranscriptPath() string { return s.transcript }

func (s *fakeStream) Close() error {
	s.closeCh.Do(func() { close(s.stop) })
	return nil
}

// fakeAgent implements ForeignAgent. It returns a scripted fakeStream (or a
// configured spawn error) and records call ordering via onSpawn (the D2 test
// captures the publisher event count at Spawn time to prove TurnStarted precedes
// Spawn).
type fakeAgent struct {
	mu sync.Mutex

	spawnErr   error
	events     []ForeignEvent
	transcript string
	block      bool
	onSpawn    func()

	spawnCalls int
	lastTurn   ForeignTurn
}

func (a *fakeAgent) Spawn(ctx context.Context, t ForeignTurn) (ForeignStream, error) {
	a.mu.Lock()
	a.spawnCalls++
	a.lastTurn = t
	cb := a.onSpawn
	a.mu.Unlock()
	if cb != nil {
		cb()
	}
	if a.spawnErr != nil {
		return nil, a.spawnErr
	}
	return &fakeStream{
		events:     a.events,
		transcript: a.transcript,
		block:      a.block,
		ctx:        ctx,
		stop:       make(chan struct{}),
	}, nil
}

func (a *fakeAgent) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.spawnCalls
}

// lastForeignTurn returns the ForeignTurn passed to the most recent Spawn, read under
// the mutex so a test can inspect resume/start-new wiring without racing the actor.
func (a *fakeAgent) lastForeignTurn() ForeignTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastTurn
}
