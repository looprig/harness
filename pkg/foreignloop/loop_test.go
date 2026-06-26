package foreignloop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/loop"
)

// validCfg is the minimal loop.Config a foreign loop accepts: a non-empty system
// prompt (the only field foreignloop.New validates).
func validCfg() loop.Config {
	return loop.Config{Model: llm.ModelSpec{Model: "m", System: "you are a test agent"}}
}

// newTestLoop wires a foreign loop to a fakePublisher with a deterministic
// correlation idGen and a working EventID factory, registering ctx cleanup.
func newTestLoop(t *testing.T, spec Spec, pub EventPublisher) (*Loop, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l, sid, err := New(ctx, mustID(t), mustID(t), loop.Provenance{}, pub, validCfg(), spec, seqIDGen(), workingFac())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, sid
}

// TestBackendInterface pins the compile-time contract: *Loop is a loop.Backend.
func TestBackendInterface(t *testing.T) {
	t.Parallel()
	var _ loop.Backend = (*Loop)(nil)
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	good := func() Spec { return Spec{Agent: &fakeAgent{}} }
	tests := []struct {
		name    string
		cfg     loop.Config
		spec    Spec
		pub     EventPublisher
		nilGen  bool
		nilFac  bool
		wantErr bool
	}{
		{name: "happy path", cfg: validCfg(), spec: good(), pub: &fakePublisher{}, wantErr: false},
		{name: "empty system prompt", cfg: loop.Config{}, spec: good(), pub: &fakePublisher{}, wantErr: true},
		{name: "nil agent", cfg: validCfg(), spec: Spec{}, pub: &fakePublisher{}, wantErr: true},
		{name: "nil publisher", cfg: validCfg(), spec: good(), pub: nil, wantErr: true},
		{name: "nil idGen", cfg: validCfg(), spec: good(), pub: &fakePublisher{}, nilGen: true, wantErr: true},
		{name: "nil factory", cfg: validCfg(), spec: good(), pub: &fakePublisher{}, nilFac: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			idGen := seqIDGen()
			if tt.nilGen {
				idGen = nil
			}
			fac := workingFac()
			if tt.nilFac {
				fac = nil
			}
			l, sid, err := New(ctx, mustID(t), mustID(t), loop.Provenance{}, tt.pub, tt.cfg, tt.spec, idGen, fac)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var cfgErr *ConfigError
				if !errors.As(err, &cfgErr) {
					t.Fatalf("want *ConfigError, got %T: %v", err, err)
				}
				return
			}
			if sid == "" {
				t.Fatal("happy New returned an empty sid")
			}
			shutdown(t, l)
		})
	}
}

// shutdown cleanly stops a loop and waits for Done, asserting the ack is nil.
func shutdown(t *testing.T, l *Loop) {
	t.Helper()
	ack := make(chan error, 1)
	select {
	case l.Commands <- command.Shutdown{Ack: ack}:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out submitting Shutdown")
	}
	select {
	case err := <-ack:
		if err != nil {
			t.Fatalf("Shutdown ack = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Shutdown ack")
	}
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after Shutdown")
	}
}

func TestShutdownClosesDone(t *testing.T) {
	t.Parallel()
	l, _ := newTestLoop(t, Spec{Agent: &fakeAgent{}}, &fakePublisher{})
	shutdown(t, l)
}

func TestSnapshotFreshLoop(t *testing.T) {
	t.Parallel()
	l, _ := newTestLoop(t, Spec{Agent: &fakeAgent{}}, &fakePublisher{})
	msgs, ti, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("fresh loop msgs = %d, want 0", len(msgs))
	}
	if ti != event.TurnIndex(0) {
		t.Fatalf("fresh loop turnIndex = %d, want 0", ti)
	}
	shutdown(t, l)
}

func TestInterruptWhileIdle(t *testing.T) {
	t.Parallel()
	l, _ := newTestLoop(t, Spec{Agent: &fakeAgent{}}, &fakePublisher{})
	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: ack}
	select {
	case got := <-ack:
		if got {
			t.Fatal("idle Interrupt ack = true, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle Interrupt never acked")
	}
	shutdown(t, l)
}

func TestSnapshotAfterExit(t *testing.T) {
	t.Parallel()
	l, _ := newTestLoop(t, Spec{Agent: &fakeAgent{}}, &fakePublisher{})
	shutdown(t, l)
	_, _, err := l.Snapshot(context.Background())
	var snapErr *SnapshotError
	if !errors.As(err, &snapErr) {
		t.Fatalf("Snapshot after exit err = %T %v, want *SnapshotError", err, err)
	}
	if snapErr.Reason != SnapshotLoopExited {
		t.Fatalf("reason = %v, want %v", snapErr.Reason, SnapshotLoopExited)
	}
}
