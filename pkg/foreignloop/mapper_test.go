package foreignloop

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/core/uuid"
)

// wantTurn is the turn counter the mapper under test is constructed with; every
// turn-scoped body field the mapper stamps must echo it.
const wantTurn event.TurnIndex = 7

// errTestGen is the deterministic failure an idGen returns to exercise the
// fail-secure path on ForeignToolUse.
var errTestGen = errors.New("test idgen failure")

// seqIDGen returns a deterministic idGen producing distinct, non-zero UUIDs so a
// test can assert correlation by value equality without crypto/rand entropy.
func seqIDGen() func() (uuid.UUID, error) {
	var n byte
	return func() (uuid.UUID, error) {
		n++ // start at 1 so the first id is non-zero
		var u uuid.UUID
		u[15] = n
		return u, nil
	}
}

// errIDGen always fails, modelling exhausted entropy / a broken generator.
func errIDGen() func() (uuid.UUID, error) {
	return func() (uuid.UUID, error) { return uuid.UUID{}, errTestGen }
}

func aiMessage(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

func TestMapperToEvents(t *testing.T) {
	t.Parallel()

	stepMsg := aiMessage("step done")
	okMsg := aiMessage("all done")

	tests := []struct {
		name    string
		fe      ForeignEvent
		genErr  bool
		wantLen int
		wantErr bool
		check   func(t *testing.T, evs []event.Event)
	}{
		{
			name:    "text delta -> TokenDelta with text chunk",
			fe:      ForeignEvent{Kind: ForeignTextDelta, Text: "hello"},
			wantLen: 1,
			check: func(t *testing.T, evs []event.Event) {
				td, ok := evs[0].(event.TokenDelta)
				if !ok {
					t.Fatalf("evs[0] = %T, want event.TokenDelta", evs[0])
				}
				if td.TurnIndex != wantTurn {
					t.Errorf("TurnIndex = %d, want %d", td.TurnIndex, wantTurn)
				}
				tc, ok := td.Chunk.(*content.TextChunk)
				if !ok {
					t.Fatalf("Chunk = %T, want *content.TextChunk", td.Chunk)
				}
				if tc.Text != "hello" {
					t.Errorf("Text = %q, want hello", tc.Text)
				}
			},
		},
		{
			name:    "thinking delta -> TokenDelta with thinking chunk",
			fe:      ForeignEvent{Kind: ForeignThinkingDelta, Text: "pondering"},
			wantLen: 1,
			check: func(t *testing.T, evs []event.Event) {
				td, ok := evs[0].(event.TokenDelta)
				if !ok {
					t.Fatalf("evs[0] = %T, want event.TokenDelta", evs[0])
				}
				tc, ok := td.Chunk.(*content.ThinkingChunk)
				if !ok {
					t.Fatalf("Chunk = %T, want *content.ThinkingChunk", td.Chunk)
				}
				if tc.Thinking != "pondering" {
					t.Errorf("Thinking = %q, want pondering", tc.Thinking)
				}
			},
		},
		{
			name:    "tool use -> ToolCallStarted with minted id",
			fe:      ForeignEvent{Kind: ForeignToolUse, ToolUseID: "toolu_1", ToolName: "Bash"},
			wantLen: 1,
			check: func(t *testing.T, evs []event.Event) {
				started, ok := evs[0].(event.ToolCallStarted)
				if !ok {
					t.Fatalf("evs[0] = %T, want event.ToolCallStarted", evs[0])
				}
				if started.ToolName != "Bash" {
					t.Errorf("ToolName = %q, want Bash", started.ToolName)
				}
				if started.ToolExecutionID == (uuid.UUID{}) {
					t.Error("ToolExecutionID is zero, want minted non-zero id")
				}
			},
		},
		{
			name:    "tool result with unknown id -> orphan soft-skip",
			fe:      ForeignEvent{Kind: ForeignToolResult, ToolUseID: "ghost", IsError: true, ResultPreview: "x"},
			wantLen: 0,
		},
		{
			name:    "step complete with message -> StepDone",
			fe:      ForeignEvent{Kind: ForeignStepComplete, Message: stepMsg},
			wantLen: 1,
			check: func(t *testing.T, evs []event.Event) {
				sd, ok := evs[0].(event.StepDone)
				if !ok {
					t.Fatalf("evs[0] = %T, want event.StepDone", evs[0])
				}
				if len(sd.Messages) != 1 {
					t.Fatalf("Messages len = %d, want 1", len(sd.Messages))
				}
				if sd.Messages[0] != stepMsg {
					t.Errorf("Messages[0] = %#v, want the source AIMessage", sd.Messages[0])
				}
			},
		},
		{
			name:    "step complete with nil message -> no event",
			fe:      ForeignEvent{Kind: ForeignStepComplete, Message: nil},
			wantLen: 0,
		},
		{
			name:    "terminal ok -> TurnDone",
			fe:      ForeignEvent{Kind: ForeignTerminalOK, Message: okMsg},
			wantLen: 1,
			check: func(t *testing.T, evs []event.Event) {
				done, ok := evs[0].(event.TurnDone)
				if !ok {
					t.Fatalf("evs[0] = %T, want event.TurnDone", evs[0])
				}
				if done.TurnIndex != wantTurn {
					t.Errorf("TurnIndex = %d, want %d", done.TurnIndex, wantTurn)
				}
				if done.Message != okMsg {
					t.Errorf("Message = %#v, want the source AIMessage", done.Message)
				}
			},
		},
		{
			name:    "terminal error -> TurnFailed with ForeignResultError",
			fe:      ForeignEvent{Kind: ForeignTerminalError, ErrText: "error_max_turns"},
			wantLen: 1,
			check: func(t *testing.T, evs []event.Event) {
				failed, ok := evs[0].(event.TurnFailed)
				if !ok {
					t.Fatalf("evs[0] = %T, want event.TurnFailed", evs[0])
				}
				if failed.TurnIndex != wantTurn {
					t.Errorf("TurnIndex = %d, want %d", failed.TurnIndex, wantTurn)
				}
				var fre *ForeignResultError
				if !errors.As(failed.Err, &fre) {
					t.Fatalf("Err = %v, want *ForeignResultError", failed.Err)
				}
				if fre.Detail != "error_max_turns" {
					t.Errorf("Detail = %q, want error_max_turns", fre.Detail)
				}
			},
		},
		{
			name:    "init -> no event",
			fe:      ForeignEvent{Kind: ForeignInit, SessionID: "sess-abc"},
			wantLen: 0,
		},
		{
			name:    "unknown kind -> no event",
			fe:      ForeignEvent{Kind: ForeignKind(250)},
			wantLen: 0,
		},
		{
			name:    "idgen failure on tool use -> error",
			fe:      ForeignEvent{Kind: ForeignToolUse, ToolUseID: "toolu_1", ToolName: "Bash"},
			genErr:  true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gen := seqIDGen()
			if tt.genErr {
				gen = errIDGen()
			}
			m := newMapper(wantTurn, gen)
			evs, err := m.toEvents(tt.fe)
			if (err != nil) != tt.wantErr {
				t.Fatalf("toEvents() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if errors.Is(err, errTestGen) {
					return // verbatim propagation of the generator error
				}
				t.Fatalf("err = %v, want it to wrap errTestGen", err)
			}
			if len(evs) != tt.wantLen {
				t.Fatalf("len(evs) = %d, want %d (%#v)", len(evs), tt.wantLen, evs)
			}
			if tt.check != nil {
				tt.check(t, evs)
			}
		})
	}
}

