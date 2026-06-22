package session

import (
	"context"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
)

// fpTool is a minimal InvokableTool whose Info reports a fixed name, used to drive
// the ToolPolicyRev derivation deterministically.
type fpTool struct{ name string }

func (t fpTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: t.name}, nil
}
func (t fpTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	return tool.TextResult(""), nil
}

// fpConfig builds a loop.Config with the given model, system prompt, and tool
// names so a test can vary exactly one fingerprint input at a time.
func fpConfig(model, system string, toolNames ...string) loop.Config {
	reg := make([]tool.InvokableTool, 0, len(toolNames))
	for _, n := range toolNames {
		reg = append(reg, fpTool{name: n})
	}
	return loop.Config{
		Client: &stubLLM{},
		Model:  llm.ModelSpec{Model: model, System: system},
		Tools:  loop.ToolSet{Registry: reg},
	}
}

// TestFingerprintFromDeterministic asserts FingerprintFrom is stable for identical
// config (same inputs -> Equal fingerprints, including tool order independence) and
// that it sets ModelID from the spec verbatim with non-empty digest fields once a
// prompt and tools are present.
func TestFingerprintFromDeterministic(t *testing.T) {
	t.Parallel()

	a := FingerprintFrom(fpConfig("model-x", "you are helpful", "Read", "Write"))
	b := FingerprintFrom(fpConfig("model-x", "you are helpful", "Read", "Write"))
	if !a.Equal(b) {
		t.Fatalf("FingerprintFrom not deterministic: %+v != %+v", a, b)
	}

	// Tool ordering must not change the fingerprint (names are sorted before hashing).
	reordered := FingerprintFrom(fpConfig("model-x", "you are helpful", "Write", "Read"))
	if !a.Equal(reordered) {
		t.Errorf("tool order changed fingerprint: %+v != %+v", a, reordered)
	}

	if a.ModelID != "model-x" {
		t.Errorf("ModelID = %q, want %q", a.ModelID, "model-x")
	}
	if a.SystemPromptRev == "" {
		t.Error("SystemPromptRev is empty, want a sha256 digest of the prompt")
	}
	if a.ToolPolicyRev == "" {
		t.Error("ToolPolicyRev is empty, want a sha256 digest of the tool set")
	}
}

// TestFingerprintFromDiffers asserts a change in any one of the three derivable
// inputs (model id, system prompt, tool set) produces a different fingerprint.
func TestFingerprintFromDiffers(t *testing.T) {
	t.Parallel()

	base := FingerprintFrom(fpConfig("model-x", "prompt A", "Read"))

	tests := []struct {
		name string
		cfg  loop.Config
	}{
		{"different model", fpConfig("model-y", "prompt A", "Read")},
		{"different system prompt", fpConfig("model-x", "prompt B", "Read")},
		{"different tool set", fpConfig("model-x", "prompt A", "Bash")},
		{"extra tool", fpConfig("model-x", "prompt A", "Read", "Write")},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FingerprintFrom(tt.cfg)
			if base.Equal(got) {
				t.Errorf("fingerprint did not change for %s: both %+v", tt.name, got)
			}
		})
	}
}

// TestFingerprintFromSwarmFieldsEmpty pins that FingerprintFrom derives ONLY the
// loop.Config fields: the swarm-level fields (AgentKind, RuntimeSkills, WorkspaceRoot)
// are NOT on loop.Config, so a bare FingerprintFrom leaves them empty/zero — they are
// injected by the composition root via WithConfigFingerprintFields and merged with
// fingerprintWith.
func TestFingerprintFromSwarmFieldsEmpty(t *testing.T) {
	t.Parallel()
	fp := FingerprintFrom(fpConfig("model-x", "prompt", "Read"))
	if fp.AgentKind != "" {
		t.Errorf("AgentKind = %q, want \"\" (not a loop.Config field)", fp.AgentKind)
	}
	if fp.RuntimeSkills {
		t.Error("RuntimeSkills = true, want false (not a loop.Config field)")
	}
	if fp.WorkspaceRoot != "" {
		t.Errorf("WorkspaceRoot = %q, want \"\" (not a loop.Config field)", fp.WorkspaceRoot)
	}
}

