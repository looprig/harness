package hustleruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

// recordingEvidenceContainment records the exact gate.EvidenceContainmentPolicy
// observed on every VerifyEvidenceContainment call, in order, so a test can
// assert precisely which ceiling was in effect for which call — never just
// the most recent one.
type recordingEvidenceContainment struct {
	mu       sync.Mutex
	policies []gate.EvidenceContainmentPolicy
}

func (c *recordingEvidenceContainment) VerifyEvidenceContainment(_ context.Context, policy gate.EvidenceContainmentPolicy, _ tool.Request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policies = append(c.policies, policy)
	return nil
}

func (c *recordingEvidenceContainment) ceilings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.policies))
	for i, policy := range c.policies {
		out[i] = policy.SecurityCeiling
	}
	return out
}

// TestEvidenceRunnerBindsEachRunToItsOwnRequestSecurityCeiling is the single
// most important test in the evidence-security-ceiling addendum. It proves
// that two RunAndFinalize calls sharing the SAME Controller (simulating two
// permission reviews at different points in a long session whose access
// posture changed in between) are each evidence-bound against THEIR OWN
// hustle.Request.SecurityCeiling — never a value frozen at controller
// construction, and never one call's ceiling leaking into the other's.
// Before this addendum, EvidenceRuntimeConfig carried a single
// controller-wide SecurityCeiling baked into the evidenceRunner once at
// construction; this test would have observed the SAME ceiling for both
// calls regardless of what each request asked for.
func TestEvidenceRunnerBindsEachRunToItsOwnRequestSecurityCeiling(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	evidenceTool := newPreparedEvidenceTool("workspace_read", "ok")

	invocation := 0
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		invocation++
		// Odd invocations (1st per RunAndFinalize call) issue one evidence-tool
		// call, which is where VerifyEvidenceContainment fires; even
		// invocations (2nd per call) issue the terminal structured result.
		if invocation%2 == 1 {
			return oneEvidenceCallResponse("call"), nil
		}
		return terminalEvidenceResponse(`{"summary":"allow"}`, nil), nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(),
		func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{evidenceTool}, nil
		},
		hustle.ToolLoopLimits{
			MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
			MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
		},
	)

	containment := &recordingEvidenceContainment{}
	factory := event.NewFactory(uuid.New, func() time.Time { return time.Unix(123, 0).UTC() })
	controller, err := New(context.Background(), Config{
		Blocking: LaneLimits{Concurrent: 1, Queued: 2}, Background: LaneLimits{Concurrent: 1, Queued: 2},
		Runtime: &RuntimeConfig{
			SessionID: sessionID, Definitions: []hustle.BoundDefinition{definition},
			AuditTimeout: time.Second, FinalizationTimeout: time.Second, WorkerDrainTimeout: time.Second,
			Stamper: factory, Audit: &runtimeTestAudit{}, Faults: &runtimeTestFaults{}, Activity: &runtimeTestActivity{},
			Evidence: &EvidenceRuntimeConfig{
				Access:         &evidenceAccessStub{access: gate.AccessAllow},
				Containment:    containment,
				AllowedKinds:   []string{evidenceReadKind},
				ReadWorkspace:  &tool.ReadWorkspaceBinding{Root: "/workspace"},
				NewExecutionID: uuid.New,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	firstRequest := runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID)
	firstRequest.SecurityCeiling = "read-only"
	if err := controller.RunAndFinalize(context.Background(), firstRequest, acceptResult, noOpFinalizer); err != nil {
		t.Fatalf("first run (ceiling=read-only): %v", err)
	}

	secondRequest := runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID)
	secondRequest.SecurityCeiling = "workspace-write"
	if err := controller.RunAndFinalize(context.Background(), secondRequest, acceptResult, noOpFinalizer); err != nil {
		t.Fatalf("second run (ceiling=workspace-write): %v", err)
	}

	got := containment.ceilings()
	want := []string{"read-only", "workspace-write"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("observed containment ceilings = %v, want %v (each run bound to its own request ceiling, not a shared stale one)", got, want)
	}
}

// TestEvidenceExecutionRejectsMissingPerRequestSecurityCeiling proves the
// fail-closed backstop for the per-request ceiling design: a Hustle whose
// definition enables evidence tools but whose Request carries no
// SecurityCeiling never reaches evidence binding/authorization at all — it
// fails immediately with a bounded EvidenceFailureInvalidBinding, rather than
// silently falling back to some other (stale or empty) ceiling.
func TestEvidenceExecutionRejectsMissingPerRequestSecurityCeiling(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		t.Fatal("inference reached with no security ceiling bound")
		return nil, nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(),
		func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
		},
		hustle.ToolLoopLimits{
			MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
			MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
		},
	)
	controller := runtimeEvidenceController(t, sessionID, definition)

	request := runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID)
	request.SecurityCeiling = ""
	err := controller.RunAndFinalize(context.Background(), request, acceptResult, noOpFinalizer)
	var runErr *RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("error = %T %v, want *RunError", err, err)
	}
	var evidenceErr *EvidenceError
	if !errors.As(runErr.Cause, &evidenceErr) || evidenceErr.Reason != EvidenceFailureInvalidBinding {
		t.Fatalf("cause = %#v, want EvidenceFailureInvalidBinding", runErr.Cause)
	}
}

