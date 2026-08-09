package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/storage/memstore"
)

type deliveryRepairJournal struct {
	records []journal.JournalRecord
}

func (j *deliveryRepairJournal) Append(_ context.Context, record journal.JournalRecord) (uint64, error) {
	j.records = append(j.records, record)
	return uint64(len(j.records)), nil
}

func deliveryReservationEvent(sessionID, requestID, childID uuid.UUID) event.DelegateDeliveryStateChanged {
	return event.DelegateDeliveryStateChanged{
		Header:       event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
		RequestID:    requestID,
		TargetLoopID: childID,
		State:        event.DelegateDeliverySteerAttemptReserved,
	}
}

func deliveryResolutionEvent(sessionID, requestID, childID uuid.UUID, state event.DelegateDeliveryState) event.DelegateDeliveryStateChanged {
	return event.DelegateDeliveryStateChanged{
		Header:       event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
		RequestID:    requestID,
		TargetLoopID: childID,
		State:        state,
	}
}

func deliveryTurnEvents(sessionID, parentID, childID, requestID, turnID uuid.UUID) []event.Event {
	return []event.Event{
		event.LoopStarted{
			Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
				Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
			},
			DisplayName:      "worker",
			InitialRequestID: uuid.UUID{},
		},
		event.TurnStarted{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID, TurnID: turnID},
			Cause:       identity.Cause{CommandID: requestID},
		}},
		event.TurnFoldedInto{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID, TurnID: turnID},
			Cause:       identity.Cause{CommandID: requestID},
		}},
		event.TurnDone{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID, TurnID: turnID},
		}, Message: aiMessage("answer")},
	}
}

func TestRestoreReservationThenFoldUsesAuthoritativeTerminal(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID, requestID, turnID := mustUUID(), mustUUID(), mustUUID(), mustUUID(), mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
	}
	events := deliveryTurnEvents(sessionID, parentID, childID, requestID, turnID)
	accepted := event.DelegateRequestAccepted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
		Cause:       identity.Cause{CommandID: requestID},
	}}
	events = append(events[:2], append([]event.Event{deliveryReservationEvent(sessionID, requestID, childID), accepted}, events[2:]...)...)
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, events, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	resolved, ok := durableResolvedRecord(manager, requestID)
	if !ok || resolved.childID != childID || resolved.status != tool.DelegateStatusCompleted || resolved.text != "answer" {
		t.Fatalf("resolved reservation+fold sequence = %+v, %v; want completed terminal", resolved, ok)
	}
	repairs := &deliveryRepairJournal{}
	factory := event.NewFactory(uuid.New, time.Now)
	if got, err := persistUnresolvedDelegateDeliveryStates(context.Background(), repairs, factory, sessionID, records, events, nil); err != nil {
		t.Fatalf("persistUnresolvedDelegateDeliveryStates: %v", err)
	} else if len(got) != 0 || len(repairs.records) != 0 {
		t.Fatalf("reservation+fold repairs = %d/%d, want none", len(got), len(repairs.records))
	}
	plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, events, nil)
	if err != nil {
		t.Fatalf("planRestoredBackgroundRequests: %v", err)
	}
	if len(plan) != 1 || plan[0].reAdmit != nil || plan[0].resolved.status != tool.DelegateStatusCompleted {
		t.Fatalf("reservation+fold restore plan = %+v, want one terminal completion", plan)
	}
}

func TestRestoreReservationThenFallbackReadmitsFallbackWithoutUnknownRepair(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID, requestID := mustUUID(), mustUUID(), mustUUID(), mustUUID()
	base := phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)
	fallback := base
	fallback.DelegateDeliveryPhase = command.DelegateDeliveryPhaseFallbackQueued
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, base),
		journal.NewCommandRecord(sessionID, childID, fallback),
	}
	replayed := []event.Event{event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
			Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
		},
		DisplayName: "worker",
	}, deliveryReservationEvent(sessionID, requestID, childID)}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	if _, ok := durableResolvedRecord(manager, requestID); ok {
		t.Fatalf("reserved fallback request entered terminal resolution: %+v", manager.resolved[requestID])
	}
	repairs := &deliveryRepairJournal{}
	factory := event.NewFactory(uuid.New, time.Now)
	if got, err := persistUnresolvedDelegateDeliveryStates(context.Background(), repairs, factory, sessionID, records, replayed, nil); err != nil {
		t.Fatalf("persistUnresolvedDelegateDeliveryStates: %v", err)
	} else if len(got) != 0 || len(repairs.records) != 0 {
		t.Fatalf("reserved fallback repairs = %d/%d, want none", len(got), len(repairs.records))
	}
	plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
	if err != nil {
		t.Fatalf("planRestoredBackgroundRequests: %v", err)
	}
	if len(plan) != 1 || plan[0].reAdmit == nil || plan[0].reAdmit.CommandID != requestID || plan[0].reAdmit.DelegateDeliveryPhase != command.DelegateDeliveryPhaseFallbackQueued {
		t.Fatalf("reserved fallback restore plan = %+v, want fallback re-admission", plan)
	}
}

