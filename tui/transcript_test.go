package tui

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
)

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
