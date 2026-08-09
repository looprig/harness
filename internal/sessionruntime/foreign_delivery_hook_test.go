package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/tool"
)

type foreignDeliveryCommandAppender struct {
	mu      sync.Mutex
	records []journal.CommandRecord
	err     error
}

func (a *foreignDeliveryCommandAppender) AppendCommand(_ context.Context, record journal.CommandRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.records = append(a.records, record)
	return nil
}

func (a *foreignDeliveryCommandAppender) snapshot() []journal.CommandRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]journal.CommandRecord, len(a.records))
	copy(result, a.records)
	return result
}

type foreignDeliveryEventAppender struct {
	mu     sync.Mutex
	events []event.Event
	err    error
	seq    uint64
}

func (a *foreignDeliveryEventAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return 0, a.err
	}
	a.seq++
	a.events = append(a.events, ev)
	return a.seq, nil
}

func (a *foreignDeliveryEventAppender) snapshot() []event.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]event.Event, len(a.events))
	copy(result, a.events)
	return result
}

func newForeignDeliveryHookFixture(t *testing.T) (*Session, *foreignDeliveryHook, *foreignDeliveryCommandAppender, *foreignDeliveryEventAppender, foreign.DeliveryIntent, command.UserInput) {
	t.Helper()
	sessionID, loopID, requestID := mustUUID(), mustUUID(), mustUUID()
	commands := &foreignDeliveryCommandAppender{}
	events := &foreignDeliveryEventAppender{}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		sessionID:     sessionID,
		sessionCtx:    ctx,
		sessionCancel: cancel,
		hub:           hub.New(sessionID, hub.WithAppender(events)),
		newID:         uuid.New,
		now:           time.Now,
		factory:       event.NewFactory(uuid.New, time.Now),
		cmdAppender:   commands,
		loops:         map[uuid.UUID]*loopHandle{loopID: {id: loopID}},
	}
	hook := newForeignDeliveryHook(s, loopID)
	cmd := command.UserInput{
		Header:       command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		Blocks:       []content.Block{&content.TextBlock{Text: "steer"}},
		TargetLoopID: loopID,
	}
	hook.bindCommand(cmd)
	t.Cleanup(cancel)
	return s, hook, commands, events, foreign.DeliveryIntent{LoopID: loopID, RequestID: requestID}, cmd
}

func TestForeignDeliveryHookCreatesIntentBeforeReservation(t *testing.T) {
	t.Parallel()
	_, hook, commands, events, intent, cmd := newForeignDeliveryHookFixture(t)

	if err := hook.CreateIntent(context.Background(), intent); err != nil {
		t.Fatalf("CreateIntent: %v", err)
	}
	if got := commands.snapshot(); len(got) != 1 {
		t.Fatalf("intent command records = %d, want 1", len(got))
	} else {
		persisted, ok := got[0].Command().(command.UserInput)
		if !ok || persisted.CommandID != cmd.CommandID || persisted.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
			t.Fatalf("intent command = %#v, want exact command with intent phase", got[0].Command())
		}
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got := events.snapshot(); len(got) != 1 {
		t.Fatalf("reservation events = %d, want 1", len(got))
	} else if state, ok := got[0].(event.DelegateDeliveryStateChanged); !ok || state.RequestID != intent.RequestID || state.TargetLoopID != intent.LoopID || state.State != event.DelegateDeliverySteerAttemptReserved {
		t.Fatalf("reservation event = %#v, want bound reserved state", got[0])
	}
}

