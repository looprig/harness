package rig

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

type stubLLM struct{}

func (*stubLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}
func (*stubLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, nil
}

func validModel(name string) model.Model {
	return model.Model{Provider: "test", APIFormat: model.APIFormatOpenAI, BaseURL: "http://localhost", Name: name}
}

func mustDefine(options ...loop.Option) loop.Definition {
	definition, err := loop.Define(options...)
	if err != nil {
		panic(err)
	}
	return definition
}

// fpTool is a minimal InvokableTool whose Info reports a fixed name, used to drive
// the ToolPolicyRev derivation deterministically.
type fpTool struct{ name string }

func (t fpTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: t.name}, nil
}
func (t fpTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	return tool.TextResult(""), nil
}

// fpConfig builds a loop.Definition with the given model, system prompt, and tool
// names so a test can vary exactly one fingerprint input at a time.
func fpConfig(model, system string, toolNames ...string) loop.Definition {
	defs := make([]tool.Definition, 0, len(toolNames))
	for _, n := range toolNames {
		name := n
		defs = append(defs, tool.NewDefinition(name, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{fpTool{name: name}}, nil
		}))
	}
	return mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel(model)), loop.WithSystem(system), loop.WithTools(defs...), loop.WithDrainTimeout(100*time.Millisecond))
}

func bindFingerprintDefinition(d loop.Definition) loop.BoundDefinition {
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	bound, err := d.Bind(context.Background(), tool.Bindings{SessionID: sessionID, LoopID: loopID})
	if err != nil {
		panic(err)
	}
	return bound
}

func fingerprintFromDefinition(d loop.Definition) event.ConfigFingerprint {
	return FingerprintFrom(bindFingerprintDefinition(d))
}

func fingerprintWithDefinition(d loop.Definition, fields ConfigFingerprintFields) event.ConfigFingerprint {
	return fingerprintWith(bindFingerprintDefinition(d), fields)
}

// TestFingerprintFromRestoreStable is the RESTORE-STABILITY guard for the
// ModelSpec→(Model + System) split: it pins a KNOWN config to its EXACT fingerprint
// so the refactor cannot silently perturb the value a persisted session was stamped
// with (a change would make every existing session fail its restore comparison). The
// three golden strings are computed INDEPENDENTLY of this package's code, exactly as
// the pre-refactor derivation did:
//   - ModelID is the selected bound model's Name verbatim, so it stays
//     "gpt-4o-2024".
//   - SystemPromptRev is sha256hex of the bound definition's effective system prompt:
//     sha256("You are a helpful assistant.").
//   - ToolPolicyRev is sha256hex of the sorted tool names joined by "\n":
//     sha256("Read\nWrite") (unchanged by this refactor).
//
// If any hashed input, field home, ordering, or hash algorithm changes, one of these
// literals stops matching and this test fails — catching an accidental fingerprint
// change before it breaks session restore for existing users.
func TestFingerprintFromRestoreStable(t *testing.T) {
	t.Parallel()
	const (
		wantModelID = "gpt-4o-2024"
		// sha256("You are a helpful assistant.")
		wantSystemPromptRev = "75357d685f238b6afd7738be9786fdafde641eb6ca9a3be7471939715a68a4de"
		// sha256("Read\nWrite") — tool names sorted then newline-joined
		wantToolPolicyRev = "fb0af83c64ef5c27e469abea2e7b687f23f281f6619218d3ea42a35a2222af25"
	)
	fp := fingerprintFromDefinition(fpConfig("gpt-4o-2024", "You are a helpful assistant.", "Read", "Write"))
	if fp.ModelID != wantModelID {
		t.Errorf("ModelID = %q, want %q", fp.ModelID, wantModelID)
	}
	if fp.SystemPromptRev != wantSystemPromptRev {
		t.Errorf("SystemPromptRev = %q, want %q (sha256 of the system prompt)", fp.SystemPromptRev, wantSystemPromptRev)
	}
	if fp.ToolPolicyRev != wantToolPolicyRev {
		t.Errorf("ToolPolicyRev = %q, want %q (sha256 of the sorted tool names)", fp.ToolPolicyRev, wantToolPolicyRev)
	}
}

