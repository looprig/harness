package tui

import "strconv"

// String renders a displayID as its decimal form. It is the stable key form used
// by renderers and print-once bookkeeping. It is defined here (rather than in
// transcript.go) because scrollbackModel is its first consumer.
func (id displayID) String() string {
	return strconv.FormatUint(uint64(id), 10)
}

// printAction is one entry's worth of fully rendered scrollback lines, tagged with
// the entry's stable ID. A later task turns each printAction into a tea.Println
// command that emits Lines to the native terminal scrollback.
type printAction struct {
	EntryID displayID
	Lines   []string
}

// scrollbackModel is the print-once engine: it tracks which committed entries have
// already been emitted to scrollback so each entry is printed exactly once, even
// across resize, re-render, or duplicate flush events. width is stored for the
// future real entry renderer and is not consulted by the print-once logic itself.
type scrollbackModel struct {
	printed map[displayID]bool
	width   int
}

// newScrollbackModel returns an empty print-once engine sized to the given width.
func newScrollbackModel(width int) scrollbackModel {
	return scrollbackModel{printed: make(map[displayID]bool), width: width}
}

// Flush renders every committed entry not yet printed, in order, and returns the
// next model plus the new print actions. Each entry's rendered lines are terminated
// with exactly one trailing blank line so consecutive entries are separated by a
// single blank line in scrollback (§Spacing). Already-printed entries are skipped
// (print-once). Flush is a pure reducer: it copies the printed map so neither the
// input model nor the input committed slice is mutated.
func (s scrollbackModel) Flush(committed []entry, render func(entry) []string) (scrollbackModel, []printAction) {
	printed := make(map[displayID]bool, len(s.printed))
	for id := range s.printed {
		printed[id] = true
	}
	next := scrollbackModel{printed: printed, width: s.width}

	var actions []printAction
	for _, e := range committed {
		if printed[e.ID] {
			continue
		}
		lines := append(render(e), "")
		actions = append(actions, printAction{EntryID: e.ID, Lines: lines})
		printed[e.ID] = true
	}
	return next, actions
}
