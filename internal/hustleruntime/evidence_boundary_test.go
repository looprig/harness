package hustleruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

type evidenceContainmentStub struct {
	mu      sync.Mutex
	calls   int
	policy  EvidenceContainmentPolicy
	request tool.Request
	err     error
	panic   bool
	verify  func(tool.Request)
}

func (s *evidenceContainmentStub) VerifyEvidenceContainment(
	_ context.Context,
	policy EvidenceContainmentPolicy,
	request tool.Request,
) error {
	s.mu.Lock()
	s.calls++
	s.policy = policy
	s.request = request.Clone()
	s.mu.Unlock()
	if s.panic {
		panic("containment-secret")
	}
	if s.verify != nil {
		s.verify(request)
	}
	return s.err
}

func (s *evidenceContainmentStub) snapshot() (int, EvidenceContainmentPolicy, tool.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.policy, s.request.Clone()
}

type typedNilEvidenceContainment struct{}

func (*typedNilEvidenceContainment) VerifyEvidenceContainment(context.Context, EvidenceContainmentPolicy, tool.Request) error {
	panic("must not verify")
}

// filesystemEvidenceContainment is a deliberately small realistic test
// implementation of the consumer seam. It demonstrates the required contract:
// resolve targets and symlinks, reject ambiguous tool-owned scopes, then enforce
// the trusted ceiling. Production consumers may use richer requirement kinds.
type filesystemEvidenceContainment struct{}

func (*filesystemEvidenceContainment) VerifyEvidenceContainment(
	_ context.Context,
	policy EvidenceContainmentPolicy,
	request tool.Request,
) error {
	if policy.SecurityCeiling != "read-only" {
		return errors.New("security ceiling rejected")
	}
	for _, requirement := range request.Requirements {
		if requirement.Scope == "ambiguous" {
			return errors.New("ambiguous scope")
		}
		target, err := filepath.EvalSymlinks(requirement.Match)
		if err != nil {
			return errors.New("target unresolved")
		}
		relative, err := filepath.Rel(policy.ReadRoot, target)
		if err != nil || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("target outside root")
		}
	}
	return nil
}

type observingEvidenceAccess struct {
	requirement tool.Requirement
	calls       int
}

type allowingConcurrentEvidenceAccess struct{}

func (allowingConcurrentEvidenceAccess) AccessFor(tool.Requirement) (uint8, error) {
	return gate.AccessAllow, nil
}

func (a *observingEvidenceAccess) AccessFor(requirement tool.Requirement) (uint8, error) {
	a.calls++
	a.requirement = requirement.Clone()
	return gate.AccessAllow, nil
}

