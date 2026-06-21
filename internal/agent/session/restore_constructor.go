package session

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/hub"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// RestoreErrorKind classifies a restore failure that is not one of the already-typed
// causes (a *ConfigMismatchError, a *RestoreDiscoveryError, or a replay/journal error).
// It is the wrapper Restore returns for the lease/journal-setup and append failures so
// every Restore exit is a typed error a caller can errors.As.
type RestoreErrorKind string

const (
	// RestoreLeaseFailed: the single-writer lease could not be acquired (another owner
	// holds it, or a KV read failed). The session must not come up — a second writer
	// would corrupt the durable log.
	RestoreLeaseFailed RestoreErrorKind = "lease_failed"
	// RestoreJournalFailed: the SessionJournal could not be constructed (its opening
	// LeaseFence was rejected, or stream setup failed).
	RestoreJournalFailed RestoreErrorKind = "journal_failed"
	// RestoreReplayFailed: opening or draining the durable stream failed (a setup, read,
	// decode, or object error). It fails closed rather than restoring partial history.
	RestoreReplayFailed RestoreErrorKind = "replay_failed"
	// RestoreAppendFailed: a restore-lifecycle event (RestoreStarted/RestoreDone, or the
	// crash-seam TurnInterrupted) could not be durably appended. The log can no longer be
	// trusted, so the session does not come up.
	RestoreAppendFailed RestoreErrorKind = "append_failed"
	// RestoreLoopFailed: the seeded primary loop could not be constructed (a config
	// validation failure).
	RestoreLoopFailed RestoreErrorKind = "loop_failed"
	// RestoreContextDone: the construction context was already cancelled.
	RestoreContextDone RestoreErrorKind = "context_done"
	// RestoreIDGenerationFailed: a crypto/rand failure minting a restore-event id.
	RestoreIDGenerationFailed RestoreErrorKind = "id_generation_failed"
)

// RestoreError is the typed wrapper for a restore failure. Kind classifies the stage;
// Cause chains the underlying typed error (a *LeaseHeldError, *ReplayReadError,
// *AppendError, *ConfigError, etc.) so a caller can errors.As both this and the cause.
// EVERY non-config/non-discovery Restore failure surfaces as this, and on any failure
// after the journal exists Restore also durably records a RestoreErrored — the session
// never comes up on a failed restore.
type RestoreError struct {
	Kind  RestoreErrorKind
	Cause error
}

