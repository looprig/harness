package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
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

func TestRestoreTerminalDeliveryPlansCategoricalHandbackAtEveryCrashPoint(t *testing.T) {
	t.Parallel()
	states := []struct {
		name        string
		eventState  event.DelegateDeliveryState
		disposition tool.DelegateDeliveryStatus
	}{
		{name: "unknown", eventState: event.DelegateDeliveryResolvedUnknown, disposition: tool.DelegateDeliveryUnknown},
		{name: "untrackable", eventState: event.DelegateDeliveryResolvedUntrackable, disposition: tool.DelegateDeliveryUntrackable},
	}
	crashPoints := []struct {
		name      string
		processed string
	}{
		{name: "no handback"},
		{name: "durable handback"},
		{name: "processed turn started", processed: "started"},
		{name: "processed turn folded", processed: "folded"},
	}
	for _, state := range states {
		state := state
		t.Run(state.name, func(t *testing.T) {
			t.Parallel()
			for _, crashPoint := range crashPoints {
				crashPoint := crashPoint
				t.Run(crashPoint.name, func(t *testing.T) {
					t.Parallel()
					sessionID, parentID, childID, requestID := mustUUID(), mustUUID(), mustUUID(), mustUUID()
					handBackID := mustUUID()
					records := []journal.JournalRecord{
						journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
					}
					replayed := []event.Event{
						event.LoopStarted{
							Header: event.Header{
								Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
								EventID:     mustUUID(),
								Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
							},
							DisplayName: "worker",
						},
						deliveryReservationEvent(sessionID, requestID, childID),
						deliveryResolutionEvent(sessionID, requestID, childID, state.eventState),
					}
					if crashPoint.processed != "" || crashPoint.name == "durable handback" {
						handBack := command.SubagentResult{
							Coordinates: identity.Coordinates{LoopID: parentID},
							Header: command.Header{
								CommandID: handBackID,
								Cause:     identity.Cause{Coordinates: identity.Coordinates{LoopID: childID}},
							},
							Blocks: backgroundCompletionBlocksWithState(childID, "worker", requestID, tool.DelegateStatusUnknown, "", state.disposition, true),
						}
						records = append(records, journal.NewCommandRecord(sessionID, parentID, handBack))
					}
					if crashPoint.processed != "" {
						message := &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: handBackBlocks(childID, "worker", requestID, state.disposition)}}
						header := event.Header{
							Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: parentID, TurnID: mustUUID()},
							EventID:     mustUUID(),
							Cause:       identity.Cause{CommandID: handBackID, Coordinates: identity.Coordinates{LoopID: childID}},
						}
						if crashPoint.processed == "started" {
							replayed = append(replayed, event.TurnStarted{Header: header, Message: message})
						} else {
							openingHeader := header
							openingHeader.EventID = mustUUID()
							replayed = append(replayed, event.TurnStarted{Header: openingHeader})
							replayed = append(replayed, event.TurnFoldedInto{Header: header, Message: message})
						}
					}

					manager := newDelegationManager(Topology{})
					if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
						t.Fatalf("seedResolvedDelegateRecords: %v", err)
					}
					plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
					if err != nil {
						t.Fatalf("planRestoredBackgroundRequests: %v", err)
					}
					if crashPoint.processed != "" {
						if len(plan) != 0 {
							t.Fatalf("processed %s restore plan = %+v, want no handback", crashPoint.processed, plan)
						}
						return
					}
					if len(plan) != 1 {
						t.Fatalf("%s restore plan = %+v, want one categorical handback", crashPoint.name, plan)
					}
					entry := plan[0]
					if entry.reAdmit != nil || entry.deliveryStatus != state.disposition {
						t.Fatalf("%s restore entry = %+v, want categorical %q without re-admission", crashPoint.name, entry, state.disposition)
					}
					if crashPoint.name == "no handback" {
						if entry.handBack != nil {
							t.Fatalf("no-handback restore entry = %+v, want a minted handback", entry)
						}
					} else if entry.handBack == nil || entry.handBack.CommandID != handBackID {
						t.Fatalf("durable handback restore entry = %+v, want command %v replay", entry.handBack, handBackID)
					}
				})
			}
		})
	}
}

