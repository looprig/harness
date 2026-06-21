//go:build integration

package journal_test

import (
	"context"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/uuid"
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

// mustAcquireLease provisions a LeaseManager over js and acquires a lease for sid,
// failing the test on error. It is the integration-test seam that satisfies
// NewSessionJournal's required Lease dependency: tests acquire ownership here and
// pass the lease in, exactly as the composition root will. The lease is released on
// cleanup. The default (production) lease TTL applies; tests that need to drive expiry
// deterministically build their own manager with an injected clock.
func mustAcquireLease(t *testing.T, js nats.JetStreamContext, sid uuid.UUID) journal.Lease {
	t.Helper()
	lm, err := journal.NewLeaseManager(js)
	if err != nil {
		t.Fatalf("NewLeaseManager: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire lease for %v: %v", sid, err)
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		_ = lease.Release(rctx)
	})
	return lease
}