// TestFingerprintFromDeterministic asserts FingerprintFrom is stable for identical
// config (same inputs -> Equal fingerprints, including tool order independence) and
// that it sets ModelID from the spec verbatim with non-empty digest fields once a
// prompt and tools are present.
func TestFingerprintFromDeterministic(t *testing.T) {
	t.Parallel()

	a := fingerprintFromDefinition(fpConfig("model-x", "you are helpful", "Read", "Write"))
	b := fingerprintFromDefinition(fpConfig("model-x", "you are helpful", "Read", "Write"))
	if !a.Equal(b) {
		t.Fatalf("FingerprintFrom not deterministic: %+v != %+v", a, b)
	}

	// Tool ordering must not change the fingerprint (names are sorted before hashing).
	reordered := fingerprintFromDefinition(fpConfig("model-x", "you are helpful", "Write", "Read"))
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

	base := fingerprintFromDefinition(fpConfig("model-x", "prompt A", "Read"))

	tests := []struct {
		name string
		cfg  loop.Definition
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
			got := fingerprintFromDefinition(tt.cfg)
			if base.Equal(got) {
				t.Errorf("fingerprint did not change for %s: both %+v", tt.name, got)
			}
		})
	}
}

func TestTopologyFingerprintIsDeterministicAndPrimerOrderSensitive(t *testing.T) {
	t.Parallel()
	planner := mustDefine(loop.WithName("planner"), loop.WithInference(&stubLLM{}, validModel("model")))
	builder := mustDefine(loop.WithName("builder"), loop.WithInference(&stubLLM{}, validModel("model")))
	bound := bindFingerprintDefinition(planner)
	a := fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{planner, builder}, []string{"planner", "builder"}, "planner")
	b := fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{planner, builder}, []string{"planner", "builder"}, "planner")
	if !a.Equal(b) || a.TopologyRev == "" {
		t.Fatalf("topology fingerprint is not deterministic: %+v %+v", a, b)
	}
	registrationReordered := fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{builder, planner}, []string{"planner", "builder"}, "planner")
	if !a.Equal(registrationReordered) {
		t.Fatal("non-semantic loop registration order changed topology fingerprint")
	}
	reordered := fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{planner, builder}, []string{"builder", "planner"}, "planner")
	if a.Equal(reordered) {
		t.Fatal("ordered primer change did not alter topology fingerprint")
	}
	activeChanged := fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{planner, builder}, []string{"planner", "builder"}, "builder")
	if a.Equal(activeChanged) {
		t.Fatal("active primer change did not alter topology fingerprint")
	}
	changedBuilder := mustDefine(loop.WithName("builder"), loop.WithInference(&stubLLM{}, validModel("other-model")))
	nonActivePolicyChanged := fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{planner, changedBuilder}, []string{"planner", "builder"}, "planner")
	if a.Equal(nonActivePolicyChanged) {
		t.Fatal("non-active loop policy change did not alter topology fingerprint")
	}
	revisedA := mustDefine(loop.WithName("builder"), loop.WithInference(&stubLLM{}, validModel("model")), loop.WithPolicyRevision("policy-a"))
	revisedB := mustDefine(loop.WithName("builder"), loop.WithInference(&stubLLM{}, validModel("model")), loop.WithPolicyRevision("policy-b"))
	policyA := fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{planner, revisedA}, []string{"planner", "builder"}, "planner")
	policyB := fingerprintWithTopology(bound, ConfigFingerprintFields{}, []loop.Definition{planner, revisedB}, []string{"planner", "builder"}, "planner")
	if policyA.Equal(policyB) {
		t.Fatal("explicit loop policy revision did not alter topology fingerprint")
	}
}

func TestFingerprintSystemRevisionIncludesInitialModeInstructions(t *testing.T) {
	t.Parallel()
	definition := func(instructions string) loop.Definition {
		return mustDefine(
			loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model-x")), loop.WithSystem("base"),
			loop.WithModes(loop.Mode{Name: "build", Instructions: instructions}), loop.WithInitialMode("build"),
		)
	}
	a := fingerprintFromDefinition(definition("instruction A"))
	b := fingerprintFromDefinition(definition("instruction B"))
	if a.SystemPromptRev == b.SystemPromptRev {
		t.Fatalf("SystemPromptRev did not change with selected mode instructions: %q", a.SystemPromptRev)
	}
}

