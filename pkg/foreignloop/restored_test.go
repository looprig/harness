package foreignloop

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
)

// newRestoredTestLoop wires a journal-seeded foreign loop to a fakePublisher with the
// deterministic correlation idGen and a working EventID factory, defaulting cwd to a
// unique tempdir so the per-(sid,cwd) spawn lock never collides.
func newRestoredTestLoop(t *testing.T, spec Spec, pub EventPublisher, seed RestoredForeign) *Loop {
	t.Helper()
	if spec.Cwd == "" {
		spec.Cwd = t.TempDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l, err := NewRestored(ctx, mustID(t), mustID(t), loop.Provenance{}, pub, validCfg(), spec, seqIDGen(), workingFac(), seed)
	if err != nil {
		t.Fatalf("NewRestored: %v", err)
	}
	return l
}

func TestNewRestoredValidation(t *testing.T) {
	t.Parallel()
	good := func() Spec { return Spec{Agent: &fakeAgent{}} }
	validSeed := func() RestoredForeign {
		return RestoredForeign{ForeignSID: "sid-x", TurnIndex: 2, Msgs: content.AgenticMessages{aiMessage("prior")}}
	}
	tests := []struct {
		name    string
		cfg     loop.BoundDefinition
		spec    Spec
		pub     EventPublisher
		nilGen  bool
		nilFac  bool
		seed    RestoredForeign
		wantErr bool
	}{
		{name: "happy path", cfg: validCfg(), spec: good(), pub: &fakePublisher{}, seed: validSeed(), wantErr: false},
		{name: "empty system prompt", cfg: nil, spec: good(), pub: &fakePublisher{}, seed: validSeed(), wantErr: true},
		{name: "nil agent", cfg: validCfg(), spec: Spec{}, pub: &fakePublisher{}, seed: validSeed(), wantErr: true},
		{name: "nil publisher", cfg: validCfg(), spec: good(), pub: nil, seed: validSeed(), wantErr: true},
		{name: "nil idGen", cfg: validCfg(), spec: good(), pub: &fakePublisher{}, nilGen: true, seed: validSeed(), wantErr: true},
		{name: "nil factory", cfg: validCfg(), spec: good(), pub: &fakePublisher{}, nilFac: true, seed: validSeed(), wantErr: true},
		{name: "empty foreign sid", cfg: validCfg(), spec: good(), pub: &fakePublisher{}, seed: RestoredForeign{TurnIndex: 1}, wantErr: true},
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
			spec := tt.spec
			if spec.Cwd == "" {
				spec.Cwd = t.TempDir()
			}
			l, err := NewRestored(ctx, mustID(t), mustID(t), loop.Provenance{}, tt.pub, tt.cfg, spec, idGen, fac, tt.seed)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewRestored() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var cfgErr *ConfigError
				if !errors.As(err, &cfgErr) {
					t.Fatalf("want *ConfigError, got %T: %v", err, err)
				}
				return
			}
			shutdown(t, l)
		})
	}
}

// TestNewRestoredSeedSnapshot proves a restored loop comes up IDLE seeded with the
// recovered committed state: Snapshot returns exactly the seed messages and turn index,
// and a Shutdown acks nil and closes Done (the actor was idle, no turn pending).
func TestNewRestoredSeedSnapshot(t *testing.T) {
	t.Parallel()
	seed := RestoredForeign{
		ForeignSID: "sid-restored",
		TurnIndex:  3,
		Msgs:       content.AgenticMessages{aiMessage("first"), aiMessage("second")},
	}
	l := newRestoredTestLoop(t, Spec{Agent: &fakeAgent{}}, &fakePublisher{}, seed)

	msgs, ti, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if ti != seed.TurnIndex {
		t.Fatalf("turnIndex = %d, want %d", ti, seed.TurnIndex)
	}
	if len(msgs) != len(seed.Msgs) {
		t.Fatalf("msgs len = %d, want %d", len(msgs), len(seed.Msgs))
	}
	for i := range msgs {
		if got, want := firstText(t, msgs[i]), firstText(t, seed.Msgs[i]); got != want {
			t.Fatalf("msgs[%d] = %q, want %q", i, got, want)
		}
	}
	shutdown(t, l)
}

func TestNewRestoredMarksForeignSIDBound(t *testing.T) {
	t.Parallel()
	seed := RestoredForeign{ForeignSID: "sid-restored-bound", TurnIndex: 3}
	l := newRestoredTestLoop(t, Spec{Agent: &fakeAgent{}}, &fakePublisher{}, seed)

	if !l.sidBound {
		t.Fatal("restored loop sidBound = false, want true")
	}
	shutdown(t, l)
}

// TestNewRestoredResumesSession proves the next turn RESUMES the recovered session: a
// restored loop seeds hasSpawned=true, so the ForeignTurn carries StartNew=false and the
// recovered sid, and the turn index advances from the seeded value.
func TestNewRestoredResumesSession(t *testing.T) {
	t.Parallel()
	seed := RestoredForeign{ForeignSID: "sid-resume-7", TurnIndex: 5}
	agent := &fakeAgent{
		transcript: writeTranscript(t, "resumed reply"),
		events:     []ForeignEvent{{Kind: ForeignTerminalOK, Message: aiMessage("done")}},
	}
	l := newRestoredTestLoop(t, Spec{Agent: agent}, &fakePublisher{}, seed)

	submitUserInput(t, l, "continue")
	waitTurnIndex(t, l, seed.TurnIndex+1)

	ft := agent.lastForeignTurn()
	if ft.StartNew {
		t.Fatal("restored ForeignTurn.StartNew = true, want false (resume)")
	}
	if ft.ForeignSID != seed.ForeignSID {
		t.Fatalf("ForeignTurn.ForeignSID = %q, want %q", ft.ForeignSID, seed.ForeignSID)
	}
	shutdown(t, l)
}

func TestBuildRestoredWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		seed    RestoredForeign
		wantErr bool
	}{
		{name: "happy build", seed: RestoredForeign{ForeignSID: "sid-b", TurnIndex: 1}, wantErr: false},
		{name: "empty sid errors", seed: RestoredForeign{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			build := BuildRestoredWith(Spec{Agent: &fakeAgent{}, Cwd: t.TempDir()})
			be, err := build(ctx, mustID(t), mustID(t), loop.Provenance{}, &fakePublisher{}, validCfg(), seqIDGen(), workingFac(), tt.seed)
			if (err != nil) != tt.wantErr {
				t.Fatalf("build err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if be != nil {
					t.Fatalf("error build returned non-nil Backend %v (must be a true nil interface)", be)
				}
				var cfgErr *ConfigError
				if !errors.As(err, &cfgErr) {
					t.Fatalf("want *ConfigError, got %T: %v", err, err)
				}
				return
			}
			if be == nil {
				t.Fatal("happy build returned a nil Backend")
			}
			l, ok := be.(*Loop)
			if !ok {
				t.Fatalf("Backend = %T, want *Loop", be)
			}
			shutdown(t, l)
		})
	}
}