func (e *RestoreError) Error() string {
	msg := "session: restore failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}
func (e *RestoreError) Unwrap() error { return e.Cause }

// Restore reconstructs a session's PRIMARY loop from the durable journal and brings it
// up IDLE, ready to continue — the payoff of the persistence layer. It is the parallel
// of New: it reuses the SAME hub/loop/factory wiring but SEEDS the primary loop from the
// folded journal instead of spawning it empty, and it does NOT publish a fresh
// SessionStarted (the session's start was recorded on the original run). The recovered
// session keeps its identity: the same sessionID and the same primary loop id.
//
// Order (per the design — RestoreStarted is the FIRST restore mutation, after the lease
// fence the journal writes at construction):
//
//  1. Acquire the single-writer lease, construct the SessionJournal (writes the opening
//     LeaseFence — the handover boundary) and the EventReplayer.
//  2. Replay the stream; read the persisted config fingerprint and compare to the live
//     config. On mismatch → *ConfigMismatchError UNLESS WithAllowConfigMismatch is set.
//  3. Append RestoreStarted (the first restore mutation).
//  4. Find the primary loop's original id (the root LoopStarted).
//  5. Fold the primary loop's Enduring events into committed msgs + turnIndex.
//  6. Crash-seam: if the fold ends on an open turn, append a TurnInterrupted to close it.
//  7. Append RestoreDone on success.
//  8. Build the Session: journal-backed hub appender + command appender, the session as
//     FaultReporter, and the primary loop SEEDED (NewRestored) under its original id, idle.
//
// On ANY failure in steps 2–7 (a config mismatch, a discovery/replay/decode/object
// failure, or an append failure) Restore durably records a RestoreErrored and returns a
// typed error; the session does NOT come up (fail-secure / no silent drift). The lease
// is the caller's to release on the returned session's teardown; on a failed restore the
// lease is released here before returning.
func Restore(
	ctx context.Context,
	cfg loop.Config,
	sessionID uuid.UUID,
	js nats.JetStreamContext,
	objectStore nats.ObjectStore,
	leases *journal.LeaseManager,
	opts ...Option,
) (*Session, error) {
	return restoreSession(ctx, cfg, sessionID, js, objectStore, leases, uuid.New, time.Now, opts...)
}

// restoreSession is the construction core of Restore with the id-gen and clock seams
// made explicit (mirroring newSession), so a same-package test can pin the stamp or
// drive a mint failure. Restore calls it with the production defaults.
func restoreSession(
	ctx context.Context,
	cfg loop.Config,
	sessionID uuid.UUID,
	js nats.JetStreamContext,
	objectStore nats.ObjectStore,
	leases *journal.LeaseManager,
	newID idGenerator,
	now event.Clock,
	opts ...Option,
) (*Session, error) {
	select {
	case <-ctx.Done():
		return nil, &RestoreError{Kind: RestoreContextDone, Cause: ctx.Err()}
	default:
	}

	// Resolve restore-time options (the allow-mismatch flag) on a probe session so the
	// fingerprint decision can read it BEFORE the real session is built. The same opts
	// are applied to the real session in step 8.
	probe := &Session{}
	for _, opt := range opts {
		opt(probe)
	}
	allowMismatch := probe.allowConfigMismatch

	// (1) Acquire the single-writer lease, then construct the journal (which writes the
	// opening LeaseFence as its first append — the handover boundary) and the replayer.
	lease, err := leases.Acquire(ctx, sessionID)
	if err != nil {
		return nil, &RestoreError{Kind: RestoreLeaseFailed, Cause: err}
	}
	j, err := journal.NewSessionJournal(js, sessionID, lease)
	if err != nil {
		releaseLease(lease)
		return nil, &RestoreError{Kind: RestoreJournalFailed, Cause: err}
	}
	replayer := journal.NewEventReplayer(js, objectStore)

	// The restore-lifecycle events (RestoreStarted/RestoreDone/RestoreErrored and the
	// crash-seam TurnInterrupted) are stamped from the SAME id-gen + clock seam the
	// session's own events use, then appended directly through the journal (the hub does
	// not exist yet). A failed mint fails the restore closed.
	factory := event.NewFactory(func() (uuid.UUID, error) { return newID() }, func() time.Time { return now() })

	// recordErrored durably appends a RestoreErrored carrying err, then releases the
	// lease and returns (nil session, the typed restore error). It is the single
	// fail-secure exit for every failure after the journal exists: the session never
	// comes up, and the failure is recorded in the durable log (a best-effort append — a
	// failure to record the failure never masks the original cause).
	recordErrored := func(restoreErr error) (*Session, error) {
		_ = appendRestoreEvent(ctx, j, factory, event.RestoreErrored{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
			Err:    restoreErr,
		})
		releaseLease(lease)
		return nil, restoreErr
	}

	// (2) Replay the whole stream once for discovery (the persisted fingerprint + the
	// primary loop id). Fail closed on any replay error.
	all, err := drainReplay(ctx, replayer, journal.ReplayRequest{
		SessionID: sessionID, From: journal.Beginning(), Follow: false,
	})
	if err != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: err})
	}

	persisted, err := firstConfigFingerprint(all)
	if err != nil {
		return recordErrored(err)
	}
	if err := checkFingerprint(persisted, FingerprintFrom(cfg), allowMismatch); err != nil {
		return recordErrored(err)
	}

	primaryLoopID, err := findPrimaryLoopID(all)
	if err != nil {
		return recordErrored(err)
	}

	// (3) RestoreStarted — the FIRST restore mutation (after the lease fence).
	if err := appendRestoreEvent(ctx, j, factory, event.RestoreStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
	}); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}

	// (5) Fold the primary loop's Enduring events (a SCOPED replay) into committed msgs +
	// turnIndex. Re-seeding the system prompt is implicit: it rides cfg.Model.System (the
	// loop never stores it in msgs), so the seeded loop carries it via cfg.
	primaryEvents, err := drainReplay(ctx, replayer, journal.ReplayRequest{
		SessionID: sessionID, LoopID: primaryLoopID, From: journal.Beginning(), Follow: false,
	})
	if err != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: err})
	}
	folded := foldPrimaryLoop(primaryEvents)

	// (6) Crash-seam: an open turn (a TurnStarted with no terminal) is closed durably
	// with a TurnInterrupted carrying the open turn's id + index, so the resumed loop
	// never observes a half-open turn.
	if folded.OpenTurn {
		turnID, turnIdx := openTurnCoords(primaryEvents)
		if err := appendRestoreEvent(ctx, j, factory, event.TurnInterrupted{
			Header: event.Header{Coordinates: identity.Coordinates{
				SessionID: sessionID, LoopID: primaryLoopID, TurnID: turnID,
			}},
			TurnIndex: turnIdx,
		}); err != nil {
			return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
		}
	}

	// (7) RestoreDone — the restore succeeded; the session is about to come up.
	if err := appendRestoreEvent(ctx, j, factory, event.RestoreDone{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
	}); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}

	// (8) Build the Session, reusing sessionID + the primary loop id (identity stable).
	// It mirrors newSession's wiring EXCEPT: no SessionStarted is published (the start
	// was recorded on the original run), and the primary loop is SEEDED via NewRestored
	// rather than spawned empty.
	s, err := buildRestoredSession(ctx, cfg, sessionID, primaryLoopID, folded, j, factory, newID, now, opts...)
	if err != nil {
		return recordErrored(err)
	}
	return s, nil
}

