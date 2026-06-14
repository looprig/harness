package event

import "github.com/inventivepotter/urvi/internal/content"

// TurnStarted is the first event written to StartTurn.Events.
type TurnStarted struct{ TurnIndex TurnIndex }

// TokenDelta is emitted for each streaming chunk from the LLM.
type TokenDelta struct {
	TurnIndex TurnIndex
	Chunk     content.Chunk
}

// TurnDone is the terminal success event. Message is the complete AI response.
type TurnDone struct {
	TurnIndex TurnIndex
	Message   *content.AIMessage
}

// TurnFailed is the terminal event for non-cancellation LLM/provider errors.
// On failure the user message is rolled back from history. Err carries the
// typed cause; callers may errors.As it to inspect and retry.
type TurnFailed struct {
	TurnIndex TurnIndex
	Err       error
}

// TurnInterrupted is the terminal event when the turn context is cancelled.
// The user message for the cancelled turn is rolled back from history.
type TurnInterrupted struct{ TurnIndex TurnIndex }

func (TurnStarted) isEvent()     {}
func (TokenDelta) isEvent()      {}
func (TurnDone) isEvent()        {}
func (TurnFailed) isEvent()      {}
func (TurnInterrupted) isEvent() {}
