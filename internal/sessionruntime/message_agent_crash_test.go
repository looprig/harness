package sessionruntime

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestMessageAgentCrashRecoveryCoversEveryDurableDeliveryPoint(t *testing.T) {
	t.Parallel()
	type crashPoint struct {
		name             string
		fallback         bool
		events           func(sessionID, parentID, childID, requestID, turnID uuid.UUID) []event.Event
		wantRepair       bool
		wantReAdmitPhase command.DelegateDeliveryPhase
		wantDelivery     tool.DelegateDeliveryStatus
		wantResolved     bool
	}
	points := []crashPoint{
		{
			name:             "before steer reservation",
			wantReAdmitPhase: command.DelegateDeliveryPhaseIntent,
		},
		{
			name: "after reservation before provider write",
			events: func(s, _, child, request, _ uuid.UUID) []event.Event {
				return []event.Event{deliveryReservationEvent(s, request, child)}
			},
			wantRepair:   true,
			wantDelivery: tool.DelegateDeliveryUnknown,
		},
		{
			name: "after provider write before acknowledgement",
			events: func(s, _, child, request, _ uuid.UUID) []event.Event {
				return []event.Event{deliveryReservationEvent(s, request, child)}
			},
			wantRepair:   true,
			wantDelivery: tool.DelegateDeliveryUnknown,
		},
		{
			name: "after acknowledgement before checked fold",
			events: func(s, _, child, request, _ uuid.UUID) []event.Event {
				return []event.Event{deliveryReservationEvent(s, request, child)}
			},
			wantRepair:   true,
			wantDelivery: tool.DelegateDeliveryUnknown,
		},
		{
			name: "after checked fold before terminal",
			events: func(s, parent, child, request, turn uuid.UUID) []event.Event {
				all := deliveryTurnEvents(s, parent, child, request, turn)
				return append([]event.Event{all[0], deliveryReservationEvent(s, request, child)}, all[1:3]...)
			},
		},
		{
			name:     "after durable fallback enqueue",
			fallback: true,
			events: func(s, _, child, request, _ uuid.UUID) []event.Event {
				return []event.Event{deliveryReservationEvent(s, request, child)}
			},
			wantReAdmitPhase: command.DelegateDeliveryPhaseFallbackQueued,
		},
		{
			name: "after durable unknown resolution",
			events: func(s, _, child, request, _ uuid.UUID) []event.Event {
				return []event.Event{deliveryReservationEvent(s, request, child), deliveryResolutionEvent(s, request, child, event.DelegateDeliveryResolvedUnknown)}
			},
			wantDelivery: tool.DelegateDeliveryUnknown,
		},
		{
			name: "after durable untrackable resolution",
			events: func(s, _, child, request, _ uuid.UUID) []event.Event {
				return []event.Event{deliveryReservationEvent(s, request, child), deliveryResolutionEvent(s, request, child, event.DelegateDeliveryResolvedUntrackable)}
			},
			wantDelivery: tool.DelegateDeliveryUntrackable,
		},
		{
			name: "after checked fold and terminal",
			events: func(s, parent, child, request, turn uuid.UUID) []event.Event {
				all := deliveryTurnEvents(s, parent, child, request, turn)
				accepted := event.DelegateRequestAccepted{Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: s, LoopID: child},
					Cause:       identity.Cause{CommandID: request},
				}}
				return append([]event.Event{all[0], deliveryReservationEvent(s, request, child), accepted}, all[1:]...)
			},
			wantResolved: true,
		},
	}
	for _, point := range points {
		point := point
		t.Run(point.name, func(t *testing.T) {
			sessionID, parentID, childID, requestID, turnID := mustUUID(), mustUUID(), mustUUID(), mustUUID(), mustUUID()
			base := phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)
			records := []journal.JournalRecord{journal.NewCommandRecord(sessionID, childID, base)}
			if point.fallback {
				fallback := base
				fallback.DelegateDeliveryPhase = command.DelegateDeliveryPhaseFallbackQueued
				records = append(records, journal.NewCommandRecord(sessionID, childID, fallback))
			}
			replayed := []event.Event{event.LoopStarted{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
				Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
			}, DisplayName: "worker"}}
			if point.events != nil {
				replayed = append(replayed, point.events(sessionID, parentID, childID, requestID, turnID)...)
			}

			repairs := &messageAgentCrashJournal{}
			factory := event.NewFactory(uuid.New, time.Now)
			gotRepairs, err := persistUnresolvedDelegateDeliveryStates(context.Background(), repairs, factory, sessionID, records, replayed, nil)
			if err != nil {
				t.Fatalf("persist crash repair: %v", err)
			}
			if (len(gotRepairs) != 0) != point.wantRepair || (len(repairs.records) != 0) != point.wantRepair {
				t.Fatalf("repair records = %d/%d, want repair=%v", len(gotRepairs), len(repairs.records), point.wantRepair)
			}
			allEvents := append(append([]event.Event(nil), replayed...), gotRepairs...)
			manager := newDelegationManager(Topology{})
			if err := seedResolvedDelegateRecords(manager, records, allEvents, nil); err != nil {
				t.Fatalf("seed crash state: %v", err)
			}
			resolved, hasResolved := durableResolvedRecord(manager, requestID)
			if hasResolved != point.wantResolved && point.wantDelivery == "" {
				t.Fatalf("resolved state present=%v, want %v (%+v)", hasResolved, point.wantResolved, resolved)
			}
			if point.wantDelivery != "" {
				if !hasResolved || resolved.status != tool.DelegateStatusUnknown {
					if point.wantDelivery == tool.DelegateDeliveryUnknown && !hasResolved {
						t.Fatalf("unknown repair did not close request: %+v/%v", resolved, hasResolved)
					}
				}
			}

			session := &Session{sessionID: sessionID, loops: map[uuid.UUID]*loopHandle{
				childID: {id: childID, parent: loop.Provenance{LoopID: parentID}, agentName: "worker"},
			}}
			plan, err := manager.planRestoredBackgroundRequests(session, records, allEvents, nil)
			if err != nil {
				t.Fatalf("plan crash recovery: %v", err)
			}
			if point.wantReAdmitPhase != "" {
				if len(plan) != 1 || plan[0].reAdmit == nil || plan[0].reAdmit.CommandID != requestID || plan[0].reAdmit.DelegateDeliveryPhase != point.wantReAdmitPhase {
					t.Fatalf("re-admission plan = %+v, want phase %q", plan, point.wantReAdmitPhase)
				}
			} else if point.wantDelivery != "" {
				if len(plan) != 1 || plan[0].reAdmit != nil || plan[0].deliveryStatus != point.wantDelivery {
					t.Fatalf("categorical recovery plan = %+v, want %q", plan, point.wantDelivery)
				}
			} else if point.wantResolved {
				if len(plan) != 1 || plan[0].reAdmit != nil || plan[0].resolved.status != tool.DelegateStatusCompleted {
					t.Fatalf("terminal recovery plan = %+v, want completed terminal", plan)
				}
			} else if len(plan) != 0 {
				t.Fatalf("open-turn recovery plan = %+v, want no duplicate admission", plan)
			}
		})
	}
}