// TestFingerprintWithMergesSwarmFields asserts fingerprintWith applies the injected
// swarm-level fields onto the loop-derived fingerprint, and that a difference in ANY
// one of them (AgentKind, RuntimeSkills, WorkspaceRoot) alone — same loop.Config —
// yields an unequal fingerprint. This is what makes a restore reject a session resuming
// under a different agent identity, skill-trust mode, or workspace.
func TestFingerprintWithMergesSwarmFields(t *testing.T) {
	t.Parallel()

	cfg := fpConfig("model-x", "prompt", "Read")
	base := ConfigFingerprintFields{
		AgentKind:     "swe:orchestrator",
		RuntimeSkills: true,
		WorkspaceRoot: "/home/user/repo",
	}
	baseFP := fingerprintWith(cfg, base)

	// The merged fingerprint carries the injected fields verbatim.
	if baseFP.AgentKind != base.AgentKind {
		t.Errorf("AgentKind = %q, want %q", baseFP.AgentKind, base.AgentKind)
	}
	if baseFP.RuntimeSkills != base.RuntimeSkills {
		t.Errorf("RuntimeSkills = %v, want %v", baseFP.RuntimeSkills, base.RuntimeSkills)
	}
	if baseFP.WorkspaceRoot != base.WorkspaceRoot {
		t.Errorf("WorkspaceRoot = %q, want %q", baseFP.WorkspaceRoot, base.WorkspaceRoot)
	}

	diffKind := base
	diffKind.AgentKind = "swe:operator"
	diffSkills := base
	diffSkills.RuntimeSkills = false
	diffRoot := base
	diffRoot.WorkspaceRoot = "/other/repo"

	tests := []struct {
		name   string
		fields ConfigFingerprintFields
	}{
		{"AgentKind differs", diffKind},
		{"RuntimeSkills differs", diffSkills},
		{"WorkspaceRoot differs", diffRoot},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := fingerprintWith(cfg, tt.fields)
			if baseFP.Equal(got) {
				t.Errorf("fingerprint did not change for %s: both %+v", tt.name, got)
			}
		})
	}
}

// TestFingerprintWithEmptyFieldsMatchesBare asserts the additive-compatibility path: a
// fingerprint computed with the zero ConfigFingerprintFields (a non-swarm caller) is
// Equal to the bare FingerprintFrom over the same config — so an old session persisted
// before the swarm fields existed restores equal to one re-derived today without them.
func TestFingerprintWithEmptyFieldsMatchesBare(t *testing.T) {
	t.Parallel()
	cfg := fpConfig("model-x", "prompt", "Read")
	if !fingerprintWith(cfg, ConfigFingerprintFields{}).Equal(FingerprintFrom(cfg)) {
		t.Error("fingerprintWith with empty fields != bare FingerprintFrom; the compatibility path is broken")
	}
}

// TestFingerprintFromEmptyTools asserts the boundary: a config with no tools and an
// empty prompt still yields a stable fingerprint (the digests of empty inputs are
// well-defined and equal across calls).
func TestFingerprintFromEmptyTools(t *testing.T) {
	t.Parallel()
	a := FingerprintFrom(loop.Config{Client: &stubLLM{}})
	b := FingerprintFrom(loop.Config{Client: &stubLLM{}})
	if !a.Equal(b) {
		t.Errorf("empty-config fingerprint not deterministic: %+v != %+v", a, b)
	}
}

// TestSessionStartedCarriesConfig is the end-to-end proof that the construction-time
// SessionStarted the session publishes carries a non-empty Config derived from the
// loop.Config. The construction-time event is unobservable by a late subscriber (the
// hub has no replay), so this asserts the wiring two ways: (1) FingerprintFrom over
// the construction cfg is non-empty, and (2) a SessionStarted published through the
// session's own hub with that Config is delivered to a subscriber carrying the
// fingerprint intact.
func TestSessionStartedCarriesConfig(t *testing.T) {
	t.Parallel()

	cfg := fpConfig("model-x", "you are helpful", "Read", "Write")
	cfg.DrainTimeout = 100 * time.Millisecond
	want := FingerprintFrom(cfg)
	if want.Equal(event.ConfigFingerprint{}) {
		t.Fatalf("FingerprintFrom(construction cfg) is empty; SessionStarted would carry no config")
	}

	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// Republish a SessionStarted carrying the construction fingerprint through the
	// session's hub; the subscriber must receive it with the Config intact (proving
	// SessionStarted carries Config end-to-end through publish -> hub -> subscriber).
	started := event.SessionStarted{Config: want}
	if err := s.PublishEvent(context.Background(), started); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}

	got, ok := firstMatching[event.SessionStarted](t, sub)
	if !ok {
		t.Fatal("no SessionStarted observed on the fan-in")
	}
	if !got.Config.Equal(want) {
		t.Errorf("delivered SessionStarted.Config = %+v, want %+v", got.Config, want)
	}
	if got.Config.Equal(event.ConfigFingerprint{}) {
		t.Error("delivered SessionStarted.Config is empty, want the derived fingerprint")
	}
}
