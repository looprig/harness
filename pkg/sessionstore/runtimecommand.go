package sessionstore

import (
	"context"
	"errors"
	"io"
	"strconv"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// CommandApplicationNotFoundError reports that the record at the named sequence is
// not an application prefix. It fails closed rather than reporting an absent
// correlation as "not applied", because that answer would let an already-applied
// command be applied a second time.
type CommandApplicationNotFoundError struct {
	SessionID uuid.UUID
	Seq       uint64
	Cause     error
}

func (e *CommandApplicationNotFoundError) Error() string {
	return "sessionstore: no command application at seq " + strconv.FormatUint(e.Seq, 10) +
		" in session " + e.SessionID.String()
}

func (e *CommandApplicationNotFoundError) Unwrap() error { return e.Cause }

// RuntimeCommandLog is the session-bound seam an applier uses to make an
// application prefix durable and to read one back. It pairs the strict append
// (through the session's own lease-fenced journal) with the privileged read
// (through the store), because the duplicate-delivery rule needs both: the append
// detects the duplicate, and the read says what the ORIGINAL delivery mapped to.
type RuntimeCommandLog struct {
	appender  *journal.JournalRuntimeCommandAppender
	store     *Store
	sessionID uuid.UUID
}

// OpenRuntimeCommandLog binds a runtime-command log to session id over j. It fails
// closed when j cannot deduplicate a redelivered append — see
// *journal.NonIdempotentJournalError for why that is a capability refusal rather
// than a wiring bug.
func (s *Store) OpenRuntimeCommandLog(id uuid.UUID, j journal.SessionJournal) (*RuntimeCommandLog, error) {
	appender, err := journal.NewJournalRuntimeCommandAppenderChecked(j)
	if err != nil {
		return nil, err
	}
	if _, err := sessionName(id); err != nil {
		return nil, err
	}
	return &RuntimeCommandLog{appender: appender, store: s, sessionID: id}, nil
}

// AppendCommandApplication makes app durable, reporting whether THIS call appended
// it. See journal.JournalRuntimeCommandAppender.AppendCommandApplication.
func (l *RuntimeCommandLog) AppendCommandApplication(ctx context.Context, app runtimecommand.Application) (journal.AppendResult, error) {
	return l.appender.AppendCommandApplication(ctx, app)
}

// AppendCommandDisposition makes d durable as the private, bodiless disposition
// frame. See journal.JournalRuntimeCommandAppender.AppendCommandDisposition.
func (l *RuntimeCommandLog) AppendCommandDisposition(ctx context.Context, d runtimecommand.CommandDisposition) (journal.AppendResult, error) {
	return l.appender.AppendCommandDisposition(ctx, d)
}

// ReadCommandApplicationAt reads back the application prefix at seq.
func (l *RuntimeCommandLog) ReadCommandApplicationAt(ctx context.Context, seq uint64) (runtimecommand.Application, error) {
	return l.store.ReadCommandApplicationAt(ctx, l.sessionID, seq)
}

// ReadCommandApplicationAt reads the private application prefix stored at seq in
// session id's journal. It positions the privileged record replayer at exactly that
// sequence rather than scanning, so the read a duplicate delivery performs costs one
// record. A record of any other kind at that sequence is a
// *CommandApplicationNotFoundError.
func (s *Store) ReadCommandApplicationAt(ctx context.Context, id uuid.UUID, seq uint64) (runtimecommand.Application, error) {
	// ONE positioning argument. OpenInternalRecordReplayer's ReplayRequest.FromSeq is
	// what positions the ledger cursor; the journal.ReplayRequest passed to Open is a
	// different type whose From this backend does not read. Passing a second, inert
	// FromSeq there would read as the positioning and invite a maintainer to delete
	// the one that works — silently turning this O(1) read into a full scan with no
	// test to notice. journal.Beginning() is honest about being ignored.
	replayer, err := s.OpenInternalRecordReplayer(id, ReplayRequest{FromSeq: seq})
	if err != nil {
		return runtimecommand.Application{}, err
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: id, From: journal.Beginning()})
	if err != nil {
		return runtimecommand.Application{}, err
	}
	defer func() { _ = cursor.Close() }()
	rec, got, err := cursor.Next(ctx)
	if err != nil {
		return runtimecommand.Application{}, &CommandApplicationNotFoundError{SessionID: id, Seq: seq, Cause: err}
	}
	if got != seq {
		return runtimecommand.Application{}, &CommandApplicationNotFoundError{SessionID: id, Seq: seq}
	}
	app, ok := rec.(journal.CommandApplicationRecord)
	if !ok {
		return runtimecommand.Application{}, &CommandApplicationNotFoundError{SessionID: id, Seq: seq}
	}
	return app.Application(), nil
}