// TestFingerprintFromSwarmFieldsEmpty pins that FingerprintFrom derives ONLY the
// loop.Definition fields: the swarm-level fields (AgentKind, RuntimeSkills, WorkspaceRoot)
// are NOT on loop.Definition, so a bare FingerprintFrom leaves them empty/zero — they are
// injected by the composition root via WithFingerprintFields and merged with
// fingerprintWith.
func TestFingerprintFromSwarmFieldsEmpty(t *testing.T) {
	t.Parallel()
	fp := fingerprintFromDefinition(fpConfig("model-x", "prompt", "Read"))
	if fp.AgentKind != "" {
		t.Errorf("AgentKind = %q, want \"\" (not a loop.Definition field)", fp.AgentKind)
	}
	if fp.RuntimeSkills {
		t.Error("RuntimeSkills = true, want false (not a loop.Definition field)")
	}
	if fp.WorkspaceRoot != "" {
		t.Errorf("WorkspaceRoot = %q, want \"\" (not a loop.Definition field)", fp.WorkspaceRoot)
	}
}

// TestFingerprintWithMergesSwarmFields asserts fingerprintWith applies the injected
// swarm-level fields onto the loop-derived fingerprint, and that a difference in ANY
// one of them (AgentKind, RuntimeSkills, WorkspaceRoot) alone — same loop.Definition —
// yields an unequal fingerprint. This is what makes a restore reject a session resuming
// under a different agent identity, skill-trust mode, or workspace.
func TestFingerprintWithMergesSwarmFields(t *testing.T) {
	t.Parallel()

	cfg := fpConfig("model-x", "prompt", "Read")
	base := ConfigFingerprintFields{
		AgentKind:     "coderig:operator",
		RuntimeSkills: true,
		WorkspaceRoot: "/home/user/repo",
	}
	baseFP := fingerprintWithDefinition(cfg, base)

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
	diffKind.AgentKind = "coderig:reviewer"
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
			got := fingerprintWithDefinition(cfg, tt.fields)
			if baseFP.Equal(got) {
				t.Errorf("fingerprint did not change for %s: both %+v", tt.name, got)
			}
		})
	}
}

// TestFingerprintWithForeignFields asserts a foreign loop's fingerprint dimensions —
// cwd (folded into WorkspaceRoot), adapter identity (AdapterID), and permission posture
// (Posture) — are all inputs to fingerprintWith: same fields yield an Equal fingerprint,
// and a change in ANY one alone (same loop.Definition) yields an unequal one. This is what
// makes a restore reject a foreign session resuming under a different working directory,
// adapter, or permission posture.
//
// The foreign exec path and child env are DELIBERATELY absent from the fingerprint
// (permitted to drift, log-only): fingerprintWith takes only (cfg, fields) and neither
// the exec path nor the child env is a field, so two calls that differ only in those
// (i.e. differ in nothing the fingerprint sees) compare Equal — asserted below.
func TestFingerprintWithForeignFields(t *testing.T) {
	t.Parallel()

	cfg := fpConfig("model-x", "prompt", "Read")
	base := ConfigFingerprintFields{
		WorkspaceRoot:             "/work/foreign",
		AdapterID:                 "claude",
		Posture:                   "default",
		NativePermissionPolicyRev: "policyrev-aaa",
	}
	baseFP := fingerprintWithDefinition(cfg, base)

	// The merged fingerprint carries the injected foreign fields verbatim.
	if baseFP.WorkspaceRoot != base.WorkspaceRoot {
		t.Errorf("WorkspaceRoot = %q, want %q", baseFP.WorkspaceRoot, base.WorkspaceRoot)
	}
	if baseFP.AgentAdapter != base.AdapterID {
		t.Errorf("AgentAdapter = %q, want %q", baseFP.AgentAdapter, base.AdapterID)
	}
	if baseFP.PermissionPosture != base.Posture {
		t.Errorf("PermissionPosture = %q, want %q", baseFP.PermissionPosture, base.Posture)
	}
	if baseFP.NativePermissionPolicyRev != base.NativePermissionPolicyRev {
		t.Errorf("NativePermissionPolicyRev = %q, want %q", baseFP.NativePermissionPolicyRev, base.NativePermissionPolicyRev)
	}

	diffCwd := base
	diffCwd.WorkspaceRoot = "/work/other"
	diffAdapter := base
	diffAdapter.AdapterID = "codex"
	diffPosture := base
	diffPosture.Posture = "acceptEdits"
	diffPolicyRev := base
	diffPolicyRev.NativePermissionPolicyRev = "policyrev-bbb"

	tests := []struct {
		name      string
		fields    ConfigFingerprintFields
		wantEqual bool
	}{
		{"identical fields stay equal", base, true},
		{"cwd (WorkspaceRoot) differs", diffCwd, false},
		{"AdapterID differs", diffAdapter, false},
		{"Posture differs", diffPosture, false},
		{"NativePermissionPolicyRev differs", diffPolicyRev, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := fingerprintWithDefinition(cfg, tt.fields)
			if baseFP.Equal(got) != tt.wantEqual {
				t.Errorf("Equal = %v, want %v for %s: base=%+v got=%+v",
					baseFP.Equal(got), tt.wantEqual, tt.name, baseFP, got)
			}
		})
	}
}

