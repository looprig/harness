package sessionruntime

import (
	"encoding/json"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

type parkedFixture struct {
	loopID, turnID, stepID uuid.UUID
	plan                   restoredGatePlan
	folded                 foldResult
}

func parkedStepMessage(ids ...string) *content.AIMessage {
	blocks := make([]content.Block, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, &content.ToolUseBlock{ID: id, Name: "T", Input: json.RawMessage(`{}`)})
	}
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}
}

func newParkedFixture(t *testing.T) *parkedFixture {
	t.Helper()
	f := &parkedFixture{loopID: mustSessionID(t), turnID: mustSessionID(t), stepID: mustSessionID(t)}
	f.plan = restoredGatePlan{open: map[gate.ID]gateEntry{}, resume: map[gate.ID]*event.ToolStepResume{}, opened: map[gate.ID]event.GateOpened{}}
	f.folded = foldResult{OpenTurn: true, OpenTurnStart: 0, OpenTurnID: f.turnID, OpenTurnCause: identity.Cause{CommandID: mustSessionID(t)}}
	return f
}

// addGate installs an open gate of kind owned by loopID in (turnID, stepID); a nil
// message stores no resume snapshot.
func (f *parkedFixture) addGate(t *testing.T, kind gate.Kind, loopID, turnID, stepID uuid.UUID, message *content.AIMessage, toolUseID string, index uint64) gate.ID {
	t.Helper()
	id := gate.ID(mustSessionID(t))
	g := gate.Gate{
		ID: id, Kind: kind, Resolver: gate.ResolverLoop, Restorable: true,
		Subject: gate.Subject{ToolExecutionID: gate.ID(mustSessionID(t)), TurnID: gate.ID(turnID), StepID: gate.ID(stepID)},
	}
	var payload gate.Payload = gate.AskUserPayload{Question: "q"}
	if kind == gate.KindPermission {
		payload = gate.PermissionPayload{Request: tool.Request{ToolName: "T", Summary: "s"}}
	}
	f.plan.open[id] = gateEntry{gate: g, route: gate.Route{GateID: id, LoopID: loopID}, payload: payload}
	f.plan.opened[id] = event.GateOpened{Gate: g}
	f.plan.order = append(f.plan.order, id)
	if message != nil {
		f.plan.resume[id] = &event.ToolStepResume{StepIndex: index, Message: message, ToolUseID: toolUseID}
	}
	return id
}

func TestPlanParkedStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		setup  func(t *testing.T, f *parkedFixture) (native bool, events []event.Event)
		wantOK bool
		gates  int
	}{
		{name: "one resumable ask_user gate", wantOK: true, gates: 1, setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a"), "a", 1)
			return true, nil
		}},
		{name: "two ask_user gates of one step", wantOK: true, gates: 2, setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			msg := parkedStepMessage("a", "b")
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, msg, "a", 1)
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a", "b"), "b", 1)
			return true, nil
		}},
		{name: "a permission gate carries its displayed request", wantOK: true, gates: 1, setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindPermission, f.loopID, f.turnID, f.stepID, parkedStepMessage("a"), "a", 0)
			return true, nil
		}},
		{name: "no open turn", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a"), "a", 1)
			f.folded.OpenTurn = false
			return true, nil
		}},
		{name: "compaction inside the open turn", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a"), "a", 1)
			f.folded.OpenTurnStart = -1
			return true, nil
		}},
		{name: "foreign engine", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a"), "a", 1)
			return false, nil
		}},
		{name: "a legacy gate without a snapshot", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindPermission, f.loopID, f.turnID, f.stepID, nil, "", 0)
			return true, nil
		}},
		{name: "one resumable and one legacy gate", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a"), "a", 1)
			f.addGate(t, gate.KindPermission, f.loopID, f.turnID, f.stepID, nil, "", 1)
			return true, nil
		}},
		{name: "gate of another turn", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, mustSessionID(t), f.stepID, parkedStepMessage("a"), "a", 1)
			return true, nil
		}},
		{name: "gates of two steps", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a", "b"), "a", 1)
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, mustSessionID(t), parkedStepMessage("a", "b"), "b", 1)
			return true, nil
		}},
		{name: "snapshots disagree on the message", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a", "b"), "a", 1)
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("b"), "b", 1)
			return true, nil
		}},
		{name: "the step already committed", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a"), "a", 1)
			return true, []event.Event{event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{StepID: f.stepID}}}}
		}},
		{name: "only another loop's gates", setup: func(t *testing.T, f *parkedFixture) (bool, []event.Event) {
			f.addGate(t, gate.KindAskUser, mustSessionID(t), f.turnID, f.stepID, parkedStepMessage("a"), "a", 1)
			return true, nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newParkedFixture(t)
			native, events := tt.setup(t, f)
			got := planParkedStep(f.plan, f.loopID, native, f.folded, events)
			if (got != nil) != tt.wantOK {
				t.Fatalf("planParkedStep() = %+v, want resumable=%v", got, tt.wantOK)
			}
			if got == nil {
				return
			}
			if len(got.Gates) != tt.gates || got.TurnID != f.turnID || got.StepID != f.stepID || got.Cause != f.folded.OpenTurnCause {
				t.Fatalf("parked step = %+v", got)
			}
			for _, g := range got.Gates {
				if (g.Kind == gate.KindPermission) != (g.PermissionRequest != nil) {
					t.Fatalf("gate %v PermissionRequest = %v for kind %s", g.GateID, g.PermissionRequest, g.Kind)
				}
			}
		})
	}
}

// TestCloseUnresumedAskUserGates: an ask_user gate is answerable only through the
// call that asked it, so one not being resumed is closed; a permission gate keeps
// its pre-resume behaviour and stays open.
func TestCloseUnresumedAskUserGates(t *testing.T) {
	t.Parallel()
	f := newParkedFixture(t)
	resumed := f.addGate(t, gate.KindAskUser, f.loopID, f.turnID, f.stepID, parkedStepMessage("a"), "a", 1)
	other := f.addGate(t, gate.KindAskUser, mustSessionID(t), mustSessionID(t), f.stepID, parkedStepMessage("b"), "b", 1)
	permission := f.addGate(t, gate.KindPermission, mustSessionID(t), mustSessionID(t), f.stepID, nil, "", 0)
	parked := planParkedStep(f.plan, f.loopID, true, f.folded, nil)
	if parked == nil {
		t.Fatal("expected the active loop's gate to be resumable")
	}
	closeUnresumedAskUserGates(&f.plan, parked)
	if _, open := f.plan.open[resumed]; !open {
		t.Fatal("the resumed ask_user gate was closed")
	}
	if _, open := f.plan.open[other]; open {
		t.Fatal("an unresumed ask_user gate stayed open")
	}
	if _, open := f.plan.open[permission]; !open {
		t.Fatal("a permission gate was closed")
	}
	if len(f.plan.unavailable) != 1 || f.plan.unavailable[0].Gate.ID != other {
		t.Fatalf("unavailable = %+v, want exactly the unresumed ask_user gate", f.plan.unavailable)
	}
	closeUnresumedAskUserGates(&f.plan, nil)
	if _, open := f.plan.open[resumed]; open {
		t.Fatal("with nothing resumed, an ask_user gate stayed open")
	}
}
