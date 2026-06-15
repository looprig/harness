package tui

import (
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
)

// displayID is a stable, monotonically assigned identifier for a committed
// transcript entry. It is allocated once when a live segment is committed and
// never reused, so a renderer can key on it across re-renders. The zero value is
// never a valid assigned ID — the first commit allocates 1.
type displayID uint64

// entryKind discriminates the source/kind of a committed transcript entry. Only
// user and assistant are defined here; tool/error/interrupted kinds arrive in a
// later task (this skeleton commits only assistant segments).
type entryKind uint8

const (
	kindUser entryKind = iota
	kindAssistant
)

// entry is one committed (finalized) row of the transcript. It stores the minimal
// data needed to render the row later: its stable ID, its kind, and the content
// blocks captured when the live segment was committed. Calls holds the tool-call
// children of an assistant segment; it is left unpopulated in this task (tool-call
// reconstruction lands in a later task).
type entry struct {
	ID     displayID
	Kind   entryKind
	Blocks []content.Block
	Calls  []ToolCallView
}

// liveSeg is the in-progress assistant segment for the current turn: the streamed
// reasoning (Thinking) and narration (Text) plus the tool calls reconstructed from
// the event stream. It is committed to an entry when the turn ends. Calls stays
// empty until the event-reconstruction state machine populates it (a later task).
// active marks that a turn is in progress. It is the transcriptModel's own segment
// type, distinct from screen.go's legacy liveSegment until Task 13 unifies them.
type liveSeg struct {
	Thinking string
	Text     string
	Calls    []ToolCallView
	active   bool
}

// empty reports whether the live segment carries no committable content — no
// streamed reasoning, no streamed narration, and no reconstructed tool call.
// active is intentionally not consulted: an active-but-content-less segment is
// still empty and must not commit.
func (s liveSeg) empty() bool {
	return s.Thinking == "" && s.Text == "" && len(s.Calls) == 0
}

// transcriptModel is the pure, side-effect-free reducer over a turn's event
// stream. committed holds finalized entries in display order; live is the
// in-progress segment for the current turn; nextID is the next stable ID to
// allocate. It is applied by value: ApplyEvent returns the next model.
type transcriptModel struct {
	committed []entry
	live      liveSeg
	nextID    displayID
}

// ApplyEvent folds one turn-stream event into the model and returns the next
// model. It handles only TurnStarted (begin/keep a live assistant segment),
// TokenDelta (route *content.TextChunk → live.Text, *content.ThinkingChunk →
// live.Thinking), and TurnDone (commit a non-empty live segment into exactly one
// entry, then reset live). Every other event type is a no-op here — tool-call
// cards and the TurnInterrupted/TurnFailed terminals land in a later task. The
// chunk routing mirrors the current screen.go handleEvent logic.
func (m transcriptModel) ApplyEvent(ev event.Event) transcriptModel {
	switch ev := ev.(type) {
	case event.TurnStarted:
		m.live.active = true
	case event.TokenDelta:
		m.applyChunk(ev.Chunk)
	case event.TurnDone:
		m.commitLive()
	}
	return m
}

// applyChunk routes one streamed chunk into the live segment: text accumulates
// into live.Text, thinking into live.Thinking. Any other chunk variant (e.g. a
// tool-use chunk) is skipped — tool-call reconstruction is a later task.
func (m *transcriptModel) applyChunk(c content.Chunk) {
	switch chunk := c.(type) {
	case *content.TextChunk:
		m.live.Text += chunk.Text
	case *content.ThinkingChunk:
		m.live.Thinking += chunk.Thinking
	}
}

// commitLive appends the live segment to committed as one kindAssistant entry
// carrying its reasoning and narration as blocks, allocates its stable ID, and
// resets the live segment. It is a no-op when the live segment is empty. Reasoning
// is stored as a leading ThinkingBlock so the streamed and final-message render
// paths agree; empty blocks are omitted so only present content survives. This
// ports commitLive from the current screen.go.
func (m *transcriptModel) commitLive() {
	if m.live.empty() {
		m.live = liveSeg{}
		return
	}
	var blocks []content.Block
	if m.live.Thinking != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: m.live.Thinking})
	}
	if m.live.Text != "" {
		blocks = append(blocks, &content.TextBlock{Text: m.live.Text})
	}
	m.nextID++
	m.committed = append(m.committed, entry{
		ID:     m.nextID,
		Kind:   kindAssistant,
		Blocks: blocks,
		Calls:  m.live.Calls,
	})
	m.live = liveSeg{}
}
