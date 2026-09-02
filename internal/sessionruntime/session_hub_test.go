package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	sessionapi "github.com/looprig/harness/pkg/session"
)

// allFilter delivers every event from every loop in both classes.
func allFilter() event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	}
}

// TestSessionStartedDeliveredToLateSubscriber proves the session emits the
// session-scoped SessionStarted through the hub at construction. Because the hub
// has no buffering for pre-subscription events, a subscriber that attaches after
// New will not receive that initial SessionStarted — but a fresh
// SessionStarted from a subsequent publish (or, here, the session's own start
// being session-scoped) confirms wiring. We assert wiring by publishing a
// session-scoped event after subscribing and seeing it arrive.
func TestHubWiringDeliversSessionEvents(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// Publish a session-scoped event through the session's PublishEvent; the
	// subscriber must receive it (proving PublishEvent -> hub -> subscriber wiring).
	if err := s.PublishEvent(context.Background(), event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID()}}}); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	select {
	case d, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed unexpectedly")
		}
		if _, isStart := d.Event.(event.SessionStarted); !isStart {
			t.Fatalf("got %T, want event.SessionStarted", d.Event)
		}
	case <-time.After(time.Second):
		t.Fatalf("subscriber did not receive the session event")
	}
}

// TestSubscribeSeamDefaultFilterDeliversSessionEvent proves the whole-session
// subscribe seam (11.1): a subscription opened with the single-loop TUI default
// filter (primary-only Ephemeral, all-loop Enduring) is usable and delivers a
// session-scoped event (SessionIdle) — which bypasses the loop filter — and that the
// concrete *hub.EventSubscription satisfies the consumer-facing event.Subscription
// contract the TUI depends on.
func TestSubscribeSeamDefaultFilterDeliversSessionEvent(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// The single-loop default: live tokens from the primary loop only, finalized
	// events from every loop. Session-scoped events ignore both scopes.
	filter := event.EventFilter{
		Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{s.ActiveLoopID(): {}}},
		Enduring:  event.LoopScope{All: true},
	}

	// The seam returns a concrete *hub.EventSubscription; it must satisfy the
	// consumer-facing event.Subscription the TUI's EventStream aliases.
	var sub event.Subscription
	sub, err = s.SubscribeEvents(filter)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// A session-scoped event must arrive through the default filter.
	if err := s.PublishEvent(context.Background(), event.SessionIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID()}}}); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	select {
	case d, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed unexpectedly")
		}
		if _, isIdle := d.Event.(event.SessionIdle); !isIdle {
			t.Fatalf("got %T, want event.SessionIdle", d.Event)
		}
	case <-time.After(time.Second):
		t.Fatalf("subscriber did not receive the session-scoped event through the default filter")
	}
	if err := sub.Err(); err != nil {
		t.Errorf("sub.Err() = %v, want nil (live subscription)", err)
	}
}

// TestWaitIdleFreshSession proves a freshly built session is idle (no turn yet),
// so WaitIdle returns nil immediately.
func TestWaitIdleFreshSession(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle on a fresh session = %v, want nil", err)
	}
}

// TestShutdownStopsSessionAndWaitIdle proves Shutdown drives the session to its
// stopped phase: WaitIdle then returns hub.ErrSessionStopped, a subscriber sees
// SessionStopped, and a late loop event published after stop is delivered but does
// not flip the phase back to idle/active (WaitIdle keeps returning ErrSessionStopped).
func TestShutdownStopsSessionAndWaitIdle(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// WaitIdle returns ErrSessionStopped (stopped, not merely idle).
	if err := s.WaitIdle(context.Background()); !errors.Is(err, hub.ErrSessionStopped) {
		t.Fatalf("WaitIdle after Shutdown = %v, want hub.ErrSessionStopped", err)
	}

	// The subscriber received SessionStopped at some point.
	sawStopped := false
	drain := time.After(time.Second)
	for !sawStopped {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed before SessionStopped seen")
			}
			if _, ok := d.Event.(event.SessionStopped); ok {
				sawStopped = true
			}
		case <-drain:
			t.Fatalf("never saw SessionStopped after Shutdown")
		}
	}

	// A late loop event published after stop is delivered but does not flip phase.
	if err := s.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID()}}}); err != nil {
		t.Fatalf("post-stop PublishEvent = %v", err)
	}
	if err := s.WaitIdle(context.Background()); !errors.Is(err, hub.ErrSessionStopped) {
		t.Fatalf("WaitIdle after post-stop publish = %v, want still hub.ErrSessionStopped", err)
	}
}