// TestMapperCorrelation drives a tool_use and its later tool_result through the
// SAME mapper and asserts the started/completed events share one ToolExecutionID.
func TestMapperCorrelation(t *testing.T) {
	t.Parallel()
	m := newMapper(3, seqIDGen())

	startEvs, err := m.toEvents(ForeignEvent{Kind: ForeignToolUse, ToolUseID: "toolu_1", ToolName: "Bash"})
	if err != nil {
		t.Fatalf("tool_use toEvents err: %v", err)
	}
	if len(startEvs) != 1 {
		t.Fatalf("tool_use produced %d events, want 1", len(startEvs))
	}
	started, ok := startEvs[0].(event.ToolCallStarted)
	if !ok {
		t.Fatalf("startEvs[0] = %T, want event.ToolCallStarted", startEvs[0])
	}

	doneEvs, err := m.toEvents(ForeignEvent{Kind: ForeignToolResult, ToolUseID: "toolu_1", IsError: true, ResultPreview: "oops"})
	if err != nil {
		t.Fatalf("tool_result toEvents err: %v", err)
	}
	if len(doneEvs) != 1 {
		t.Fatalf("tool_result produced %d events, want 1", len(doneEvs))
	}
	completed, ok := doneEvs[0].(event.ToolCallCompleted)
	if !ok {
		t.Fatalf("doneEvs[0] = %T, want event.ToolCallCompleted", doneEvs[0])
	}

	if started.ToolExecutionID == (uuid.UUID{}) {
		t.Fatal("ToolExecutionID is zero")
	}
	if started.ToolExecutionID != completed.ToolExecutionID {
		t.Errorf("ToolExecutionID mismatch: started %v, completed %v", started.ToolExecutionID, completed.ToolExecutionID)
	}
	if !completed.IsError {
		t.Error("IsError = false, want true")
	}
	if completed.ResultPreview != "oops" {
		t.Errorf("ResultPreview = %q, want oops", completed.ResultPreview)
	}
}
