package tui

import (
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// callID is defined in screen_test.go (same package): it builds a deterministic,
// non-zero uuid.UUID from a single byte so tests can correlate
// ToolCallStarted/ToolCallCompleted without crypto/rand.

// TestSplitLines covers the tool-result preview splitter (transcript.go).
func TestSplitLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty yields nil", in: "", want: nil},
		{name: "single line", in: "one", want: []string{"one"}},
		{name: "two lines", in: "a\nb", want: []string{"a", "b"}},
		{name: "trailing newline keeps empty tail", in: "a\n", want: []string{"a", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := splitLines(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("splitLines(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("splitLines(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// toolStarted builds a real event.ToolCallStarted for the given call.
func toolStarted(id uuid.UUID, name, summary string) event.Event {
	return event.ToolCallStarted{CallID: id, ToolName: name, Summary: summary}
}

// toolCompleted builds a real event.ToolCallCompleted for the given call.
func toolCompleted(id uuid.UUID, isErr bool, preview string) event.Event {
	return event.ToolCallCompleted{CallID: id, IsError: isErr, ResultPreview: preview}
}

// textChunk builds a real *content.TextChunk TokenDelta event carrying t.
func textChunk(s string) event.Event {
	return event.TokenDelta{Chunk: &content.TextChunk{Text: s}}
}

// thinkingChunk builds a real *content.ThinkingChunk TokenDelta event carrying s.
func thinkingChunk(s string) event.Event {
	return event.TokenDelta{Chunk: &content.ThinkingChunk{Thinking: s}}
}

// aiMessage builds an *content.AIMessage from leading thinking, narration text, and
// any tool-use blocks (each by id+name), in that block order. Empty thinking/text
// are omitted so the blocks mirror the materialized AIMessage shape.
func aiMessage(thinking, text string, tools ...content.ToolUseBlock) *content.AIMessage {
	var blocks []content.Block
	if thinking != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: thinking})
	}
	if text != "" {
		blocks = append(blocks, &content.TextBlock{Text: text})
	}
	for i := range tools {
		tb := tools[i]
		blocks = append(blocks, &tb)
	}
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}
}

// toolUse builds a ToolUseBlock with the given provider id, name, and raw input.
func toolUse(id, name, input string) content.ToolUseBlock {
	return content.ToolUseBlock{ID: id, Name: name, Input: []byte(input)}
}

// toolResult builds a *content.ToolResultMessage answering toolUseID with text.
func toolResult(toolUseID, text string) *content.ToolResultMessage {
	return &content.ToolResultMessage{
		Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: text}}},
		ToolUseID: toolUseID,
	}
}

// toolResultErr builds a *content.ToolResultMessage answering toolUseID with an
// error result (IsError=true), used to exercise the stepToolCard fallback status.
func toolResultErr(toolUseID, text string) *content.ToolResultMessage {
	r := toolResult(toolUseID, text)
	r.IsError = true
	return r
}

// stepDone builds an event.StepDone carrying the given finalized group.
func stepDone(msgs ...content.Conversation) event.Event {
	return event.StepDone{Messages: content.AgenticMessages(msgs)}
}

// blockText returns the Text of b if it is a *content.TextBlock, else "".
func blockText(b content.Block) string {
	if tb, ok := b.(*content.TextBlock); ok {
		return tb.Text
	}
	return ""
}

// blockThinking returns the Thinking of b if it is a *content.ThinkingBlock, else "".
func blockThinking(b content.Block) string {
	if th, ok := b.(*content.ThinkingBlock); ok {
		return th.Thinking
	}
	return ""
}

func TestTranscriptApplyEvent(t *testing.T) {
	tests := []struct {
		name   string
		events []event.Event
		want   func(t *testing.T, m transcriptModel)
	}{
		{
			name: "text chunk accumulates into live",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("ab"),
				textChunk("cd"),
			},
			want: func(t *testing.T, m transcriptModel) {
				if got := m.live.Text; got != "abcd" {
					t.Errorf("live.Text = %q, want %q", got, "abcd")
				}
				if m.live.Thinking != "" {
					t.Errorf("live.Thinking = %q, want empty", m.live.Thinking)
				}
				if len(m.committed) != 0 {
					t.Errorf("committed = %d entries, want 0", len(m.committed))
				}
			},
		},
		{
			name: "thinking chunk accumulates into live",
			events: []event.Event{
				event.TurnStarted{},
				thinkingChunk("rea"),
				thinkingChunk("soning"),
			},
			want: func(t *testing.T, m transcriptModel) {
				if got := m.live.Thinking; got != "reasoning" {
					t.Errorf("live.Thinking = %q, want %q", got, "reasoning")
				}
				if m.live.Text != "" {
					t.Errorf("live.Text = %q, want empty", m.live.Text)
				}
				if len(m.committed) != 0 {
					t.Errorf("committed = %d entries, want 0", len(m.committed))
				}
			},
		},
		{
			name: "TurnDone commits live to one entry with stable ID",
			events: []event.Event{
				event.TurnStarted{},
				thinkingChunk("because reasons"),
				textChunk("the answer"),
				event.TurnDone{},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d entries, want exactly 1", len(m.committed))
				}
				e := m.committed[0]
				if e.ID == 0 {
					t.Errorf("entry ID = 0, want nonzero stable ID")
				}
				if e.Kind != kindAssistant {
					t.Errorf("entry Kind = %v, want kindAssistant", e.Kind)
				}
				// blocks: leading ThinkingBlock, then TextBlock (mirrors commitLive).
				if len(e.Blocks) != 2 {
					t.Fatalf("entry Blocks = %d, want 2 (thinking + text)", len(e.Blocks))
				}
				if got := blockThinking(e.Blocks[0]); got != "because reasons" {
					t.Errorf("Blocks[0] thinking = %q, want %q", got, "because reasons")
				}
				if got := blockText(e.Blocks[1]); got != "the answer" {
					t.Errorf("Blocks[1] text = %q, want %q", got, "the answer")
				}
				// live reset after commit.
				if !m.live.empty() {
					t.Errorf("live not reset after TurnDone: %+v", m.live)
				}
			},
		},
		{
			name: "empty live is not committed on TurnDone",
			events: []event.Event{
				event.TurnStarted{},
				event.TurnDone{},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 0 {
					t.Errorf("committed = %d entries, want 0 (empty live must not commit)", len(m.committed))
				}
			},
		},
		{
			name: "TurnDone with an unresolved running call commits it (not dropped) preserving status, and resets live",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(1), "Bash", "sleep 9"),
				event.TurnDone{},
			},
			want: func(t *testing.T, m transcriptModel) {
				// The leftover running call must be flushed, not silently dropped.
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1 (leftover call flushed, not dropped)", len(m.committed))
				}
				e := m.committed[0]
				if e.Kind != kindTool {
					t.Fatalf("committed[0].Kind = %v, want kindTool", e.Kind)
				}
				if e.ID == 0 {
					t.Errorf("entry ID = 0, want nonzero stable ID")
				}
				if len(e.Calls) != 1 {
					t.Fatalf("entry Calls = %d, want 1", len(e.Calls))
				}
				c := e.Calls[0]
				if c.CallID != callID(1) {
					t.Errorf("CallID = %v, want %v", c.CallID, callID(1))
				}
				// TurnDone is a normal completion: status preserved (NOT forced cancelled).
				if c.Status != ToolRunning {
					t.Errorf("Status = %v, want ToolRunning (TurnDone preserves status, does not cancel)", c.Status)
				}
				// live fully reset.
				if !m.live.empty() || m.live.active {
					t.Errorf("live not reset after TurnDone: %+v", m.live)
				}
				if len(m.live.Calls) != 0 {
					t.Errorf("live.Calls = %d, want 0 after TurnDone", len(m.live.Calls))
				}
			},
		},
		{
			name: "TurnDone after a completed call commits exactly one kindTool entry (no double-commit)",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(1), "Bash", "ls"),
				toolCompleted(callID(1), false, "out"),
				event.TurnDone{},
			},
			want: func(t *testing.T, m transcriptModel) {
				// Defensive commitLive path (no StepDone for this step): the resolved
				// live card is flushed exactly once by TurnDone — never duplicated.
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want exactly 1 (flushed once, no duplication)", len(m.committed))
				}
				if m.committed[0].Kind != kindTool {
					t.Errorf("committed[0].Kind = %v, want kindTool", m.committed[0].Kind)
				}
				if m.committed[0].Calls[0].Status != ToolOK {
					t.Errorf("Status = %v, want ToolOK", m.committed[0].Calls[0].Status)
				}
				if !m.live.empty() {
					t.Errorf("live not reset after TurnDone: %+v", m.live)
				}
			},
		},
		{
			name: "two turns produce distinct stable IDs",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("first"),
				event.TurnDone{},
				event.TurnStarted{},
				textChunk("second"),
				event.TurnDone{},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d entries, want 2", len(m.committed))
				}
				if m.committed[0].ID == m.committed[1].ID {
					t.Errorf("entry IDs not distinct: both %d", m.committed[0].ID)
				}
				if m.committed[0].ID == 0 || m.committed[1].ID == 0 {
					t.Errorf("entry IDs must be nonzero: %d, %d", m.committed[0].ID, m.committed[1].ID)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			for _, ev := range tt.events {
				m = m.ApplyEvent(ev)
			}
			tt.want(t, m)
		})
	}
}

