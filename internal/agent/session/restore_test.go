package session

import (
	"reflect"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
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
		name        string
		events      []event.Event
		wantMsgs    content.AgenticMessages
		wantTurnIdx event.TurnIndex
	}{
		{
			name:        "empty sequence yields empty msgs",
			events:      nil,
			wantMsgs:    content.AgenticMessages{},
			wantTurnIdx: 0,
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
			wantTurnIdx: 1,
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
			wantTurnIdx: 1,
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
			wantTurnIdx: 2,
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
			wantTurnIdx: 1,
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
			wantTurnIdx: 1,
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
			wantTurnIdx: 1,
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
			wantTurnIdx: 1,
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
			wantTurnIdx: 1,
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
		})
	}
}