// buildRestoredSession assembles the live Session for a successful restore: the hub
// wired with the journal-backed event appender, the shared Factory, and the session as
// the hub's FaultReporter; the journal-backed command appender; and the primary loop
// seeded (NewRestored) with the folded committed state under its ORIGINAL id, idle. It
// publishes NO SessionStarted and spawns NO empty loop — both are the deliberate
// difference from New.
func buildRestoredSession(
	ctx context.Context,
	cfg loop.Config,
	sessionID, primaryLoopID uuid.UUID,
	folded foldResult,
	j journal.SessionJournal,
	factory *event.Factory,
	newID idGenerator,
	now event.Clock,
	opts ...Option,
) (*Session, error) {
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	s := &Session{
		SessionID:     sessionID,
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         make(map[uuid.UUID]*loopHandle),
		newID:         newID,
		now:           now,
		cmdAppender:   nopCommandAppender{},
		factory:       factory,
	}
	// Apply the same opts the probe read (WithCommandAppender wires the durable intent
	// log; WithAllowConfigMismatch is a no-op here — already consumed). A nil appender
	// option leaves the nop default installed.
	for _, opt := range opts {
		opt(s)
	}

	// The hub uses the journal-backed REQUIRED durable tap and the session as its
	// FaultReporter (fail-secure on a required-append failure), sharing the Factory so a
	// hub-synthesized session event is stamped from the same seam.
	appender, err := journal.NewJournalEventAppenderChecked(j)
	if err != nil {
		sessionCancel()
		return nil, &RestoreError{Kind: RestoreJournalFailed, Cause: err}
	}
	s.hub = hub.New(sessionID, hub.WithAppender(appender), hub.WithFactory(factory), hub.WithFaultReporter(s))

	// Seed the primary loop under its ORIGINAL id (identity stable), coming up idle with
	// the folded committed history + turnIndex. No empty loop is spawned and no
	// LoopStarted is published — the loop already exists in the durable record.
	loopCtx, cancel := context.WithCancel(sessionCtx)
	l, err := loop.NewRestored(loopCtx, sessionID, primaryLoopID, s, cfg,
		loop.RestoredState{Msgs: folded.Msgs, TurnIndex: folded.TurnIndex})
	if err != nil {
		cancel()
		sessionCancel()
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: err}
	}
	s.loops[primaryLoopID] = &loopHandle{loop: l, parent: loop.Provenance{}, cancel: cancel}
	s.primaryLoopID = primaryLoopID
	return s, nil
}

// appendRestoreEvent stamps ev (EventID + CreatedAt) via the Factory and appends it to
// the journal as an EventRecord. A mint failure surfaces as a typed restore error; an
// append failure returns the journal's typed error unchanged (the caller wraps it). It
// is the single chokepoint for the restore-lifecycle events written before the hub
// exists.
func appendRestoreEvent(ctx context.Context, j journal.SessionJournal, factory *event.Factory, ev event.Event) error {
	stamped, err := factory.Stamp(ev.EventHeader())
	if err != nil {
		return &RestoreError{Kind: RestoreIDGenerationFailed, Cause: err}
	}
	if _, err := j.Append(ctx, journal.NewEventRecord(withRestoreHeader(ev, stamped))); err != nil {
		return err
	}
	return nil
}

// withRestoreHeader returns a copy of a restore-lifecycle event with hdr substituted for
// its Header. The set is exhaustive over the events appendRestoreEvent stamps; an
// unexpected type panics (a programming error — no other event is appended here).
func withRestoreHeader(ev event.Event, hdr event.Header) event.Event {
	switch e := ev.(type) {
	case event.RestoreStarted:
		e.Header = hdr
		return e
	case event.RestoreDone:
		e.Header = hdr
		return e
	case event.RestoreErrored:
		e.Header = hdr
		return e
	case event.TurnInterrupted:
		e.Header = hdr
		return e
	default:
		panic("session: withRestoreHeader called on a non-restore event type")
	}
}

// openTurnCoords returns the TurnID + TurnIndex of the LAST TurnStarted in the primary
// loop's events — the open (unterminated) turn when foldPrimaryLoop reports OpenTurn.
// The crash-seam TurnInterrupted is stamped with these so it closes the exact turn that
// crashed. It is only called when the fold detected an open turn, so a last TurnStarted
// always exists; the zero return is the defensive fall-through.
func openTurnCoords(events []event.Event) (uuid.UUID, event.TurnIndex) {
	for i := len(events) - 1; i >= 0; i-- {
		if ts, ok := events[i].(event.TurnStarted); ok {
			return ts.TurnID, ts.TurnIndex
		}
	}
	return uuid.UUID{}, 0
}

// drainReplay opens a cold cursor for req and reads it to io.EOF, returning the events
// in stream-sequence order. Any non-EOF read error (a setup, read, decode, or object
// failure) fails closed: the partial slice is discarded and the typed error is returned,
// so a restore never reconstructs truncated history. The cursor is always Closed.
func drainReplay(ctx context.Context, replayer journal.EventReplayer, req journal.ReplayRequest) ([]event.Event, error) {
	cursor, err := replayer.Open(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close() }()

	var out []event.Event
	for {
		ev, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
}

// releaseLease releases the lease on a bounded context, best-effort. On a failed restore
// the lease must not be held (a successor must be able to re-acquire); a release failure
// is swallowed (the bucket TTL is the backstop) since the restore is already failing.
func releaseLease(lease journal.Lease) {
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = lease.Release(rctx)
}
