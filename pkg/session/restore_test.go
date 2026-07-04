package session

import (
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/core/uuid"
)

// foldUserMsg builds a *content.UserMessage carrying a single text block, the
// committed form the loop appends for TurnStarted/TurnFoldedInto. (aiMessage is
// shared from drain_test.go in the same package.)
func foldUserMsg(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// foldToolResult builds a *content.ToolResultMessage, the tool half of a step
// group (one AIMessage may be followed by zero or more of these).
func foldToolResult(id, text string) *content.ToolResultMessage {
	return &content.ToolResultMessage{
		Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: text}}},
		ToolUseID: id,
	}
}

// foldStepGroup builds a StepDone whose Messages is the finalized step group the
// loop commits: the single AIMessage followed by its ToolResultMessages, in
// order. (A bare-AIMessage StepDone differs from drain_test.go's stepDone only in
// not stamping a TurnID, which the fold ignores.)
func foldStepGroup(group ...content.Conversation) event.StepDone {
	return event.StepDone{Messages: content.AgenticMessages(group)}
}

func TestFoldPrimaryLoop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		events       []event.Event
		wantMsgs     content.AgenticMessages
		wantTurnIdx  event.TurnIndex
		wantOpenTurn bool
	}{
		{
			name:         "empty sequence yields empty msgs",
			events:       nil,
			wantMsgs:     content.AgenticMessages{},
			wantTurnIdx:  0,
			wantOpenTurn: false,
		},
		{
			name: "single turn: user + one step group + TurnDone",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("hello")},
				foldStepGroup(aiMessage("hi there")),
				event.TurnDone{Message: aiMessage("hi there")},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("hello"),
				aiMessage("hi there"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: false,
		},
		{
			name: "single turn with tool step: user + (AI + tool results) + final AI + TurnDone",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("use a tool")},
				foldStepGroup(aiMessage("calling tool"), foldToolResult("t1", "result a"), foldToolResult("t2", "result b")),
				foldStepGroup(aiMessage("done")),
				event.TurnDone{Message: aiMessage("done")},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("use a tool"),
				aiMessage("calling tool"),
				foldToolResult("t1", "result a"),
				foldToolResult("t2", "result b"),
				aiMessage("done"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: false,
		},
		{
			name: "multi-turn: two complete turns in order",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("first")},
				foldStepGroup(aiMessage("answer one")),
				event.TurnDone{Message: aiMessage("answer one")},
				event.TurnStarted{Message: foldUserMsg("second")},
				foldStepGroup(aiMessage("answer two")),
				event.TurnDone{Message: aiMessage("answer two")},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("first"),
				aiMessage("answer one"),
				foldUserMsg("second"),
				aiMessage("answer two"),
			},
			wantTurnIdx:  2,
			wantOpenTurn: false,
		},
		{
			name: "TurnFoldedInto lands the folded user message at the fold point",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("start work")},
				foldStepGroup(aiMessage("calling tool"), foldToolResult("t1", "tool out")),
				event.TurnFoldedInto{Message: foldUserMsg("folded follow-up")},
				foldStepGroup(aiMessage("final answer")),
				event.TurnDone{Message: aiMessage("final answer")},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("start work"),
				aiMessage("calling tool"),
				foldToolResult("t1", "tool out"),
				foldUserMsg("folded follow-up"),
				aiMessage("final answer"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: false,
		},
		{
			name: "TurnFailed terminal is a no-op for msgs (committed steps stay)",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("try")},
				foldStepGroup(aiMessage("partial committed step")),
				event.TurnFailed{},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("try"),
				aiMessage("partial committed step"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: false,
		},
		{
			name: "TurnInterrupted terminal is a no-op for msgs",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("try")},
				foldStepGroup(aiMessage("committed before interrupt")),
				event.TurnInterrupted{},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("try"),
				aiMessage("committed before interrupt"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: false,
		},
		{
			name: "lifecycle events do not contribute to msgs",
			events: []event.Event{
				event.LoopStarted{},
				event.SessionStarted{},
				event.RestoreStarted{},
				event.TurnStarted{Message: foldUserMsg("hello")},
				event.InputQueued{},
				foldStepGroup(aiMessage("hi")),
				event.LoopIdle{},
				event.TurnDone{Message: aiMessage("hi")},
				event.SessionIdle{},
				event.RestoreDone{},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("hello"),
				aiMessage("hi"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: false,
		},
		{
			name: "InputCancelled does not contribute to msgs",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("hello")},
				foldStepGroup(aiMessage("hi")),
				event.TurnDone{Message: aiMessage("hi")},
				event.InputCancelled{Message: foldUserMsg("retracted")},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("hello"),
				aiMessage("hi"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: false,
		},
		// --- Task 8.2: open-turn (crash-seam) detection + interrupted-turn contract ---
		{
			name: "open turn: TurnStarted + completed steps, NO terminal -> open, no partial",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("first")},
				foldStepGroup(aiMessage("answer one")),
				event.TurnDone{Message: aiMessage("answer one")},
				event.TurnStarted{Message: foldUserMsg("crashed mid-turn")},
				foldStepGroup(aiMessage("calling tool"), foldToolResult("t1", "tool out")),
				// no terminal: the loop crashed after committing this step but before
				// finishing the in-flight (uncommitted, no StepDone) next step.
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("first"),
				aiMessage("answer one"),
				foldUserMsg("crashed mid-turn"),
				aiMessage("calling tool"),
				foldToolResult("t1", "tool out"),
				// NO partial assistant step — the in-flight step never emitted StepDone.
			},
			wantTurnIdx:  2,
			wantOpenTurn: true,
		},
		{
			name: "open turn: TurnStarted with zero StepDones -> open, msgs = just the user message",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("only the user message committed")},
				// crash before the first step committed.
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("only the user message committed"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: true,
		},
		{
			name: "cleanly-ended history (…TurnDone) is not open",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("hello")},
				foldStepGroup(aiMessage("hi")),
				event.TurnDone{Message: aiMessage("hi")},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("hello"),
				aiMessage("hi"),
			},
			wantTurnIdx:  1,
			wantOpenTurn: false,
		},
		{
			name: "open turn after a clean turn then a bare TurnStarted -> open",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("done turn")},
				foldStepGroup(aiMessage("answered")),
				event.TurnInterrupted{},
				event.TurnStarted{Message: foldUserMsg("reopened")},
			},
			wantMsgs: content.AgenticMessages{
				foldUserMsg("done turn"),
				aiMessage("answered"),
				foldUserMsg("reopened"),
			},
			wantTurnIdx:  2,
			wantOpenTurn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := foldPrimaryLoop(tt.events)
			if !reflect.DeepEqual(got.Msgs, tt.wantMsgs) {
				t.Errorf("foldPrimaryLoop() msgs =\n  %#v\nwant\n  %#v", got.Msgs, tt.wantMsgs)
			}
			if got.TurnIndex != tt.wantTurnIdx {
				t.Errorf("foldPrimaryLoop() turnIndex = %d, want %d", got.TurnIndex, tt.wantTurnIdx)
			}
			if got.OpenTurn != tt.wantOpenTurn {
				t.Errorf("foldPrimaryLoop() openTurn = %v, want %v", got.OpenTurn, tt.wantOpenTurn)
			}
		})
	}
}

