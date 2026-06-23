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
// single blank line in scrollback (§Spacing) — EXCEPT where the next entry attaches
// tightly to this one (attachesTight): a tool card belongs to the assistant/tool
// step group above it, so the assistant bullet and its "⎿ …" cards render with no
// intervening blank line, reading as one message (matching the live tail). Already-
// printed entries are skipped (print-once). Flush is a pure reducer: it copies the
// printed map so neither the input model nor the input committed slice is mutated.
func (s scrollbackModel) Flush(committed []entry, render func(entry) []string) (scrollbackModel, []printAction) {
	printed := make(map[displayID]bool, len(s.printed))
	for id := range s.printed {
		printed[id] = true
	}
	next := scrollbackModel{printed: printed, width: s.width}

	var actions []printAction
	for i, e := range committed {
		if printed[e.ID] {
			continue
		}
		lines := render(e)
		if !attachesTight(committed, i) {
			lines = append(lines, "")
		}
		actions = append(actions, printAction{EntryID: e.ID, Lines: lines})
		printed[e.ID] = true
	}
	return next, actions
}

// attachesTight reports whether the entry following committed[i] renders flush
// against it with NO separating blank line — i.e. the next entry is a tool card
// (kindTool) that belongs to the same assistant/tool step group as committed[i].
// A promoted tool card renders AS its own assistant bullet, so it STARTS a new group
// and is never tight; every non-promoted tool card commits directly beneath its
// step's assistant narration / "Multiple actions" umbrella (or a sibling card), so
// it attaches tightly. A reconciled Subagent card (Agent set) is its OWN "●"-level
// card (renderSubagentCard), NOT a tool child of the message above, so it is never
// tight — it gets the same blank-line separation as an assistant bullet (otherwise it
// glues to the orchestrator's thinking/narration). Returns false at the tail.
func attachesTight(committed []entry, i int) bool {
	j := i + 1
	if j >= len(committed) {
		return false
	}
	next := committed[j]
	return next.Kind == kindTool && !next.promoted && !isSubagentEntry(next)
}

// isSubagentEntry reports whether a committed entry is a reconciled Subagent card —
// a single-call kindTool entry carrying an Agent label (the same discriminator
// renderEntry uses to route to renderSubagentCard). Such a card renders at the "●"
// level, so the scrollback spacing treats it like an assistant bullet, not a tool child.
func isSubagentEntry(e entry) bool {
	return e.Kind == kindTool && len(e.Calls) == 1 && e.Calls[0].Agent != ""
}