func TestEvidenceRunnerContainsEveryOwnedRequestBeforeAccessAndExecution(t *testing.T) {
	t.Parallel()

	const originalMatch = "/workspace/original"
	candidate := newPreparedEvidenceTool("workspace_read", "ok")
	var retained []tool.Requirement
	candidate.request = func(id uuid.UUID) tool.Request {
		retained = []tool.Requirement{{
			Kind: evidenceReadKind, Match: originalMatch, Description: "read evidence",
		}}
		return tool.Request{
			ToolName: "workspace_read", ExecutionID: id.String(), Requirements: retained,
		}
	}
	access := &observingEvidenceAccess{}
	verifier := &evidenceContainmentStub{verify: func(request tool.Request) {
		// A hostile verifier may mutate only its private view. A hostile preparer
		// retaining its returned slice may mutate only the discarded original.
		request.Requirements[0].Match = "/outside/verifier"
		retained[0].Match = "/outside/preparer"
	}}
	runner, err := newEvidenceRunner(
		access,
		verifier,
		EvidenceContainmentPolicy{ReadRoot: "/workspace", SecurityCeiling: "read-only"},
		[]string{evidenceReadKind},
		func() (uuid.UUID, error) { return mustRuntimeTestID(t), nil },
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = runner.run(
		context.Background(),
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
		[]evidenceToolCall{{id: "call", name: "workspace_read", input: []byte(`{}`)}},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
	)
	if err != nil {
		t.Fatal(err)
	}
	calls, policy, verified := verifier.snapshot()
	if calls != 1 || policy.ReadRoot != "/workspace" || policy.SecurityCeiling != "read-only" {
		t.Fatalf("containment = calls:%d policy:%#v, want one exact policy check", calls, policy)
	}
	if verified.Requirements[0].Match != originalMatch {
		t.Fatalf("verifier request = %#v, want authoritative prepared request", verified)
	}
	if access.calls != 1 || access.requirement.Match != originalMatch {
		t.Fatalf("access = calls:%d requirement:%#v, want independent owned request", access.calls, access.requirement)
	}
	if len(candidate.seen) != 1 || candidate.seen[0].Request.Requirements[0].Match != originalMatch {
		t.Fatalf("tool prepared call = %#v, want independent owned request", candidate.seen)
	}
	if retained[0].Match != "/outside/preparer" {
		t.Fatal("test did not exercise retained preparer alias")
	}
}

func TestEvidenceRunnerContainmentFailuresAreClosedAndPrecedeAccess(t *testing.T) {
	t.Parallel()

	const secret = "outside-root-or-symlink-secret"
	tests := []struct {
		name     string
		verifier EvidenceContainmentVerifier
		want     EvidenceFailureReason
	}{
		{name: "outside root", verifier: &evidenceContainmentStub{err: errors.New(secret)}, want: EvidenceFailureContainmentRefused},
		{name: "ambiguous symlink", verifier: &evidenceContainmentStub{err: errors.New(secret)}, want: EvidenceFailureContainmentRefused},
		{name: "security ceiling", verifier: &evidenceContainmentStub{err: errors.New(secret)}, want: EvidenceFailureContainmentRefused},
		{name: "panic", verifier: &evidenceContainmentStub{panic: true}, want: EvidenceFailureInternal},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			access := &observingEvidenceAccess{}
			runner, err := newEvidenceRunner(
				access,
				testCase.verifier,
				EvidenceContainmentPolicy{ReadRoot: "/workspace", SecurityCeiling: "read-only"},
				[]string{evidenceReadKind},
				func() (uuid.UUID, error) { return mustRuntimeTestID(t), nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			candidate := newPreparedEvidenceTool("workspace_read", "ok")
			_, err = runner.run(
				context.Background(),
				[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
				[]evidenceToolCall{{id: "call", name: "workspace_read", input: []byte(`{}`)}},
				hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			)
			assertEvidenceFailure(t, err, testCase.want)
			if access.calls != 0 {
				t.Fatalf("access calls = %d, want containment before access", access.calls)
			}
			if _, runs := candidate.counts(); runs != 0 {
				t.Fatal("containment refusal executed tool")
			}
			if err != nil && (errors.Is(err, testCase.verifier.(*evidenceContainmentStub).err) ||
				strings.Contains(err.Error(), secret)) {
				t.Fatalf("error leaked containment cause: %v", err)
			}
		})
	}
}

func TestEvidenceRunnerRequiresContainmentVerifier(t *testing.T) {
	t.Parallel()

	for _, verifier := range []EvidenceContainmentVerifier{
		nil,
		(*typedNilEvidenceContainment)(nil),
	} {
		_, err := newEvidenceRunner(
			&evidenceAccessStub{access: gate.AccessAllow},
			verifier,
			EvidenceContainmentPolicy{ReadRoot: "/workspace", SecurityCeiling: "read-only"},
			[]string{evidenceReadKind},
			uuid.New,
		)
		assertEvidenceFailure(t, err, EvidenceFailureInvalidBinding)
	}
}

func TestEvidenceRuntimeConfigRequiresCanonicalContainmentPolicy(t *testing.T) {
	t.Parallel()

	definition := evidenceRuntimeDefinition(t, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
	})
	tests := []struct {
		name   string
		mutate func(*EvidenceRuntimeConfig)
	}{
		{name: "nil verifier", mutate: func(config *EvidenceRuntimeConfig) { config.Containment = nil }},
		{name: "typed nil verifier", mutate: func(config *EvidenceRuntimeConfig) {
			config.Containment = (*typedNilEvidenceContainment)(nil)
		}},
		{name: "relative root", mutate: func(config *EvidenceRuntimeConfig) {
			config.ReadWorkspace.Root = "workspace"
		}},
		{name: "unclean root", mutate: func(config *EvidenceRuntimeConfig) {
			config.ReadWorkspace.Root = "/workspace/../workspace"
		}},
		{name: "empty ceiling", mutate: func(config *EvidenceRuntimeConfig) {
			config.SecurityCeiling = ""
		}},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			config := validRuntimeConfig(t, definition)
			evidence := &EvidenceRuntimeConfig{
				Access:          &evidenceAccessStub{access: gate.AccessAllow},
				Containment:     &evidenceContainmentStub{},
				AllowedKinds:    []string{evidenceReadKind},
				ReadWorkspace:   &tool.ReadWorkspaceBinding{Root: "/workspace"},
				SecurityCeiling: "read-only",
				NewExecutionID:  uuid.New,
			}
			testCase.mutate(evidence)
			config.Evidence = evidence
			runtime, err := newRuntimeController(context.Background(), config)
			if runtime != nil {
				runtime.cancelExecutions()
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) ||
				configErr.Reason != ConfigMissingCollaborator ||
				configErr.Field != "runtime.evidence" {
				t.Fatalf("newRuntimeController() = (%#v,%T %v), want missing runtime.evidence", runtime, err, err)
			}
		})
	}
}