// TestExpectCancelExpectTurnSessionWiring proves the session-internal wake-token
// methods delegate to the hub and derive the right session-scoped edges. They are
// inert in production (no async subagents yet) but the wiring is exercised here.
func TestExpectCancelExpectTurnSessionWiring(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	subagent := mustUUID()
	s.expectTurn(context.Background(), subagent)

	// WaitIdle must now block (a wake token makes the session Active).
	blockCtx, blockCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer blockCancel()
	if err := s.WaitIdle(blockCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle while a wake token is held = %v, want DeadlineExceeded (blocked)", err)
	}

	// SessionActive should have been delivered for the expectTurn edge.
	if !drainFor[event.SessionActive](t, sub) {
		t.Fatalf("expectTurn did not derive SessionActive")
	}

	s.cancelExpectTurn(context.Background(), subagent)
	if !drainFor[event.SessionIdle](t, sub) {
		t.Fatalf("cancelExpectTurn did not derive SessionIdle")
	}

	// Now idle again.
	if err := s.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after cancelExpectTurn = %v, want nil", err)
	}
}

// drainFor reads from the subscription until an event of type T arrives or a
// timeout elapses. It returns true if T was seen.
func drainFor[T event.Event](t *testing.T, sub event.Subscription) bool {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				return false
			}
			if _, match := d.Event.(T); match {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// firstMatching reads from the subscription until an event of type T arrives or a
// timeout elapses, returning the matched event and true (zero value, false on
// miss). Unlike drainFor it hands back the concrete event so a test can inspect
// its Header/Cause.
func firstMatching[T event.Event](t *testing.T, sub event.Subscription) (T, bool) {
	t.Helper()
	var zero T
	deadline := time.After(time.Second)
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				return zero, false
			}
			if got, match := d.Event.(T); match {
				return got, true
			}
		case <-deadline:
			return zero, false
		}
	}
}

// TestLoopStartedPublishedOnNewLoop proves Session.NewLoop publishes exactly one
// Enduring LoopStarted to a subscriber active at creation time, carrying the NEW
// loop in Header.Coordinates and the spawning provenance in Header.Cause
// (Agency=AgencyMachine). It also proves there is no replay: a subscriber that
// attaches AFTER NewLoop never sees that LoopStarted.
func TestLoopStartedPublishedOnNewLoop(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Subscribe to all Enduring events BEFORE creating the second loop, so this
	// subscriber is active at creation time and must receive the LoopStarted.
	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	parent := loop.Provenance{LoopID: mustUUID(), TurnID: mustUUID(), StepID: mustUUID()}
	loopID, err := s.NewLoop(parent, cfg(&stubLLM{chunks: []content.Chunk{textChunk("y")}}))
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	ev, ok := firstMatching[event.LoopStarted](t, sub)
	if !ok {
		t.Fatal("subscriber active at NewLoop did not receive a LoopStarted")
	}
	// Header.Coordinates is the NEW loop: SessionID+LoopID set, Turn/Step zero.
	if ev.SessionID != s.SessionID() {
		t.Errorf("LoopStarted SessionID = %v, want %v", ev.SessionID, s.SessionID())
	}
	if ev.LoopID != loopID {
		t.Errorf("LoopStarted LoopID = %v, want returned loop id %v", ev.LoopID, loopID)
	}
	if !ev.TurnID.IsZero() || !ev.StepID.IsZero() {
		t.Errorf("LoopStarted Coordinates Turn/Step = %v/%v, want both zero", ev.TurnID, ev.StepID)
	}
	if ev.EventID.IsZero() {
		t.Error("LoopStarted EventID is zero, want a freshly minted id")
	}
	// Header.Cause is the spawning loop/turn/step, machine-originated.
	if ev.Cause.LoopID != parent.LoopID {
		t.Errorf("LoopStarted Cause.LoopID = %v, want parent %v", ev.Cause.LoopID, parent.LoopID)
	}
	if ev.Cause.TurnID != parent.TurnID {
		t.Errorf("LoopStarted Cause.TurnID = %v, want parent %v", ev.Cause.TurnID, parent.TurnID)
	}
	if ev.Cause.StepID != parent.StepID {
		t.Errorf("LoopStarted Cause.StepID = %v, want parent %v", ev.Cause.StepID, parent.StepID)
	}
	if ev.Cause.Agency != identity.AgencyMachine {
		t.Errorf("LoopStarted Cause.Agency = %v, want AgencyMachine", ev.Cause.Agency)
	}
	// It is Enduring (durable loop-tree record).
	if ev.Class() != event.Enduring {
		t.Errorf("LoopStarted Class = %v, want Enduring", ev.Class())
	}

	// No replay: a subscriber attaching AFTER NewLoop must not see that LoopStarted.
	late, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("late SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = late.Close() })
	if _, ok := firstMatching[event.LoopStarted](t, late); ok {
		t.Fatal("late subscriber received a replayed LoopStarted, want none (no replay)")
	}
}

// committedSessionAppender is an injected event appender that reports the exact
// canonical body it "stored" for each public enduring event, so a Session built over
// it can advertise the committed-public-event capability end to end.
type committedSessionAppender struct {
	mu     sync.Mutex
	stored map[uint64][]byte
	seq    uint64
}

func newCommittedSessionAppender() *committedSessionAppender {
	return &committedSessionAppender{stored: make(map[uint64][]byte)}
}

func (a *committedSessionAppender) SupportsCommittedPublicBodies() bool { return true }

func (a *committedSessionAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	commit, err := a.AppendEventCommitted(ctx, ev)
	return commit.Sequence, err
}

func (a *committedSessionAppender) AppendEventResult(ctx context.Context, ev event.Event) (uint64, bool, error) {
	commit, err := a.AppendEventCommitted(ctx, ev)
	return commit.Sequence, commit.Appended, err
}

func (a *committedSessionAppender) AppendEventCommitted(_ context.Context, ev event.Event) (event.AppendCommit, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	commit := event.AppendCommit{Sequence: a.seq, Appended: true}
	if ev.Class() != event.Enduring || ev.Visibility() != event.Public {
		return commit, nil
	}
	body := []byte(`{"stored":` + strconv.FormatUint(a.seq, 10) + `}`)
	a.stored[a.seq] = body
	// Minted from the event's OWN header, as sessionwire.Project does; the hub's
	// pairing guard rejects a commit whose id belongs to another event.
	commit.EventID = ev.EventHeader().EventID.String()
	commit.PublicBody = body
	commit.CoveredThrough = a.seq
	return commit, nil
}

func (a *committedSessionAppender) storedBody(seq uint64) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stored[seq]
}

