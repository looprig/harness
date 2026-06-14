package tui

import "github.com/inventivepotter/urvi/tui/styles"

// RenderStatusLine returns the one-line status indicator for the given status.
// StatusIdle renders to the empty string (unstyled); every other status renders
// its label through styles.StatusStyle.
func RenderStatusLine(s Status) string {
	switch s {
	case StatusRunning:
		return styles.StatusStyle.Render("thinking…")
	case StatusInterrupting:
		return styles.StatusStyle.Render("interrupting…")
	case StatusResetting:
		return styles.StatusStyle.Render("clearing…")
	case StatusIdle:
		return ""
	default:
		return ""
	}
}
