package tui

import (
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

func TestRenderMD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		md       string
		width    int
		wantWord string // substring expected in the output (empty → expect blank)
	}{
		{name: "happy path", md: "hello world", width: 80, wantWord: "hello"},
		{name: "heading", md: "# Title here", width: 80, wantWord: "Title"},
		{name: "narrow width", md: "wrapme please", width: 10, wantWord: "wrapme"},
		{name: "empty", md: "", width: 80, wantWord: ""},
		{name: "zero width", md: "zerowidth", width: 0, wantWord: "zerowidth"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := renderMD(tt.md, tt.width)
			if tt.wantWord == "" {
				if strings.TrimSpace(got) != "" {
					t.Errorf("renderMD(%q) = %q, want empty/whitespace", tt.md, got)
				}
				return
			}
			if !strings.Contains(got, tt.wantWord) {
				t.Errorf("renderMD(%q) = %q, want to contain %q", tt.md, got, tt.wantWord)
			}
		})
	}
}

func TestRenderMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		msgs   []DisplayMessage
		live   liveSegment
		queued map[int]bool
		want   []string // substrings that must all appear in the output
	}{
		{
			name: "user text",
			msgs: []DisplayMessage{
				{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "hello from user"}}},
			},
			want: []string{"hello from user"},
		},
		{
			name: "user image placeholder",
			msgs: []DisplayMessage{
				{Role: RoleUser, Blocks: []content.Block{
					&content.ImageBlock{
						MediaType: content.MediaTypeImagePNG,
						Source:    content.ImageSource{Data: make([]byte, 12)},
					},
				}},
			},
			want: []string{"[image: image/png, 12 bytes]"},
		},
		{
			name: "assistant markdown",
			msgs: []DisplayMessage{
				{Role: RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "assistant reply text"}}},
			},
			want: []string{"assistant reply text"},
		},
		{
			name: "assistant concatenates text blocks",
			msgs: []DisplayMessage{
				{Role: RoleAssistant, Blocks: []content.Block{
					&content.TextBlock{Text: "alpha"},
					&content.TextBlock{Text: "beta"},
				}},
			},
			want: []string{"alpha", "beta"},
		},
		{
			name: "system",
			msgs: []DisplayMessage{
				{Role: RoleSystem, Blocks: []content.Block{&content.TextBlock{Text: "system notice"}}},
			},
			want: []string{"system notice"},
		},
		{
			name: "error",
			msgs: []DisplayMessage{
				{Role: RoleError, Blocks: []content.Block{&content.TextBlock{Text: "boom failure"}}},
			},
			want: []string{"boom failure"},
		},
		{
			name: "interrupted nil blocks",
			msgs: []DisplayMessage{
				{Role: RoleInterrupted, Blocks: nil},
			},
			want: []string{"interrupted"},
		},
		{
			name: "queued marker",
			msgs: []DisplayMessage{
				{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "do this later"}}},
			},
			queued: map[int]bool{0: true},
			want:   []string{"do this later", "(queued)"},
		},
		{
			name: "live stream only",
			msgs: nil,
			live: liveSegment{text: "partial answer"},
			want: []string{"partial answer"},
		},
		{
			name: "stream appended after messages",
			msgs: []DisplayMessage{
				{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "question"}}},
			},
			live: liveSegment{text: "streaming reply"},
			want: []string{"question", "streaming reply"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := renderMessages(tt.msgs, tt.live, tt.queued, false, 80)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("renderMessages() = %q, want to contain %q", got, w)
				}
			}
		})
	}
}

// TestRenderMessagesNoStream verifies an empty stream is not appended as a row.
func TestRenderMessagesNoStream(t *testing.T) {
	t.Parallel()

	got := renderMessages(nil, liveSegment{}, nil, false, 80)
	if strings.TrimSpace(got) != "" {
		t.Errorf("renderMessages(nil, liveSegment{}, ...) = %q, want empty", got)
	}
}
