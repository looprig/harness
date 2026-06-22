//go:build integration

package persistence

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestEngineLifecycle is the embedded-server smoke: Open starts an in-process JetStream
// server (no TCP) on a temp StoreDir, the returned JetStreamContext can create a stream,
// and Close shuts the server cleanly. A second Open on the SAME StoreDir (simulating a
// process restart) sees the persisted stream — proving the StoreDir is durable across
// the server lifecycle, the property restore depends on.
func TestEngineLifecycle(t *testing.T) {
	// Point the containment root at a temp dir (the production root is $XDG_DATA_HOME
	// when set, else home), then place the StoreDir under it — the realistic config.
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "looprig", "jetstream")

	eng, err := Open(EngineOptions{DataDir: dir, SyncInterval: 2 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	js := eng.JetStream()
	if js == nil {
		t.Fatal("JetStream() returned nil")
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: "S", Subjects: []string{"s.>"}}); err != nil {
		t.Fatalf("AddStream: %v", err)
	}
	if _, err := js.Publish("s.x", []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Restart: a fresh engine on the SAME StoreDir must see the persisted stream + message.
	eng2, err := Open(EngineOptions{DataDir: dir, SyncInterval: 2 * time.Second})
	if err != nil {
		t.Fatalf("Open (restart): %v", err)
	}
	t.Cleanup(func() { _ = eng2.Close() })
	js2 := eng2.JetStream()
	info, err := js2.StreamInfo("S")
	if err != nil {
		t.Fatalf("StreamInfo after restart: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Errorf("restarted stream has %d msgs, want 1 (StoreDir not durable)", info.State.Msgs)
	}
}

// TestOpenRejectsBadStoreDir proves Open fails closed with a typed error when the
// StoreDir cannot be resolved (it does not silently fall back to an unconfined path).
func TestOpenRejectsBadStoreDir(t *testing.T) {
	if _, err := Open(EngineOptions{DataDir: "", SyncInterval: time.Second}); err == nil {
		t.Fatal("Open with empty DataDir succeeded, want a typed StoreDirError")
	}
}
