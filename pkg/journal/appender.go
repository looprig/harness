package journal

import (
	"context"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// NilJournalError reports that a JournalEventAppender was constructed over a nil
// SessionJournal — a composition-wiring bug. The checked constructor fails loud with
// this typed error rather than letting the nil surface as a panic at the first append.
type NilJournalError struct{}

func (*NilJournalError) Error() string {
	return "journal: JournalEventAppender requires a non-nil SessionJournal"
}

// CommittedPublicBody is what a durable journal stored, for one record, on the
// PUBLIC side of its envelope: the canonical public event id it committed the record
// under and the exact canonical body bytes it wrote. Both are zero for a private
// record, for a non-event record, and for a deduplicated retry (which stored
// nothing). Body is owned by the returned value; the journal must not retain a
// reference it later mutates.
type CommittedPublicBody struct {
	EventID string
	Body    []byte
}

// CommittedAppendResult is AppendResult widened with the committed public bytes. It
// embeds AppendResult so every existing Sequence/Appended reading applies unchanged.
type CommittedAppendResult struct {
	AppendResult
	Public CommittedPublicBody
}

// CommittedPublicJournal is the OPTIONAL extension a SessionJournal implementation
// may satisfy to report the EXACT canonical public bytes it stored for a record. It
// embeds IdempotentJournal, so a committed-bytes implementation is usable anywhere a
// plain or idempotent SessionJournal is expected and the existing seams are never
// weakened.
//
// The reason the bytes are reported rather than re-derived is that a second
// projection is a second answer. A consumer joining a durable tail to a live stream
// dedupes on (sequence, event id) and compares bodies; if the live body were
// re-projected it could differ from the stored one — in key order, in a field a later
// projector version adds — and the consumer would render two different things for one
// event without any error anywhere. Only the journal that wrote the bytes can say
// what they are.
type CommittedPublicJournal interface {
	IdempotentJournal
	// AppendCommitted behaves exactly like AppendIdempotent — same fencing, same
	// dedup, same errors — and additionally reports the public event id and the
	// exact canonical public body it stored for a newly appended PUBLIC event
	// record. A deduplicated retry reports Appended=false with a zero Public: this
	// call stored nothing, so it has no stored bytes of its own to report.
	AppendCommitted(ctx context.Context, rec JournalRecord) (CommittedAppendResult, error)
}

// catalogUpdater is the narrow seam the appender notifies AFTER a successful durable
// append so the derived session catalog can index the event (best-effort). It is a
// single-method interface (Interface Segregation): the appender depends on
// UpdateOnEvent alone, never the full Catalog. The *Catalog satisfies it; the nop
// default (nopCatalogUpdater) keeps existing wiring and headless mode unchanged.
//
// Its contract is best-effort by design: UpdateOnEvent must NEVER return a non-nil
// error (the catalog is derivable, so a failed index is logged and swallowed inside
// it). The appender therefore ignores its return, and a catalog failure can never
// affect the append's success/failure semantics.
type catalogUpdater interface {
	UpdateOnEvent(ctx context.Context, ev event.Event, seq uint64) error
}

// nopCatalogUpdater is the default catalogUpdater: it indexes nothing. It is the safe
// default so an appender constructed without a catalog (existing tests, headless mode)
// behaves exactly as before — no catalog, no extra I/O.
type nopCatalogUpdater struct{}

func (nopCatalogUpdater) UpdateOnEvent(context.Context, event.Event, uint64) error { return nil }

// JournalEventAppender adapts a SessionJournal (the write side) to the narrow
// "append one Enduring event" seam the session hub depends on. The hub holds an
// unexported eventAppender interface (AppendEvent(ctx, event.Event) error); this type
// satisfies it structurally, so the composition root (Phase 10) wires it in via
// hub.WithAppender without the hub ever importing the journal package (Dependency
// Inversion). Beyond the journal it holds an optional catalog updater (nop by default):
// after a successful Append it notifies the catalog best-effort so the replay-free
// session index stays current. One responsibility: wrap the event in an EventRecord
// (which self-derives its subject from the event's scope+coordinates and its idempotency
// id from the EventID), append it, then best-effort index it.
type JournalEventAppender struct {
	journal SessionJournal
	catalog catalogUpdater
}

// AppenderOption configures a JournalEventAppender at construction. Applied in order
// over a defaults struct (nop catalog), so a later option overrides an earlier one.
type AppenderOption func(*JournalEventAppender)

// WithCatalog injects the catalog updater the appender notifies after a successful
// append (best-effort). A nil updater is ignored (the nop default is kept), so the
// appender owns its invariant — it never holds a nil catalog and never nil-derefs.
func WithCatalog(c catalogUpdater) AppenderOption {
	return func(a *JournalEventAppender) {
		if c != nil {
			a.catalog = c
		}
	}
}

// NewJournalEventAppender wraps journal as an event appender. It does NOT guard
// against a nil journal — use NewJournalEventAppenderChecked at the composition root
// where a wiring bug must fail loud. This unchecked form exists for call sites that
// have already validated the journal (and for the structural-satisfaction assertion).
func NewJournalEventAppender(journal SessionJournal, opts ...AppenderOption) *JournalEventAppender {
	a := &JournalEventAppender{journal: journal, catalog: nopCatalogUpdater{}}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// NewJournalEventAppenderChecked is the fail-loud constructor for the composition
// root: it returns a typed *NilJournalError if journal is nil rather than deferring
// the failure to a nil-deref at the first append.
func NewJournalEventAppenderChecked(journal SessionJournal, opts ...AppenderOption) (*JournalEventAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	return NewJournalEventAppender(journal, opts...), nil
}

// AppendEvent durably appends one Enduring event: it wraps ev in an EventRecord and
// calls the journal's Append, returning the underlying typed error unchanged (the hub
// maps it onto a SessionPersistenceFault — never swallowed). The EventRecord routes a
// session-scoped event to the session subject and a loop-scoped event to its loop
// event subject, and uses the event's EventID as the Nats-Msg-Id (idempotency). An
// Ephemeral event is never appended by the hub; if one were passed, the serializer's
// event.MarshalEvent fails closed inside Append, so this path stays fail-secure.
//
// It returns the assigned durable journal sequence so the hub can ride it on the LIVE
// delivery (event.Delivery.JournalSeq) — the sequence NEVER enters the persisted event
// codec. ONLY after the durable append succeeds does it best-effort notify the catalog
// (with the same sequence) so the replay-free session index stays current. The catalog
// update is the soft tail: its error is swallowed inside UpdateOnEvent and cannot change
// this method's return — the durable append stays strict, the catalog is derivable. On an
// append failure the catalog is NOT touched (the event did not durably land) and seq 0 is
// returned alongside the error.
//
// When the underlying journal additionally satisfies IdempotentJournal (the optional
// dedup seam), a redelivered event whose EventID already names a durable record is
// detected there and reported as AppendResult.Appended=false; this method then
// returns the ORIGINAL sequence without re-notifying the catalog — a duplicate was
// already indexed by its first, genuine append, so republishing it a second time
// would be redundant. A journal that does not implement the optional interface
// behaves exactly as before (every successful Append notifies the catalog).
func (a *JournalEventAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	seq, _, err := a.AppendEventResult(ctx, ev)
	return seq, err
}

// AppendEventResult is the result-preserving event append seam used by the Hub
// trusted publication path. Appended is true only when this call created a new
// durable frame; an identical idempotent retry returns the original sequence and
// Appended=false. The legacy AppendEvent method above deliberately discards only
// this boolean so existing callers retain their API and error behavior; this method
// in turn discards only the committed public fields AppendEventCommitted adds.
func (a *JournalEventAppender) AppendEventResult(ctx context.Context, ev event.Event) (uint64, bool, error) {
	commit, err := a.AppendEventCommitted(ctx, ev)
	return commit.Sequence, commit.Appended, err
}

// SupportsCommittedPublicBodies reports whether this appender can report the EXACT
// canonical public bytes a public enduring append stored. It is true only when the
// underlying SessionJournal implements the optional CommittedPublicJournal seam.
//
// It exists because the capability is a property of the injected JOURNAL, not of the
// appender type: one *JournalEventAppender always has AppendEventCommitted in its
// method set, so a type assertion alone cannot tell a committed-bytes appender from
// a legacy one. A consumer that requires committed bytes (the Host runtime adapter,
// via the hub's segregated committed-public-event capability) must consult this
// predicate; a false result means the capability is NOT advertised, not that it
// failed.
func (a *JournalEventAppender) SupportsCommittedPublicBodies() bool {
	_, ok := a.journal.(CommittedPublicJournal)
	return ok
}

// AppendEventCommitted is the committed-result event append seam. It is the single
// core behind AppendEvent and AppendEventResult, which discard the fields they do not
// need, so all three share one dedup/catalog decision.
//
// Over a CommittedPublicJournal it returns the winning sequence, whether THIS call
// committed a new frame, and — for a newly committed PUBLIC ENDURING event — the
// committed public EventID, the exact stored canonical body, and CoveredThrough equal
// to that same sequence. Over any other SessionJournal it returns sequence and
// appended state exactly as before and leaves the three committed-public fields zero:
// bytes that were never reported back must never be invented here, because the whole
// point of carrying them is that they are the stored ones.
//
// A deduplicated retry (Appended=false) returns the ORIGINAL sequence and NO body and
// NO coverage. This call committed nothing, so it may not advertise a watermark its
// own append did not earn; the original append already delivered its body live, and
// the durable bytes stay durable at that sequence. (Whether a later public READ can
// serve them back is a separate question with a size-dependent answer — see the
// caveat on event.Delivery.PublicBody.)
func (a *JournalEventAppender) AppendEventCommitted(ctx context.Context, ev event.Event) (event.AppendCommit, error) {
	rec := NewEventRecord(ev)
	if committed, ok := a.journal.(CommittedPublicJournal); ok {
		result, err := committed.AppendCommitted(ctx, rec)
		if err != nil {
			return event.AppendCommit{}, err
		}
		if !result.Appended {
			return event.AppendCommit{Sequence: result.Sequence}, nil
		}
		_ = a.catalog.UpdateOnEvent(ctx, ev, result.Sequence)
		commit := event.AppendCommit{Sequence: result.Sequence, Appended: true}
		// The public fields ride only a PUBLIC ENDURING event that the journal
		// actually stored a canonical body for. The class/visibility check is made
		// here, at the seam that publishes the bytes, rather than trusted from the
		// backend: an ephemeral public projection carries no public EventID by
		// contract, and a private record carries no public body at all.
		if ev.Class() == event.Enduring && ev.Visibility() == event.Public &&
			result.Public.EventID != "" && len(result.Public.Body) > 0 {
			commit.EventID = result.Public.EventID
			commit.PublicBody = result.Public.Body
			commit.CoveredThrough = result.Sequence
		}
		return commit, nil
	}
	if idem, ok := a.journal.(IdempotentJournal); ok {
		result, err := idem.AppendIdempotent(ctx, rec)
		if err != nil {
			return event.AppendCommit{}, err
		}
		if !result.Appended {
			return event.AppendCommit{Sequence: result.Sequence}, nil
		}
		_ = a.catalog.UpdateOnEvent(ctx, ev, result.Sequence)
		return event.AppendCommit{Sequence: result.Sequence, Appended: true}, nil
	}
	seq, err := a.journal.Append(ctx, rec)
	if err != nil {
		return event.AppendCommit{}, err
	}
	// Best-effort, post-success: UpdateOnEvent never returns a non-nil error by
	// contract, so the catalog can never fail the append. The return is ignored.
	_ = a.catalog.UpdateOnEvent(ctx, ev, seq)
	return event.AppendCommit{Sequence: seq, Appended: true}, nil
}

// JournalCommandAppender adapts a SessionJournal to the narrow "append one command"
// seam the session depends on for the intent log. The session holds an unexported
// commandAppender interface (AppendCommand(ctx, CommandRecord) error); this type
// satisfies it structurally, so the composition root (Phase 10) wires it in without
// the session importing journal internals beyond the CommandRecord constructor
// (Dependency Inversion). It carries no state beyond the journal — one method, one
// responsibility: append a CommandRecord (which the SESSION built with the dispatch
// target loopID, since a command — Interrupt/Shutdown especially — does not carry its
// own routing) and return the underlying typed error.
//
// Unlike the event appender, the SESSION treats this seam as AUDIT-ONLY: a non-nil
// error is logged and the dispatch proceeds (losing a command record must never block
// the user's action). This façade itself never swallows — it returns the journal's
// error unchanged so the session owns the log-and-proceed decision.
type JournalCommandAppender struct {
	journal SessionJournal
}

// NewJournalCommandAppender wraps journal as a command appender. Like the event
// appender's unchecked form, it does NOT guard against a nil journal — use
// NewJournalCommandAppenderChecked at the composition root where a wiring bug must
// fail loud.
func NewJournalCommandAppender(journal SessionJournal) *JournalCommandAppender {
	return &JournalCommandAppender{journal: journal}
}

// NewJournalCommandAppenderChecked is the fail-loud constructor for the composition
// root: it returns a typed *NilJournalError if journal is nil rather than deferring the
// failure to a nil-deref at the first append.
func NewJournalCommandAppenderChecked(journal SessionJournal) (*JournalCommandAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	return &JournalCommandAppender{journal: journal}, nil
}

// AppendCommand appends one intent-log command record: it calls the journal's Append
// with the session-built CommandRecord and returns the underlying typed error
// unchanged (the session logs+proceeds — audit-only — never faulting the session on a
// command-append failure). The CommandRecord routes to the target loop's command
// (intent-log) subject and uses the command's CommandID as the Nats-Msg-Id
// (idempotency). The returned sequence is discarded — the session needs only the
// success/failure signal.
func (a *JournalCommandAppender) AppendCommand(ctx context.Context, rec CommandRecord) error {
	_, err := a.journal.Append(ctx, rec)
	return err
}

// JournalGateAppender adapts a SessionJournal to the session gate directory's
// strict durable append seam. GatePreparedRecord is appended as a private record;
// GateOpened and GateResolved are public Enduring events and are wrapped in
// EventRecord exactly like JournalEventAppender.
type JournalGateAppender struct {
	journal SessionJournal
}

// NewJournalGateAppender wraps journal as a gate appender. Like the other unchecked
// appender constructors, it expects a validated journal; use
// NewJournalGateAppenderChecked at composition roots.
func NewJournalGateAppender(journal SessionJournal) *JournalGateAppender {
	return &JournalGateAppender{journal: journal}
}

// NewJournalGateAppenderChecked fails loud on nil journal so composition wiring bugs
// surface at construction instead of the first gate operation.
func NewJournalGateAppenderChecked(journal SessionJournal) (*JournalGateAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	return &JournalGateAppender{journal: journal}, nil
}

func (a *JournalGateAppender) AppendGatePrepared(ctx context.Context, rec GatePreparedRecord) error {
	_, err := a.journal.Append(ctx, rec)
	return err
}

func (a *JournalGateAppender) AppendGateOpened(ctx context.Context, ev event.GateOpened) error {
	_, err := a.journal.Append(ctx, NewEventRecord(ev))
	return err
}

func (a *JournalGateAppender) AppendGateResolved(ctx context.Context, ev event.GateResolved) error {
	_, err := a.journal.Append(ctx, NewEventRecord(ev))
	return err
}

// NonIdempotentJournalError reports that a runtime-command appender was asked to
// wrap a SessionJournal that cannot deduplicate a redelivered append. It is a
// CAPABILITY refusal, not a wiring bug: the duplicate-delivery contract is
// implemented BY the journal's dedup, so a journal without it cannot honor the
// contract and the seam must decline to advertise it rather than apply an admitted
// command twice.
type NonIdempotentJournalError struct{}

func (*NonIdempotentJournalError) Error() string {
	return "journal: runtime-command applications require an IdempotentJournal"
}

// JournalRuntimeCommandAppender adapts an IdempotentJournal to the narrow
// "append one application prefix" seam the session's runtime-command applier
// depends on. It is a SEPARATE appender from JournalCommandAppender on purpose:
// the intent log is audit-only and swallows its failures, while this append is the
// crash-safety barrier in front of a runtime-visible effect and its failure MUST
// stop the effect.
type JournalRuntimeCommandAppender struct {
	journal IdempotentJournal
}

// NewJournalRuntimeCommandAppenderChecked wraps journal as a runtime-command
// appender. It fails loud on a nil journal and fails closed on a journal that does
// not advertise IdempotentJournal.
func NewJournalRuntimeCommandAppenderChecked(journal SessionJournal) (*JournalRuntimeCommandAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	idempotent, ok := journal.(IdempotentJournal)
	if !ok {
		return nil, &NonIdempotentJournalError{}
	}
	return &JournalRuntimeCommandAppender{journal: idempotent}, nil
}

// AppendCommandApplication durably appends app's correlation as a private record
// and reports whether THIS call made it durable. Appended=false means an identical
// prefix was already durable — the command was already applied — and Sequence is
// the ORIGINAL append's sequence. A public CommandID already durable under a
// DIFFERENT mapping surfaces as *IdempotencyCollisionError.
func (a *JournalRuntimeCommandAppender) AppendCommandApplication(ctx context.Context, app runtimecommand.Application) (AppendResult, error) {
	return a.journal.AppendIdempotent(ctx, NewCommandApplicationRecord(app))
}
