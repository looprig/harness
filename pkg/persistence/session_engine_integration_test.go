//go:build integration

package persistence

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
)

// newIntegrationStoreRoot points the data root at a temp dir and opens a real store root.
func newIntegrationStoreRoot(t *testing.T) *SessionStoreRoot {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root, err := OpenSessionStoreRoot()
	if err != nil {
		t.Fatalf("OpenSessionStoreRoot: %v", err)
	}
	return root
}

// TestSessionEngineLifecycle opens a real embedded engine for one session, proves a second
// open of the same session is rejected as locked, and that a clean close releases the lock
// so the session can be reopened.
func TestSessionEngineLifecycle(t *testing.T) {
	root := newIntegrationStoreRoot(t)
	id := mustUUID(t)

	first, err := root.OpenSessionEngine(id)
	if err != nil {
		t.Fatalf("OpenSessionEngine: %v", err)
	}
	js := first.JetStream()
	if js == nil {
		t.Fatal("JetStream() returned nil")
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: "S", Subjects: []string{"s.>"}}); err != nil {
		t.Fatalf("AddStream: %v", err)
	}

	// A second open of the same live session must fail closed, before any server starts.
	if _, err := root.OpenSessionEngine(id); err == nil {
		t.Fatal("second OpenSessionEngine succeeded, want *SessionLockedError")
	} else {
		var locked *SessionLockedError
		if !errors.As(err, &locked) {
			t.Fatalf("second open error = %T %v, want *SessionLockedError", err, err)
		}
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After a clean close the lock is released and the session reopens; its StoreDir is
	// durable, so the stream created above survives.
	reopened, err := root.OpenSessionEngine(id)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.JetStream().StreamInfo("S"); err != nil {
		t.Fatalf("StreamInfo after reopen: %v", err)
	}
}

// TestSessionEngineDistinctIDsCoexist proves two different sessions run simultaneously with
// isolated StoreDirs.
func TestSessionEngineDistinctIDsCoexist(t *testing.T) {
	root := newIntegrationStoreRoot(t)

	a, err := root.OpenSessionEngine(mustUUID(t))
	if err != nil {
		t.Fatalf("OpenSessionEngine(a): %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	b, err := root.OpenSessionEngine(mustUUID(t))
	if err != nil {
		t.Fatalf("OpenSessionEngine(b): %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	if _, err := a.JetStream().AddStream(&nats.StreamConfig{Name: "A", Subjects: []string{"a.>"}}); err != nil {
		t.Fatalf("AddStream(a): %v", err)
	}
	if _, err := b.JetStream().AddStream(&nats.StreamConfig{Name: "B", Subjects: []string{"b.>"}}); err != nil {
		t.Fatalf("AddStream(b): %v", err)
	}

	// Each engine sees only its own stream — the StoreDirs are isolated per session.
	if _, err := a.JetStream().StreamInfo("B"); err == nil {
		t.Error("session a can see session b's stream; StoreDirs are not isolated")
	}
}
