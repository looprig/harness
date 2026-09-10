package rig

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
)

// countSessionStarted replays the durable stream filed under id and counts the
// SessionStarted records in it. A stream that cannot be opened at all counts zero and
// says so through ok, so a caller can tell "no such session" from "a session with no
// start record" instead of folding both into a bare zero.
func countSessionStarted(t *testing.T, store *sessionstore.Store, id uuid.UUID) (count int, ok bool) {
	t.Helper()
	replayer, err := store.OpenEventReplayer(id, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		return 0, false
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		return 0, false
	}
	defer cursor.Close()
	for {
		ev, _, nextErr := cursor.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			return count, true
		}
		if nextErr != nil {
			t.Fatalf("replay under %s: %v", id, nextErr)
		}
		if _, isStart := ev.(event.SessionStarted); isStart {
			count++
		}
	}
}

// TestWithSessionIDAdoptsTheCallersID is the whole point of the option: a session id
// decided BEFORE launch must be the id the runtime runs under AND the id the durable
// journal is filed under, because a caller that recorded the id in an immutable binding
// has no second chance to learn a different one.
//
// The two arms are a matched pair. The first creates a session WITHOUT the option and
// establishes that the caller's chosen id names nothing in the store — the negative that
// makes the second arm's "the stream is now there" mean something, and the positive
// control for a "nothing was found" assertion that would otherwise pass against a broken
// replayer.
func TestWithSessionIDAdoptsTheCallersID(t *testing.T) {
	store, _ := lifecycleStore(t)
	r := lifecycleRig(t, store)
	chosen, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}

	unchosen, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	minted := unchosen.SessionID()
	if err := unchosen.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if minted == chosen {
		t.Fatalf("NewSession without the option minted the caller's id %s; the arms below cannot be told apart", chosen)
	}
	if got, ok := countSessionStarted(t, store, minted); !ok || got != 1 {
		t.Fatalf("self-minted session %s: SessionStarted count = %d (readable %v), want 1; the replayer cannot see a session that exists", minted, got, ok)
	}
	if got, _ := countSessionStarted(t, store, chosen); got != 0 {
		t.Fatalf("the caller's id %s already names %d SessionStarted before it was ever used", chosen, got)
	}

	adopted, err := r.NewSession(context.Background(), WithSessionID(chosen))
	if err != nil {
		t.Fatalf("NewSession(WithSessionID): %v", err)
	}
	defer func() { _ = adopted.Shutdown(context.Background()) }()
	if got := adopted.SessionID(); got != chosen {
		t.Errorf("SessionID() = %s, want the caller's %s", got, chosen)
	}
	if got, ok := countSessionStarted(t, store, chosen); !ok || got != 1 {
		t.Errorf("durable stream under the caller's id %s: SessionStarted count = %d (readable %v), want 1; the journal was bound to a different id", chosen, got, ok)
	}
}