// TestFingerprintWithExecAndEnvNotInputs pins that the foreign loop's exec path and
// child env are NOT fingerprinted (they are permitted to drift across a restore and are
// log-only). fingerprintWith's only inputs are the loop.Definition and ConfigFingerprintFields;
// there is no exec-path or env field, so two fingerprints over the same cfg + same fields
// are Equal regardless of any exec-path/env change that happened out of band.
func TestFingerprintWithExecAndEnvNotInputs(t *testing.T) {
	t.Parallel()

	cfg := fpConfig("model-x", "prompt", "Read")
	fields := ConfigFingerprintFields{
		WorkspaceRoot: "/work/foreign",
		AdapterID:     "claude",
		Posture:       "default",
	}
	// Two calls identical in every fingerprint input. Were exec path or child env an
	// input, this would be where they would diverge; they are intentionally absent, so
	// the fingerprints must be Equal.
	if !fingerprintWithDefinition(cfg, fields).Equal(fingerprintWithDefinition(cfg, fields)) {
		t.Error("fingerprintWith is non-deterministic for identical inputs; exec path / env must not be fingerprinted")
	}
}

// TestFingerprintWithEmptyFieldsMatchesBare asserts the additive-compatibility path: a
// fingerprint computed with the zero ConfigFingerprintFields (a non-swarm caller) is
// Equal to the bare FingerprintFrom over the same config — so an old session persisted
// before the swarm fields existed restores equal to one re-derived today without them.
func TestFingerprintWithEmptyFieldsMatchesBare(t *testing.T) {
	t.Parallel()
	cfg := fpConfig("model-x", "prompt", "Read")
	if !fingerprintWithDefinition(cfg, ConfigFingerprintFields{}).Equal(fingerprintFromDefinition(cfg)) {
		t.Error("fingerprintWith with empty fields != bare FingerprintFrom; the compatibility path is broken")
	}
}

// TestFingerprintFromEmptyTools asserts the boundary: a config with no tools and an
// empty prompt still yields a stable fingerprint (the digests of empty inputs are
// well-defined and equal across calls).
func TestFingerprintFromEmptyTools(t *testing.T) {
	t.Parallel()
	a := fingerprintFromDefinition(fpConfig("m", ""))
	b := fingerprintFromDefinition(fpConfig("m", ""))
	if !a.Equal(b) {
		t.Errorf("empty-config fingerprint not deterministic: %+v != %+v", a, b)
	}
}

