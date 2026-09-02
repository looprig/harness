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

// hookedAppends observes every append seam a journal offers, through one shared
// hook-invocation core. It holds the delegate under each optional contract it
// satisfies; the nil ones are simply never reached, because the wrapper type built
// over it does not expose the corresponding method.
type hookedAppends struct {
	runner     *hook.Runner
	sessionID  uuid.UUID
	plain      SessionJournal
	idempotent IdempotentJournal
	committed  CommittedPublicJournal
}

func (h *hookedAppends) Append(ctx context.Context, record JournalRecord) (uint64, error) {
	return observeAppend(ctx, h.runner, h.sessionID, record, h.plain.Append)
}

func (h *hookedAppends) AppendIdempotent(ctx context.Context, record JournalRecord) (AppendResult, error) {
	return observeAppend(ctx, h.runner, h.sessionID, record, h.idempotent.AppendIdempotent)
}

func (h *hookedAppends) AppendCommitted(ctx context.Context, record JournalRecord) (CommittedAppendResult, error) {
	return observeAppend(ctx, h.runner, h.sessionID, record, h.committed.AppendCommitted)
}

// The three wrapper types below exist so the hooked journal advertises EXACTLY the
// contracts its delegate advertises — no more, and no less. Go has no way to add a
// method conditionally, so the capability is selected by construction: each type
// promotes the narrower one and adds one method.
type hookedJournal struct{ core *hookedAppends }

func (h hookedJournal) Append(ctx context.Context, record JournalRecord) (uint64, error) {
	return h.core.Append(ctx, record)
}

type hookedIdempotentJournal struct{ hookedJournal }

func (h hookedIdempotentJournal) AppendIdempotent(ctx context.Context, record JournalRecord) (AppendResult, error) {
	return h.core.AppendIdempotent(ctx, record)
}

type hookedCommittedJournal struct{ hookedIdempotentJournal }

func (h hookedCommittedJournal) AppendCommitted(ctx context.Context, record JournalRecord) (CommittedAppendResult, error) {
	return h.core.AppendCommitted(ctx, record)
}

var (
	_ SessionJournal         = hookedJournal{}
	_ IdempotentJournal      = hookedIdempotentJournal{}
	_ CommittedPublicJournal = hookedCommittedJournal{}
)

// WithHooks observes each durable append while preserving the journal's result AND
// its optional contracts. Observation must not amputate capability: a decorator that
// exposed only Append would silently demote an idempotent, committed-bytes journal to
// a plain one for every deployment that configures a journal-append hook — the hub
// would lose the Appended=false signal and re-broadcast deduplicated retries, and the
// segregated committed-public-event capability would vanish. So the wrapper is chosen
// to match the delegate's own contracts, widest first.
func WithHooks(j SessionJournal, runner *hook.Runner, sessionID uuid.UUID) SessionJournal {
	if j == nil {
		return nil
	}
	if !runner.Handles(hook.OperationJournalAppend) {
		return j
	}
	core := &hookedAppends{runner: runner, sessionID: sessionID, plain: j}
	base := hookedJournal{core: core}
	switch delegate := j.(type) {
	case CommittedPublicJournal:
		core.idempotent = delegate
		core.committed = delegate
		return hookedCommittedJournal{hookedIdempotentJournal{base}}
	case IdempotentJournal:
		core.idempotent = delegate
		return hookedIdempotentJournal{base}
	default:
		return base
	}
}

// observeAppend is the shared journal-append hook invocation, generic over the append
// seam's result type so every seam observes identically. A record whose metadata
// cannot be derived without panicking, and a runner that refuses to start, both
// delegate unobserved — the append itself is never blocked by observation.
func observeAppend[T any](
	ctx context.Context,
	runner *hook.Runner,
	sessionID uuid.UUID,
	record JournalRecord,
	next func(context.Context, JournalRecord) (T, error),
) (T, error) {
	family, recordID, observable := describeRecord(record)
	if !observable {
		return next(ctx, record)
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
		return next(ctx, record)
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
	value, err := next(hookCtx, record)
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
	return value, err
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
	return func(next AppendFunc) AppendFunc {
		return func(ctx context.Context, record JournalRecord) (uint64, error) {
			return observeAppend(ctx, runner, sessionID, record, next)
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
	default:
		return "", "", false
	}
	return family, record.IdempotencyID(), true
}