// TestTranscriptLiveActive locks the live.active contract: TurnStarted marks the
// segment active, and committing a non-empty live segment on TurnDone resets live
// to the zero liveSeg{} (active false again). The empty-live case is included to
// show TurnDone still clears active even when nothing commits.
func TestTranscriptLiveActive(t *testing.T) {
	tests := []struct {
		name       string
		events     []event.Event
		wantActive bool
	}{
		{
			name:       "TurnStarted marks live active",
			events:     []event.Event{event.TurnStarted{}},
			wantActive: true,
		},
		{
			name: "active stays true while streaming chunks",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("partial"),
			},
			wantActive: true,
		},
		{
			name: "TurnDone committing non-empty live resets active to false",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("the answer"),
				event.TurnDone{},
			},
			wantActive: false,
		},
		{
			name: "TurnDone on empty live resets active to false",
			events: []event.Event{
				event.TurnStarted{},
				event.TurnDone{},
			},
			wantActive: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			for _, ev := range tt.events {
				m = m.ApplyEvent(ev)
			}
			if m.live.active != tt.wantActive {
				t.Errorf("live.active = %v, want %v", m.live.active, tt.wantActive)
			}
		})
	}
}

// TestTranscriptToolCalls covers the tool-call state machine: a started call adds
// a running card to live.Calls; a completed call resolves into exactly ONE
// committed kindTool entry at terminal state and leaves live.Calls without it.
func TestTranscriptToolCalls(t *testing.T) {
	tests := []struct {
		name   string
		events []event.Event
		want   func(t *testing.T, m transcriptModel)
	}{
		{
			name: "ToolCallStarted adds a running card to live.Calls",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(1), "Bash", "ls -la"),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.live.Calls) != 1 {
					t.Fatalf("live.Calls = %d, want 1", len(m.live.Calls))
				}
				c := m.live.Calls[0]
				if c.CallID != callID(1) {
					t.Errorf("CallID = %v, want %v", c.CallID, callID(1))
				}
				if c.ToolName != "Bash" {
					t.Errorf("ToolName = %q, want %q", c.ToolName, "Bash")
				}
				if c.Summary != "ls -la" {
					t.Errorf("Summary = %q, want %q", c.Summary, "ls -la")
				}
				if c.Status != ToolRunning {
					t.Errorf("Status = %v, want ToolRunning", c.Status)
				}
				if len(m.committed) != 0 {
					t.Errorf("committed = %d, want 0 (no terminal yet)", len(m.committed))
				}
			},
		},
		{
			name: "ToolCallCompleted (ok) resolves the live card IN PLACE (no commit until StepDone)",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(1), "Bash", "ls"),
				toolCompleted(callID(1), false, "file1\nfile2"),
			},
			want: func(t *testing.T, m transcriptModel) {
				// The card stays in the live tail, resolved; nothing is committed yet —
				// committing is the step boundary's job (StepDone), not the event's.
				if len(m.committed) != 0 {
					t.Fatalf("committed = %d, want 0 (no StepDone/terminal yet)", len(m.committed))
				}
				if len(m.live.Calls) != 1 {
					t.Fatalf("live.Calls = %d, want 1 (resolved in place, not removed)", len(m.live.Calls))
				}
				c := m.live.Calls[0]
				if c.Status != ToolOK {
					t.Errorf("Status = %v, want ToolOK", c.Status)
				}
				if len(c.Result) != 2 || c.Result[0] != "file1" || c.Result[1] != "file2" {
					t.Errorf("Result = %#v, want [file1 file2]", c.Result)
				}
			},
		},
		{
			name: "ToolCallCompleted (error) resolves the live card to ToolError in place",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(2), "Fetch", "GET /x"),
				toolCompleted(callID(2), true, "boom"),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 0 {
					t.Fatalf("committed = %d, want 0 (no StepDone/terminal yet)", len(m.committed))
				}
				if len(m.live.Calls) != 1 {
					t.Fatalf("live.Calls = %d, want 1", len(m.live.Calls))
				}
				c := m.live.Calls[0]
				if c.Status != ToolError {
					t.Errorf("Status = %v, want ToolError", c.Status)
				}
				if len(c.Result) != 1 || c.Result[0] != "boom" {
					t.Errorf("Result = %#v, want [boom]", c.Result)
				}
			},
		},
		{
			name: "StepDone commits the resolved live cards' redacted Summary/preview by position",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(1), "Bash", "a"),
				toolCompleted(callID(1), false, "out1"),
				toolStarted(callID(2), "Bash", "b"),
				toolCompleted(callID(2), false, "out2"),
				// The finalized step group: two tool-use blocks, in the same order.
				stepDone(
					aiMessage("", "", toolUse("tu-1", "Bash", `{}`), toolUse("tu-2", "Bash", `{}`)),
					toolResult("tu-1", "out1"),
					toolResult("tu-2", "out2"),
				),
			},
			want: func(t *testing.T, m transcriptModel) {
				// bare assistant (card-only) + two tool entries.
				if len(m.committed) != 3 {
					t.Fatalf("committed = %d, want 3 (bare assistant, tool, tool)", len(m.committed))
				}
				if m.committed[1].Kind != kindTool || m.committed[2].Kind != kindTool {
					t.Errorf("kinds = %v,%v, want kindTool both", m.committed[1].Kind, m.committed[2].Kind)
				}
				if m.committed[1].ID == m.committed[2].ID {
					t.Errorf("tool entry IDs not distinct: both %d", m.committed[1].ID)
				}
				// The committed cards reuse the LIVE cards' redacted Summary by position.
				if got := m.committed[1].Calls[0].Summary; got != "a" {
					t.Errorf("tool[0] Summary = %q, want the redacted live summary %q", got, "a")
				}
				if got := m.committed[2].Calls[0].Summary; got != "b" {
					t.Errorf("tool[1] Summary = %q, want the redacted live summary %q", got, "b")
				}
				if !m.live.empty() {
					t.Errorf("live not reset after StepDone: %+v", m.live)
				}
			},
		},
		{
			name: "StepDone fallback (no live card) reads ToolResultMessage.IsError → ToolError",
			events: []event.Event{
				event.TurnStarted{},
				// No toolStarted/toolCompleted: live.Calls is empty, so stepToolCard
				// takes the fallback branch keyed on the stored ToolResultMessage.
				stepDone(
					aiMessage("", "", toolUse("tu-err", "Bash", `{}`)),
					toolResultErr("tu-err", "tool error: boom"),
				),
			},
			want: func(t *testing.T, m transcriptModel) {
				// bare assistant (card-only) + one tool entry committed via fallback.
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2 (bare assistant, tool)", len(m.committed))
				}
				if m.committed[1].Kind != kindTool {
					t.Errorf("committed[1].Kind = %v, want kindTool", m.committed[1].Kind)
				}
				if len(m.committed[1].Calls) != 1 {
					t.Fatalf("committed[1].Calls = %d, want 1", len(m.committed[1].Calls))
				}
				if got := m.committed[1].Calls[0].Status; got != ToolError {
					t.Errorf("fallback card Status = %v, want ToolError (from IsError)", got)
				}
			},
		},
		{
			name: "StepDone fallback (no live card) with IsError false → ToolOK",
			events: []event.Event{
				event.TurnStarted{},
				stepDone(
					aiMessage("", "", toolUse("tu-ok", "Bash", `{}`)),
					toolResult("tu-ok", "all good"),
				),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2 (bare assistant, tool)", len(m.committed))
				}
				if m.committed[1].Kind != kindTool {
					t.Errorf("committed[1].Kind = %v, want kindTool", m.committed[1].Kind)
				}
				if len(m.committed[1].Calls) != 1 {
					t.Fatalf("committed[1].Calls = %d, want 1", len(m.committed[1].Calls))
				}
				if got := m.committed[1].Calls[0].Status; got != ToolOK {
					t.Errorf("fallback card Status = %v, want ToolOK", got)
				}
			},
		},
		{
			name: "unknown completed CallID is a no-op (no commit, no panic)",
			events: []event.Event{
				event.TurnStarted{},
				toolCompleted(callID(9), false, "orphan"),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 0 {
					t.Errorf("committed = %d, want 0 (unknown CallID is a no-op)", len(m.committed))
				}
				if len(m.live.Calls) != 0 {
					t.Errorf("live.Calls = %d, want 0", len(m.live.Calls))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			for _, ev := range tt.events {
				m = m.ApplyEvent(ev)
			}
			tt.want(t, m)
		})
	}
}

