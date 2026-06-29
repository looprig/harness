// Package journalsource bridges the journal read side to the transcript builder. It is
// the ONLY place that imports both github.com/ciram-co/looprig/pkg/journal and
// github.com/ciram-co/looprig/pkg/transcript: pkg/transcript stays storage-pure (it
// never imports journal) and pkg/journal never knows about transcript. This adapter maps
// a journal.RecordReplayer's stream onto a transcript.RecordSource, translating
// journal.EventRecord/CommandRecord into their transcript counterparts and DROPPING
// journal.FenceRecord (lease-handover boundaries are an internal journal concern that
// must never reach the transcript builder).
package journalsource

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/transcript"
)

// Open adapts a journal.RecordReplayer into a transcript.RecordSource over the records
// selected by req. The returned source is LAZY: it does not touch rr until the first
// Next call, so an Open-time failure (e.g. a missing stream) surfaces from that first
// Next rather than from this constructor. The source is a single-reader handle (not safe
// for concurrent Next), matching the underlying RecordCursor's contract.
func Open(rr journal.RecordReplayer, req journal.ReplayRequest) transcript.RecordSource {
	return &recordSource{rr: rr, req: req}
}

// recordSource is the lazy journal.RecordReplayer → transcript.RecordSource adapter. It
// opens the cursor on the first Next, maps each journal record, skips fences, and closes
// the cursor exactly once at EOF or on a read error so the consumer cannot leak it.
type recordSource struct {
	rr  journal.RecordReplayer
	req journal.ReplayRequest

	cursor journal.RecordCursor
	opened bool // the cursor Open has been attempted (success or failure)
	done   bool // the cursor is closed; every further Next returns io.EOF
}

// Next opens the cursor on first call, then returns the next mapped record. It loops past
// any journal.FenceRecord (fences are dropped). It returns io.EOF when the underlying
// cursor reaches EOF, and surfaces any other read error verbatim (so transcript.Reconstruct
// can wrap it in a *ReconstructError). On EOF or on a read error it closes the cursor
// exactly once; on an Open-time error there is no cursor yet, so it only latches done.
// Either way, subsequent calls return io.EOF without re-closing.
func (s *recordSource) Next(ctx context.Context) (transcript.Record, error) {
	if s.done {
		return nil, io.EOF
	}
	if !s.opened {
		s.opened = true
		cur, err := s.rr.Open(ctx, s.req)
		if err != nil {
			// No cursor was built, so there is nothing to close; just latch done.
			s.done = true
			return nil, err
		}
		s.cursor = cur
	}

	for {
		rec, _, err := s.cursor.Next(ctx)
		if err != nil {
			s.closeCursor()
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}

		switch r := rec.(type) {
		case journal.EventRecord:
			return transcript.EventRecord{Event: r.Event()}, nil
		case journal.CommandRecord:
			return transcript.CommandRecord{Command: r.Command()}, nil
		case journal.FenceRecord:
			// Drop fences — lease boundaries must never reach the transcript builder.
			continue
		default:
			// journal.JournalRecord is a sealed sum (Event/Command/Fence), so this arm
			// is unreachable today. Fail secure rather than silently skip an unmapped
			// record: a future variant must be handled explicitly, not dropped.
			s.closeCursor()
			return nil, &UnexpectedRecordError{RecordType: fmt.Sprintf("%T", rec)}
		}
	}
}

// closeCursor closes the underlying cursor exactly once. Any Close error is intentionally
// discarded: Next's contract is to return io.EOF (or the read error) at termination, so a
// teardown error must not displace that signal — this is a deliberate, documented
// exception to the no-bare-discard rule, scoped to best-effort cleanup.
func (s *recordSource) closeCursor() {
	if s.done {
		return
	}
	s.done = true
	if s.cursor != nil {
		_ = s.cursor.Close()
	}
}

// ExportUnavailableError reports that a transcript export was requested for a session with
// no journal stream to replay (e.g. a purely in-memory session). It is returned by CALLERS
// of journalsource (swe's sessionAgent.ExportSource), not by Open itself, and lives here so
// both swe and the TUI can errors.As it without importing each other. The zero value yields
// a generic, user-facing message so callers can construct &ExportUnavailableError{}.
type ExportUnavailableError struct {
	// Reason optionally explains why export is unavailable; the zero value falls back to a
	// generic "session is not journal-backed" message.
	Reason string
}

func (e *ExportUnavailableError) Error() string {
	if e.Reason != "" {
		return "transcript export unavailable: " + e.Reason
	}
	return "transcript export unavailable: session is not journal-backed"
}

// UnexpectedRecordError reports that the journal cursor yielded a JournalRecord variant the
// bridge does not know how to map. Because journal.JournalRecord is a sealed sum, this is an
// unreachable fail-secure arm: a future variant must add an explicit mapping rather than be
// silently dropped. It is errors.As-able for diagnostics.
type UnexpectedRecordError struct {
	// RecordType is the Go type name of the unmapped journal record.
	RecordType string
}

func (e *UnexpectedRecordError) Error() string {
	return "transcript/journalsource: unexpected journal record type " + e.RecordType
}
