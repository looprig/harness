package journal

import "context"

// AroundAppend decorates one durable append of ANY result type. next must be invoked
// exactly once, with the context the delegated call should run under — a decorator may
// substitute its own (the operation-hook decorator does) or pass the caller's through
// (the offload-GC admission gate does). The error next returns is the append's error;
// returning a different one substitutes it, and returning without calling next skips
// the append entirely.
//
// The result VALUE never appears here, which is the point: an append seam that returns
// a sequence, an AppendResult, or a CommittedAppendResult is decorated by the same
// function, so a decorator is written once and cannot be written wrong for one seam and
// right for another.
type AroundAppend func(ctx context.Context, rec JournalRecord, next func(context.Context) error) error

// Decorate wraps inner so every append runs inside around, and returns a journal that
// advertises EXACTLY the optional contracts inner advertises — no more, and no less.
//
// This selection exists in ONE place on purpose. A decorator that exposes only Append
// silently demotes an idempotent, committed-bytes journal to a plain one, and the
// damage is invisible: the hub stops seeing Appended=false and re-broadcasts
// deduplicated retries, and the committed-public-event capability does not degrade but
// VANISHES — a Host adapter asks for it, is told no, and the deployment reads as
// headless with no error raised anywhere. That defect shipped twice, in two sibling
// decorators applied two lines apart at the same composition-root seam. Both now route
// through here, so a future seam is added once rather than once per decorator.
//
// Adding a seam means: a case in the type switch, a wrapper type promoting the narrower
// one, and a row in TestDecoratePreservesOptionalJournalContracts.
func Decorate(inner SessionJournal, around AroundAppend) SessionJournal {
	if inner == nil {
		return nil
	}
	if around == nil {
		return inner
	}
	core := &decoratedAppends{around: around, plain: inner}
	base := decoratedJournal{core: core}
	switch delegate := inner.(type) {
	case CommittedPublicJournal:
		core.idempotent = delegate
		core.committed = delegate
		return decoratedCommittedJournal{decoratedIdempotentJournal{base}}
	case IdempotentJournal:
		core.idempotent = delegate
		return decoratedIdempotentJournal{base}
	default:
		return base
	}
}

// decoratedAppends holds the delegate under each optional contract it satisfies and
// routes every seam through the one decorator. The nil fields are never reached: the
// wrapper type built over this core does not expose the corresponding method.
type decoratedAppends struct {
	around     AroundAppend
	plain      SessionJournal
	idempotent IdempotentJournal
	committed  CommittedPublicJournal
}

func (d *decoratedAppends) Append(ctx context.Context, rec JournalRecord) (uint64, error) {
	return aroundAppend(ctx, d.around, rec, d.plain.Append)
}

func (d *decoratedAppends) AppendIdempotent(ctx context.Context, rec JournalRecord) (AppendResult, error) {
	return aroundAppend(ctx, d.around, rec, d.idempotent.AppendIdempotent)
}

func (d *decoratedAppends) AppendCommitted(ctx context.Context, rec JournalRecord) (CommittedAppendResult, error) {
	return aroundAppend(ctx, d.around, rec, d.committed.AppendCommitted)
}

// aroundAppend adapts a result-returning append to AroundAppend's error-only shape by
// capturing the result in a closure. It is the one place the two shapes meet.
func aroundAppend[T any](
	ctx context.Context,
	around AroundAppend,
	rec JournalRecord,
	call func(context.Context, JournalRecord) (T, error),
) (T, error) {
	var result T
	err := around(ctx, rec, func(inner context.Context) error {
		var callErr error
		result, callErr = call(inner, rec)
		return callErr
	})
	return result, err
}

// The three wrapper types below select the advertised contract by construction. Go has
// no way to add a method conditionally, so each type promotes the narrower one and adds
// exactly one method.
type decoratedJournal struct{ core *decoratedAppends }

func (d decoratedJournal) Append(ctx context.Context, rec JournalRecord) (uint64, error) {
	return d.core.Append(ctx, rec)
}

type decoratedIdempotentJournal struct{ decoratedJournal }

func (d decoratedIdempotentJournal) AppendIdempotent(ctx context.Context, rec JournalRecord) (AppendResult, error) {
	return d.core.AppendIdempotent(ctx, rec)
}

type decoratedCommittedJournal struct{ decoratedIdempotentJournal }

func (d decoratedCommittedJournal) AppendCommitted(ctx context.Context, rec JournalRecord) (CommittedAppendResult, error) {
	return d.core.AppendCommitted(ctx, rec)
}

var (
	_ SessionJournal         = decoratedJournal{}
	_ IdempotentJournal      = decoratedIdempotentJournal{}
	_ CommittedPublicJournal = decoratedCommittedJournal{}
)
