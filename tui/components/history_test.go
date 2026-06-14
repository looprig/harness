package components

import (
	"strings"
	"testing"
)

func TestChatHistorySetContent(t *testing.T) {
	t.Parallel()

	h := NewChatHistory(80, 10)
	h.SetContent("line1\nline2")

	got := h.View()
	if got == "" {
		t.Fatalf("View() = empty, want non-empty after SetContent")
	}
	if !strings.Contains(got, "line1") {
		t.Errorf("View() = %q, want to contain %q", got, "line1")
	}
}

func TestChatHistoryClear(t *testing.T) {
	t.Parallel()

	h := NewChatHistory(80, 10)
	h.SetContent("secret-content")
	h.Clear()

	if strings.Contains(h.View(), "secret-content") {
		t.Errorf("View() still contains cleared content %q", "secret-content")
	}
}

func TestChatHistoryResize(t *testing.T) {
	t.Parallel()

	h := NewChatHistory(80, 10)
	h.SetContent("line1\nline2\nline3")
	h.Resize(40, 5)

	// Must not panic and must still render.
	if h.View() == "" {
		t.Errorf("View() = empty after Resize, want non-empty")
	}
}

func TestChatHistoryBottomStick(t *testing.T) {
	t.Parallel()

	// Content far taller than the viewport: 50 lines into height 5.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("line\n")
	}

	h := NewChatHistory(80, 5)

	// A fresh viewport starts at the bottom (offset 0, no content), so SetContent
	// re-pins to the bottom of the new tall content.
	if !h.AtBottom() {
		t.Fatalf("AtBottom() = false on fresh viewport, want true")
	}

	h.SetContent(b.String())

	if !h.AtBottom() {
		t.Errorf("AtBottom() = false after SetContent from bottom, want true (should re-pin)")
	}
}
