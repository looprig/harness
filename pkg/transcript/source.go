package transcript

import (
	"context"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/uuid"
)

// Record is one journaled item the builder folds in: an enduring Event or a
// Command. It is a sealed sum type — only EventRecord and CommandRecord satisfy
// it (the unexported marker prevents other packages from adding variants).
type Record interface{ isRecord() }

// EventRecord wraps an enduring loop event for the builder to fold in.
type EventRecord struct{ Event event.Event }

// CommandRecord wraps a user command for the builder to fold in.
type CommandRecord struct{ Command command.Command }

func (EventRecord) isRecord()   {}
func (CommandRecord) isRecord() {}

// RecordSource yields Records in journal-sequence order from the beginning,
// returning io.EOF once the stream is exhausted.
type RecordSource interface {
	Next(ctx context.Context) (Record, error)
}

// SystemPromptResolver supplies the live system-prompt text for a loop (see
// design Decision 4). ok is false when no prompt is available for the loop (e.g.
// a restored session whose live config is gone).
type SystemPromptResolver interface {
	SystemPrompt(loopID uuid.UUID) (text string, ok bool)
}