// TestTranscriptOrdering locks the per-step append-only ordering rule under the
// StepDone-group model: within a step the AIMessage prose commits as its own
// assistant entry AHEAD of that step's tool card, and a SECOND step's prose lands in a
// later assistant entry — yielding the natural reading order prose1 → tool card →
// prose2 across two StepDone groups (the OLD single-turn interleave maps onto two
// steps now).
func TestTranscriptOrdering(t *testing.T) {
	t.Parallel()
	var m transcriptModel
	for _, ev := range []event.Event{
		event.TurnStarted{},
		// Step 1: prose then a tool use, finalized.
		textChunk("before tool"),
		toolStarted(callID(1), "Bash", "run"),
		toolCompleted(callID(1), false, "done"),
		stepDone(
			aiMessage("", "before tool", toolUse("tu-1", "Bash", `{}`)),
			toolResult("tu-1", "done"),
		),
		// Step 2: the trailing prose, finalized.
		textChunk("after tool"),
		stepDone(aiMessage("", "after tool")),
		event.TurnDone{},
	} {
		m = m.ApplyEvent(ev)
	}
	if len(m.committed) != 3 {
		t.Fatalf("committed = %d, want 3 (prose1, tool, prose2)", len(m.committed))
	}
	// [0] step-1 assistant prose committed BEFORE the tool card.
	if m.committed[0].Kind != kindAssistant {
		t.Fatalf("committed[0].Kind = %v, want kindAssistant", m.committed[0].Kind)
	}
	if got := blockText(m.committed[0].Blocks[0]); got != "before tool" {
		t.Errorf("committed[0] text = %q, want %q", got, "before tool")
	}
	// [1] the tool card, AFTER prose1.
	if m.committed[1].Kind != kindTool {
		t.Errorf("committed[1].Kind = %v, want kindTool", m.committed[1].Kind)
	}
	// [2] step-2 prose, AFTER the tool card.
	if m.committed[2].Kind != kindAssistant {
		t.Fatalf("committed[2].Kind = %v, want kindAssistant", m.committed[2].Kind)
	}
	if got := blockText(m.committed[2].Blocks[0]); got != "after tool" {
		t.Errorf("committed[2] text = %q, want %q", got, "after tool")
	}
	// IDs strictly increasing in commit order.
	if !(m.committed[0].ID < m.committed[1].ID && m.committed[1].ID < m.committed[2].ID) {
		t.Errorf("IDs not strictly increasing: %d,%d,%d", m.committed[0].ID, m.committed[1].ID, m.committed[2].ID)
	}
}

