package tea

import "github.com/charmbracelet/x/ansi"

// chunkOffset returns the number of screen rows a slice of logical lines
// occupies when written via insertAbove. It mirrors the exact row model used
// by (*cursedRenderer).insertAbove: every line counts as 1 (including blank
// lines), and a line strictly wider than the terminal width w adds an extra
// (lineWidth / w) rows for its soft-wraps. A width of 0 (or less) disables the
// wrap contribution entirely, matching insertAbove's `w > 0` guard.
//
// This is factored out so paging stays self-consistent with insertAbove: both
// must agree on how many rows a payload consumes.
func chunkOffset(lines []string, w int) int {
	offset := len(lines)
	for _, line := range lines {
		lineWidth := ansi.StringWidth(line)
		if w > 0 && lineWidth > w {
			offset += (lineWidth / w)
		}
	}
	return offset
}

// pageByOffset greedily splits lines into consecutive chunks such that each
// chunk's chunkOffset(chunk, w) is <= capRows. Lines are added to the current
// chunk until adding the next line would exceed capRows, at which point a new
// chunk is started.
//
// A single line whose own offset already exceeds capRows is emitted as its own
// one-element chunk (we do not split a single line in this task). Operating on
// []string preserves blank lines as real chunk members, and concatenating the
// returned chunks in order reproduces the input slice exactly. An empty input
// yields no chunks.
func pageByOffset(lines []string, w, capRows int) [][]string {
	var pages [][]string
	var cur []string

	for _, line := range lines {
		lineOffset := chunkOffset([]string{line}, w)

		// If this single line alone exceeds capRows, flush the current chunk
		// and emit the line as its own chunk (cannot be split in this task).
		if lineOffset > capRows {
			if len(cur) > 0 {
				pages = append(pages, cur)
				cur = nil
			}
			pages = append(pages, []string{line})
			continue
		}

		// If appending this line would push the current chunk over capRows,
		// flush the current chunk first.
		if len(cur) > 0 && chunkOffset(cur, w)+lineOffset > capRows {
			pages = append(pages, cur)
			cur = nil
		}

		cur = append(cur, line)
	}

	if len(cur) > 0 {
		pages = append(pages, cur)
	}

	return pages
}