func TestRestoreTerminalDeliveryDoesNotHandBackForegroundPhase(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		state event.DelegateDeliveryState
	}{
		{name: "unknown", state: event.DelegateDeliveryResolvedUnknown},
		{name: "untrackable", state: event.DelegateDeliveryResolvedUntrackable},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			sessionID, parentID, childID, requestID := mustUUID(), mustUUID(), mustUUID(), mustUUID()
			foreground := phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)
			foreground.BackgroundHandBack = false
			records := []journal.JournalRecord{journal.NewCommandRecord(sessionID, childID, foreground)}
			replayed := []event.Event{
				event.LoopStarted{
					Header: event.Header{
						Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
						EventID:     mustUUID(),
						Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
					},
					DisplayName: "worker",
				},
				deliveryReservationEvent(sessionID, requestID, childID),
				deliveryResolutionEvent(sessionID, requestID, childID, test.state),
			}
			manager := newDelegationManager(Topology{})
			if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
				t.Fatalf("seedResolvedDelegateRecords: %v", err)
			}
			plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
			if err != nil {
				t.Fatalf("planRestoredBackgroundRequests: %v", err)
			}
			if len(plan) != 0 {
				t.Fatalf("foreground %s restore plan = %+v, want no parent hand-back", test.name, plan)
			}
		})
	}
}

func handBackBlocks(childID uuid.UUID, name string, requestID uuid.UUID, disposition tool.DelegateDeliveryStatus) []content.Block {
	return backgroundCompletionBlocksWithState(childID, name, requestID, tool.DelegateStatusUnknown, "", disposition, true)
}

func TestRestoreTerminalDeliveryReconcilesExactlyOneCategoricalHandback(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		eventState  event.DelegateDeliveryState
		disposition tool.DelegateDeliveryStatus
	}{
		{name: "unknown", eventState: event.DelegateDeliveryResolvedUnknown, disposition: tool.DelegateDeliveryUnknown},
		{name: "untrackable", eventState: event.DelegateDeliveryResolvedUntrackable, disposition: tool.DelegateDeliveryUntrackable},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			sessionID, parentID, childID, requestID := mustUUID(), mustUUID(), mustUUID(), mustUUID()
			records := []journal.JournalRecord{
				journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
			}
			replayed := []event.Event{
				event.LoopStarted{
					Header: event.Header{
						Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
						EventID:     mustUUID(),
						Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
					},
					DisplayName: "worker",
				},
				deliveryReservationEvent(sessionID, requestID, childID),
				deliveryResolutionEvent(sessionID, requestID, childID, test.eventState),
			}
			manager := newDelegationManager(Topology{})
			if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
				t.Fatalf("seedResolvedDelegateRecords: %v", err)
			}
			plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
			if err != nil || len(plan) != 1 {
				t.Fatalf("restore plan = %+v, err=%v; want one categorical entry", plan, err)
			}
			parentBackend := &channelBackend{Commands: make(chan command.Command, 1), Done: make(chan struct{})}
			childBackend := &channelBackend{Commands: make(chan command.Command, 1), Done: make(chan struct{})}
			s := &Session{
				sessionCtx: context.Background(), sessionID: sessionID, hub: hub.New(sessionID),
				newID: uuid.New, now: time.Now,
				loops: map[uuid.UUID]*loopHandle{
					parentID: {id: parentID, backend: parentBackend},
					childID:  {id: childID, backend: childBackend, parent: loop.Provenance{LoopID: parentID}},
				},
			}
			manager.reconcileRestoredBackgroundRequests(s, plan)
			select {
			case raw := <-parentBackend.Commands:
				result, ok := raw.(command.SubagentResult)
				if !ok {
					t.Fatalf("restored command = %T, want SubagentResult", raw)
				}
				completion, ok := decodeBackgroundCompletion(result.Blocks)
				if !ok || completion.CorrelationID != requestID.String() || completion.State != tool.AgentStateWorking || completion.DeliveryStatus != test.disposition || completion.ResponseStatus != tool.DelegateResponseUnknown || completion.Response != "" {
					t.Fatalf("restored categorical completion = %+v, %v; want %q working/unknown empty", completion, ok, test.disposition)
				}
			default:
				t.Fatal("restore did not emit categorical handback")
			}
			select {
			case childCommand := <-childBackend.Commands:
				t.Fatalf("restore sent child command %T, want no re-admission", childCommand)
			default:
			}
		})
	}
}