// TestTranscriptTerminals covers the TurnInterrupted and TurnFailed terminals:
// remaining live prose/thinking is committed, any still-running call is cancelled
// and committed, the appropriate tombstone/error entry is appended, and live is
// reset.
func TestTranscriptTerminals(t *testing.T) {
	tests := []struct {
		name   string
		events []event.Event
		want   func(t *testing.T, m transcriptModel)
	}{
		{
			name: "TurnInterrupted commits prose, cancels running call, appends tombstone, resets live",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("partial answer"),
				toolStarted(callID(1), "Bash", "sleep"),
				event.TurnInterrupted{},
			},
			want: func(t *testing.T, m transcriptModel) {
				// prose committed, cancelled tool committed, interrupted tombstone.
				if len(m.committed) != 3 {
					t.Fatalf("committed = %d, want 3 (prose, cancelled tool, tombstone)", len(m.committed))
				}
				if m.committed[0].Kind != kindAssistant {
					t.Errorf("committed[0].Kind = %v, want kindAssistant", m.committed[0].Kind)
				}
				if m.committed[1].Kind != kindTool {
					t.Errorf("committed[1].Kind = %v, want kindTool", m.committed[1].Kind)
				}
				if got := m.committed[1].Calls[0].Status; got != ToolCancelled {
					t.Errorf("running call status = %v, want ToolCancelled", got)
				}
				if m.committed[2].Kind != kindInterrupted {
					t.Errorf("committed[2].Kind = %v, want kindInterrupted", m.committed[2].Kind)
				}
				if !m.live.empty() || m.live.active {
					t.Errorf("live not reset after interrupt: %+v", m.live)
				}
			},
		},
		{
			name: "TurnInterrupted with no live content appends only the tombstone",
			events: []event.Event{
				event.TurnStarted{},
				event.TurnInterrupted{},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1 (tombstone only)", len(m.committed))
				}
				if m.committed[0].Kind != kindInterrupted {
					t.Errorf("Kind = %v, want kindInterrupted", m.committed[0].Kind)
				}
			},
		},
		{
			name: "TurnFailed commits prose then appends an error-level notice carrying the message",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("partial"),
				event.TurnFailed{Err: errBoom{}},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2 (prose, error)", len(m.committed))
				}
				if m.committed[0].Kind != kindAssistant {
					t.Errorf("committed[0].Kind = %v, want kindAssistant", m.committed[0].Kind)
				}
				if m.committed[1].Kind != kindNotice || m.committed[1].Level != noticeError {
					t.Fatalf("committed[1] = (kind %v, level %d), want (kindNotice, noticeError)", m.committed[1].Kind, m.committed[1].Level)
				}
				if got := blockText(m.committed[1].Blocks[0]); got != "boom" {
					t.Errorf("error text = %q, want %q", got, "boom")
				}
				if !m.live.empty() || m.live.active {
					t.Errorf("live not reset after failure: %+v", m.live)
				}
			},
		},
		{
			name: "TurnFailed with nil Err still appends an error-level notice and resets live",
			events: []event.Event{
				event.TurnStarted{},
				event.TurnFailed{Err: nil},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1 (error only)", len(m.committed))
				}
				if m.committed[0].Kind != kindNotice || m.committed[0].Level != noticeError {
					t.Errorf("committed[0] = (kind %v, level %d), want (kindNotice, noticeError)", m.committed[0].Kind, m.committed[0].Level)
				}
				if !m.live.empty() {
					t.Errorf("live not reset after failure: %+v", m.live)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			for _, ev := range tt.events {
				m = m.ApplyEvent(ev)
			}
			tt.want(t, m)
		})
	}
}

// TestTranscriptStepDoneSelfHeal locks the StepDone-group rendering + self-heal
// contract (Phase 11.2): provisional live prose accumulated from TokenDeltas is
// REPLACED by the finalized StepDone.Messages on commit (a dropped/partial delta
// does not survive past StepDone), the committed entries are built from the stored
// AIMessage (+ its ToolResultMessages), and the live segment is reset to empty.
func TestTranscriptStepDoneSelfHeal(t *testing.T) {
	tests := []struct {
		name   string
		events []event.Event
		want   func(t *testing.T, m transcriptModel)
	}{
		{
			name: "no-tool step: provisional text replaced by finalized AIMessage prose",
			events: []event.Event{
				event.TurnStarted{},
				// Provisional/partial deltas: a torn stream that dropped the tail.
				thinkingChunk("because rea"),
				textChunk("the ans"),
				// The authoritative finalized group: full thinking + full text.
				stepDone(aiMessage("because reasons", "the answer")),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want exactly 1 (the finalized AIMessage)", len(m.committed))
				}
				e := m.committed[0]
				if e.Kind != kindAssistant {
					t.Fatalf("committed[0].Kind = %v, want kindAssistant", e.Kind)
				}
				if e.ID == 0 {
					t.Errorf("entry ID = 0, want nonzero stable ID")
				}
				// SNAP: the finalized message, NOT the partial provisional text.
				if got := thinkingText(e.Blocks); got != "because reasons" {
					t.Errorf("committed thinking = %q, want finalized %q (self-heal)", got, "because reasons")
				}
				if got := assistantText(e.Blocks); got != "the answer" {
					t.Errorf("committed text = %q, want finalized %q (self-heal)", got, "the answer")
				}
				// the provisional live segment is gone: dropped deltas do not survive.
				if !m.live.empty() {
					t.Errorf("live not reset after StepDone: %+v", m.live)
				}
			},
		},
		{
			name: "provisional text that OVER-ran the finalized message is discarded on snap",
			events: []event.Event{
				event.TurnStarted{},
				// A stale/duplicated provisional render: longer than the truth.
				textChunk("the answer is forty-two and then some garbage"),
				stepDone(aiMessage("", "the answer is forty-two")),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1", len(m.committed))
				}
				if got := assistantText(m.committed[0].Blocks); got != "the answer is forty-two" {
					t.Errorf("committed text = %q, want the finalized %q (provisional discarded)", got, "the answer is forty-two")
				}
			},
		},
		{
			name: "tool-using step: AIMessage prose entry then a separate tool entry carrying the result",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("let me check"),
				stepDone(
					aiMessage("", "let me check", toolUse("tu-1", "Grep", `{"q":"x"}`)),
					toolResult("tu-1", "match\nanother"),
				),
			},
			want: func(t *testing.T, m transcriptModel) {
				// SEPARATE entries: the assistant prose, then the tool card. NOT merged.
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2 (assistant prose, tool)", len(m.committed))
				}
				if m.committed[0].Kind != kindAssistant {
					t.Errorf("committed[0].Kind = %v, want kindAssistant", m.committed[0].Kind)
				}
				if got := assistantText(m.committed[0].Blocks); got != "let me check" {
					t.Errorf("assistant text = %q, want %q", got, "let me check")
				}
				tool := m.committed[1]
				if tool.Kind != kindTool {
					t.Fatalf("committed[1].Kind = %v, want kindTool", tool.Kind)
				}
				if len(tool.Calls) != 1 {
					t.Fatalf("tool entry Calls = %d, want 1", len(tool.Calls))
				}
				c := tool.Calls[0]
				if c.ToolName != "Grep" {
					t.Errorf("tool name = %q, want %q", c.ToolName, "Grep")
				}
				if len(c.Result) != 2 || c.Result[0] != "match" || c.Result[1] != "another" {
					t.Errorf("tool result = %#v, want [match another] (from the stored ToolResultMessage)", c.Result)
				}
				// IDs strictly increasing in commit order.
				if !(m.committed[0].ID < m.committed[1].ID) {
					t.Errorf("IDs not increasing: %d, %d", m.committed[0].ID, m.committed[1].ID)
				}
				if !m.live.empty() {
					t.Errorf("live not reset after StepDone: %+v", m.live)
				}
			},
		},
		{
			name: "tool-use-only step (no narration) commits a bare assistant entry then the tool entry",
			events: []event.Event{
				event.TurnStarted{},
				stepDone(
					aiMessage("", "", toolUse("tu-9", "ReadFile", `{"path":"a"}`)),
					toolResult("tu-9", "contents"),
				),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2 (bare assistant, tool)", len(m.committed))
				}
				if m.committed[0].Kind != kindAssistant {
					t.Errorf("committed[0].Kind = %v, want kindAssistant (bare bullet)", m.committed[0].Kind)
				}
				if m.committed[1].Kind != kindTool {
					t.Errorf("committed[1].Kind = %v, want kindTool", m.committed[1].Kind)
				}
				if m.committed[1].Calls[0].ToolName != "ReadFile" {
					t.Errorf("tool name = %q, want ReadFile", m.committed[1].Calls[0].ToolName)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			for _, ev := range tt.events {
				m = m.ApplyEvent(ev)
			}
			tt.want(t, m)
		})
	}
}