// ScanCommandEffect is the PRIVILEGED whole-journal scan a recovery closure needs:
// it reports the application prefix for commandID, if any, and whether an enduring
// event caused by runtimeID was committed ANYWHERE in the journal.
//
// POSITION IS NOT PART OF THE PREDICATE, and that is a decision rather than an
// oversight. An event carrying that runtime id in its cause cannot exist unless the
// command was dispatched, so where it sits proves nothing extra; and restricting the
// match to "after the prefix" would mean that in the one journal shape nobody can
// explain — an effect with no prefix before it — the scan reported "no effect" and
// licensed a tombstone. The reported PrefixSeq and EffectSeq are locators, not an
// ordering claim; see TestClosureRefusesAnEffectThatPrecedesThePrefix.
//
// It exists for one guard and is worth the walk for that guard alone. A successor
// writing not_applied over a command whose effect actually committed converts a real
// effect into a tombstone that all future grants must honour, and the idempotency
// index cannot catch it: the collision guard only fires when the predecessor's
// DISPOSITION landed, and the dangerous case is precisely the one where the effect
// landed and the disposition did not.
//
// It is O(journal). That is a stated cost, not an oversight — the settlement design
// asks for bounded recovery I/O and this is not bounded — and it is on the recovery
// path only. Bounding it needs an index this package does not keep.
func (l *RuntimeCommandLog) ScanCommandEffect(ctx context.Context, commandID runtimecommand.CommandID, runtimeID uuid.UUID) (runtimecommand.EffectScan, error) {
	return l.store.ScanCommandEffect(ctx, l.sessionID, commandID, runtimeID)
}

// ScanCommandEffect walks session id's journal for the application prefix naming
// commandID and for the FIRST enduring event whose Cause.CommandID is runtimeID, at
// ANY sequence. See the method above for why position is not part of the predicate.
func (s *Store) ScanCommandEffect(ctx context.Context, id uuid.UUID, commandID runtimecommand.CommandID, runtimeID uuid.UUID) (runtimecommand.EffectScan, error) {
	replayer, err := s.OpenInternalRecordReplayer(id, ReplayRequest{})
	if err != nil {
		return runtimecommand.EffectScan{}, err
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: id, From: journal.Beginning()})
	if err != nil {
		return runtimecommand.EffectScan{}, err
	}
	defer func() { _ = cursor.Close() }()
	var scan runtimecommand.EffectScan
	for {
		rec, seq, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return scan, nil
		}
		if err != nil {
			// A frame this walk cannot read is NOT "no effect". Fail closed: the whole
			// purpose of the scan is to refuse a tombstone over an effect that may be
			// sitting in the record it could not decode.
			return runtimecommand.EffectScan{}, err
		}
		switch r := rec.(type) {
		case journal.CommandDispositionRecord:
			if d := r.Disposition(); d.CommandID == commandID && scan.DispositionSeq == 0 {
				scan.DispositionSeq = seq
				scan.Disposition = d
			}
		case journal.CommandApplicationRecord:
			if app := r.Application(); app.CommandID == commandID {
				scan.PrefixSeq = seq
				scan.DurableRuntimeID = app.RuntimeCommandID
				scan.DurableKind = app.Kind
			}
		case journal.EventRecord:
			// The correlation is the CAUSE, never the event type: every event a command
			// causes carries the command's runtime id in Cause.CommandID, and a scan
			// that enumerated types would go blind the moment the vocabulary grew.
			//
			// The walk is in ledger order and the prefix is written BEFORE the effect,
			// so an event seen while PrefixSeq is still zero cannot be this command's
			// effect under any ordering this writer produces — but it is counted
			// anyway, because "before the prefix" is not a state this guard may treat
			// as safe: the conservative direction here is to refuse the closure.
			if !scan.EffectFound && r.Event().EventHeader().Cause.CommandID == runtimeID {
				scan.EffectFound = true
				scan.EffectSeq = seq
			}
		}
	}
}
