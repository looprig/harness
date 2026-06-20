package tui

import "math/rand/v2"

// tips are short one-line hints shown faint below the status row to teach the
// composer's affordances. One shows at a time and rotates each turn (Screen.tip,
// refreshed via nextTip on every turn terminal).
var tips = []string{
	"Shift+Enter (or Ctrl+J) inserts a newline; Enter sends.",
	"Attach a file by typing @path anywhere in your message.",
	"Type /help to list the available slash commands.",
	"/clear starts a fresh session; your scrollback stays.",
	"Press Esc to interrupt a running turn.",
	"Ctrl+T toggles full thinking + tool detail on or off.",
	"Ctrl+C exits.",
}

// nextTip returns a random tip other than current, so the hint visibly changes from one
// turn to the next. It degrades gracefully for a zero- or one-element set.
func nextTip(current string) string {
	switch len(tips) {
	case 0:
		return ""
	case 1:
		return tips[0]
	}
	for {
		// #nosec G404 -- tips are cosmetic UI hints, not security-sensitive.
		t := tips[rand.IntN(len(tips))]
		if t != current {
			return t
		}
	}
}
