package sessionwire

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

// The runtime scope exists because a Core SessionID is OPAQUE and Factory binds the
// rig to a DERIVED id (factory/internal/admission/create.go), so the Harness event's
// SessionID is, in production, never the rendering of the Core id. Before
// ReadScope.RuntimeSessionID a Host could project no gate and no journal event at all.

const opaqueCoreSession coresessionwire.SessionID = "tenant-a/session/opaque-1"

func runtimeScopedGate(sessionID uuid.UUID) OpenGate {
	return OpenGate{
		Event: event.GateOpened{
			Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: catalogUUID(0xB2), TurnID: catalogUUID(0xB3), StepID: catalogUUID(0xB4)},
				EventID:     catalogUUID(0xB5), CreatedAt: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC),
			},
			Gate: gate.Gate{
				ID: gate.ID(catalogUUID(0xB6)), Kind: gate.KindPermission, Resolver: gate.ResolverLoop,
				Prompt: gate.Prompt{Title: "Permission", Body: "approve?", Controls: gate.ApprovalControls()},
			},
		},
		JournalSeq: 3, Deadline: time.Date(2026, 9, 19, 11, 0, 0, 0, time.UTC), Answerability: coresessionwire.GateAnswerabilityResident,
	}
}

func runtimeScopedRecord(sessionID uuid.UUID) JournalRecord {
	return JournalRecord{JournalSeq: 2, Event: event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sessionID},
		EventID:     catalogUUID(0xB7), CreatedAt: time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC),
	}}}
}

func scopeWith(core coresessionwire.SessionID, runtime uuid.UUID) ReadScope {
	return ReadScope{
		TenantID: "tenant-a", SessionID: core, AgentID: "agent-primary",
		Residency: coresessionwire.SessionResidencyResident, RuntimeSessionID: runtime,
	}
}

// TestRuntimeSessionIDScopesEventsWhenSet is the two-arm table for BOTH projections
// that check an event's session. The row that matters most is "core id rendering but
// another runtime": when the runtime scope is set it is the ONLY comparison, so an
// event matching the Core id's rendering is still refused.
func TestRuntimeSessionIDScopesEventsWhenSet(t *testing.T) {
	t.Parallel()
	rig := catalogUUID(0xB1)
	other := catalogUUID(0xB8)
	coreRendering := coresessionwire.SessionID(other.String())
	for _, row := range []struct {
		name    string
		scope   ReadScope
		eventID uuid.UUID
		ok      bool
	}{
		{name: "set: event under the runtime id, opaque core id", scope: scopeWith(opaqueCoreSession, rig), eventID: rig, ok: true},
		{name: "set: event under another runtime id", scope: scopeWith(opaqueCoreSession, rig), eventID: other, ok: false},
		{name: "set: event matches the core id's rendering, not the runtime id", scope: scopeWith(coreRendering, rig), eventID: other, ok: false},
		{name: "set: zero event session", scope: scopeWith(opaqueCoreSession, rig), eventID: uuid.UUID{}, ok: false},
		{name: "unset: event matches the core id's rendering", scope: scopeWith(coreRendering, uuid.UUID{}), eventID: other, ok: true},
		{name: "unset: opaque core id matches no event", scope: scopeWith(opaqueCoreSession, uuid.UUID{}), eventID: rig, ok: false},
		{name: "unset: event under another session", scope: scopeWith(coreRendering, uuid.UUID{}), eventID: rig, ok: false},
		// The up-front zero guard is the ONLY thing that refuses this row as session_id:
		// the zero UUID renders as a valid Core id, so without the guard the event
		// would pass the scope check and be refused later as Field "event".
		{name: "unset: zero event session under the zero UUID's rendering", scope: scopeWith(coresessionwire.SessionID(uuid.UUID{}.String()), uuid.UUID{}), eventID: uuid.UUID{}, ok: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			projections := map[string]func() error{
				"gate page": func() error {
					_, err := ProjectGatePage(row.scope, []OpenGate{runtimeScopedGate(row.eventID)}, 3, 1, "", "")
					return err
				},
				"journal page": func() error {
					_, err := ProjectJournalPage(row.scope, []JournalRecord{runtimeScopedRecord(row.eventID)}, 2, 2, "", "")
					return err
				},
			}
			for name, project := range projections {
				err := project()
				if row.ok {
					if err != nil {
						t.Errorf("%s: error = %v, want the event projected", name, err)
					}
					continue
				}
				var projectionErr *ReadProjectionError
				if !errors.As(err, &projectionErr) || projectionErr.Field != "session_id" {
					t.Errorf("%s: error = %v, want a session_id *ReadProjectionError", name, err)
				}
			}
		})
	}
}

// TestRuntimeSessionIDChangesOnlyTheScopeCheck proves the field is additive in the
// strict sense: for events a caller could already project, setting it to the id the
// events carry yields byte-identical pages. It decides which events are in scope and
// nothing about what a projected page contains.
func TestRuntimeSessionIDChangesOnlyTheScopeCheck(t *testing.T) {
	t.Parallel()
	rig := catalogUUID(0xB1)
	core := coresessionwire.SessionID(rig.String())
	unset, set := scopeWith(core, uuid.UUID{}), scopeWith(core, rig)

	gatesUnset, err := ProjectGatePage(unset, []OpenGate{runtimeScopedGate(rig)}, 3, 1, "", "")
	if err != nil {
		t.Fatalf("ProjectGatePage(unset): %v", err)
	}
	gatesSet, err := ProjectGatePage(set, []OpenGate{runtimeScopedGate(rig)}, 3, 1, "", "")
	if err != nil {
		t.Fatalf("ProjectGatePage(set): %v", err)
	}
	journalUnset, err := ProjectJournalPage(unset, []JournalRecord{runtimeScopedRecord(rig)}, 2, 2, "", "")
	if err != nil {
		t.Fatalf("ProjectJournalPage(unset): %v", err)
	}
	journalSet, err := ProjectJournalPage(set, []JournalRecord{runtimeScopedRecord(rig)}, 2, 2, "", "")
	if err != nil {
		t.Fatalf("ProjectJournalPage(set): %v", err)
	}
	for name, pair := range map[string][2]any{"gate page": {gatesUnset, gatesSet}, "journal page": {journalUnset, journalSet}} {
		a, errA := json.Marshal(pair[0])
		b, errB := json.Marshal(pair[1])
		if errA != nil || errB != nil {
			t.Fatalf("%s: marshal: %v / %v", name, errA, errB)
		}
		if string(a) != string(b) {
			t.Errorf("%s differs with the runtime scope set:\nunset: %s\nset:   %s", name, a, b)
		}
	}
}
