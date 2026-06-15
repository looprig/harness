package components

import (
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// ChatHistory is a scrolling viewport over pre-rendered transcript text. It sticks
// to the bottom (newest) only when the user is already scrolled to the bottom, so
// new content doesn't yank the view away while the user is reading history.
//
// It is a dumb widget: it takes already-rendered content as a string and knows
// nothing about messages or markdown. Package tui renders the transcript and feeds
// the resulting string here.
type ChatHistory struct {
	vp viewport.Model
}

// NewChatHistory returns a viewport sized to the given dimensions.
func NewChatHistory(width, height int) ChatHistory {
	vp := viewport.New()
	vp.SetWidth(width)
	vp.SetHeight(height)
	return ChatHistory{vp: vp}
}

// SetContent replaces the rendered transcript. If the view was at the bottom
// before (or this is the first content), it re-pins to the bottom; otherwise it
// preserves the user's scroll position.
func (h *ChatHistory) SetContent(s string) {
	wasBottom := h.vp.AtBottom()
	h.vp.SetContent(s)
	if wasBottom {
		h.vp.GotoBottom()
	}
}

// Resize sets the viewport dimensions.
func (h *ChatHistory) Resize(width, height int) {
	h.vp.SetWidth(width)
	h.vp.SetHeight(height)
}

// Clear empties the content and returns to the top.
func (h *ChatHistory) Clear() {
	h.vp.SetContent("")
	h.vp.GotoTop()
}

// Update forwards scroll/key/mouse messages to the viewport.
func (h *ChatHistory) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	h.vp, cmd = h.vp.Update(msg)
	return cmd
}

// View renders the viewport.
func (h *ChatHistory) View() string {
	return h.vp.View()
}

// AtBottom reports whether the view is scrolled to the bottom (exposed for
// tests/Screen).
func (h *ChatHistory) AtBottom() bool {
	return h.vp.AtBottom()
}