func TestFrozenFingerprintRetainsFullInitialFieldsWithoutBinding(t *testing.T) {
	var binds atomic.Int32
	definition, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("base")), loop.WithSystem("base system"),
		loop.WithTools(tool.NewDefinition("Read", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{fpTool{name: "Read"}}, nil
		})),
		loop.WithModes(loop.Mode{Name: "build", Model: validModel("selected"), Instructions: "build instructions"}),
		loop.WithInitialMode("build"),
		loop.WithPolicyRevision("policy-v1"),
		loop.WithPermissionFactory(func(context.Context, tool.Bindings) (loop.PermissionGate, error) {
			binds.Add(1)
			return lifecyclePermissionGate{}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := frozenFingerprint(ConfigFingerprintFields{}, []loop.Definition{definition}, []string{"agent"}, "agent")
	if binds.Load() != 0 {
		t.Fatalf("definition-time fingerprint invoked bind factory %d times", binds.Load())
	}
	if fingerprint.ModelID != "selected" {
		t.Fatalf("ModelID = %q, want selected", fingerprint.ModelID)
	}
	if want := hexSHA256("base system\n\nbuild instructions"); fingerprint.SystemPromptRev != want {
		t.Fatalf("SystemPromptRev = %q, want %q", fingerprint.SystemPromptRev, want)
	}
	if want := hexSHA256("Read"); fingerprint.ToolPolicyRev != want {
		t.Fatalf("ToolPolicyRev = %q, want %q", fingerprint.ToolPolicyRev, want)
	}
}

func TestFrozenFingerprintUsesProducedToolNamesWithoutBinding(t *testing.T) {
	t.Parallel()

	builds := 0
	files := tool.NewBundleDefinition("Files", []string{"ReadFile", "WriteFile", "EditFile"}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		builds++
		return []tool.InvokableTool{fpTool{name: "unexpected"}}, nil
	})
	definition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")), loop.WithTools(files))

	fingerprint := frozenFingerprint(ConfigFingerprintFields{}, []loop.Definition{definition}, []string{"agent"}, "agent")
	if builds != 0 {
		t.Fatalf("tool factory builds = %d, want 0", builds)
	}
	if want := hexSHA256("EditFile\nReadFile\nWriteFile"); fingerprint.ToolPolicyRev != want {
		t.Fatalf("ToolPolicyRev = %q, want %q", fingerprint.ToolPolicyRev, want)
	}
}

func TestFrozenFingerprintNormalizesProducedToolNames(t *testing.T) {
	t.Parallel()

	definition := tool.NewBundleDefinition("bundle", []string{" Write ", "Read"}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{fpTool{name: "Read"}, fpTool{name: "Write"}}, nil
	})
	agent := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")), loop.WithTools(definition))
	fingerprint := frozenFingerprint(ConfigFingerprintFields{}, []loop.Definition{agent}, []string{"agent"}, "agent")
	if want := hexSHA256("Read\nWrite"); fingerprint.ToolPolicyRev != want {
		t.Fatalf("ToolPolicyRev = %q, want normalized digest %q", fingerprint.ToolPolicyRev, want)
	}
}

func TestFrozenFingerprintIncludesStructurallyInjectedSubagent(t *testing.T) {
	t.Parallel()

	primer := mustDefine(loop.WithName("primer"), loop.WithInference(&stubLLM{}, validModel("model")), loop.WithDelegates("delegate"))
	delegate := mustDefine(loop.WithName("delegate"), loop.WithInference(&stubLLM{}, validModel("delegate-model")))
	fingerprint := frozenFingerprint(ConfigFingerprintFields{}, []loop.Definition{primer, delegate}, []string{"primer"}, "primer")

	if want := hexSHA256("Subagent"); fingerprint.ToolPolicyRev != want {
		t.Fatalf("ToolPolicyRev = %q, want injected Subagent digest %q", fingerprint.ToolPolicyRev, want)
	}
}

func TestTopologyRevisionIncludesDelegateProducedToolMetadata(t *testing.T) {
	t.Parallel()

	bundle := func(produced string) tool.Definition {
		return tool.NewBundleDefinition("delegate-tool", []string{produced}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{fpTool{name: produced}}, nil
		})
	}
	primer := mustDefine(loop.WithName("primer"), loop.WithInference(&stubLLM{}, validModel("primer")), loop.WithDelegates("worker"))
	workerA := mustDefine(loop.WithName("worker"), loop.WithInference(&stubLLM{}, validModel("worker")), loop.WithTools(bundle("Read")))
	workerB := mustDefine(loop.WithName("worker"), loop.WithInference(&stubLLM{}, validModel("worker")), loop.WithTools(bundle("Inspect")))

	a := frozenFingerprint(ConfigFingerprintFields{}, []loop.Definition{primer, workerA}, []string{"primer"}, "primer")
	b := frozenFingerprint(ConfigFingerprintFields{}, []loop.Definition{primer, workerB}, []string{"primer"}, "primer")
	if a.TopologyRev == b.TopologyRev {
		t.Fatal("TopologyRev ignored delegate definition produced-name drift")
	}
}