// TestTranscriptMultiStepSeparateEntries locks that a multi-step (tool-using) turn
// renders as MULTIPLE separate assistant + tool entries, in order — never collapsed
// into one merged entry. Two StepDone groups (a tool step then a final no-tool
// answer) followed by the lifecycle TurnDone must yield: assistant, tool, assistant.
func TestTranscriptMultiStepSeparateEntries(t *testing.T) {
	t.Parallel()
	var m transcriptModel
	for _, ev := range []event.Event{
		event.TurnStarted{},
		// Step 1: assistant asks for a tool; its result comes back.
		textChunk("checking"),
		stepDone(
			aiMessage("", "checking", toolUse("tu-1", "Bash", `{"cmd":"ls"}`)),
			toolResult("tu-1", "file1\nfile2"),
		),
		// Step 2: the final no-tool answer.
		textChunk("all done"),
		stepDone(aiMessage("", "all done")),
		// Lifecycle terminal: no new content (every step already committed via StepDone).
		event.TurnDone{},
	} {
		m = m.ApplyEvent(ev)
	}

	if len(m.committed) != 3 {
		t.Fatalf("committed = %d, want 3 (step1 assistant, step1 tool, step2 assistant) — NOT merged", len(m.committed))
	}
	wantKinds := []entryKind{kindAssistant, kindTool, kindAssistant}
	for i, want := range wantKinds {
		if m.committed[i].Kind != want {
			t.Errorf("committed[%d].Kind = %v, want %v", i, m.committed[i].Kind, want)
		}
	}
	if got := assistantText(m.committed[0].Blocks); got != "checking" {
		t.Errorf("step1 assistant text = %q, want %q", got, "checking")
	}
	if c := m.committed[1].Calls[0]; c.ToolName != "Bash" {
		t.Errorf("step1 tool name = %q, want Bash", c.ToolName)
	}
	if got := assistantText(m.committed[2].Blocks); got != "all done" {
		t.Errorf("step2 assistant text = %q, want %q", got, "all done")
	}
	// IDs strictly increasing in commit order across both steps.
	if !(m.committed[0].ID < m.committed[1].ID && m.committed[1].ID < m.committed[2].ID) {
		t.Errorf("IDs not strictly increasing: %d,%d,%d", m.committed[0].ID, m.committed[1].ID, m.committed[2].ID)
	}
	if !m.live.empty() || m.live.active {
		t.Errorf("live not reset after the turn: %+v", m.live)
	}
}

// errBoom is a typed test error whose message is "boom".
type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// TestCommitUser locks the user-message commit: CommitUser appends exactly one
// kindUser entry carrying the submitted blocks with a fresh, nonzero, stable ID;
// a second CommitUser allocates a distinct ID; and empty blocks still commit one
// user entry (the submit path validates emptiness upstream, not here).
func TestCommitUser(t *testing.T) {
	tests := []struct {
		name   string
		blocks [][]content.Block
		want   func(t *testing.T, m transcriptModel)
	}{
		{
			name:   "single user message commits one entry with a nonzero ID",
			blocks: [][]content.Block{{&content.TextBlock{Text: "hello there"}}},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1", len(m.committed))
				}
				e := m.committed[0]
				if e.Kind != kindUser {
					t.Errorf("Kind = %v, want kindUser", e.Kind)
				}
				if e.ID == 0 {
					t.Errorf("entry ID = 0, want nonzero stable ID")
				}
				if len(e.Blocks) != 1 || blockText(e.Blocks[0]) != "hello there" {
					t.Errorf("Blocks = %#v, want one TextBlock %q", e.Blocks, "hello there")
				}
			},
		},
		{
			name: "two user messages get distinct stable IDs",
			blocks: [][]content.Block{
				{&content.TextBlock{Text: "first"}},
				{&content.TextBlock{Text: "second"}},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2", len(m.committed))
				}
				if m.committed[0].ID == m.committed[1].ID {
					t.Errorf("user entry IDs not distinct: both %d", m.committed[0].ID)
				}
				if m.committed[0].ID == 0 || m.committed[1].ID == 0 {
					t.Errorf("user entry IDs must be nonzero: %d, %d", m.committed[0].ID, m.committed[1].ID)
				}
			},
		},
		{
			name:   "empty blocks still commit a single user entry",
			blocks: [][]content.Block{{}},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1", len(m.committed))
				}
				if m.committed[0].Kind != kindUser {
					t.Errorf("Kind = %v, want kindUser", m.committed[0].Kind)
				}
				if m.committed[0].ID == 0 {
					t.Errorf("entry ID = 0, want nonzero stable ID")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			for _, b := range tt.blocks {
				m = m.CommitUser(b)
			}
			tt.want(t, m)
		})
	}
}

// TestCommitUserDoesNotDisturbLive locks that committing a user message neither
// reads nor resets the in-progress live segment: a user message can be queued
// mid-turn (Running) without truncating the streaming assistant output.
func TestCommitUserDoesNotDisturbLive(t *testing.T) {
	t.Parallel()
	var m transcriptModel
	m = m.ApplyEvent(event.TurnStarted{})
	m = m.ApplyEvent(textChunk("streaming so far"))
	m = m.CommitUser([]content.Block{&content.TextBlock{Text: "queued msg"}})
	if m.live.Text != "streaming so far" {
		t.Errorf("live.Text = %q, want preserved %q", m.live.Text, "streaming so far")
	}
	if !m.live.active {
		t.Errorf("live.active = false, want true (CommitUser must not end the turn)")
	}
	if len(m.committed) != 1 || m.committed[0].Kind != kindUser {
		t.Fatalf("committed = %#v, want one kindUser entry", m.committed)
	}
}