func TestForeignDeliveryHookFallbackPersistsExactCommandOnce(t *testing.T) {
	t.Parallel()
	_, hook, commands, events, intent, cmd := newForeignDeliveryHookFixture(t)
	if err := hook.CreateIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
		t.Fatal(err)
	}
	if err := hook.QueueFallback(context.Background(), foreign.DeliveryFallback(intent)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
	if err := hook.QueueFallback(context.Background(), foreign.DeliveryFallback(intent)); err == nil {
		t.Fatal("duplicate QueueFallback succeeded, want fail closed")
	}
	got := commands.snapshot()
	if len(got) != 2 {
		t.Fatalf("command records = %d, want intent plus one fallback", len(got))
	}
	fallback, ok := got[1].Command().(command.UserInput)
	if !ok {
		t.Fatalf("fallback command = %T, want command.UserInput", got[1].Command())
	}
	if fallback.CommandID != cmd.CommandID || fallback.TargetLoopID != cmd.TargetLoopID || fallback.DelegateDeliveryPhase != command.DelegateDeliveryPhaseFallbackQueued || len(fallback.Blocks) != len(cmd.Blocks) {
		t.Fatalf("fallback command = %#v, want exact command id/route/payload with fallback phase", fallback)
	}
	if len(events.snapshot()) != 1 {
		t.Fatalf("fallback emitted %d events, want reservation only", len(events.snapshot()))
	}
}

func TestForeignDeliveryHookInjectedResolutionRequiresAuthoritativeFold(t *testing.T) {
	t.Parallel()
	s, hook, _, events, intent, _ := newForeignDeliveryHookFixture(t)
	if err := hook.CreateIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
		t.Fatal(err)
	}
	turnID := mustUUID()
	resolution := foreign.DeliveryResolution{LoopID: intent.LoopID, RequestID: intent.RequestID, TurnID: turnID, State: foreign.DeliveryResolutionInjected}
	if err := hook.Resolve(context.Background(), resolution); err == nil {
		t.Fatal("Resolve(injected) succeeded before TurnFoldedInto, want fail closed")
	}
	if err := s.PublishEventChecked(context.Background(), event.TurnFoldedInto{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: intent.LoopID, TurnID: turnID},
		EventID:     mustUUID(),
		Cause:       identity.Cause{CommandID: intent.RequestID},
	}}); err != nil {
		t.Fatalf("PublishEventChecked(TurnFoldedInto): %v", err)
	}
	if err := hook.Resolve(context.Background(), resolution); err != nil {
		t.Fatalf("Resolve(injected): %v", err)
	}
	got := events.snapshot()
	if len(got) != 2 {
		t.Fatalf("resolution events = %d, want reservation plus authoritative fold", len(got))
	}
	stateEvents := 0
	for _, ev := range got {
		if _, ok := ev.(event.DelegateDeliveryStateChanged); ok {
			stateEvents++
		}
	}
	if stateEvents != 1 {
		t.Fatalf("state events = %d, want reservation only (fold is authoritative)", stateEvents)
	}
}

func TestForeignDeliveryHookIgnoresFoldFromAnotherSessionOrRoute(t *testing.T) {
	t.Parallel()
	s, hook, _, _, intent, _ := newForeignDeliveryHookFixture(t)
	if err := hook.CreateIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
		t.Fatal(err)
	}
	turnID := mustUUID()
	hook.observeFold(event.TurnFoldedInto{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: mustUUID(), LoopID: intent.LoopID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: intent.RequestID},
	}})
	hook.observeFold(event.TurnFoldedInto{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: intent.LoopID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: intent.RequestID, Coordinates: identity.Coordinates{LoopID: mustUUID()}},
	}})
	if err := hook.Resolve(context.Background(), foreign.DeliveryResolution{
		LoopID: intent.LoopID, RequestID: intent.RequestID, TurnID: turnID,
		State: foreign.DeliveryResolutionInjected,
	}); err == nil {
		t.Fatal("Resolve accepted a fold from another session or route")
	}
}

func TestForeignDeliveryHookUnknownAndUntrackableAreCheckedAndTerminal(t *testing.T) {
	t.Parallel()
	for _, state := range []foreign.DeliveryResolutionState{foreign.DeliveryResolutionUnknown, foreign.DeliveryResolutionUntrackable} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			_, hook, _, events, intent, _ := newForeignDeliveryHookFixture(t)
			if err := hook.CreateIntent(context.Background(), intent); err != nil {
				t.Fatal(err)
			}
			if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
				t.Fatal(err)
			}
			if err := hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: intent.LoopID, RequestID: intent.RequestID, State: state}); err != nil {
				t.Fatalf("Resolve(%s): %v", state, err)
			}
			if err := hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: intent.LoopID, RequestID: intent.RequestID, State: state}); err == nil {
				t.Fatalf("duplicate Resolve(%s) succeeded, want fail closed", state)
			}
			if got := events.snapshot(); len(got) != 2 {
				t.Fatalf("events = %d, want reservation plus terminal resolution", len(got))
			}
		})
	}
}

