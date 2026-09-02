package sessionstore

import (
	"context"
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
