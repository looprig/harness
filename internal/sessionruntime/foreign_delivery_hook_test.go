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
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/storage/memstore"
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
	cmd.Blocks[0].(*content.TextBlock).Text = "mutated-after-bind"
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
	fallbackText, _ := fallback.Blocks[0].(*content.TextBlock)
	if fallback.CommandID != cmd.CommandID || fallback.TargetLoopID != cmd.TargetLoopID || fallback.DelegateDeliveryPhase != command.DelegateDeliveryPhaseFallbackQueued || len(fallback.Blocks) != len(cmd.Blocks) || fallbackText == nil || fallbackText.Text != "steer" {
		t.Fatalf("fallback command = %#v, want exact command id/route/payload with fallback phase", fallback)
	}
	if len(events.snapshot()) != 1 {
		t.Fatalf("fallback emitted %d events, want reservation only", len(events.snapshot()))
	}
	hook.mu.Lock()
	commandsRetained, foldsRetained := len(hook.commands), len(hook.folds)
	hook.mu.Unlock()
	if commandsRetained != 0 || foldsRetained != 0 {
		t.Fatalf("fallback retained payload state commands=%d folds=%d, want both zero", commandsRetained, foldsRetained)
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
	secretErr := errors.New("bearer-secret-must-not-escape")
	commands.err = secretErr
	commands.mu.Unlock()

	err := hook.CreateIntent(context.Background(), intent)
	if err == nil {
		t.Fatal("CreateIntent succeeded despite append failure")
	}
	if strings.Contains(err.Error(), "bearer-secret-must-not-escape") {
		t.Fatalf("publication error leaked underlying secret: %q", err)
	}
	if errors.Is(err, secretErr) {
		t.Fatal("publication error exposed underlying secret through errors.Is")
	}
	if len(err.Error()) > 128 {
		t.Fatalf("publication error length = %d, want bounded <= 128: %q", len(err.Error()), err)
	}
}

func TestForeignDeliveryHookFailedIntentPublicationReleasesBoundCommand(t *testing.T) {
	t.Parallel()
	_, hook, commands, _, intent, _ := newForeignDeliveryHookFixture(t)
	commands.mu.Lock()
	commands.err = errors.New("append failed")
	commands.mu.Unlock()
	for attempt := 0; attempt < 32; attempt++ {
		if err := hook.CreateIntent(context.Background(), intent); err == nil {
			t.Fatal("CreateIntent succeeded despite publication failure")
		}
		hook.mu.Lock()
		retained := len(hook.commands)
		hook.mu.Unlock()
		if retained != 0 {
			t.Fatalf("failed CreateIntent attempt %d retained %d command payloads", attempt, retained)
		}
		if err := hook.bindCommand(command.UserInput{
			Header:       command.Header{CommandID: intent.RequestID, Agency: identity.AgencyMachine},
			TargetLoopID: intent.LoopID,
		}); err != nil {
			t.Fatalf("rebind attempt %d: %v", attempt, err)
		}
	}
}

func TestForeignDeliveryHookTerminalStateCardinalityBounded(t *testing.T) {
	t.Parallel()
	_, hook, commands, _, baseIntent, _ := newForeignDeliveryHookFixture(t)
	commands.mu.Lock()
	commands.err = nil
	commands.mu.Unlock()
	if err := hook.CreateIntent(context.Background(), baseIntent); err != nil {
		t.Fatal(err)
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(baseIntent)); err != nil {
		t.Fatal(err)
	}
	if err := hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: baseIntent.LoopID, RequestID: baseIntent.RequestID, State: foreign.DeliveryResolutionUnknown}); err != nil {
		t.Fatal(err)
	}
	const requests = foreignDeliveryTombstoneLimit*4 + 17
	for i := 0; i < requests; i++ {
		requestID := mustUUID()
		cmd := command.UserInput{
			Header:       command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
			TargetLoopID: hook.loopID,
		}
		if err := hook.bindCommand(cmd); err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		intent := foreign.DeliveryIntent{LoopID: hook.loopID, RequestID: requestID}
		if err := hook.CreateIntent(context.Background(), intent); err != nil {
			t.Fatalf("CreateIntent %d: %v", i, err)
		}
		if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
			t.Fatalf("Reserve %d: %v", i, err)
		}
		if i%2 == 0 {
			if err := hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: hook.loopID, RequestID: requestID, State: foreign.DeliveryResolutionUnknown}); err != nil {
				t.Fatalf("Resolve %d: %v", i, err)
			}
		} else if err := hook.QueueFallback(context.Background(), foreign.DeliveryFallback(intent)); err != nil {
			t.Fatalf("QueueFallback %d: %v", i, err)
		}
	}
	hook.mu.Lock()
	phases, tombstones := len(hook.phases), len(hook.tombstones)
	commandsRetained, foldsRetained := len(hook.commands), len(hook.folds)
	hook.mu.Unlock()
	if phases > foreignDeliveryTombstoneLimit || tombstones > foreignDeliveryTombstoneLimit || commandsRetained != 0 || foldsRetained != 0 {
		t.Fatalf("hook state cardinality phases=%d tombstones=%d commands=%d folds=%d, want bounded payload-free state", phases, tombstones, commandsRetained, foldsRetained)
	}
}