// kindDeclaringEvidenceDefinition wraps a real tool.Definition and adds the
// optional tool.EvidenceKindDeclarer capability with an explicit, fixed set
// of declared kinds — the test seam for proving construction-time fail-fast
// behavior without needing a real evidence tool implementation that varies
// its Requirement.Kind by argument.
type kindDeclaringEvidenceDefinition struct {
	tool.Definition
	kinds []string
}

func (d kindDeclaringEvidenceDefinition) EvidenceRequirementKinds() []string {
	return append([]string(nil), d.kinds...)
}

// TestNewRuntimeControllerFailsFastOnUndeclaredEvidenceKind proves the
// construction-time (not first-call) fail-fast check: a classifier
// registering an evidence tool that declares a Requirement.Kind absent from
// the consumer's AllowedKinds allowlist must be rejected at
// newRuntimeController with a typed error naming the missing kind, never
// discovered lazily as an opaque forbidden-capability failure the first time
// a review actually calls the tool.
func TestNewRuntimeControllerFailsFastOnUndeclaredEvidenceKind(t *testing.T) {
	t.Parallel()
	const missingKind = "git.read"
	info := evidenceToolInfo("workspace_read")
	base := tool.NewEvidenceDefinition("workspace-read", tool.RequiresWorkspaceRead, []tool.ToolInfo{info},
		func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
		},
	)
	declaring := kindDeclaringEvidenceDefinition{Definition: base, kinds: []string{evidenceReadKind, missingKind}}

	definition, err := hustle.Define(
		hustle.WithName("test.evidence-missing-kind"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 1024}),
		hustle.WithSystemPrompt("Review only.", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
		hustle.WithNamedInference(successfulRuntimeClient(nil), runtimeStructuredTestModel()),
		hustle.WithOutputSchema(runtimeTestOutputSchema()),
		hustle.WithEvidenceTools(hustle.EvidenceToolPolicy{
			Revision: "evidence-v1",
			Limits: hustle.ToolLoopLimits{
				MaxRounds: 2, MaxCalls: 4, MaxCallsPerRound: 2,
				MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
			},
			Definitions: []tool.Definition{declaring},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), hustle.Bindings{})
	if err != nil {
		t.Fatal(err)
	}

	config := validRuntimeConfig(t, bound)
	config.Evidence = &EvidenceRuntimeConfig{
		Access:      &evidenceAccessStub{access: gate.AccessAllow},
		Containment: &evidenceContainmentStub{},
		// Deliberately missing missingKind: only evidenceReadKind is allowed.
		AllowedKinds:   []string{evidenceReadKind},
		ReadWorkspace:  &tool.ReadWorkspaceBinding{Root: "/workspace"},
		NewExecutionID: uuid.New,
	}

	runtime, err := newRuntimeController(context.Background(), config)
	if runtime != nil {
		runtime.cancelExecutions()
	}
	var kindErr *ConfigEvidenceKindError
	if !errors.As(err, &kindErr) || kindErr.Kind != missingKind || kindErr.Name != bound.Name() {
		t.Fatalf("newRuntimeController() = (%#v,%T %v), want *ConfigEvidenceKindError naming %q", runtime, err, err, missingKind)
	}
}

// TestNewRuntimeControllerAcceptsFullyDeclaredEvidenceKinds is the GREEN
// counterpart: when every declared kind is present in AllowedKinds,
// construction succeeds.
func TestNewRuntimeControllerAcceptsFullyDeclaredEvidenceKinds(t *testing.T) {
	t.Parallel()
	info := evidenceToolInfo("workspace_read")
	base := tool.NewEvidenceDefinition("workspace-read", tool.RequiresWorkspaceRead, []tool.ToolInfo{info},
		func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
		},
	)
	declaring := kindDeclaringEvidenceDefinition{Definition: base, kinds: []string{evidenceReadKind}}

	definition, err := hustle.Define(
		hustle.WithName("test.evidence-declared-kind"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 1024}),
		hustle.WithSystemPrompt("Review only.", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
		hustle.WithNamedInference(successfulRuntimeClient(nil), runtimeStructuredTestModel()),
		hustle.WithOutputSchema(runtimeTestOutputSchema()),
		hustle.WithEvidenceTools(hustle.EvidenceToolPolicy{
			Revision: "evidence-v1",
			Limits: hustle.ToolLoopLimits{
				MaxRounds: 2, MaxCalls: 4, MaxCallsPerRound: 2,
				MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
			},
			Definitions: []tool.Definition{declaring},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), hustle.Bindings{})
	if err != nil {
		t.Fatal(err)
	}

	config := validRuntimeConfig(t, bound)
	config.Evidence = &EvidenceRuntimeConfig{
		Access:         &evidenceAccessStub{access: gate.AccessAllow},
		Containment:    &evidenceContainmentStub{},
		AllowedKinds:   []string{evidenceReadKind},
		ReadWorkspace:  &tool.ReadWorkspaceBinding{Root: "/workspace"},
		NewExecutionID: uuid.New,
	}

	runtime, err := newRuntimeController(context.Background(), config)
	if err != nil {
		t.Fatalf("newRuntimeController() error = %v, want success", err)
	}
	runtime.cancelExecutions()
}
