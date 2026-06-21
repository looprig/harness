//go:build integration

package coding

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/persistence"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// openEngine starts an embedded engine on dir (creating it under a temp XDG root) and
// tears it down via cleanup. It is the CLI-shaped composition: a real embedded server
// over a persistent on-disk StoreDir, exactly as cmd/cli wires it.
func openEngine(t *testing.T, dir string) *persistence.Engine {
	t.Helper()
	eng, err := persistence.Open(persistence.EngineOptions{DataDir: dir, SyncInterval: 2 * time.Second})
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// drainTurn submits input through the persisted agent and drains a fresh subscription to
// the turn terminal — deterministic (unlike a WaitIdle that can race the fire-and-forget
// submit). The subscription is created BEFORE the submit so the terminal is never missed.
func drainTurn(t *testing.T, c *Coding, text string) {
	t.Helper()
	sub, err := c.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a terminal")
			}
			switch ev.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return
			}
		case <-timeout:
			t.Fatal("no terminal within deadline")
		}
	}
}

// TestPersistenceWiringRoundTrip is the headline CLI-shaped wiring smoke for Task 10.3:
// a NEW persisted session (built through the real composition wiring — embedded engine +
// journal + lease + appenders) runs a turn that persists, the agent is Closed (releasing
// the lease), the embedded server is RESTARTED on the SAME StoreDir, and the SAME session
// is RESUMED via the resume path. The restored session's ReplayBacklog reproduces the
// committed Enduring events and a fresh turn continues — proving the durable log survived
// a full process-restart cycle. (This is the integration smoke; Phase 11 adds the
// rigorous property tests.)
func TestPersistenceWiringRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "urvi", "jetstream")

	// --- original run: a NEW persisted session ---
	eng := openEngine(t, dir)
	p, err := NewPersistence(eng.JetStream())
	if err != nil {
		t.Fatalf("NewPersistence: %v", err)
	}

	c, err := newPersistentWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, testSpec(), p, SessionSelector{})
	if err != nil {
		t.Fatalf("newPersistentWithClient (new): %v", err)
	}
	sessionID := c.SessionID()
	if sessionID.IsZero() {
		t.Fatal("new persisted session has a zero SessionID")
	}

	drainTurn(t, c, "hello")

	// The new session is NOT a restore, so ReplayBacklog returns nil (the TUI skips the
	// cold repaint). The durable events are nonetheless on disk (asserted after restore).
	if backlog, err := c.ReplayBacklog(context.Background()); err != nil {
		t.Fatalf("new-session ReplayBacklog: %v", err)
	} else if len(backlog) != 0 {
		t.Errorf("new-session ReplayBacklog returned %d events, want 0", len(backlog))
	}

	// Clean shutdown releases the lease so a successor (the restored session) can
	// re-acquire without waiting out the TTL.
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close (original): %v", err)
	}

	// --- simulate restart: shut the server, re-open a fresh engine on the SAME StoreDir ---
	if err := eng.Close(); err != nil {
		t.Fatalf("engine Close (restart): %v", err)
	}
	eng2 := openEngine(t, dir)
	p2, err := NewPersistence(eng2.JetStream())
	if err != nil {
		t.Fatalf("NewPersistence (restart): %v", err)
	}

	// --- resume the same session ---
	c2, err := newPersistentWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("after restore")}}, testSpec(), p2, SessionSelector{Resume: sessionID})
	if err != nil {
		t.Fatalf("newPersistentWithClient (resume): %v", err)
	}
	t.Cleanup(func() { _ = c2.Close(context.Background()) })

	// Identity is stable across the restart.
	if c2.SessionID() != sessionID {
		t.Errorf("resumed SessionID = %v, want %v", c2.SessionID(), sessionID)
	}

	// The restored session's ReplayBacklog reproduces the committed Enduring history: it
	// MUST contain the user turn's committed messages (a TurnStarted + at least one
	// StepDone or terminal). The headline property: the durable projection survived.
	backlog, err := c2.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("resumed ReplayBacklog: %v", err)
	}
	if !hasType(backlog, event.TurnStarted{}) {
		t.Errorf("resumed backlog missing TurnStarted: %v", typeNames(backlog))
	}
	if !hasType(backlog, event.RestoreDone{}) {
		t.Errorf("resumed backlog missing RestoreDone (restore was not bracketed): %v", typeNames(backlog))
	}

	// The session continues: a fresh turn is accepted and reaches a terminal.
	drainTurn(t, c2, "continue")
}

// hasType reports whether evs contains an event of the same concrete type as want.
func hasType(evs []event.Event, want event.Event) bool {
	wt := reflect.TypeOf(want)
	for _, e := range evs {
		if reflect.TypeOf(e) == wt {
			return true
		}
	}
	return false
}

func typeNames(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = reflect.TypeOf(e).String()
	}
	return out
}

// TestPersistenceListAndResumeSeams covers the --list / --resume building blocks at the
// agent layer: a NEW persisted session appears in the catalog listing (replay-free), and
// the lease is released on Close so a successor can re-acquire (the resume path proves
// re-acquisition end-to-end in the round-trip above; here we assert the listing seam).
func TestPersistenceListAndResumeSeams(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "urvi", "jetstream")

	eng := openEngine(t, dir)
	p, err := NewPersistence(eng.JetStream())
	if err != nil {
		t.Fatalf("NewPersistence: %v", err)
	}

	c, err := newPersistentWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("reply")}}, testSpec(), p, SessionSelector{})
	if err != nil {
		t.Fatalf("newPersistentWithClient: %v", err)
	}
	sessionID := c.SessionID()
	drainTurn(t, c, "list me")

	// The listing reads the KV catalog only (no replay) and includes the running session.
	metas, err := p.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if !containsSession(metas, sessionID) {
		t.Errorf("ListSessions did not include %v", sessionID)
	}

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After a clean Close the lease is released: a successor Restore re-acquires it.
	c2, err := newPersistentWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("resumed")}}, testSpec(), p, SessionSelector{Resume: sessionID})
	if err != nil {
		t.Fatalf("resume after Close (lease not released?): %v", err)
	}
	_ = c2.Close(context.Background())
}

func containsSession(metas []journal.SessionMeta, id uuid.UUID) bool {
	for _, m := range metas {
		if m.SessionID == id {
			return true
		}
	}
	return false
}
