package sessionruntime

import (
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/tool"
)

func phasedBackgroundCommand(requestID, childID uuid.UUID, phase command.DelegateDeliveryPhase) command.UserInput {
	return command.UserInput{
		Header:                command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		TargetLoopID:          childID,
		BackgroundHandBack:    true,
		DelegateDeliveryPhase: phase,
	}
}

func TestMessageAgentRestoreRecognizesPhasedIntentAndFallback(t *testing.T) {
	t.Parallel()
	sessionID, childID := mustUUID(), mustUUID()
	intentID, fallbackID := mustUUID(), mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(intentID, childID, command.DelegateDeliveryPhaseIntent)),
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(fallbackID, childID, command.DelegateDeliveryPhaseFallbackQueued)),
	}
	got, err := backgroundDelegateIntents(records)
	if err != nil {
		t.Fatalf("backgroundDelegateIntents: %v", err)
	}
	if got[intentID] != childID {
		t.Fatalf("intent target = %v, want %v", got[intentID], childID)
	}
	if got[fallbackID] != childID {
		t.Fatalf("fallback target = %v, want %v", got[fallbackID], childID)
	}
}

func TestMessageAgentRestoreUnresolvedReservationBecomesUnknownWithoutSteer(t *testing.T) {
	t.Parallel()
	sessionID, childID := mustUUID(), mustUUID()
	requestID := mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
	}
	reservation := event.DelegateDeliveryStateChanged{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
		RequestID: requestID, TargetLoopID: childID,
		State: event.DelegateDeliverySteerAttemptReserved,
	}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, []event.Event{reservation}, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	resolved, ok := durableResolvedRecord(manager, requestID)
	if !ok || resolved.childID != childID || resolved.status != tool.DelegateStatusUnknown {
		t.Fatalf("unresolved reservation = %+v, %v; want child=%v status=%v", resolved, ok, childID, tool.DelegateStatusUnknown)
	}
}

func TestMessageAgentRestoreRejectsPhasedStateRouteContradiction(t *testing.T) {
	t.Parallel()
	sessionID, childID, wrongChild := mustUUID(), mustUUID(), mustUUID()
	requestID := mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseFallbackQueued)),
	}
	contradiction := event.DelegateDeliveryStateChanged{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
		RequestID: requestID, TargetLoopID: wrongChild,
		State: event.DelegateDeliveryResolvedUnknown,
	}
	err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, []event.Event{contradiction}, nil)
	var mismatch *journal.CommandRouteMismatchError
	if !errors.As(err, &mismatch) || mismatch.RecordLoopID != wrongChild || mismatch.TargetLoopID != childID {
		t.Fatalf("phased state contradiction error = %T %+v, want typed route mismatch", err, err)
	}
}

func TestMessageAgentRestoreFoldsAllRequestsIntoOneExactTerminal(t *testing.T) {
	t.Parallel()
	childID, turnID := mustUUID(), mustUUID()
	requestA, requestB, requestC, queuedOnly := mustUUID(), mustUUID(), mustUUID(), mustUUID()
	coord := identity.Coordinates{LoopID: childID, TurnID: turnID}
	got := foldDelegateTerminals([]event.Event{
		event.TurnStarted{Header: event.Header{Coordinates: coord, Cause: identity.Cause{CommandID: requestA}}},
		// InputQueued is an ephemeral admission hint, not evidence that a request
		// belongs to this turn and not an idempotence key for restore.
		event.InputQueued{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: queuedOnly}}},
		event.TurnFoldedInto{Header: event.Header{Coordinates: coord, Cause: identity.Cause{CommandID: requestB}}},
		event.TurnFoldedInto{Header: event.Header{Coordinates: coord, Cause: identity.Cause{CommandID: requestC}}},
		event.TurnDone{Header: event.Header{Coordinates: coord}, Message: aiMessage("one answer")},
	})
	for _, requestID := range []uuid.UUID{requestA, requestB, requestC} {
		resolved, ok := got[requestID]
		if !ok || resolved.childID != childID || resolved.status != tool.DelegateStatusCompleted || resolved.text != "one answer" {
			t.Fatalf("request %v resolved = %+v, %v; want one completed answer", requestID, resolved, ok)
		}
	}
	if _, queued := got[queuedOnly]; queued {
		t.Fatalf("InputQueued-only request %v became a terminal resolution: %+v", queuedOnly, got[queuedOnly])
	}
}

func TestMessageAgentRestoreReadmitsUnopenedPhasedCommands(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID := mustUUID(), mustUUID(), mustUUID()
	intentID, fallbackID := mustUUID(), mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(intentID, childID, command.DelegateDeliveryPhaseIntent)),
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(fallbackID, childID, command.DelegateDeliveryPhaseFallbackQueued)),
	}
	// InputQueued is deliberately the only ephemeral evidence for the fallback;
	// it must not suppress re-admission of the exact durable command.
	replayed := []event.Event{
		event.LoopStarted{Header: event.Header{
			Coordinates: identity.Coordinates{LoopID: childID},
			Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
		}, InitialRequestID: mustUUID()},
		event.InputQueued{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: fallbackID}}},
	}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
	if err != nil {
		t.Fatalf("planRestoredBackgroundRequests: %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("restore re-admission plan = %+v, want two phased commands", plan)
	}
	seen := make(map[uuid.UUID]bool, len(plan))
	for _, entry := range plan {
		if entry.reAdmit == nil || entry.reAdmit.CommandID.IsZero() {
			t.Fatalf("restore plan entry = %+v, want exact re-admission command", entry)
		}
		seen[entry.reAdmit.CommandID] = true
	}
	if !seen[intentID] || !seen[fallbackID] {
		t.Fatalf("restore re-admission ids = %v, want intent=%v fallback=%v", seen, intentID, fallbackID)
	}
}

func TestMessageAgentRestoreReservationNeverReadmits(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID := mustUUID(), mustUUID(), mustUUID()
	requestID := mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
	}
	replayed := []event.Event{
		event.LoopStarted{Header: event.Header{
			Coordinates: identity.Coordinates{LoopID: childID},
			Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
		}, InitialRequestID: mustUUID()},
		event.DelegateDeliveryStateChanged{
			Header:       event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
			RequestID:    requestID,
			TargetLoopID: childID,
			State:        event.DelegateDeliverySteerAttemptReserved,
		},
	}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
	if err != nil {
		t.Fatalf("planRestoredBackgroundRequests: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("unknown delivery plan = %+v, want one durable unknown hand-back", plan)
	}
	if plan[0].reAdmit != nil {
		t.Fatalf("unknown delivery was scheduled for steer: %+v", plan[0].reAdmit)
	}
	if plan[0].resolved.childID != childID || plan[0].resolved.status != tool.DelegateStatusUnknown {
		t.Fatalf("unknown delivery plan resolution = %+v, want child=%v status=%v", plan[0].resolved, childID, tool.DelegateStatusUnknown)
	}
}
