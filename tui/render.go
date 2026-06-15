package tui

import (
	"fmt"
	"strings"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/tui/styles"
)

// rowSep separates transcript rows in the rendered output.
const rowSep = "\n\n"

// queuedMarker is appended to a user row that is still queued for sending.
const queuedMarker = " (queued)"

// renderMD renders markdown to ANSI for the given wrap width. On a glamour
// construction or render error it falls back to the raw text prefixed with the
// dot marker, so the UI always gets readable output and never an error.
func renderMD(md string, width int) string {
	if md == "" {
		return ""
	}

	r, err := styles.NewMarkdownRenderer(width)
	if err != nil {
		return styles.Dot + md
	}
	out, err := r.Render(md)
	if err != nil {
		return styles.Dot + md
	}
	return styles.Dot + strings.TrimRight(out, "\n")
}

// renderMessages renders the whole transcript to a single string. It dispatches
// on each message's DisplayRole and, within a row, on each block's concrete
// type. Rows whose index is in queued get a trailing marker. A non-empty live
// segment is appended as a trailing in-progress assistant row.
//
// The expandTools flag controls whether tool-call previews render collapsed or
// fully; it is threaded for later phases and currently unused — live.calls is
// always empty in this phase, so the live segment renders exactly its text, as
// the old trailing stream did.
func renderMessages(msgs []DisplayMessage, live liveSegment, queued map[int]bool, expandTools bool, width int) string {
	_ = expandTools // reserved for tool-card rendering (Phase 3/4)
	rows := make([]string, 0, len(msgs)+1)
	for i, m := range msgs {
		row := renderRow(m, width)
		if queued[i] {
			row += queuedMarker
		}
		rows = append(rows, row)
	}
	if live.text != "" {
		rows = append(rows, renderMD(live.text, width))
	}
	return strings.Join(rows, rowSep)
}

// renderRow renders a single transcript message according to its role.
func renderRow(m DisplayMessage, width int) string {
	switch m.Role {
	case RoleUser:
		return styles.UserStyle.Render(renderInlineBlocks(m.Blocks))
	case RoleAssistant:
		return renderMD(assistantText(m.Blocks), width)
	case RoleSystem:
		return styles.SystemStyle.Render(firstText(m.Blocks))
	case RoleError:
		return styles.ErrorStyle.Render(firstText(m.Blocks))
	case RoleInterrupted:
		return styles.InterruptedStyle.Render("└─ interrupted")
	default:
		return ""
	}
}

// renderInlineBlocks renders each block to plain text and joins with newlines.
// Used for user rows where blocks are shown verbatim (no markdown).
func renderInlineBlocks(blocks []content.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		parts = append(parts, renderBlock(blk))
	}
	return strings.Join(parts, "\n")
}

// assistantText concatenates the text of every TextBlock and renders any other
// block as its placeholder, joined with newlines, for markdown rendering.
func assistantText(blocks []content.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		parts = append(parts, renderBlock(blk))
	}
	return strings.Join(parts, "\n")
}

// firstText returns the text of the first TextBlock, or "" if there is none.
// Used by single-block roles (system, error).
func firstText(blocks []content.Block) string {
	for _, blk := range blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// renderBlock renders one block to its display string via a type switch over the
// sealed Block interface. Unknown types fall through to a safe placeholder.
func renderBlock(blk content.Block) string {
	switch b := blk.(type) {
	case *content.TextBlock:
		return b.Text
	case *content.ImageBlock:
		return fmt.Sprintf("[image: %s, %d bytes]", string(b.MediaType), len(b.Source.Data))
	default:
		return "[unsupported block]"
	}
}