// turnStartedWithID builds a TurnStarted carrying a specific TurnID + TurnIndex, so
// openTurnCoords' return can be matched against the exact open turn.
func turnStartedWithID(turnID uuid.UUID, idx event.TurnIndex, user string) event.TurnStarted {
	return event.TurnStarted{
		Header:    event.Header{Coordinates: identity.Coordinates{TurnID: turnID}},
		TurnIndex: idx,
		Message:   foldUserMsg(user),
	}
}

// TestOpenTurnCoordsCoupledToOpenTurnInvariant locks the invariant openTurnCoords
// relies on: whenever foldPrimaryLoop reports OpenTurn (a TurnStarted with no later
// terminal), a trailing TurnStarted exists, so openTurnCoords returns that turn's
// (TurnID, TurnIndex) — never its zero fall-through. The two stay coupled: the restore
// constructor calls openTurnCoords ONLY under folded.OpenTurn, so the zero return is
// unreachable in production, and this test fails if a future change ever lets an
// OpenTurn fold yield a zero coordinate (a half-open turn that could not be closed).
func TestOpenTurnCoordsCoupledToOpenTurnInvariant(t *testing.T) {
	t.Parallel()

	open := uuid.UUID{0x07}
	prior := uuid.UUID{0x06}

	tests := []struct {
		name        string
		events      []event.Event
		wantTurnID  uuid.UUID
		wantTurnIdx event.TurnIndex
	}{
		{
			name: "open turn after a clean turn: coords are the LAST (open) TurnStarted",
			events: []event.Event{
				turnStartedWithID(prior, 1, "done turn"),
				foldStepGroup(aiMessage("answered")),
				event.TurnDone{Message: aiMessage("answered")},
				turnStartedWithID(open, 2, "reopened"),
			},
			wantTurnID:  open,
			wantTurnIdx: 2,
		},
		{
			name: "open turn with completed steps and a mid-turn fold: coords are that turn",
			events: []event.Event{
				turnStartedWithID(open, 1, "start work"),
				foldStepGroup(aiMessage("calling tool"), foldToolResult("t1", "out")),
				event.TurnFoldedInto{Message: foldUserMsg("folded follow-up")},
				foldStepGroup(aiMessage("more")),
				// no terminal: crashed mid-turn.
			},
			wantTurnID:  open,
			wantTurnIdx: 1,
		},
		{
			name: "bare open turn (no steps): coords are that turn",
			events: []event.Event{
				turnStartedWithID(open, 3, "only user committed"),
			},
			wantTurnID:  open,
			wantTurnIdx: 3,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Precondition: these sequences are exactly the ones openTurnCoords is called
			// on — the fold reports an open turn.
			if folded := foldPrimaryLoop(tt.events); !folded.OpenTurn {
				t.Fatalf("test setup: fold did not report OpenTurn for %q", tt.name)
			}

			gotID, gotIdx := openTurnCoords(tt.events)
			if gotID == (uuid.UUID{}) {
				t.Fatalf("openTurnCoords returned the ZERO TurnID on an OpenTurn fold (the dead fall-through fired)")
			}
			if gotID != tt.wantTurnID {
				t.Errorf("openTurnCoords TurnID = %v, want %v", gotID, tt.wantTurnID)
			}
			if gotIdx != tt.wantTurnIdx {
				t.Errorf("openTurnCoords TurnIndex = %d, want %d", gotIdx, tt.wantTurnIdx)
			}
		})
	}
}
