package components

import (
	"path/filepath"
	"strings"

	"github.com/ciram-co/looprig/pkg/tui/styles"
)

// FileItem is one @path completion candidate. Path is the value to complete to (e.g.
// "src" or "src/main.go"); IsDir drives the trailing "/" affordance and whether
// selecting it keeps the panel open to drill in.
type FileItem struct {
	Path  string
	IsDir bool
}

// FileComplete is a filtered file list with a wrapping cursor — the @path completion
// panel, the disk-backed sibling of SlashComplete. It is display-only: package tui
// computes the candidate list (the filesystem read) and feeds it here.
type FileComplete struct {
	items  []FileItem
	cursor int
}

// NewFileComplete returns a completer over items, or nil when empty (nil = hidden).
func NewFileComplete(items []FileItem) *FileComplete {
	if len(items) == 0 {
		return nil
	}
	return &FileComplete{items: items, cursor: 0}
}

// Selected returns the item under the cursor.
func (f *FileComplete) Selected() FileItem { return f.items[f.cursor] }

// Up moves the cursor up, wrapping to the bottom.
func (f *FileComplete) Up() { f.cursor = (f.cursor - 1 + len(f.items)) % len(f.items) }

// Down moves the cursor down, wrapping to the top.
func (f *FileComplete) Down() { f.cursor = (f.cursor + 1) % len(f.items) }

// label is the displayed name for an item: its base name, plus a trailing "/" for a
// directory so a folder reads as drillable at a glance.
func (i FileItem) label() string {
	name := filepath.Base(i.Path)
	if i.IsDir {
		return name + "/"
	}
	return name
}

// View renders the filtered list, marking the cursor row (the same shape as
// SlashComplete.View so the @ panel and the / panel look identical).
func (f *FileComplete) View() string {
	rows := make([]string, len(f.items))
	for i, item := range f.items {
		if i == f.cursor {
			rows[i] = styles.UserStyle.Render("> " + item.label())
			continue
		}
		rows[i] = "  " + item.label()
	}
	return strings.Join(rows, "\n")
}