func TestRestoreTerminalDeliveryReplaysUnprocessedHandbackCommand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		eventState  event.DelegateDeliveryState
		disposition tool.DelegateDeliveryStatus
	}{
		{name: "unknown", eventState: event.DelegateDeliveryResolvedUnknown, disposition: tool.DelegateDeliveryUnknown},
		{name: "untrackable", eventState: event.DelegateDeliveryResolvedUntrackable, disposition: tool.DelegateDeliveryUntrackable},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			sessionID, parentID, childID, requestID, handBackID := mustUUID(), mustUUID(), mustUUID(), mustUUID(), mustUUID()
			records := []journal.JournalRecord{
				journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
			}
			handBack := command.SubagentResult{
				Coordinates: identity.Coordinates{LoopID: parentID},
				Header: command.Header{
					CommandID: handBackID,
					Cause:     identity.Cause{Coordinates: identity.Coordinates{LoopID: childID}},
				},
				Blocks: handBackBlocks(childID, "worker", requestID, test.disposition),
			}
			records = append(records, journal.NewCommandRecord(sessionID, parentID, handBack))
			replayed := []event.Event{
				event.LoopStarted{
					Header: event.Header{
						Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
						EventID:     mustUUID(),
						Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
					},
					DisplayName: "worker",
				},
				deliveryReservationEvent(sessionID, requestID, childID),
				deliveryResolutionEvent(sessionID, requestID, childID, test.eventState),
			}
			manager := newDelegationManager(Topology{})
			if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
				t.Fatalf("seedResolvedDelegateRecords: %v", err)
			}
			plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
			if err != nil || len(plan) != 1 || plan[0].handBack == nil || plan[0].handBack.CommandID != handBackID {
				t.Fatalf("restore replay plan = %+v, err=%v; want exact handback %v", plan, err, handBackID)
			}
			parentBackend := &channelBackend{Commands: make(chan command.Command, 1), Done: make(chan struct{})}
			childBackend := &channelBackend{Commands: make(chan command.Command, 1), Done: make(chan struct{})}
			s := &Session{
				sessionCtx: context.Background(), sessionID: sessionID, hub: hub.New(sessionID),
				newID: uuid.New, now: time.Now,
				loops: map[uuid.UUID]*loopHandle{
					parentID: {id: parentID, backend: parentBackend},
					childID:  {id: childID, backend: childBackend, parent: loop.Provenance{LoopID: parentID}},
				},
			}
			manager.reconcileRestoredBackgroundRequests(s, plan)
			select {
			case raw := <-parentBackend.Commands:
				got, ok := raw.(command.SubagentResult)
				if !ok || got.CommandID != handBackID {
					t.Fatalf("replayed command = %T/%v, want SubagentResult/%v", raw, got.CommandID, handBackID)
				}
			case <-childBackend.Commands:
				t.Fatal("restore re-admitted child for terminal delivery")
			default:
				t.Fatal("restore did not replay durable handback")
			}
		})
	}
}

func TestRestoreDeliveryStateRequiresSessionRoute(t *testing.T) {
	t.Parallel()
	sessionID, childID, requestID := mustUUID(), mustUUID(), mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
	}
	wrongSession := deliveryReservationEvent(mustUUID(), requestID, childID)
	err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, []event.Event{wrongSession}, nil)
	var contradiction *delegateRestoreContradictionError
	if !errors.As(err, &contradiction) {
		t.Fatalf("wrong-session delivery state error = %T %v, want contradiction", err, err)
	}
}

func TestRestoreLifecycleEvidenceRequiresSessionRoute(t *testing.T) {
	t.Parallel()
	sessionID, wrongSession, childID, requestID, turnID := mustUUID(), mustUUID(), mustUUID(), mustUUID(), mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
	}
	coord := func(sid uuid.UUID) identity.Coordinates {
		return identity.Coordinates{SessionID: sid, LoopID: childID, TurnID: turnID}
	}
	started := event.TurnStarted{Header: event.Header{Coordinates: coord(sessionID), Cause: identity.Cause{CommandID: requestID}}}
	for _, tc := range []struct {
		name   string
		events func() []event.Event
	}{
		{name: "opening", events: func() []event.Event {
			return []event.Event{event.TurnStarted{Header: event.Header{Coordinates: coord(wrongSession), Cause: identity.Cause{CommandID: requestID}}}}
		}},
		{name: "fold", events: func() []event.Event {
			return []event.Event{started, event.TurnFoldedInto{Header: event.Header{Coordinates: coord(wrongSession), Cause: identity.Cause{CommandID: requestID}}}}
		}},
		{name: "rejected", events: func() []event.Event {
			return []event.Event{event.TurnRejected{Header: event.Header{Coordinates: identity.Coordinates{SessionID: wrongSession, LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.RejectQueueFull}}
		}},
		{name: "cancelled", events: func() []event.Event {
			return []event.Event{event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{SessionID: wrongSession, LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.CancelClientRetracted}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, tc.events(), nil); err == nil {
				t.Fatalf("wrong-session %s evidence was accepted", tc.name)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		terminal event.Event
	}{
		{name: "done", terminal: event.TurnDone{Header: event.Header{Coordinates: coord(wrongSession)}}},
		{name: "failed", terminal: event.TurnFailed{Header: event.Header{Coordinates: coord(wrongSession)}, Err: errors.New("failed")}},
		{name: "interrupted", terminal: event.TurnInterrupted{Header: event.Header{Coordinates: coord(wrongSession)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, []event.Event{started, tc.terminal}, nil); err == nil {
				t.Fatalf("wrong-session %s terminal was accepted", tc.name)
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
