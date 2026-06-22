//go:build integration

package journal_test

import (
	"context"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/nats-io/nats.go"
)

// TestJournalCommandAppenderReplayable is the Task-7.5 end-to-end assertion: a command
// appended through the REAL JournalCommandAppender (the session's audit-only intent-log
// seam) lands on the target loop's .cmd subject, keyed by the command's CommandID, and
// round-trips back through command.UnmarshalCommand — including the session-stamped
// Header.CreatedAt. This proves the intent log is durable and replayable, and that the
// new CreatedAt field survives the codec on the wire.
func TestJournalCommandAppenderReplayable(t *testing.T) {
	sid := seedUUID(0xC0)
	lid := seedUUID(0xC1)
	createdAt := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}

	appender := journal.NewJournalCommandAppender(j)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A UserInput as the session would build it at the dispatch boundary: a minted
	// CommandID, user agency, and the clock-stamped CreatedAt.
	want := command.UserInput{
		Header: command.Header{
			CommandID: seedUUID(0xC2),
			Agency:    identity.AgencyUser,
			CreatedAt: createdAt,
		},
	}

	// The session targets the loop explicitly (commands carry no addressing of their
	// own); the record routes to that loop's .cmd subject.
	if err := appender.AppendCommand(ctx, journal.NewCommandRecord(sid, lid, want)); err != nil {
		t.Fatalf("AppendCommand: %v", err)
	}

	// The opening LeaseFence is seq 1; the command is the next append at seq 2.
	raw, err := js.GetMsg(journal.StreamName(sid), 2)
	if err != nil {
		t.Fatalf("GetMsg(seq 2): %v", err)
	}
	if raw.Subject != journal.LoopCommandSubject(sid, lid) {
		t.Errorf("stored subject = %q, want %q (loop .cmd subject)", raw.Subject, journal.LoopCommandSubject(sid, lid))
	}
	if got := raw.Header.Get(nats.MsgIdHdr); got != want.CommandID.String() {
		t.Errorf("stored %s = %q, want CommandID %q", nats.MsgIdHdr, got, want.CommandID.String())
	}

	got, err := command.UnmarshalCommand(raw.Data)
	if err != nil {
		t.Fatalf("UnmarshalCommand: %v", err)
	}
	ui, ok := got.(command.UserInput)
	if !ok {
		t.Fatalf("decoded %T, want command.UserInput", got)
	}
	if ui.CommandID != want.CommandID {
		t.Errorf("decoded CommandID = %v, want %v", ui.CommandID, want.CommandID)
	}
	if ui.Agency != want.Agency {
		t.Errorf("decoded Agency = %v, want %v", ui.Agency, want.Agency)
	}
	if !ui.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("decoded CreatedAt = %v, want %v (must survive the codec)", ui.CreatedAt, want.CreatedAt)
	}
}