func TestForeignDeliveryHookRejectsRouteMismatchAndInvalidTransitions(t *testing.T) {
	t.Parallel()
	_, hook, _, _, intent, _ := newForeignDeliveryHookFixture(t)
	wrongLoop := mustUUID()
	if err := hook.CreateIntent(context.Background(), foreign.DeliveryIntent{LoopID: wrongLoop, RequestID: intent.RequestID}); err == nil {
		t.Fatal("CreateIntent accepted wrong loop route")
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err == nil {
		t.Fatal("Reserve succeeded before CreateIntent")
	}
	if err := hook.CreateIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := hook.CreateIntent(context.Background(), intent); err == nil {
		t.Fatal("duplicate CreateIntent succeeded")
	}
	if err := hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: intent.LoopID, RequestID: intent.RequestID, State: foreign.DeliveryResolutionUnknown}); err == nil {
		t.Fatal("Resolve(unknown) before reservation succeeded")
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation{LoopID: intent.LoopID, RequestID: mustUUID()}); err == nil {
		t.Fatal("Reserve accepted unknown request")
	}
}

func TestForeignDeliveryHookPublicationFailureDoesNotCommitResolution(t *testing.T) {
	t.Parallel()
	s, hook, _, events, intent, _ := newForeignDeliveryHookFixture(t)
	if err := hook.CreateIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	events.mu.Lock()
	events.err = errors.New("append failed")
	events.mu.Unlock()
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err == nil {
		t.Fatal("Reserve succeeded despite checked publication failure")
	}
	events.mu.Lock()
	events.err = nil
	events.mu.Unlock()
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
		t.Fatalf("Reserve retry: %v", err)
	}
	if got := len(events.snapshot()); got != 1 {
		t.Fatalf("events = %d, want one committed reservation after retry", got)
	}
	_ = s
}

func TestForeignDeliveryHookPublicationErrorsAreBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	_, hook, commands, _, intent, _ := newForeignDeliveryHookFixture(t)
	commands.mu.Lock()
	commands.err = errors.New("bearer-secret-must-not-escape")
	commands.mu.Unlock()

	err := hook.CreateIntent(context.Background(), intent)
	if err == nil {
		t.Fatal("CreateIntent succeeded despite append failure")
	}
	if strings.Contains(err.Error(), "bearer-secret-must-not-escape") {
		t.Fatalf("publication error leaked underlying secret: %q", err)
	}
	if len(err.Error()) > 128 {
		t.Fatalf("publication error length = %d, want bounded <= 128: %q", len(err.Error()), err)
	}
}

func TestForeignDeliveryHookRestoreReservationRepairsUnknown(t *testing.T) {
	t.Parallel()
	sessionID, loopID, requestID := mustUUID(), mustUUID(), mustUUID()
	cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, TargetLoopID: loopID, DelegateDeliveryPhase: command.DelegateDeliveryPhaseIntent}
	records := []journal.JournalRecord{journal.NewCommandRecord(sessionID, loopID, cmd)}
	events := []event.Event{event.DelegateDeliveryStateChanged{
		Header:       event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
		RequestID:    requestID,
		TargetLoopID: loopID,
		State:        event.DelegateDeliverySteerAttemptReserved,
	}}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, events, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	resolved, ok := durableResolvedRecord(manager, requestID)
	if !ok || resolved.status != tool.DelegateStatusUnknown || resolved.childID != loopID {
		t.Fatalf("resolved reservation = %+v, %v; want unknown bound request", resolved, ok)
	}
}
