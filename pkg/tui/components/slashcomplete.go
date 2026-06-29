package components

import (
	"strings"

	"github.com/ciram-co/looprig/pkg/tui/styles"
)

// SlashCmd is one slash command's display metadata. The action is dispatched by
// package tui keyed on Name; this widget only filters and displays.
type SlashCmd struct {
	Name string // e.g. "/clear"
	Desc string // e.g. "clear the conversation"
}

// CmdExport is the canonical name of the transcript-export slash command. It is the
// single source of truth shared by the SlashCommands entry below and package tui's
// dispatch (slashAction + runSlash), so the command name cannot drift between the
// completer table and the action that handles it.
const CmdExport = "/export"

// SlashCommands is the canonical list (exported so package tui can map Name→action).
var SlashCommands = []SlashCmd{
	{"/clear", "clear the conversation"},
	{"/help", "list commands"},
	{CmdExport, "export session transcript to HTML"},
}

// SlashComplete is a filtered command list with a wrapping cursor.
type SlashComplete struct {
	items  []SlashCmd
	cursor int
}

// NewSlashComplete returns a completer for the commands whose Name has prefix
// (case-sensitive, prefix includes the leading '/'). Returns nil when nothing
// matches (nil = panel hidden).
func NewSlashComplete(prefix string) *SlashComplete {
	var matches []SlashCmd
	for _, c := range SlashCommands {
		if strings.HasPrefix(c.Name, prefix) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	return &SlashComplete{items: matches, cursor: 0}
}

// Selected returns the item under the cursor.
func (s *SlashComplete) Selected() SlashCmd {
	return s.items[s.cursor]
}

// Up moves the cursor up, wrapping to the bottom.
func (s *SlashComplete) Up() {
	s.cursor = (s.cursor - 1 + len(s.items)) % len(s.items)
}

// Down moves the cursor down, wrapping to the top.
func (s *SlashComplete) Down() {
	s.cursor = (s.cursor + 1) % len(s.items)
}

// View renders the filtered list, marking the cursor row.
func (s *SlashComplete) View() string {
	rows := make([]string, len(s.items))
	for i, item := range s.items {
		row := "  " + item.Name + "  " + item.Desc
		if i == s.cursor {
			rows[i] = styles.UserStyle.Render("> " + item.Name + "  " + item.Desc)
			continue
		}
		rows[i] = row
	}
	return strings.Join(rows, "\n")
}