// TestTranscriptPromptCommit covers the prompt-open boundary in ApplyEvent: a
// PermissionRequested / UserInputRequested commits any pending live prose FIRST
// (append-only ordering), then appends a single kindPromptRecord entry carrying
// the FULL prompt context (permission: ToolName + Description; user input:
// Question + ALL Choices). The live segment is NOT reset — the turn continues
// while the gate is pending.
func TestTranscriptPromptCommit(t *testing.T) {
	tests := []struct {
		name   string
		events []event.Event
		want   func(t *testing.T, m transcriptModel)
	}{
		{
			name: "PermissionRequested commits pending prose then a promptRecord with tool+description",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("I'll run a command."),
				event.PermissionRequested{
					CallID:  callID(1),
					Request: tool.BashRequest{Command: "rm -rf build"},
				},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2 (prose, promptRecord)", len(m.committed))
				}
				if m.committed[0].Kind != kindAssistant {
					t.Errorf("committed[0].Kind = %v, want kindAssistant (prose committed first)", m.committed[0].Kind)
				}
				rec := m.committed[1]
				if rec.Kind != kindPromptRecord {
					t.Fatalf("committed[1].Kind = %v, want kindPromptRecord", rec.Kind)
				}
				if rec.ID == 0 {
					t.Errorf("promptRecord ID = 0, want nonzero stable ID")
				}
				if rec.Prompt == nil {
					t.Fatal("promptRecord Prompt context is nil")
				}
				if rec.Prompt.Kind != promptPermission {
					t.Errorf("Prompt.Kind = %v, want promptPermission", rec.Prompt.Kind)
				}
				if rec.Prompt.ToolName != "Bash" {
					t.Errorf("Prompt.ToolName = %q, want %q", rec.Prompt.ToolName, "Bash")
				}
				if rec.Prompt.Description != "rm -rf build" {
					t.Errorf("Prompt.Description = %q, want %q", rec.Prompt.Description, "rm -rf build")
				}
				// the full context must survive into the rendered scrollback record.
				out := strings.Join(renderEntry(rec, false, 80), "\n")
				if !strings.Contains(stripANSI(out), "Bash") || !strings.Contains(stripANSI(out), "rm -rf build") {
					t.Errorf("rendered promptRecord = %q, want it to contain ToolName + Description", stripANSI(out))
				}
				// live is NOT reset: the turn continues while the gate is pending.
				if !m.live.active {
					t.Errorf("live.active = false, want true (prompt does not end the turn)")
				}
			},
		},
		{
			name: "PermissionRequested on UnknownRequest records the tool name and summary",
			events: []event.Event{
				event.TurnStarted{},
				event.PermissionRequested{
					CallID:  callID(2),
					Request: tool.UnknownRequest{Tool: "Mystery", Summary: "do a thing"},
				},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1 (promptRecord only; no pending prose)", len(m.committed))
				}
				rec := m.committed[0]
				if rec.Kind != kindPromptRecord || rec.Prompt == nil {
					t.Fatalf("committed[0] = %+v, want a kindPromptRecord with Prompt context", rec)
				}
				if rec.Prompt.ToolName != "Mystery" || rec.Prompt.Description != "do a thing" {
					t.Errorf("Prompt = {%q,%q}, want {Mystery, do a thing}", rec.Prompt.ToolName, rec.Prompt.Description)
				}
			},
		},
		{
			name: "PermissionRequested with nil Request records empty context without panicking",
			events: []event.Event{
				event.TurnStarted{},
				event.PermissionRequested{CallID: callID(3), Request: nil},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1", len(m.committed))
				}
				rec := m.committed[0]
				if rec.Kind != kindPromptRecord || rec.Prompt == nil {
					t.Fatalf("committed[0] = %+v, want a kindPromptRecord with (empty) Prompt context", rec)
				}
				if rec.Prompt.ToolName != "" || rec.Prompt.Description != "" {
					t.Errorf("Prompt = {%q,%q}, want both empty for nil Request", rec.Prompt.ToolName, rec.Prompt.Description)
				}
			},
		},
		{
			name: "UserInputRequested commits prose then a promptRecord with question + all choices",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("Need a decision."),
				event.UserInputRequested{
					CallID:   callID(4),
					Question: "Which source?",
					Choices:  []string{"alpha", "beta", "gamma"},
				},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2 (prose, promptRecord)", len(m.committed))
				}
				if m.committed[0].Kind != kindAssistant {
					t.Errorf("committed[0].Kind = %v, want kindAssistant", m.committed[0].Kind)
				}
				rec := m.committed[1]
				if rec.Kind != kindPromptRecord || rec.Prompt == nil {
					t.Fatalf("committed[1] = %+v, want a kindPromptRecord with Prompt context", rec)
				}
				if rec.Prompt.Kind != promptUserInput {
					t.Errorf("Prompt.Kind = %v, want promptUserInput", rec.Prompt.Kind)
				}
				if rec.Prompt.Question != "Which source?" {
					t.Errorf("Prompt.Question = %q, want %q", rec.Prompt.Question, "Which source?")
				}
				if len(rec.Prompt.Choices) != 3 {
					t.Fatalf("Prompt.Choices = %d, want 3", len(rec.Prompt.Choices))
				}
				// every choice must survive into the rendered scrollback record.
				out := stripANSI(strings.Join(renderEntry(rec, false, 80), "\n"))
				for _, c := range []string{"Which source?", "alpha", "beta", "gamma"} {
					if !strings.Contains(out, c) {
						t.Errorf("rendered promptRecord = %q, want it to contain %q", out, c)
					}
				}
			},
		},
		{
			name: "UserInputRequested with no choices records a free-text question",
			events: []event.Event{
				event.TurnStarted{},
				event.UserInputRequested{CallID: callID(5), Question: "free answer?", Choices: nil},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1", len(m.committed))
				}
				rec := m.committed[0]
				if rec.Kind != kindPromptRecord || rec.Prompt == nil {
					t.Fatalf("committed[0] = %+v, want a kindPromptRecord", rec)
				}
				if rec.Prompt.Question != "free answer?" || len(rec.Prompt.Choices) != 0 {
					t.Errorf("Prompt = {%q, %d choices}, want {free answer?, 0}", rec.Prompt.Question, len(rec.Prompt.Choices))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			for _, ev := range tt.events {
				m = m.ApplyEvent(ev)
			}
			tt.want(t, m)
		})
	}
}

