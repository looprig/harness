package journal

import (
	"context"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
)

type hookedJournal struct {
	journal   SessionJournal
	hooks     *hook.Runner
	sessionID uuid.UUID
}

// WithHooks observes each durable append while preserving the journal's result.
func WithHooks(j SessionJournal, runner *hook.Runner, sessionID uuid.UUID) SessionJournal {
	if j == nil {
		return nil
	}
	return &hookedJournal{journal: j, hooks: runner, sessionID: sessionID}
}

func (j *hookedJournal) Append(ctx context.Context, record JournalRecord) (uint64, error) {
	call := hook.Call{
		Operation:   hook.OperationJournalAppend,
		StartedAt:   time.Now(),
		Coordinates: identity.Coordinates{SessionID: j.sessionID},
		JournalAppend: &hook.JournalAppendData{
			Family:   recordFamily(record),
			RecordID: record.IdempotencyID(),
		},
	}
	hookCtx, finish, startErr := j.hooks.Start(ctx, call)
	if startErr != nil {
		hookCtx = ctx
		finish = func(hook.Result) {}
	}
	seq, err := j.journal.Append(hookCtx, record)
	outcome := hook.OutcomeCompleted
	switch {
	case hookCtx.Err() != nil:
		outcome = hook.OutcomeCanceled
	case err != nil:
		outcome = hook.OutcomeFailed
	}
	finish(hook.Result{
		Call:    call,
		EndedAt: time.Now(),
		Outcome: outcome,
		Err:     err,
	})
	return seq, err
}

func recordFamily(record JournalRecord) hook.RecordFamily {
	switch record.(type) {
	case EventRecord, *EventRecord:
		return hook.RecordEvent
	case CommandRecord, *CommandRecord:
		return hook.RecordCommand
	case GatePreparedRecord, *GatePreparedRecord:
		return hook.RecordGatePrepared
	case FenceRecord, *FenceRecord:
		return hook.RecordFence
	default:
		panic("journal: unknown sealed record family")
	}
}
