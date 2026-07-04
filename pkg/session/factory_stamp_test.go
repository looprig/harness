package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/core/uuid"
)

// seqGen mints a deterministic, distinct UUID per call (1, 2, 3, ...) so a session
// test can assert minted EventIDs are non-zero without coupling to crypto/rand. It
// is safe for concurrent use.
type seqGen struct {
	mu sync.Mutex
	n  byte
}

func (g *seqGen) gen() (uuid.UUID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return uuid.UUID{g.n}, nil
}

// TestLoopStartedStamped is the END-TO-END proof that the session stamps every
// LoopStarted it publishes with a minted EventID + CreatedAt from its Factory. A
// post-construction NewLoop publishes a LoopStarted that a subscriber attached
// BEFORE the call observes (the construction-time primary LoopStarted uses the
// identical path but is unobservable by a late subscriber — the hub has no replay).
// The clock and id-gen are pinned so the assertion is deterministic.
func TestLoopStartedStamped(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Pin the clock and id-gen AFTER construction; the session's Factory reads them
	// live (closures over s.now/s.newID), so the next NewLoop's LoopStarted is
	// deterministically stamped.
	ts := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return ts }
	gen := &seqGen{}
	s.newID = gen.gen

	// Subscribe BEFORE NewLoop (the hub has no replay) so the LoopStarted it publishes
	// is observed.
	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	if _, err := s.NewLoop(loop.Provenance{}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("y")}})); err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	ls, ok := firstMatching[event.LoopStarted](t, sub)
	if !ok {
		t.Fatal("no LoopStarted observed on the fan-in")
	}
	h := ls.EventHeader()
	if h.EventID.IsZero() {
		t.Error("LoopStarted EventID is zero, want a minted id")
	}
	if !h.CreatedAt.Equal(ts) {
		t.Errorf("LoopStarted CreatedAt = %v, want %v (factory clock)", h.CreatedAt, ts)
	}
	// The loop coordinates the producer set must survive the stamp.
	if h.SessionID != s.SessionID || h.LoopID.IsZero() {
		t.Errorf("LoopStarted coordinates lost: SessionID=%v LoopID=%v", h.SessionID, h.LoopID)
	}
}

// TestSessionStartedStamped proves the session's construction-time SessionStarted
// carries a minted EventID + CreatedAt from the Factory. The construction-time
// event is unobservable by a late subscriber (the hub has no replay), so this is a
// white-box check: it pins the seams, runs the SAME factory the session built in
// New, and asserts the stamp the construction publish applied — that the session
// builds and wires the Factory from newID + now so SessionStarted is never a
// zero-EventID event.
func TestSessionStartedStamped(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 9, 30, 0, 0, time.UTC)

	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// The session must have built a Factory in New (the seam the construction-time
	// SessionStarted stamped through).
	if s.factory == nil {
		t.Fatal("session.factory is nil; New must build the event Factory")
	}

	// Pin the seams the Factory reads live, then drive the same stamp the
	// construction SessionStarted used: a non-zero EventID + the pinned CreatedAt,
	// with the SessionID coordinate preserved.
	s.now = func() time.Time { return ts }
	gen := &seqGen{}
	s.newID = gen.gen

	stamped, err := s.factory.Stamp(event.SessionStarted{}.EventHeader())
	if err != nil {
		t.Fatalf("factory.Stamp: %v", err)
	}
	if stamped.EventID.IsZero() {
		t.Error("SessionStarted EventID is zero, want a minted id")
	}
	if !stamped.CreatedAt.Equal(ts) {
		t.Errorf("SessionStarted CreatedAt = %v, want %v (factory clock)", stamped.CreatedAt, ts)
	}
}

// failOnCallGen mints a fresh UUID for the first failAfter calls, then fails every
// subsequent call with err. It lets a test let the session id mint succeed (call 1)
// while failing the construction-time SessionStarted EventID mint (call 2), so the
// REAL New failure branch — factory.Stamp errors → New returns nil + typed error,
// before the SessionStarted publish — is exercised. Safe for concurrent use.
type failOnCallGen struct {
	mu        sync.Mutex
	n         int
	failAfter int
	err       error
}

func (g *failOnCallGen) gen() (uuid.UUID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	if g.n > g.failAfter {
		return uuid.UUID{}, g.err
	}
	return uuid.New()
}

// TestNewSessionStartedMintErrorFails proves New propagates a mint error from the
// construction-time SessionStarted stamp: if the EventID for SessionStarted cannot
// be minted (crypto/rand failure), New aborts with a typed *SessionError of kind
// SessionIDGenerationFailed and returns a nil session — it never publishes a
// zero-EventID SessionStarted. This drives the REAL New failure branch (session.go
// ~437) via the newSession test seam, which injects a newID that succeeds for the
// session id (the 1st mint) and then fails the SessionStarted EventID (the 2nd
// mint, the first FACTORY mint).
func TestNewSessionStartedMintErrorFails(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	// Succeed once (the session id), then fail (the SessionStarted EventID stamp).
	gen := &failOnCallGen{failAfter: 1, err: genErr}

	s, err := newSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}), gen.gen, time.Now)
	if s != nil {
		t.Fatalf("newSession returned a non-nil session on a mint failure; want nil (no half-built session)")
	}
	var sessErr *SessionError
	if !errors.As(err, &sessErr) {
		t.Fatalf("newSession err = %v (%T), want a *SessionError", err, err)
	}
	if sessErr.Kind != SessionIDGenerationFailed {
		t.Fatalf("SessionError.Kind = %q, want %q", sessErr.Kind, SessionIDGenerationFailed)
	}
	if !errors.Is(err, genErr) {
		t.Fatalf("err does not wrap the underlying mint error %v: %v", genErr, err)
	}
}