func TestForeignDeliveryHookRestoredCompletionReleasesBoundCommand(t *testing.T) {
	t.Parallel()
	s, original, _, _, intent, cmd := newForeignDeliveryHookFixture(t)
	hook := newForeignDeliveryHook(s, original.loopID)
	cmd.DelegateDeliveryPhase = command.DelegateDeliveryPhaseIntent
	if err := hook.observeRestoredCommand(cmd); err != nil {
		t.Fatalf("observeRestoredCommand: %v", err)
	}
	hook.observeFold(event.TurnFoldedInto{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: hook.sessionID, LoopID: intent.LoopID, TurnID: mustUUID()},
		Cause:       identity.Cause{CommandID: intent.RequestID},
	}})
	hook.mu.Lock()
	commandsRetained, foldsRetained := len(hook.commands), len(hook.folds)
	hook.mu.Unlock()
	if commandsRetained != 0 || foldsRetained != 0 {
		t.Fatalf("restored completion retained payload state commands=%d folds=%d", commandsRetained, foldsRetained)
	}
}

func TestForeignDeliveryHookFallbackPublicationFailureDoesNotCommitTransition(t *testing.T) {
	t.Parallel()
	_, hook, commands, _, intent, _ := newForeignDeliveryHookFixture(t)
	if err := hook.CreateIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
		t.Fatal(err)
	}
	commands.mu.Lock()
	commands.err = errors.New("fallback append failed")
	commands.mu.Unlock()
	if err := hook.QueueFallback(context.Background(), foreign.DeliveryFallback(intent)); err == nil {
		t.Fatal("QueueFallback succeeded despite checked publication failure")
	}
	commands.mu.Lock()
	commands.err = nil
	commands.mu.Unlock()
	if err := hook.QueueFallback(context.Background(), foreign.DeliveryFallback(intent)); err != nil {
		t.Fatalf("QueueFallback retry: %v", err)
	}
	if got := len(commands.snapshot()); got != 2 {
		t.Fatalf("command records = %d, want intent plus one committed fallback", got)
	}
}

func TestForeignDeliveryHookCleansPayloadAfterTerminalResolution(t *testing.T) {
	t.Parallel()
	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		s, hook, _, _, intent, _ := newForeignDeliveryHookFixture(t)
		if err := hook.CreateIntent(context.Background(), intent); err != nil {
			t.Fatal(err)
		}
		if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
			t.Fatal(err)
		}
		if err := hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: intent.LoopID, RequestID: intent.RequestID, State: foreign.DeliveryResolutionUnknown}); err != nil {
			t.Fatal(err)
		}
		hook.observeFold(event.TurnFoldedInto{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: intent.LoopID, TurnID: mustUUID()},
			Cause:       identity.Cause{CommandID: intent.RequestID},
		}})
		hook.mu.Lock()
		commandsRetained, foldsRetained := len(hook.commands), len(hook.folds)
		hook.mu.Unlock()
		if commandsRetained != 0 || foldsRetained != 0 {
			t.Fatalf("unknown resolution retained payload state commands=%d folds=%d, want both zero", commandsRetained, foldsRetained)
		}
	})
	t.Run("injected", func(t *testing.T) {
		s, hook, _, _, intent, _ := newForeignDeliveryHookFixture(t)
		if err := hook.CreateIntent(context.Background(), intent); err != nil {
			t.Fatal(err)
		}
		if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
			t.Fatal(err)
		}
		fold := event.TurnFoldedInto{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: intent.LoopID, TurnID: mustUUID()},
			Cause:       identity.Cause{CommandID: intent.RequestID},
		}}
		hook.observeFold(fold)
		if err := hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: intent.LoopID, RequestID: intent.RequestID, TurnID: fold.TurnID, State: foreign.DeliveryResolutionInjected}); err != nil {
			t.Fatal(err)
		}
		hook.observeFold(fold)
		hook.mu.Lock()
		commandsRetained, foldsRetained := len(hook.commands), len(hook.folds)
		hook.mu.Unlock()
		if commandsRetained != 0 || foldsRetained != 0 {
			t.Fatalf("injected resolution retained payload state commands=%d folds=%d, want both zero", commandsRetained, foldsRetained)
		}
	})
}

