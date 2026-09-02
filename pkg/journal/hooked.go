package journal

import (
	"context"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
)

// AppendFunc is one journal append operation.
type AppendFunc func(context.Context, JournalRecord) (uint64, error)

// AppendMiddleware decorates one AppendFunc. Implementations must invoke next
// synchronously exactly once with the supplied record and return its exact
// sequence and error.
type AppendMiddleware func(next AppendFunc) AppendFunc

type appendPanicError struct{}

func (*appendPanicError) Error() string { return "journal: append panicked" }

// WithHooks observes each durable append while preserving the journal's result AND its
// optional contracts. Observation must not amputate capability: a decorator that
// exposed only Append would silently demote an idempotent, committed-bytes journal to a
// plain one for every deployment that configures a journal-append hook — the hub would
// lose the Appended=false signal and re-broadcast deduplicated retries, and the
// segregated committed-public-event capability would vanish. Decorate owns that
// preservation for every decorator in the workspace; this function supplies only the
// observation.
func WithHooks(j SessionJournal, runner *hook.Runner, sessionID uuid.UUID) SessionJournal {
	if j == nil {
		return nil
	}
	if !runner.Handles(hook.OperationJournalAppend) {
		return j
	}
	return Decorate(j, observeAppend(runner, sessionID))
}

// observeAppend builds the journal-append hook's AroundAppend. Because it decorates by
// error rather than by result type, ONE implementation observes every append seam — the
// plain sequence, the idempotent result, and the committed result — so an added seam
// cannot acquire a subtly different observation. A record whose metadata cannot be
// derived without panicking, and a runner that refuses to start, both delegate
// unobserved: the append itself is never blocked by observation.
func observeAppend(runner *hook.Runner, sessionID uuid.UUID) AroundAppend {
	return func(ctx context.Context, record JournalRecord, next func(context.Context) error) error {
		family, recordID, observable := describeRecord(record)
		if !observable {
			return next(ctx)
		}
		call := hook.Call{
			Operation:   hook.OperationJournalAppend,
			StartedAt:   time.Now(),
			Coordinates: identity.Coordinates{SessionID: sessionID},
			JournalAppend: &hook.JournalAppendData{
				Family:   family,
				RecordID: recordID,
			},
		}
		hookCtx, finish, startErr := runner.Start(ctx, call)
		if startErr != nil {
			return next(ctx)
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				finish(hook.Result{
					Call:    call,
					EndedAt: time.Now(),
					Outcome: hook.OutcomeFailed,
					Err:     &appendPanicError{},
				})
				panic(recovered)
			}
		}()
		err := next(hookCtx)
		outcome := hook.OutcomeCompleted
		switch {
		case err == nil:
			outcome = hook.OutcomeCompleted
		case ctx.Err() != nil || hookCtx.Err() != nil:
			outcome = hook.OutcomeCanceled
		default:
			outcome = hook.OutcomeFailed
		}
		finish(hook.Result{
			Call:    call,
			EndedAt: time.Now(),
			Outcome: outcome,
			Err:     err,
		})
		return err
	}
}

// HookMiddleware observes safe, classifiable journal records with runner.
// Records whose metadata cannot be derived without panicking bypass observation
// and delegate unchanged. It is the AppendFunc-shaped form of the same observation
// WithHooks applies, kept for callers that decorate a single append function (the
// sessionstore opening-append middleware).
func HookMiddleware(runner *hook.Runner, sessionID uuid.UUID) AppendMiddleware {
	if !runner.Handles(hook.OperationJournalAppend) {
		return nil
	}
	around := observeAppend(runner, sessionID)
	return func(next AppendFunc) AppendFunc {
		return func(ctx context.Context, record JournalRecord) (uint64, error) {
			return aroundAppend(ctx, around, record, next)
		}
	}
}

func describeRecord(record JournalRecord) (family hook.RecordFamily, recordID string, ok bool) {
	if record == nil {
		return "", "", false
	}
	defer func() {
		if recover() != nil {
			family = ""
			recordID = ""
			ok = false
		}
	}()
	switch record.(type) {
	case EventRecord, *EventRecord:
		family = hook.RecordEvent
	case CommandRecord, *CommandRecord:
		family = hook.RecordCommand
	case GatePreparedRecord, *GatePreparedRecord:
		family = hook.RecordGatePrepared
	case FenceRecord, *FenceRecord:
		family = hook.RecordFence
	case CommandApplicationRecord, *CommandApplicationRecord:
		family = hook.RecordCommandApplication
	default:
		return "", "", false
	}
	return family, record.IdempotencyID(), true
}
