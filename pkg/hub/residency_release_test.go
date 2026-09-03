package hub

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

type seqAppender struct {
	seq    uint64
	events []event.Event
}

func (a *seqAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	a.seq++
	a.events = append(a.events, ev)
	return a.seq, nil
}

// TestPublishEventCommittedReportsTheAppendSequence exists because the session's
// residency release must record WHICH durable record its workspace checkpoint
// committed as. PublishEventChecked returns only an error, so the sequence the
// journal assigned was unreachable to the publisher; this reports it.
func TestPublishEventCommittedReportsTheAppendSequence(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := &seqAppender{}
	h := New(sid, WithAppender(app))
	// Two prior appends so a correct implementation cannot pass by returning a
	// constant 1, and the reported sequence is the event's OWN, not a count.
	for range 2 {
		if err := h.PublishEventChecked(context.Background(), event.WorkspaceCheckpointed{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: mustID(t)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := h.PublishEventCommitted(context.Background(), event.WorkspaceCheckpointed{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: mustID(t)},
	})
	if err != nil {
		t.Fatalf("PublishEventCommitted: %v", err)
	}
	if commit.Sequence != 3 {
		t.Errorf("commit.Sequence = %d, want 3 (the third append)", commit.Sequence)
	}
	if !commit.Appended {
		t.Error("commit.Appended = false, want true for a fresh append")
	}
}

// TestPublishEventCommittedIsCheckedOnAppendFailure pins the "checked" half: the
// caller must learn the append failed rather than reading a zero sequence as success.
func TestPublishEventCommittedIsCheckedOnAppendFailure(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	want := errors.New("journal down")
	h := New(sid, WithAppender(&refusingAppender{err: want}), WithFaultReporter(&recordingReporter{}))
	commit, err := h.PublishEventCommitted(context.Background(), event.WorkspaceCheckpointed{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: mustID(t)},
	})
	if !errors.Is(err, want) {
		t.Fatalf("PublishEventCommitted err = %v, want it to wrap %v", err, want)
	}
	if commit.Sequence != 0 || commit.Appended {
		t.Errorf("commit = %+v, want the zero commit on a failed append", commit)
	}
}

// TestAbortSessionAppendsNoSessionStopped is the property the nonterminal residency
// release depends on: the local close must not make the logical session terminal.
func TestAbortSessionAppendsNoSessionStopped(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := &seqAppender{}
	h := New(sid, WithAppender(app))
	<-h.AbortSession(ErrResidencyReleased)
	for _, ev := range app.events {
		if _, stopped := ev.(event.SessionStopped); stopped {
			t.Fatal("AbortSession appended a SessionStopped, making a released session terminal")
		}
	}
	if err := h.WaitIdle(context.Background()); !errors.Is(err, ErrSessionStopped) {
		t.Errorf("WaitIdle after local close = %v, want ErrSessionStopped (the in-memory phase is stopped)", err)
	}
}

type refusingAppender struct{ err error }

func (a *refusingAppender) AppendEvent(context.Context, event.Event) (uint64, error) {
	return 0, a.err
}
