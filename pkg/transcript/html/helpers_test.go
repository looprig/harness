package html

import (
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/transcript"
)

func TestMessageText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *transcript.Message
		want string
	}{
		{
			name: "nil message",
			msg:  nil,
			want: "",
		},
		{
			name: "no blocks",
			msg:  &transcript.Message{Role: content.RoleUser},
			want: "",
		},
		{
			name: "single text block",
			msg:  &transcript.Message{Blocks: []content.Block{&content.TextBlock{Text: "hello"}}},
			want: "hello",
		},
		{
			name: "multiple text blocks newline-joined",
			msg: &transcript.Message{Blocks: []content.Block{
				&content.TextBlock{Text: "first"},
				&content.TextBlock{Text: "second"},
			}},
			want: "first\nsecond",
		},
		{
			name: "skips thinking and tool-use blocks",
			msg: &transcript.Message{Blocks: []content.Block{
				&content.ThinkingBlock{Thinking: "secret reasoning"},
				&content.TextBlock{Text: "visible"},
				&content.ToolUseBlock{ID: "tu1", Name: "Bash"},
			}},
			want: "visible",
		},
		{
			name: "only non-text blocks yields empty",
			msg: &transcript.Message{Blocks: []content.Block{
				&content.ThinkingBlock{Thinking: "secret"},
				&content.ToolUseBlock{ID: "tu1", Name: "Bash"},
			}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := messageText(tt.msg); got != tt.want {
				t.Errorf("messageText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMessageTime(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC)
	fallback := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		msg      *transcript.Message
		fallback time.Time
		want     time.Time
	}{
		{
			name:     "nil message falls back",
			msg:      nil,
			fallback: fallback,
			want:     fallback,
		},
		{
			name:     "zero message time falls back",
			msg:      &transcript.Message{},
			fallback: fallback,
			want:     fallback,
		},
		{
			name:     "non-zero message time wins",
			msg:      &transcript.Message{At: at},
			fallback: fallback,
			want:     at,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := messageTime(tt.msg, tt.fallback); !got.Equal(tt.want) {
				t.Errorf("messageTime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatClock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{name: "zero time empty", in: time.Time{}, want: ""},
		{name: "fixed time", in: time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC), want: "10:00:02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatClock(tt.in); got != tt.want {
				t.Errorf("formatClock() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{name: "zero time em dash", in: time.Time{}, want: "—"},
		{name: "fixed time", in: time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC), want: "2026-06-28 10:00:00 UTC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatTimestamp(tt.in); got != tt.want {
				t.Errorf("formatTimestamp() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNewTurnView asserts the multi-step, empty, and nil-User branches directly,
// rather than only through the single happy-path golden.
func TestNewTurnView(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC)
	aiStep := func(text string) *transcript.Step {
		return &transcript.Step{AI: &transcript.Message{
			Role:   content.RoleAssistant,
			At:     at,
			Blocks: []content.Block{&content.TextBlock{Text: text}},
		}}
	}

	tests := []struct {
		name      string
		turn      *transcript.Turn
		wantSteps int
	}{
		{
			name: "nil user, no steps",
			turn: &transcript.Turn{Index: 1, StartedAt: at},
		},
		{
			name: "user but empty (no steps)",
			turn: &transcript.Turn{
				Index:     2,
				StartedAt: at,
				User:      &transcript.Message{Blocks: []content.Block{&content.TextBlock{Text: "hi"}}},
			},
		},
		{
			name: "two steps",
			turn: &transcript.Turn{
				Index:     3,
				StartedAt: at,
				User:      &transcript.Message{Blocks: []content.Block{&content.TextBlock{Text: "go"}}},
				Steps:     []*transcript.Step{aiStep("one"), aiStep("two")},
			},
			wantSteps: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tv, err := newTurnView("operator", tt.turn)
			if err != nil {
				t.Fatalf("newTurnView() error = %v", err)
			}
			if len(tv.Steps) != tt.wantSteps {
				t.Errorf("len(Steps) = %d, want %d", len(tv.Steps), tt.wantSteps)
			}
			// nil User yields empty rendered HTML; a present User yields non-empty.
			gotEmptyUser := tv.User == ""
			wantEmptyUser := tt.turn.User == nil
			if gotEmptyUser != wantEmptyUser {
				t.Errorf("User empty = %v, want %v (User=%q)", gotEmptyUser, wantEmptyUser, tv.User)
			}
			for i, sv := range tv.Steps {
				if sv.AgentName != "operator" {
					t.Errorf("step %d AgentName = %q, want %q", i, sv.AgentName, "operator")
				}
				if sv.AI == "" {
					t.Errorf("step %d AI HTML is empty", i)
				}
			}
		})
	}
}
