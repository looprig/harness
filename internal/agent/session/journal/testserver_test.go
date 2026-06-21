//go:build integration

package journal_test

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// newEmbeddedJS starts an in-process JetStream server (no TCP) on a temp StoreDir and
// returns a connected client. Everything is torn down via t.Cleanup.
func newEmbeddedJS(t *testing.T) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		JetStream:  true,
		StoreDir:   t.TempDir(),
		DontListen: true, // no TCP socket
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	return nc, js
}

func TestEmbeddedServerSmoke(t *testing.T) {
	_, js := newEmbeddedJS(t)
	if _, err := js.AddStream(&nats.StreamConfig{Name: "S", Subjects: []string{"s.>"}}); err != nil {
		t.Fatalf("AddStream: %v", err)
	}
}