func TestTopologyRevisionIncludesNoninitialModeProducedToolMetadata(t *testing.T) {
	t.Parallel()

	bundle := func(produced string) tool.Definition {
		return tool.NewBundleDefinition("mode-tool", []string{produced}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{fpTool{name: produced}}, nil
		})
	}
	define := func(reviewTool string) loop.Definition {
		return mustDefine(
			loop.WithName("primer"),
			loop.WithInference(&stubLLM{}, validModel("model")),
			loop.WithModes(
				loop.Mode{Name: "build"},
				loop.Mode{Name: "review", Tools: []tool.Definition{bundle(reviewTool)}},
			),
			loop.WithInitialMode("build"),
		)
	}
	a := frozenFingerprint(ConfigFingerprintFields{}, []loop.Definition{define("Read")}, []string{"primer"}, "primer")
	b := frozenFingerprint(ConfigFingerprintFields{}, []loop.Definition{define("Inspect")}, []string{"primer"}, "primer")
	if a.TopologyRev == b.TopologyRev {
		t.Fatal("TopologyRev ignored noninitial-mode produced-name drift")
	}
}

// TestSessionStartedCarriesConfig is the end-to-end proof that the construction-time
// SessionStarted the session publishes carries a non-empty Config derived from the
// loop.Definition. The construction-time event is unobservable by a late subscriber (the
// hub has no replay), so this asserts the wiring two ways: (1) FingerprintFrom over
// the construction cfg is non-empty, and (2) a SessionStarted published through the
// session's own hub with that Config is delivered to a subscriber carrying the
// fingerprint intact.

// TestExternalCapabilityRevReachesBothFingerprintPaths pins the property that
// makes the field usable at all: the two derivations must agree.
//
// There are two, and they exist for good reasons that make them easy to drift
// apart. fingerprintWith runs at session creation, from a BOUND definition, and
// stamps what SessionStarted records. frozenFingerprint runs at RESTORE, from
// immutable definitions alone, so a restore can compare before it constructs
// workspace or loop collaborators. A field added to one and forgotten in the
// other is the worst possible outcome for this type: every restore would compare
// a fingerprint that was never stamped, and reject a session nobody changed —
// or, in the other direction, silently accept drift.
func TestExternalCapabilityRevReachesBothFingerprintPaths(t *testing.T) {
	t.Parallel()

	const rev = "aa08cfe9f431598f187f5bec202f211f3bc50325ec3e0415b63aabdcdbf9b5fd"
	fields := ConfigFingerprintFields{ExternalCapabilityRev: rev}
	def := fpConfig("gpt-4o-2024", "You are a helpful assistant.", "Read")

	bound := fingerprintWithDefinition(def, fields)
	if bound.ExternalCapabilityRev != rev {
		t.Errorf("fingerprintWith ExternalCapabilityRev = %q, want %q: the field a session is STAMPED with is dropped", bound.ExternalCapabilityRev, rev)
	}

	frozen := frozenFingerprint(fields, []loop.Definition{def}, []string{"agent"}, "agent")
	if frozen.ExternalCapabilityRev != rev {
		t.Errorf("frozenFingerprint ExternalCapabilityRev = %q, want %q: the field a RESTORE compares is dropped, so every restore of a session with MCP would spuriously mismatch", frozen.ExternalCapabilityRev, rev)
	}

	// Empty stays empty on both paths: a rig that attaches no external capability
	// must produce a fingerprint that compares Equal to a journal predating the
	// field.
	none := ConfigFingerprintFields{}
	if got := fingerprintWithDefinition(def, none).ExternalCapabilityRev; got != "" {
		t.Errorf("fingerprintWith with no external capability = %q, want empty", got)
	}
	if got := frozenFingerprint(none, []loop.Definition{def}, []string{"agent"}, "agent").ExternalCapabilityRev; got != "" {
		t.Errorf("frozenFingerprint with no external capability = %q, want empty", got)
	}
}
