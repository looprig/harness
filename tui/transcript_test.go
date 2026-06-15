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
				// commit-once: completed call already committed and removed from live;
				// TurnDone's flush must NOT re-commit it.
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want exactly 1 (commit-once, no duplication)", len(m.committed))
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
			name: "ToolCallCompleted (ok) commits one kindTool entry and removes it from live",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(1), "Bash", "ls"),
				toolCompleted(callID(1), false, "file1\nfile2"),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.live.Calls) != 0 {
					t.Fatalf("live.Calls = %d, want 0 (commit-once removes it)", len(m.live.Calls))
				}
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want exactly 1", len(m.committed))
				}
				e := m.committed[0]
				if e.Kind != kindTool {
					t.Errorf("Kind = %v, want kindTool", e.Kind)
				}
				if e.ID == 0 {
					t.Errorf("entry ID = 0, want nonzero stable ID")
				}
				if len(e.Calls) != 1 {
					t.Fatalf("entry Calls = %d, want 1", len(e.Calls))
				}
				c := e.Calls[0]
				if c.Status != ToolOK {
					t.Errorf("Status = %v, want ToolOK", c.Status)
				}
				if len(c.Result) != 2 || c.Result[0] != "file1" || c.Result[1] != "file2" {
					t.Errorf("Result = %#v, want [file1 file2]", c.Result)
				}
			},
		},
		{
			name: "ToolCallCompleted (error) commits one kindTool entry in ToolError",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(2), "Fetch", "GET /x"),
				toolCompleted(callID(2), true, "boom"),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.live.Calls) != 0 {
					t.Fatalf("live.Calls = %d, want 0", len(m.live.Calls))
				}
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1", len(m.committed))
				}
				c := m.committed[0].Calls[0]
				if c.Status != ToolError {
					t.Errorf("Status = %v, want ToolError", c.Status)
				}
				if len(c.Result) != 1 || c.Result[0] != "boom" {
					t.Errorf("Result = %#v, want [boom]", c.Result)
				}
			},
		},
		{
			name: "two tool calls commit two distinct kindTool entries with distinct IDs",
			events: []event.Event{
				event.TurnStarted{},
				toolStarted(callID(1), "Bash", "a"),
				toolCompleted(callID(1), false, "out1"),
				toolStarted(callID(2), "Bash", "b"),
				toolCompleted(callID(2), false, "out2"),
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 2 {
					t.Fatalf("committed = %d, want 2", len(m.committed))
				}
				if m.committed[0].Kind != kindTool || m.committed[1].Kind != kindTool {
					t.Errorf("kinds = %v,%v, want kindTool both", m.committed[0].Kind, m.committed[1].Kind)
				}
				if m.committed[0].ID == m.committed[1].ID {
					t.Errorf("tool entry IDs not distinct: both %d", m.committed[0].ID)
				}
				if len(m.live.Calls) != 0 {
					t.Errorf("live.Calls = %d, want 0", len(m.live.Calls))
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

// TestTranscriptOrdering locks the append-only scrollback ordering rule: prose
// streamed BEFORE a tool call is committed as its own assistant entry ahead of the
// tool card, and prose streamed AFTER lands in a later assistant entry — yielding
// the natural reading order prose1 → tool card → prose2.
func TestTranscriptOrdering(t *testing.T) {
	t.Parallel()
	var m transcriptModel
	for _, ev := range []event.Event{
		event.TurnStarted{},
		textChunk("before tool"),
		toolStarted(callID(1), "Bash", "run"),
		toolCompleted(callID(1), false, "done"),
		textChunk("after tool"),
		event.TurnDone{},
	} {
		m = m.ApplyEvent(ev)
	}
	if len(m.committed) != 3 {
		t.Fatalf("committed = %d, want 3 (prose1, tool, prose2)", len(m.committed))
	}
	// [0] assistant prose committed BEFORE the tool card.
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
	// [2] the trailing prose, AFTER the tool card.
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
			name: "TurnFailed commits prose then appends error entry carrying the message",
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
				if m.committed[1].Kind != kindError {
					t.Fatalf("committed[1].Kind = %v, want kindError", m.committed[1].Kind)
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
			name: "TurnFailed with nil Err still appends an error entry and resets live",
			events: []event.Event{
				event.TurnStarted{},
				event.TurnFailed{Err: nil},
			},
			want: func(t *testing.T, m transcriptModel) {
				if len(m.committed) != 1 {
					t.Fatalf("committed = %d, want 1 (error only)", len(m.committed))
				}
				if m.committed[0].Kind != kindError {
					t.Errorf("Kind = %v, want kindError", m.committed[0].Kind)
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