func TestEvidenceContainmentPolicyCarriesNoRuntimeAuthority(t *testing.T) {
	t.Parallel()

	policyType := reflect.TypeOf(EvidenceContainmentPolicy{})
	if policyType.NumField() != 2 ||
		policyType.Field(0).Name != "ReadRoot" ||
		policyType.Field(1).Name != "SecurityCeiling" {
		t.Fatalf("EvidenceContainmentPolicy fields = %#v, want root and ceiling only", policyType)
	}
	var _ EvidenceContainmentVerifier = (*evidenceContainmentStub)(nil)
}

func TestEvidenceContainmentConsumerRejectsOutsideSymlinkAmbiguityAndCeiling(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		match   string
		scope   string
		ceiling string
		wantErr bool
	}{
		{name: "inside", match: filepath.Join(root, "inside"), ceiling: "read-only"},
		{name: "outside root", match: outside, ceiling: "read-only", wantErr: true},
		{name: "symlink escape", match: filepath.Join(root, "escape"), ceiling: "read-only", wantErr: true},
		{name: "ambiguous scope", match: filepath.Join(root, "inside"), scope: "ambiguous", ceiling: "read-only", wantErr: true},
		{name: "security ceiling", match: filepath.Join(root, "inside"), ceiling: "workspace-write", wantErr: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			candidate := newPreparedEvidenceTool("workspace_read", "ok")
			candidate.request = func(id uuid.UUID) tool.Request {
				return tool.Request{
					ToolName: "workspace_read", ExecutionID: id.String(),
					Requirements: []tool.Requirement{{
						Kind: evidenceReadKind, Scope: testCase.scope,
						Match: testCase.match, Description: "read evidence",
					}},
				}
			}
			access := &observingEvidenceAccess{}
			runner, err := newEvidenceRunner(
				access,
				&filesystemEvidenceContainment{},
				EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: testCase.ceiling},
				[]string{evidenceReadKind},
				uuid.New,
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.run(
				context.Background(),
				[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
				[]evidenceToolCall{{id: "call", name: "workspace_read", input: []byte(`{}`)}},
				hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			)
			if testCase.wantErr {
				assertEvidenceFailure(t, err, EvidenceFailureContainmentRefused)
				if access.calls != 0 {
					t.Fatal("refused containment reached access")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEvidenceWorkerRecoversExecutionIDPanicWithoutLeakingValue(t *testing.T) {
	t.Parallel()

	const secret = "execution-id-panic-secret"
	candidate := newPreparedEvidenceTool("workspace_read", "ok")
	runner, err := newEvidenceRunner(
		allowingConcurrentEvidenceAccess{},
		&evidenceContainmentStub{},
		EvidenceContainmentPolicy{ReadRoot: "/workspace", SecurityCeiling: "read-only"},
		[]string{evidenceReadKind},
		func() (uuid.UUID, error) { panic(secret) },
	)
	if err != nil {
		t.Fatal(err)
	}
	controller := runtimeEvidenceController(
		t,
		mustRuntimeTestID(t),
		evidenceRuntimeDefinition(t, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{candidate}, nil
		}),
	)
	faults := &runtimeTestFaults{}
	controller.runtime.faults = faults
	controller.runtime.evidenceRunner = runner
	results, err := controller.runtime.runEvidence(
		context.Background(),
		hustle.RunID(mustRuntimeTestID(t)),
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
		[]evidenceToolCall{{id: "call", name: "workspace_read", input: []byte(`{}`)}},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
	)
	if results != nil {
		t.Fatalf("results = %#v, want nil", results)
	}
	assertEvidenceFailure(t, err, EvidenceFailureInternal)
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("panic leaked through error: %v", err)
	}
	faults.mu.Lock()
	defer faults.mu.Unlock()
	var panicErr *EvidenceWorkerPanicError
	if len(faults.faults) != 1 || !errors.As(faults.faults[0], &panicErr) {
		t.Fatalf("faults = %#v, want one redacted evidence worker panic", faults.faults)
	}
}

func TestEvidenceRunnerConcurrentCallsOnSharedToolOwnPreparedRequests(t *testing.T) {
	t.Parallel()

	candidate := newPreparedEvidenceTool("workspace_read", "ok")
	candidate.request = func(id uuid.UUID) tool.Request {
		return tool.Request{
			ToolName: "workspace_read", ExecutionID: id.String(),
			Requirements: []tool.Requirement{{
				Kind: evidenceReadKind, Match: "/workspace/" + id.String(),
				Description: "read evidence",
			}},
		}
	}
	runner, err := newEvidenceRunner(
		allowingConcurrentEvidenceAccess{},
		&evidenceContainmentStub{verify: func(request tool.Request) {
			request.Requirements[0].Match = "/outside/mutated"
		}},
		EvidenceContainmentPolicy{ReadRoot: "/workspace", SecurityCeiling: "read-only"},
		[]string{evidenceReadKind},
		uuid.New,
	)
	if err != nil {
		t.Fatal(err)
	}
	catalog := []hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, callID := range []string{"first", "second"} {
		callID := callID
		go func() {
			<-start
			_, runErr := runner.run(
				context.Background(),
				catalog,
				[]evidenceToolCall{{id: callID, name: "workspace_read", input: []byte(`{}`)}},
				hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			)
			errs <- runErr
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	if len(candidate.seen) != 2 {
		t.Fatalf("prepared calls = %d, want 2", len(candidate.seen))
	}
	for _, prepared := range candidate.seen {
		want := "/workspace/" + prepared.ExecutionID.String()
		if got := prepared.Request.Requirements[0].Match; got != want {
			t.Fatalf("prepared request match = %q, want %q", got, want)
		}
	}
}

func TestEvidenceBindingIgnoredCancellationPoisonsControllerWithinDrainTimeout(t *testing.T) {
	t.Parallel()

	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var factoryCalls atomic.Int32
	definition := runtimeEvidenceDefinitionWithTimeout(
		t,
		&runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
			t.Fatal("inference reached before binding completed")
			return nil, nil
		}},
		runtimeEvidenceModel(),
		func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			factoryCalls.Add(1)
			once.Do(func() { close(started) })
			<-release
			return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
		},
		hustle.ToolLoopLimits{
			MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
			MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
		},
		20*time.Millisecond,
	)
	controller := runtimeEvidenceControllerWith(
		t, sessionID, definition, &runtimeTestAudit{}, time.Second, 10*time.Millisecond,
	)

	err := controller.RunAndFinalize(
		context.Background(),
		runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID),
		acceptResult,
		noOpFinalizer,
	)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.ReasonCode != hustle.ReasonTimeout {
		t.Fatalf("error = %T %v, want bounded timeout", err, err)
	}
	select {
	case <-started:
	default:
		t.Fatal("binding factory did not start")
	}
	secondErr := controller.RunAndFinalize(
		context.Background(),
		runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID),
		acceptResult,
		noOpFinalizer,
	)
	var admission *AdmissionError
	if !errors.As(secondErr, &admission) || admission.Reason != AdmissionPoisoned {
		t.Fatalf("second error = %T %v, want poisoned admission", secondErr, secondErr)
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("binding workers = %d, want one bounded admitted worker", got)
	}
	close(release)
}