type messageAgentCrashJournal struct {
	records []journal.JournalRecord
}

func (j *messageAgentCrashJournal) Append(_ context.Context, record journal.JournalRecord) (uint64, error) {
	j.records = append(j.records, record)
	return uint64(len(j.records)), nil
}

func TestMessageAgentCrashRestoreRotatesCapabilityAndRevokesPriorToken(t *testing.T) {
	t.Parallel()
	loopID := mustUUID()
	controller := &recordingDelegateController{}
	firstSession := &Session{sessionID: mustUUID(), sessionCtx: context.Background(), foreignDeliveryHooks: make(map[uuid.UUID]*foreignDeliveryHook)}
	firstBroker, _ := newInMemoryCollabBroker(t, mustUUID(), controller, bytes.Repeat([]byte{0x11}, collabCapabilityBytes))
	firstSession.collabBroker = firstBroker
	firstServices, firstHook := firstSession.foreignServicesForTrackedWithController(loopID, controller)
	firstToken := firstServices.Broker.Capability()
	if len(firstToken) != collabCapabilityBytes || !firstBroker.HasRawCapability(firstToken) {
		t.Fatalf("first restore capability = %d bytes/authenticated=%v, want fresh authenticated token", len(firstToken), firstBroker.HasRawCapability(firstToken))
	}
	if err := firstBroker.Revoke(loopID); err != nil {
		t.Fatalf("revoke first capability: %v", err)
	}
	if firstBroker.HasRawCapability(firstToken) {
		t.Fatal("pre-restore capability remained valid after origin revocation")
	}
	firstSession.unregisterForeignDeliveryHook(firstHook)

	secondSession := &Session{sessionID: mustUUID(), sessionCtx: context.Background(), foreignDeliveryHooks: make(map[uuid.UUID]*foreignDeliveryHook)}
	secondBroker, _ := newInMemoryCollabBroker(t, mustUUID(), controller, bytes.Repeat([]byte{0x22}, collabCapabilityBytes))
	secondSession.collabBroker = secondBroker
	secondServices, _ := secondSession.foreignServicesForTrackedWithController(loopID, controller)
	secondToken := secondServices.Broker.Capability()
	if len(secondToken) != collabCapabilityBytes || !secondBroker.HasRawCapability(secondToken) {
		t.Fatalf("restored capability = %d bytes/authenticated=%v, want fresh authenticated token", len(secondToken), secondBroker.HasRawCapability(secondToken))
	}
	if bytes.Equal(firstToken, secondToken) {
		t.Fatal("restore replayed the prior origin capability")
	}
	if secondBroker.HasRawCapability(firstToken) {
		t.Fatal("restored broker accepted capability minted by prior session")
	}
}