func TestRestoreResolvedUnknownThenFoldFailsClosed(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID, requestID, turnID := mustUUID(), mustUUID(), mustUUID(), mustUUID(), mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
	}
	events := deliveryTurnEvents(sessionID, parentID, childID, requestID, turnID)
	accepted := event.DelegateRequestAccepted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
		Cause:       identity.Cause{CommandID: requestID},
	}}
	events = append(events[:2], append([]event.Event{accepted}, events[2:]...)...)
	events = append(events[:2], append([]event.Event{
		deliveryResolutionEvent(sessionID, requestID, childID, event.DelegateDeliveryResolvedUnknown),
	}, events[2:]...)...)
	err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, events, nil)
	var contradiction *delegateRestoreContradictionError
	if !errors.As(err, &contradiction) {
		t.Fatalf("unknown+fold restore error = %T %v, want contradiction", err, err)
	}
}

func TestRestoreTerminalDeliveryStateRequiresReservation(t *testing.T) {
	t.Parallel()
	for _, state := range []event.DelegateDeliveryState{
		event.DelegateDeliveryResolvedUnknown,
		event.DelegateDeliveryResolvedUntrackable,
	} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			sessionID, childID, requestID := mustUUID(), mustUUID(), mustUUID()
			records := []journal.JournalRecord{
				journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
			}
			events := []event.Event{deliveryResolutionEvent(sessionID, requestID, childID, state)}
			err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, events, nil)
			var contradiction *delegateRestoreContradictionError
			if !errors.As(err, &contradiction) {
				t.Fatalf("terminal without reservation error = %T %v, want contradiction", err, err)
			}
		})
	}
}

func TestRestoreFallbackConflictsWithTerminalDeliveryStateRegardlessOrder(t *testing.T) {
	t.Parallel()
	for _, terminalFirst := range []bool{false, true} {
		terminalFirst := terminalFirst
		t.Run(map[bool]string{false: "reservation_then_terminal", true: "terminal_then_reservation"}[terminalFirst], func(t *testing.T) {
			t.Parallel()
			sessionID, childID, requestID := mustUUID(), mustUUID(), mustUUID()
			intent := phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)
			fallback := intent
			fallback.DelegateDeliveryPhase = command.DelegateDeliveryPhaseFallbackQueued
			records := []journal.JournalRecord{
				journal.NewCommandRecord(sessionID, childID, intent),
				journal.NewCommandRecord(sessionID, childID, fallback),
			}
			reservation := deliveryReservationEvent(sessionID, requestID, childID)
			terminal := deliveryResolutionEvent(sessionID, requestID, childID, event.DelegateDeliveryResolvedUnknown)
			events := []event.Event{reservation, terminal}
			if terminalFirst {
				events = []event.Event{terminal, reservation}
			}
			err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, events, nil)
			var contradiction *delegateRestoreContradictionError
			if !errors.As(err, &contradiction) {
				t.Fatalf("fallback terminal corruption error = %T %v, want contradiction", err, err)
			}
		})
	}
}

func TestRealJournalRejectsFallbackTerminalCorruption(t *testing.T) {
	t.Parallel()
	sessionID, childID, requestID := mustUUID(), mustUUID(), mustUUID()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	durable, err := store.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatal(err)
	}
	intent := phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)
	fallback := intent
	fallback.DelegateDeliveryPhase = command.DelegateDeliveryPhaseFallbackQueued
	for _, record := range []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, intent),
		journal.NewCommandRecord(sessionID, childID, fallback),
		journal.NewEventRecord(deliveryReservationEvent(sessionID, requestID, childID)),
		journal.NewEventRecord(deliveryResolutionEvent(sessionID, requestID, childID, event.DelegateDeliveryResolvedUnknown)),
	} {
		if _, err := durable.Append(context.Background(), record); err != nil {
			t.Fatalf("append corrupt record: %v", err)
		}
	}
	replayer, err := store.OpenInternalRecordReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatal(err)
	}
	all, err := drainRecordReplay(context.Background(), replayer, journal.ReplayRequest{Follow: false})
	if err != nil {
		t.Fatal(err)
	}
	var records []journal.JournalRecord
	var events []event.Event
	for _, record := range all {
		switch typed := record.(type) {
		case journal.CommandRecord:
			records = append(records, typed)
		case journal.EventRecord:
			events = append(events, typed.Event())
		}
	}
	err = seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, events, nil)
	var contradiction *delegateRestoreContradictionError
	if !errors.As(err, &contradiction) {
		t.Fatalf("real journal corruption error = %T %v, want contradiction", err, err)
	}
}