// TestCommitStepAssistantDoneHeadline covers the doneHeadline signal on a committed
// assistant entry (design §3 rule 4): a StepDone whose AIMessage has tool-use blocks
// but NO prose/thinking commits a kindAssistant entry marked doneHeadline (it renders
// the static "● Done"); any prose/thinking clears it (the prose IS the headline); and
// the interrupt/failure partial path NEVER sets it (an interrupted step is not "done").
func TestCommitStepAssistantDoneHeadline(t *testing.T) {
	tests := []struct {
		name   string
		events []event.Event
		want   bool // doneHeadline on committed[0] (always a kindAssistant in these rows)
	}{
		{
			name: "empty-text tool step → doneHeadline",
			events: []event.Event{
				event.TurnStarted{},
				stepDone(aiMessage("", "", toolUse("tu-1", "Bash", `{}`)), toolResult("tu-1", "out")),
			},
			want: true,
		},
		{
			name: "tool step WITH narration → no doneHeadline (prose is the headline)",
			events: []event.Event{
				event.TurnStarted{},
				stepDone(aiMessage("", "reading config", toolUse("tu-1", "Bash", `{}`)), toolResult("tu-1", "out")),
			},
			want: false,
		},
		{
			name: "tool step WITH thinking → no doneHeadline",
			events: []event.Event{
				event.TurnStarted{},
				stepDone(aiMessage("plan it", "", toolUse("tu-1", "Bash", `{}`)), toolResult("tu-1", "out")),
			},
			want: false,
		},
		{
			name: "interrupted partial prose → no doneHeadline (not done)",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("partial answer"),
				toolStarted(callID(1), "Bash", "sleep"),
				event.TurnInterrupted{},
			},
			want: false,
		},
		{
			name: "failed partial prose → no doneHeadline (not done)",
			events: []event.Event{
				event.TurnStarted{},
				textChunk("partial"),
				event.TurnFailed{Err: errBoom{}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			for _, ev := range tt.events {
				m = m.ApplyEvent(ev)
			}
			if len(m.committed) == 0 {
				t.Fatalf("committed = 0 entries, want at least 1")
			}
			e := m.committed[0]
			if e.Kind != kindAssistant {
				t.Fatalf("committed[0].Kind = %v, want kindAssistant", e.Kind)
			}
			if e.doneHeadline != tt.want {
				t.Errorf("committed[0].doneHeadline = %v, want %v", e.doneHeadline, tt.want)
			}
		})
	}
}

// userMsg builds a *content.UserMessage carrying one TextBlock, the authoritative
// payload a TurnStarted/TurnFoldedInto event carries for the committed user row.
func userMsg(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// userBlocks builds the []content.Block a submit produces, the blocks RecordSubmit
// remembers for the queued affordance.
func userBlocks(text string) []content.Block {
	return []content.Block{&content.TextBlock{Text: text}}
}

// kindUserCount counts the committed kindUser rows in m.
func kindUserCount(m transcriptModel) int {
	n := 0
	for _, e := range m.committed {
		if e.Kind == kindUser {
			n++
		}
	}
	return n
}

// queuedTexts returns the first-text-block text of each ready queued affordance, in
// order, for assertions.
func queuedTexts(m transcriptModel) []string {
	var out []string
	for _, blocks := range m.QueuedInputs() {
		out = append(out, blockText(blocks[0]))
	}
	return out
}

// TestTranscriptUserRowFromTurnEvent locks the event-driven user row: a TurnStarted /
// TurnFoldedInto with TriggeredByLoopID == 0 and a Message commits exactly ONE
// kindUser row equal to the Message blocks; a SUBAGENT hand-back (TriggeredByLoopID
// != 0) commits NO user row; a nil Message commits no row either.
func TestTranscriptUserRowFromTurnEvent(t *testing.T) {
	primary := callID(0)     // genuine user input: the zero (untriggered) loop id
	subagent := callID(0xBB) // a non-zero TriggeredByLoopID => subagent hand-back

	tests := []struct {
		name     string
		event    event.Event
		wantRows int
		wantText string // checked only when wantRows == 1
	}{
		{
			name:     "TurnStarted genuine user input commits one row",
			event:    event.TurnStarted{Header: event.Header{TriggeredByLoopID: primary}, InputID: callID(1), Message: userMsg("hello")},
			wantRows: 1,
			wantText: "hello",
		},
		{
			name:     "TurnFoldedInto genuine user input commits one row",
			event:    event.TurnFoldedInto{Header: event.Header{TriggeredByLoopID: primary}, InputID: callID(1), Message: userMsg("folded")},
			wantRows: 1,
			wantText: "folded",
		},
		{
			name:     "TurnStarted subagent hand-back commits no row",
			event:    event.TurnStarted{Header: event.Header{TriggeredByLoopID: subagent}, InputID: callID(1), Message: userMsg("handback")},
			wantRows: 0,
		},
		{
			name:     "TurnFoldedInto subagent hand-back commits no row",
			event:    event.TurnFoldedInto{Header: event.Header{TriggeredByLoopID: subagent}, InputID: callID(1), Message: userMsg("handback")},
			wantRows: 0,
		},
		{
			name:     "TurnStarted nil message commits no row",
			event:    event.TurnStarted{Header: event.Header{TriggeredByLoopID: primary}, InputID: callID(1)},
			wantRows: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			m = m.ApplyEvent(tt.event)
			if got := kindUserCount(m); got != tt.wantRows {
				t.Fatalf("kindUser rows = %d, want %d", got, tt.wantRows)
			}
			if tt.wantRows == 1 {
				e := m.committed[len(m.committed)-1]
				if got := blockText(e.Blocks[0]); got != tt.wantText {
					t.Errorf("committed user row text = %q, want %q", got, tt.wantText)
				}
			}
		})
	}
}

// TestTranscriptUserRowRequiresPrimaryLoop locks the loop-scoping half of the
// user-row decision: a TurnStarted / TurnFoldedInto whose Header.LoopID is NOT the
// model's primaryLoopID commits NO kindUser row — even with TriggeredByLoopID == 0
// and a non-nil Message. This is the subagent-own-turn case: a subagent's INITIAL
// task arrives at its loop as a command.UserInput, so its emitted TurnStarted has
// TriggeredByLoopID == 0 and LoopID == <the subagent loop>; the DefaultEventFilter
// delivers it (Enduring from All loops), so it reaches ApplyEvent — but it must NOT
// become a human user row (§5/§6: subagent loops' own turns surface only via
// StepDone). A turn whose LoopID == primaryLoopID still commits the row.
func TestTranscriptUserRowRequiresPrimaryLoop(t *testing.T) {
	primary := callID(0xA1) // the model's primary loop id
	subLoop := callID(0xC2) // a different (subagent) loop id

	tests := []struct {
		name     string
		event    event.Event
		wantRows int
	}{
		{
			name:     "TurnStarted on the PRIMARY loop commits a row",
			event:    event.TurnStarted{Header: event.Header{LoopID: primary}, InputID: callID(1), Message: userMsg("genuine")},
			wantRows: 1,
		},
		{
			name:     "TurnFoldedInto on the PRIMARY loop commits a row",
			event:    event.TurnFoldedInto{Header: event.Header{LoopID: primary}, InputID: callID(1), Message: userMsg("folded")},
			wantRows: 1,
		},
		{
			name:     "TurnStarted on a SUBAGENT loop commits no row (its own initial task)",
			event:    event.TurnStarted{Header: event.Header{LoopID: subLoop}, InputID: callID(1), Message: userMsg("subagent task")},
			wantRows: 0,
		},
		{
			name:     "TurnFoldedInto on a SUBAGENT loop commits no row",
			event:    event.TurnFoldedInto{Header: event.Header{LoopID: subLoop}, InputID: callID(1), Message: userMsg("subagent fold")},
			wantRows: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := transcriptModel{primaryLoopID: primary}
			m = m.ApplyEvent(tt.event)
			if got := kindUserCount(m); got != tt.wantRows {
				t.Fatalf("kindUser rows = %d, want %d", got, tt.wantRows)
			}
		})
	}
}

