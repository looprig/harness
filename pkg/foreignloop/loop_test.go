package foreignloop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

type boundTestClient struct{}

func (boundTestClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unused")
}
func (boundTestClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("unused")
}

// validCfg is the minimal loop.BoundDefinition a foreign loop accepts: a non-empty system
// prompt (the only field foreignloop.New validates).
func validCfg() loop.BoundDefinition {
	return promptCfg("you are a test agent", "")
}

func promptCfg(system, instructions string) loop.BoundDefinition {
	opts := []loop.Option{loop.WithName("agent"), loop.WithInference(boundTestClient{}, model.Model{Provider: "lmstudio", APIFormat: model.APIFormatOpenAI, BaseURL: "http://localhost:1234", Name: "m"}), loop.WithSystem(system)}
	if instructions != "" {
		opts = append(opts, loop.WithModes(loop.Mode{Name: "mode", Instructions: instructions}), loop.WithInitialMode("mode"))
	}
	d, err := loop.Define(opts...)
	if err != nil {
		panic(err)
	}
	bound, err := d.Bind(context.Background(), tool.Bindings{SessionID: mustID(panicT{}), LoopID: mustID(panicT{})})
	if err != nil {
		panic(err)
	}
	return bound
}

type panicT struct{}

func (panicT) Fatal(args ...any) { panic(args) }

// newTestLoop wires a foreign loop to a fakePublisher with a deterministic
// correlation idGen and a working EventID factory, registering ctx cleanup.
func newTestLoop(t *testing.T, spec Spec, pub EventPublisher) (*Loop, string) {
	t.Helper()
	// The deterministic seqIDGen mints the SAME first uuid in every test, so every loop
	// shares a sid. The per-(sid,cwd) spawn lock would then collide across tests sharing
	// a cwd; give each loop its own tempdir cwd (the fake agent ignores cwd) unless the
	// caller pinned one to drive a specific lock path.
	if spec.Cwd == "" {
		spec.Cwd = t.TempDir()
	}
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
		cfg     loop.BoundDefinition
		spec    Spec
		pub     EventPublisher
		nilGen  bool
		nilFac  bool
		wantErr bool
	}{
		{name: "happy path", cfg: validCfg(), spec: good(), pub: &fakePublisher{}, wantErr: false},
		{name: "empty system prompt", cfg: nil, spec: good(), pub: &fakePublisher{}, wantErr: true},
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

func TestValidateWiringAcceptsInstructionsOnlyPrompt(t *testing.T) {
	t.Parallel()
	if err := validateWiring(promptCfg("", "mode instructions"), Spec{Agent: &fakeAgent{}}, seqIDGen(), workingFac(), &fakePublisher{}); err != nil {
		t.Fatalf("validateWiring instructions-only: %v", err)
	}
}

func TestNewLateBoundSpecReturnsEmptyInitialSID(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeAgent{}
	idGenCalls := 0
	l, sid, err := New(ctx, mustID(t), mustID(t), loop.Provenance{}, &fakePublisher{}, validCfg(), Spec{
		Agent:   fake,
		Cwd:     t.TempDir(),
		SIDMode: SIDLateBound,
	}, func() (uuid.UUID, error) {
		idGenCalls++
		return uuid.New()
	}, workingFac())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if sid != "" {
		t.Fatalf("sid = %q, want empty", sid)
	}
	if idGenCalls != 0 {
		t.Fatalf("idGen calls = %d, want 0 for late-bound sid", idGenCalls)
	}
	shutdown(t, l)
}

func TestNewRejectsUnknownSIDMode(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l, sid, err := New(ctx, mustID(t), mustID(t), loop.Provenance{}, &fakePublisher{}, validCfg(), Spec{
		Agent:   &fakeAgent{},
		Cwd:     t.TempDir(),
		SIDMode: SIDMode(99),
	}, seqIDGen(), workingFac())
	if err == nil {
		shutdown(t, l)
		t.Fatalf("New sid=%q err=nil, want invalid SIDMode error", sid)
	}
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("err = %T %v, want *ConfigError", err, err)
	}
	if cfgErr.Field != "Spec.SIDMode" {
		t.Fatalf("ConfigError.Field = %q, want Spec.SIDMode", cfgErr.Field)
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

func TestManagedAcceptanceMintFailureReturnsExactErrorAndStartsNoForeignWork(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("foreign acceptance event id mint failed")
	agent := &fakeAgent{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pub := &fakePublisher{}
	l, _, err := New(ctx, mustID(t), mustID(t), loop.Provenance{}, pub, validCfg(), Spec{Agent: agent, Cwd: t.TempDir(), SIDMode: SIDLateBound}, seqIDGen(), event.NewFactory(func() (uuid.UUID, error) {
		return uuid.UUID{}, sentinel
	}, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t)
	accepted := make(chan error, 1)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, NoFold: true, TargetLoopID: l.loopID, Accepted: accepted}
	if got := <-accepted; got != sentinel {
		t.Fatalf("acceptance error = %T %v, want exact sentinel", got, got)
	}
	if agent.calls() != 0 {
		t.Fatalf("foreign spawn calls = %d, want 0", agent.calls())
	}
	for _, ev := range pub.snapshot() {
		if ev.EventHeader().Cause.CommandID == id {
			t.Fatalf("failed acceptance published or started work: %T", ev)
		}
	}
}

func TestManagedAcceptanceAppendFailureReturnsExactErrorAndStartsNoForeignWork(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("foreign acceptance durable append failed")
	agent := &fakeAgent{}
	pub := &fakePublisher{checkedErr: sentinel}
	l, _ := newTestLoop(t, Spec{Agent: agent, SIDMode: SIDLateBound}, pub)
	id := mustID(t)
	accepted := make(chan error, 1)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, NoFold: true, TargetLoopID: l.loopID, Accepted: accepted}
	if got := <-accepted; got != sentinel {
		t.Fatalf("acceptance error = %T %v, want exact sentinel", got, got)
	}
	if agent.calls() != 0 {
		t.Fatalf("foreign spawn calls = %d, want 0", agent.calls())
	}
	for _, ev := range pub.snapshot() {
		if ev.EventHeader().Cause.CommandID == id {
			t.Fatalf("failed acceptance published or started work: %T", ev)
		}
	}
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