func TestForeignDeliveryHookUsesRealJournalCommandAppender(t *testing.T) {
	t.Parallel()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	sessionID, loopID, requestID := mustUUID(), mustUUID(), mustUUID()
	lease, err := store.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	durable, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		sessionID:     sessionID,
		sessionCtx:    ctx,
		sessionCancel: cancel,
		hub:           hub.New(sessionID, hub.WithAppender(journal.NewJournalEventAppender(durable))),
		factory:       event.NewFactory(uuid.New, time.Now),
		cmdAppender:   journal.NewJournalCommandAppender(durable),
		loops:         map[uuid.UUID]*loopHandle{loopID: {id: loopID}},
	}
	t.Cleanup(func() {
		cancel()
		_ = lease.Release(context.Background())
	})
	hook := newForeignDeliveryHook(s, loopID)
	cmd := command.UserInput{
		Header:       command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		Blocks:       []content.Block{&content.TextBlock{Text: "real journal"}},
		NoFold:       true,
		TargetLoopID: loopID,
	}
	if err := hook.bindCommand(cmd); err != nil {
		t.Fatalf("bindCommand: %v", err)
	}
	intent := foreign.DeliveryIntent{LoopID: loopID, RequestID: requestID}
	if err := hook.CreateIntent(context.Background(), intent); err != nil {
		t.Fatalf("CreateIntent: %v", err)
	}
	if err := hook.Reserve(context.Background(), foreign.DeliveryReservation(intent)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := hook.QueueFallback(context.Background(), foreign.DeliveryFallback(intent)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
}

func TestForeignServiceDelegateBindsBeforeActorCreateIntent(t *testing.T) {
	t.Parallel()
	loopID := mustUUID()
	backend := &channelBackend{Commands: make(chan command.Command), Done: make(chan struct{})}
	commands := &foreignDeliveryCommandAppender{}
	s := &Session{
		sessionID:   mustUUID(),
		sessionCtx:  context.Background(),
		newID:       uuid.New,
		cmdAppender: commands,
		loops:       map[uuid.UUID]*loopHandle{loopID: {id: loopID, backend: backend}},
	}
	newForeignDeliveryHook(s, loopID)
	actorErr := make(chan error, 1)
	go func() {
		input, ok := (<-backend.Commands).(command.UserInput)
		if !ok {
			actorErr <- errors.New("actor received non-user input")
			return
		}
		if input.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
			input.Accepted <- errors.New("intent phase missing before actor admission")
			actorErr <- errors.New("intent phase missing before actor admission")
			return
		}
		input.Accepted <- nil
		actorErr <- nil
	}()

	requestID, _, err := s.enqueueDelegateTurn(context.Background(), loopID, delegateBlocks("foreign"), false,
		func(_, childID uuid.UUID) *requestTracker { return &requestTracker{childID: childID} },
		func(uuid.UUID, *requestTracker) {})
	if err != nil {
		t.Fatalf("enqueueDelegateTurn: %v", err)
	}
	if err := <-actorErr; err != nil {
		t.Fatal(err)
	}
	got := commands.snapshot()
	if len(got) != 1 {
		t.Fatalf("durable command records = %d, want one hook-owned intent", len(got))
	}
	persisted, ok := got[0].Command().(command.UserInput)
	if !ok || persisted.CommandID != requestID || persisted.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
		t.Fatalf("persisted command = %#v, want bound intent %v", got[0].Command(), requestID)
	}
}

func TestRestoredDelegateBindsForeignCommandBeforeAdmission(t *testing.T) {
	t.Parallel()
	loopID, requestID := mustUUID(), mustUUID()
	backend := &channelBackend{Commands: make(chan command.Command), Done: make(chan struct{})}
	sub := newTrackingDelegateSubscription()
	commands := &foreignDeliveryCommandAppender{}
	s := &Session{
		sessionID:   mustUUID(),
		sessionCtx:  context.Background(),
		cmdAppender: commands,
		loops:       map[uuid.UUID]*loopHandle{loopID: {id: loopID, backend: backend}},
		delegateSubscribe: func(event.EventFilter) (event.Subscription, error) {
			return sub, nil
		},
	}
	hook := newForeignDeliveryHook(s, loopID)
	m := newDelegationManager(Topology{})
	cmd := command.UserInput{
		Header:                command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		Blocks:                []content.Block{&content.TextBlock{Text: "restored foreign"}},
		NoFold:                true,
		TargetLoopID:          loopID,
		DelegateDeliveryPhase: command.DelegateDeliveryPhaseIntent,
	}
	entry := restoredBackgroundPlan{requestID: requestID, childID: loopID, reAdmit: &cmd}
	received := make(chan command.UserInput, 1)
	go func() {
		got, ok := (<-backend.Commands).(command.UserInput)
		if ok {
			received <- got
		}
	}()
	m.readmitRestoredBackgroundRequest(s, entry)
	select {
	case got := <-received:
		if got.CommandID != requestID || got.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
			t.Fatalf("restored command = %#v, want exact intent %v", got, requestID)
		}
		hook.mu.Lock()
		bound, ok := hook.commands[requestID]
		hook.mu.Unlock()
		if !ok || bound.CommandID != requestID || bound.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
			t.Fatalf("restored command was not bound before admission: %#v, %v", bound, ok)
		}
		rebound := cmd
		rebound.Accepted = make(chan error, 1)
		if err := hook.bindCommand(rebound); err != nil {
			t.Fatalf("rebind with recreated Accepted channel: %v", err)
		}
		if got := len(commands.snapshot()); got != 0 {
			t.Fatalf("restore re-admission appended %d journal commands, want none", got)
		}
	case <-time.After(time.Second):
		t.Fatal("restored command was not admitted")
	}
	t.Cleanup(func() { close(sub.events) })
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