// TestTranscriptQueuedAffordance locks the full queued-input lifecycle: RecordSubmit
// then InputQueued shows the affordance; a later TurnStarted promotes it to exactly
// one committed user row (from the event Message) and clears the affordance. It also
// covers the race where InputQueued arrives BEFORE RecordSubmit — the affordance
// stays hidden (blockless) until the blocks land, then shows.
func TestTranscriptQueuedAffordance(t *testing.T) {
	id := callID(0x42)

	t.Run("RecordSubmit then InputQueued then TurnStarted", func(t *testing.T) {
		t.Parallel()
		var m transcriptModel
		m = m.RecordSubmit(id, userBlocks("queued one"))
		// Not shown until InputQueued.
		if got := queuedTexts(m); len(got) != 0 {
			t.Fatalf("queued before InputQueued = %v, want none (not shown yet)", got)
		}
		m = m.ApplyEvent(event.InputQueued{InputID: id})
		if got := queuedTexts(m); len(got) != 1 || got[0] != "queued one" {
			t.Fatalf("queued after InputQueued = %v, want [queued one]", got)
		}
		// TurnStarted promotes to one committed row and clears the affordance.
		m = m.ApplyEvent(event.TurnStarted{InputID: id, Message: userMsg("queued one")})
		if got := kindUserCount(m); got != 1 {
			t.Errorf("kindUser rows = %d, want exactly 1 (promoted once)", got)
		}
		if got := queuedTexts(m); len(got) != 0 {
			t.Errorf("queued after TurnStarted = %v, want none (affordance cleared)", got)
		}
	})

	t.Run("InputQueued races ahead of RecordSubmit", func(t *testing.T) {
		t.Parallel()
		var m transcriptModel
		// InputQueued arrives first: a shown-but-blockless placeholder; render skips it.
		m = m.ApplyEvent(event.InputQueued{InputID: id})
		if got := queuedTexts(m); len(got) != 0 {
			t.Fatalf("queued with no blocks yet = %v, want none (blockless placeholder skipped)", got)
		}
		// RecordSubmit fills the blocks: now it shows.
		m = m.RecordSubmit(id, userBlocks("late blocks"))
		if got := queuedTexts(m); len(got) != 1 || got[0] != "late blocks" {
			t.Errorf("queued after late RecordSubmit = %v, want [late blocks]", got)
		}
	})
}

// TestTranscriptInputCancelled locks that InputCancelled drops the queued affordance
// and commits NO row — a retracted/returned input simply disappears from the pending
// area.
func TestTranscriptInputCancelled(t *testing.T) {
	t.Parallel()
	id := callID(0x55)

	var m transcriptModel
	m = m.RecordSubmit(id, userBlocks("cancel me"))
	m = m.ApplyEvent(event.InputQueued{InputID: id})
	if got := queuedTexts(m); len(got) != 1 {
		t.Fatalf("setup: queued = %v, want one", got)
	}
	m = m.ApplyEvent(event.InputCancelled{InputID: id, Reason: event.CancelClientRetracted, Message: userMsg("cancel me")})
	if got := queuedTexts(m); len(got) != 0 {
		t.Errorf("queued after InputCancelled = %v, want none (affordance dropped)", got)
	}
	if got := kindUserCount(m); got != 0 {
		t.Errorf("kindUser rows = %d, want 0 (cancelled input commits no row)", got)
	}
}

// TestTranscriptTurnRejected locks that TurnRejected drops the affordance AND commits
// an error notice naming the reason — a rejected message must not silently vanish.
func TestTranscriptTurnRejected(t *testing.T) {
	id := callID(0x66)

	tests := []struct {
		name   string
		reason event.RejectReason
		want   string
	}{
		{name: "busy", reason: event.RejectBusy, want: "loop busy"},
		{name: "queue full", reason: event.RejectQueueFull, want: "queue full"},
		{name: "shutting down", reason: event.RejectShuttingDown, want: "shutting down"},
		{name: "internal", reason: event.RejectInternal, want: "internal error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var m transcriptModel
			m = m.RecordSubmit(id, userBlocks("rejected"))
			m = m.ApplyEvent(event.InputQueued{InputID: id})
			m = m.ApplyEvent(event.TurnRejected{InputID: id, Reason: tt.reason})

			if got := queuedTexts(m); len(got) != 0 {
				t.Errorf("queued after TurnRejected = %v, want none (affordance dropped)", got)
			}
			if got := kindUserCount(m); got != 0 {
				t.Errorf("kindUser rows = %d, want 0 (a rejected message is surfaced as a notice, not a user row)", got)
			}
			rec := m.committed[len(m.committed)-1]
			if rec.Kind != kindNotice || rec.Level != noticeError {
				t.Fatalf("last committed = (kind %d, level %d), want (kindNotice, noticeError)", rec.Kind, rec.Level)
			}
			text := blockText(rec.Blocks[0])
			if !strings.Contains(text, tt.want) {
				t.Errorf("rejection notice = %q, want it to mention %q", text, tt.want)
			}
		})
	}
}

// TestTranscriptRecordSubmitValueCopy locks the value-copy contract on the queued
// slice: RecordSubmit returns a new model whose queue mutation does not alias the
// prior model's backing array — a parent transcriptModel value kept around stays
// unchanged after a child records another submit.
func TestTranscriptRecordSubmitValueCopy(t *testing.T) {
	t.Parallel()

	base := transcriptModel{}.RecordSubmit(callID(1), userBlocks("first"))
	base = base.ApplyEvent(event.InputQueued{InputID: callID(1)})

	// Branch a child off base, recording a second submit. base must not gain it.
	child := base.RecordSubmit(callID(2), userBlocks("second"))
	child = child.ApplyEvent(event.InputQueued{InputID: callID(2)})

	if got := queuedTexts(base); len(got) != 1 || got[0] != "first" {
		t.Errorf("base queued = %v, want [first] (child must not mutate base's backing array)", got)
	}
	if got := queuedTexts(child); len(got) != 2 {
		t.Errorf("child queued = %v, want two entries", got)
	}
}