// TestSessionCommittedPublicEventsRequiresCommittedAppender asserts the capability
// THROUGH the session call site, not one level away at the hub. A session over a
// legacy appender answers "no capability" and still serves SubscribeEvents; a session
// over a committed appender hands back a source whose deliveries carry the stored
// bytes, the committed id, and a watermark equal to their own sequence.
func TestSessionCommittedPublicEventsRequiresCommittedAppender(t *testing.T) {
	t.Parallel()

	legacy, err := newTestSession(context.Background(), cfg(&stubLLM{}), WithEventAppender(&recordingEventAppender{}))
	if err != nil {
		t.Fatalf("New(legacy): %v", err)
	}
	t.Cleanup(func() { _ = legacy.Shutdown(context.Background()) })
	var provider sessionapi.CommittedPublicEventProvider = legacy
	if source, ok := provider.CommittedPublicEvents(); ok || source != nil {
		t.Fatalf("legacy session CommittedPublicEvents() = (%v, %t), want (nil, false)", source, ok)
	}
	if _, err := legacy.SubscribeEvents(allFilter()); err != nil {
		t.Fatalf("legacy SubscribeEvents: %v", err)
	}

	appender := newCommittedSessionAppender()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}), WithEventAppender(appender))
	if err != nil {
		t.Fatalf("New(committed): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	source, ok := sessionapi.CommittedPublicEventProvider(s).CommittedPublicEvents()
	if !ok || source == nil {
		t.Fatalf("committed session CommittedPublicEvents() = (%v, %t), want a source and true", source, ok)
	}
	sub, err := source.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	if err := s.PublishEvent(context.Background(), event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: s.SessionID()},
	}}); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	select {
	case d, open := <-sub.Events():
		if !open {
			t.Fatal("committed subscription closed before delivering")
		}
		if !d.Committed() {
			t.Fatalf("committed delivery = %+v, want committed bytes", d)
		}
		if !bytes.Equal(d.PublicBody, appender.storedBody(d.JournalSeq)) {
			t.Errorf("PublicBody = %s, want the stored body %s", d.PublicBody, appender.storedBody(d.JournalSeq))
		}
		if d.CoveredThrough != d.JournalSeq {
			t.Errorf("CoveredThrough = %d, want %d", d.CoveredThrough, d.JournalSeq)
		}
	case <-time.After(time.Second):
		t.Fatal("committed subscription delivered nothing")
	}
}