// TestWithSessionIDIsRestorableUnderTheAdoptedID is the consequence a caller actually
// depends on: the id it recorded before launch is the id RestoreSession answers to. A
// runtime that adopted the id for its in-memory SessionID() but filed its durable state
// elsewhere would satisfy the assertion above on SessionID() alone.
func TestWithSessionIDIsRestorableUnderTheAdoptedID(t *testing.T) {
	store, _ := lifecycleStore(t)
	r := lifecycleRig(t, store)
	chosen, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.NewSession(context.Background(), WithSessionID(chosen))
	if err != nil {
		t.Fatal(err)
	}
	releaser, ok := s.(session.Releaser)
	if !ok {
		t.Fatal("the live session does not satisfy session.Releaser; it cannot be released nonterminally")
	}
	if err := releaser.ReleaseResidency(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := r.RestoreSession(context.Background(), chosen)
	if err != nil {
		t.Fatalf("RestoreSession(%s): %v", chosen, err)
	}
	defer func() { _ = restored.Shutdown(context.Background()) }()
	if got := restored.SessionID(); got != chosen {
		t.Errorf("restored SessionID() = %s, want %s", got, chosen)
	}
}

// TestWithSessionIDValidation proves the option fails CLOSED at the boundary rather than
// silently falling back to a minted id. A zero id reaching this option is a wiring slip —
// the caller believed it was naming a session and was not — and a caller that recorded
// that non-answer in an immutable binding would name nothing forever. The internal
// sessionruntime option ignores a zero id on purpose (it defends the composition root
// against nulling out a field); at the PUBLIC boundary the same input is a caller error.
func TestWithSessionIDValidation(t *testing.T) {
	t.Parallel()
	nonzero, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	other, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		opts    []SessionOption
		wantErr bool
		kind    SessionOptionErrorKind
	}{
		{name: "valid id", opts: []SessionOption{WithSessionID(nonzero)}},
		{name: "composes with a seed", opts: []SessionOption{WithSessionID(nonzero), WithSeedSnapshot("abc")}},
		{name: "zero id rejected", opts: []SessionOption{WithSessionID(uuid.UUID{})}, wantErr: true, kind: SessionOptionZeroSessionID},
		{name: "duplicate id rejected", opts: []SessionOption{WithSessionID(nonzero), WithSessionID(other)}, wantErr: true, kind: SessionOptionDuplicateSessionID},
		{name: "duplicate of the SAME id still rejected", opts: []SessionOption{WithSessionID(nonzero), WithSessionID(nonzero)}, wantErr: true, kind: SessionOptionDuplicateSessionID},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := resolveSessionOptions(tt.opts)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("resolveSessionOptions: %v", err)
				}
				if resolved.sessionID != nonzero {
					t.Errorf("resolved sessionID = %s, want %s", resolved.sessionID, nonzero)
				}
				return
			}
			var optErr *SessionOptionError
			if !errors.As(err, &optErr) {
				t.Fatalf("error = %T %v, want *SessionOptionError", err, err)
			}
			if optErr.Kind != tt.kind {
				t.Errorf("kind = %s, want %s", optErr.Kind, tt.kind)
			}
		})
	}
}

// TestWithSessionIDDoesNotVerifyFreshness characterizes the ordering constraint the
// option's doc comment states, so the constraint has a reader and not only a sentence.
//
// Harness does NOT check that the caller's id names no existing session. Reusing the id
// of a session that has already ended does not fail and does not create a second
// session: it re-opens the SAME durable stream under a fresh grant and appends another
// SessionStarted to it, leaving one stream with two starts. Uniqueness is therefore the
// caller's obligation, and the lease is not a substitute for it — a lease only refuses a
// LIVE holder, and this one had none.
//
// This is a measurement of current behaviour, not an endorsement of it. If harness ever
// starts refusing a reused id, this test is the one that should fail.
func TestWithSessionIDDoesNotVerifyFreshness(t *testing.T) {
	store, _ := lifecycleStore(t)
	r := lifecycleRig(t, store)
	chosen, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.NewSession(context.Background(), WithSessionID(chosen))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := countSessionStarted(t, store, chosen); !ok || got != 1 {
		t.Fatalf("after the first session: SessionStarted count = %d (readable %v), want 1", got, ok)
	}

	second, err := r.NewSession(context.Background(), WithSessionID(chosen))
	if err != nil {
		t.Fatalf("reusing an ended session's id was refused: %v; the doc comment claims it is not", err)
	}
	defer func() { _ = second.Shutdown(context.Background()) }()
	if got := second.SessionID(); got != chosen {
		t.Errorf("reused SessionID() = %s, want %s", got, chosen)
	}
	got, ok := countSessionStarted(t, store, chosen)
	if !ok {
		t.Fatal("the reused stream became unreadable")
	}
	if got != 2 {
		t.Errorf("SessionStarted count under the reused id = %d, want 2 (one stream, two starts)", got)
	}
}
