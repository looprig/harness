package sessionruntime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/delegationtool"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

type failChildStartAppender struct {
	enabled atomic.Bool
	err     error
}

type failDelegateAcceptanceAppender struct {
	enabled atomic.Bool
	err     error
}

type blockingChildTurnStartedAppender struct {
	enabled atomic.Bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingChildTurnStartedAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.TurnStarted); !ok || !a.enabled.Load() {
		return 1, nil
	}
	a.once.Do(func() { close(a.reached) })
	select {
	case <-a.release:
		return 1, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (a *failDelegateAcceptanceAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.DelegateRequestAccepted); ok && a.enabled.Load() {
		return 0, a.err
	}
	return 1, nil
}

func (a *failChildStartAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.LoopStarted); ok && a.enabled.Load() {
		return 0, a.err
	}
	return 1, nil
}

// delegation_test.go drives the parent-scoped tool.DelegateController end-to-end against
// REAL child loops (a stub LLM emitting one final message). It exercises the
// security-critical invariants: agent authorization + resolution, mode validation
// before quota, ownership (registry-derived) rejection of siblings/ancestors/unrelated
// loops, the action set per delegation style, quota reservation before construction, and
// the wait:true / wait:false→wait request correlation.

func delegateParent(style loop.DelegationStyle, delegates ...identity.AgentName) loop.Definition {
	return mustDefine(
		loop.WithName("parent"),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("parent")}}, validModel("parent")),
		loop.WithDelegates(delegates...),
		loop.WithDelegation(loop.Delegation{Style: style}),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func delegateChild(name, finalText string) loop.Definition {
	return mustDefine(
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk(finalText)}}, validModel(name)),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func delegateBlockingChild(name string) loop.Definition {
	return mustDefine(
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(&stubLLM{blockUntilCancel: true}, validModel(name)),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

type releasedFailureLLM struct {
	started chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (l *releasedFailureLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("releasedFailureLLM.Invoke not used")
}
func (l *releasedFailureLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return stream.NewStreamReader(func() (content.Chunk, error) {
		l.once.Do(func() { close(l.started) })
		<-l.release
		return nil, l.err
	}, nil), nil
}

func delegateChildWithModes(name, finalText string) loop.Definition {
	return mustDefine(
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk(finalText)}}, validModel(name)),
		loop.WithModes(
			loop.Mode{Name: "build", Effort: testEffortHigh, Instructions: "build-i"},
			loop.Mode{Name: "review", Effort: model.EffortLow, Instructions: "review-i"},
		),
		loop.WithInitialMode("build"),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func newDelegationSession(t *testing.T, parent loop.Definition, options []Option, children ...loop.Definition) *Session {
	t.Helper()
	defs := append([]loop.Definition{parent}, children...)
	topo := Topology{Definitions: defs, Primers: []identity.AgentName{parent.Name()}, ActivePrimer: parent.Name()}
	opts := append([]Option{WithFingerprintProvider(testFingerprintProvider)}, options...)
	s, err := newSessionTopology(context.Background(), topo, uuid.New, time.Now, opts...)
	if err != nil {
		t.Fatalf("newSessionTopology: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

func delegateCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitResponse(ctx context.Context, controller tool.DelegateController, session *Session, agentID, responseID uuid.UUID, timeoutSeconds *int) (tool.DelegateResult, error) {
	return controller.(*scopedController).waitResponse(ctx, session, agentID, responseID, timeoutSeconds)
}

// TestDelegateStartSyncReturnsChildText proves the synchronous start path: the scoped
// controller spawns the authorized child, drives one turn, and returns its final text.
// The child is registered as owned by the parent.
func TestDelegateStartSyncReturnsChildText(t *testing.T) {
	t.Parallel()
	s := newDelegationSession(t, delegateParent(loop.DelegationManaged, "child"), nil, delegateChild("child", "child final"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), delegateParent(loop.DelegationManaged, "child"))

	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Name: "api_planner", Message: "go", WaitForResponse: true})
	if err != nil {
		t.Fatalf("Execute(start) error = %v", err)
	}
	if res.ResponseStatus != tool.DelegateResponseCompleted || res.State != tool.AgentStateIdle {
		t.Errorf("result state/status = %v/%v, want idle/completed", res.State, res.ResponseStatus)
	}
	if res.Response != "child final" {
		t.Errorf("output = %q, want %q", res.Response, "child final")
	}
	if res.AgentID.IsZero() {
		t.Fatal("delegate id is zero")
	}
	s.loopsMu.RLock()
	handle, ok := s.loops[res.AgentID]
	s.loopsMu.RUnlock()
	if !ok || handle.parent.LoopID != s.ActiveLoopID() {
		t.Errorf("child not registered as owned by parent %v", s.ActiveLoopID())
	}
}

func TestDelegateTerminalResponseResultIsIdleBeforeLiveStateUpdate(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent).(*scopedController)

	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: active.AgentID})
	}()
	waitDelegateMechanicalStatus(t, ctrl, active.AgentID, tool.DelegateStatusRunning)

	for _, status := range []tool.DelegateStatusValue{
		tool.DelegateStatusCompleted,
		tool.DelegateStatusInterrupted,
		tool.DelegateStatusFailed,
		tool.DelegateStatusTimedOut,
	} {
		result := ctrl.responseResult(s, active.AgentID, mustUUID(), status, "terminal")
		if result.State != tool.AgentStateIdle {
			t.Errorf("responseResult(%v).State = %v, want idle", status, result.State)
		}
	}

	persistent, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: active.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(persistent.Agents) != 1 || persistent.Agents[0].State != tool.AgentStateWorking {
		t.Fatalf("persistent agent state = %+v, want working", persistent.Agents)
	}
}

// TestDelegateStartValidation covers the boundary refusals that must NOT spawn: an
// unauthorized agent, an agent not in the topology, and an undeclared mode.
func TestDelegateStartValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  tool.DelegateRequest
		kind DelegateErrorKind
	}{
		{name: "unauthorized agent", req: tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "stranger", Message: "m", WaitForResponse: true}, kind: DelegateUnauthorizedAgent},
		{name: "unknown agent not in topology", req: tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "ghost", Message: "m", WaitForResponse: true}, kind: DelegateUnknownAgent},
		{name: "undeclared mode", req: tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", AgentMode: "nope", Message: "m", WaitForResponse: true}, kind: DelegateUnknownMode},
	}
	// The parent authorizes "child" and "ghost", but only "child" resolves in the topology.
	parent := delegateParent(loop.DelegationManaged, "child", "ghost")
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newDelegationSession(t, parent, nil, delegateChild("child", "final"))
			ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
			before := s.spawnedCount()
			_, err := ctrl.Execute(delegateCtx(t), tt.req)
			var de *DelegateError
			if !errors.As(err, &de) || de.Kind != tt.kind {
				t.Fatalf("error = %v, want DelegateError kind %d", err, tt.kind)
			}
			if got := s.spawnedCount(); got != before {
				t.Errorf("spawned count = %d, want unchanged %d (no spawn on refusal)", got, before)
			}
		})
	}
}

func TestDelegateStartFailsClosedForRoleMissingFromPopulatedCatalog(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "native child")
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType:  "other",
		AgentHarness:  "codex",
		Profile:       "acp/codex",
		Credential:    loop.CredentialNativeAuth,
		Source:        loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
		Default:       true,
	}})
	if err != nil {
		t.Fatal(err)
	}

	s := newDelegationSession(t, parent, []Option{WithRuntimeCatalog(catalog)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	before := s.spawnedCount()
	_, err = ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true})
	var runtimeErr *DelegateError
	if !errors.As(err, &runtimeErr) || runtimeErr.Kind != DelegateRuntimeUnavailable {
		t.Fatalf("missing-role start error = %v, want DelegateRuntimeUnavailable", err)
	}
	if got := s.spawnedCount(); got != before {
		t.Fatalf("spawned count = %d, want unchanged %d", got, before)
	}
}

func TestDelegateStartKeepsNativeFallbackForEmptyCatalog(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "native child")
	emptyCatalog, err := loop.NewRuntimeCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newDelegationSession(t, parent, []Option{WithRuntimeCatalog(emptyCatalog)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	result, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true})
	if err != nil {
		t.Fatalf("empty-catalog native start error = %v", err)
	}
	if result.Response != "native child" {
		t.Fatalf("empty-catalog output = %q, want %q", result.Response, "native child")
	}
}

// TestDelegateActionSetEnforcement proves the parent-scoped controller re-enforces the
// action set independent of crafted JSON: a sync-only parent's controller rejects every
// managed action, while a managed controller admits them.
func TestDelegateActionSetEnforcement(t *testing.T) {
	t.Parallel()
	s := newDelegationSession(t, delegateParent(loop.DelegationManaged, "child"), nil, delegateChild("child", "final"))
	del := mustUUID()
	managedOnly := []tool.DelegateOperation{tool.DelegateSend, tool.DelegateInterrupt, tool.DelegateStatus}
	for _, op := range managedOnly {
		op := op
		syncCtrl := s.delegation.controllerFor(s.ActiveLoopID(), delegateParent(loop.DelegationSyncOnly, "child"))
		_, err := syncCtrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: op, AgentID: del})
		var de *DelegateError
		if !errors.As(err, &de) || de.Kind != DelegateActionUnavailable {
			t.Fatalf("sync-only op %v error = %v, want DelegateActionUnavailable", op, err)
		}
	}

	syncCtrl := s.delegation.controllerFor(s.ActiveLoopID(), delegateParent(loop.DelegationSyncOnly, "child"))
	_, err := syncCtrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "m", WaitForResponse: false})
	var de *DelegateError
	if !errors.As(err, &de) || de.Kind != DelegateActionUnavailable {
		t.Fatalf("sync-only wait:false start error = %v, want DelegateActionUnavailable", err)
	}
}

// TestDelegateOwnershipRejection proves a scoped controller addresses ONLY children of
// its bound parent: an owned child is addressable, but a controller bound to a different
// parent rejects it as not owned.
func TestDelegateOwnershipRejection(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "final"))
	owner := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	res, err := owner.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	childID := res.AgentID

	// A controller bound to an unrelated parent loop id owns nothing here.
	stranger := s.delegation.controllerFor(mustUUID(), parent)
	tests := []struct {
		name string
		req  tool.DelegateRequest
	}{
		{name: "send", req: tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: childID, Message: "m"}},
		{name: "interrupt", req: tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: childID}},
		{name: "status", req: tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: childID}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := stranger.Execute(delegateCtx(t), tt.req)
			var de *DelegateError
			if !errors.As(err, &de) || de.Kind != DelegateNotOwned {
				t.Fatalf("error = %v, want DelegateNotOwned", err)
			}
		})
	}

	// The real owner CAN interrupt its child.
	if _, err := owner.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: childID}); err != nil {
		t.Fatalf("owner interrupt error = %v", err)
	}
}

// TestDelegateModeSelectiveStart proves a supplied valid mode starts the child DIRECTLY
// in that mode (the child's live mode is the selected one), without a synthetic mode
// change.
func TestDelegateModeSelectiveStart(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChildWithModes("child", "final"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", AgentMode: "review", Message: "go", WaitForResponse: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	handle, ok := s.Loop(res.AgentID)
	if !ok {
		t.Fatal("child not registered")
	}
	if handle.Mode() != "review" {
		t.Errorf("child live mode = %q, want review (started directly in the selected mode)", handle.Mode())
	}
	listed, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: res.AgentID})
	if err != nil || len(listed.Agents) != 1 || listed.Agents[0].AgentMode != "review" {
		t.Fatalf("listed initial mode = %+v, %v; want review", listed.Agents, err)
	}
}

func TestDelegateRuntimeIsRevalidatedAndPinnedAtChildBind(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "runtime child")
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "child", AgentHarness: "test", Profile: "acp/test", Default: true, DefaultModel: "runtime", SmallModel: "runtime-small",
		Credential: loop.CredentialGatewayBacked,
		Models: []loop.RuntimeModelOption{
			{Alias: "runtime", Target: validModel("runtime-target"), DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh}},
			{Alias: "runtime-small", Target: validModel("runtime-small-target"), DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh}},
		},
	}, {
		SubagentType: "child", AgentHarness: "test", Profile: "acp/test",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
	}})
	if err != nil {
		t.Fatal(err)
	}
	builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	registry := &foreign.BuilderRegistry{}
	if err := registry.Register("acp/test", builder.build, builder.buildRestored); err != nil {
		t.Fatal(err)
	}
	s := newDelegationSession(t, parent, []Option{WithRuntimeCatalog(catalog), WithForeignBuilderRegistry(registry)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	request := tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true, Runtime: &tool.DelegateRuntime{Harness: "test", Profile: "acp/test", Model: "runtime", SmallModel: "runtime-small", Effort: "high"}}
	result, err := ctrl.Execute(delegateCtx(t), request)
	if err != nil {
		t.Fatalf("runtime start: %v", err)
	}
	s.loopsMu.RLock()
	handle, ok := s.loops[result.AgentID]
	s.loopsMu.RUnlock()
	if !ok || handle == nil {
		t.Fatal("runtime child not registered")
	}
	if handle.bound.Model().Name != "runtime-target" || handle.bound.Engine() != loop.EngineAdapter {
		t.Fatalf("bound runtime = engine=%v model=%q, want adapter/runtime-target", handle.bound.Engine(), handle.bound.Model().Name)
	}
	listed, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: result.AgentID})
	if err != nil || len(listed.Agents) != 1 {
		t.Fatalf("list selected runtime = %+v, %v", listed.Agents, err)
	}
	listedRuntime := listed.Agents[0].Runtime
	if listedRuntime.Harness != "test" || listedRuntime.Source != "gateway" || listedRuntime.Model != "runtime" || listedRuntime.Effort != "high" {
		t.Fatalf("listed runtime = %+v, want test/gateway/runtime/high", listedRuntime)
	}
	defaultResult, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "default", WaitForResponse: true})
	if err != nil {
		t.Fatalf("default runtime start: %v", err)
	}
	s.loopsMu.RLock()
	defaultHandle := s.loops[defaultResult.AgentID]
	s.loopsMu.RUnlock()
	if defaultHandle == nil || defaultHandle.bound.RuntimeProfile() != "acp/test" || defaultHandle.bound.Model().Name != "runtime-target" {
		t.Fatalf("default bound runtime = %#v, want catalog default adapter runtime", defaultHandle)
	}
	_, err = ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "bad", WaitForResponse: true, Runtime: &tool.DelegateRuntime{Harness: "test", Profile: "acp/test", Model: "missing", Effort: "high"}})
	var runtimeErr *DelegateError
	if !errors.As(err, &runtimeErr) || runtimeErr.Kind != DelegateRuntimeInvalid {
		t.Fatalf("invalid runtime error = %v, want typed runtime refusal", err)
	}
	_, err = ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "bad small", WaitForResponse: true, Runtime: &tool.DelegateRuntime{Harness: "test", Profile: "acp/test", Model: "runtime", SmallModel: "wrong-small", Effort: "high"}})
	if !errors.As(err, &runtimeErr) || runtimeErr.Kind != DelegateRuntimeInvalid {
		t.Fatalf("invalid small model error = %v, want typed runtime refusal", err)
	}
	managedResult, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: "child", Message: "managed", WaitForResponse: true,
		Runtime: &tool.DelegateRuntime{Harness: "test", Profile: "acp/test", Source: "native", SelectionKind: "harness-managed", Explicit: tool.DelegateRuntimeExplicit{Source: true}},
	})
	if err != nil {
		t.Fatalf("managed runtime start: %v", err)
	}
	s.loopsMu.RLock()
	managedHandle := s.loops[managedResult.AgentID]
	s.loopsMu.RUnlock()
	if managedHandle == nil {
		t.Fatal("managed runtime child not registered")
	}
	managedIdentity := managedHandle.bound.RuntimeIdentity()
	if managedIdentity.Source != loop.RuntimeSourceNative || managedIdentity.SelectionKind != loop.RuntimeSelectionHarnessManaged || managedIdentity.ModelAlias != "" || managedIdentity.TargetModel != "" || managedIdentity.Effort != model.EffortNone {
		t.Fatalf("managed bound identity = %+v, want native/harness-managed without model/effort", managedIdentity)
	}
	for _, runtime := range []*tool.DelegateRuntime{
		{Harness: "test", Profile: "acp/test", Source: "gateway", SelectionKind: "explicit", Effort: "high", Explicit: tool.DelegateRuntimeExplicit{Source: true}},
		{Harness: "test", Profile: "acp/test", Source: "native", SelectionKind: "harness-managed", Model: "runtime", Explicit: tool.DelegateRuntimeExplicit{Source: true}},
		{Harness: "test", Profile: "acp/test", Source: "native", SelectionKind: "harness-managed", Effort: "high", Explicit: tool.DelegateRuntimeExplicit{Source: true}},
	} {
		_, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "invalid mixed source", WaitForResponse: true, Runtime: runtime})
		if !errors.As(err, &runtimeErr) || runtimeErr.Kind != DelegateRuntimeInvalid {
			t.Fatalf("invalid mixed-source runtime %+v error = %v, want typed runtime refusal", runtime, err)
		}
	}
}

func TestDelegateRuntimeAcceptsPreparedExplicitSingleChoiceAndRejectsInvalidMembership(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "runtime child")
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "child", AgentHarness: "test", Profile: "acp/test", Default: true,
		Credential: loop.CredentialGatewayBacked, DefaultModel: "runtime",
		Models: []loop.RuntimeModelOption{{
			Alias: "runtime", Target: validModel("runtime-target"),
			DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	registry := &foreign.BuilderRegistry{}
	if err := registry.Register("acp/test", builder.build, builder.buildRestored); err != nil {
		t.Fatal(err)
	}
	s := newDelegationSession(t, parent, []Option{WithRuntimeCatalog(catalog), WithForeignBuilderRegistry(registry)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	startTool := delegationtool.NewStartAgent(ctrl, loop.DelegationManaged, []delegationtool.AgentCatalogEntry{{Name: "child"}}, catalog)
	_, prepared, err := startTool.PrepareCall(context.Background(), mustUUID(), `{"agent_type":"child","instructions":"go","model":"runtime","effort":"medium"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	artifact, ok := prepared.(tool.DelegateArtifact)
	if !ok || artifact.Runtime == nil || !artifact.Runtime.Explicit.Model || !artifact.Runtime.Explicit.Effort {
		t.Fatalf("prepared artifact = %#v, want explicit model and effort", prepared)
	}
	result, err := ctrl.Execute(delegateCtx(t), artifact.Request)
	if err != nil {
		t.Fatalf("Execute(prepared request) error = %v", err)
	}
	if result.AgentID.IsZero() {
		t.Fatal("Execute(prepared request) returned zero agent id")
	}

	for name, mutate := range map[string]func(*tool.DelegateRuntime){
		"model":  func(runtime *tool.DelegateRuntime) { runtime.Model = "missing" },
		"effort": func(runtime *tool.DelegateRuntime) { runtime.Effort = "high" },
	} {
		t.Run("invalid "+name+" membership", func(t *testing.T) {
			invalid := *artifact.Runtime
			mutate(&invalid)
			_, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
				Operation: tool.DelegateStart, AgentType: "child", Message: "bad", WaitForResponse: true, Runtime: &invalid,
			})
			var runtimeErr *DelegateError
			if !errors.As(err, &runtimeErr) || runtimeErr.Kind != DelegateRuntimeInvalid {
				t.Fatalf("Execute(invalid %s) error = %v, want typed runtime refusal", name, err)
			}
		})
	}
}

func TestDelegateRuntimeProjectsStableModelAliasFromSecondHarnessSharingProfile(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "runtime child")
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "child", AgentHarness: "alpha", Profile: "acp/shared", Default: true,
		Credential: loop.CredentialGatewayBacked, DefaultModel: "luna",
		Models: []loop.RuntimeModelOption{{
			Alias: "luna", Target: validModel("luna-target"),
			DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow, model.EffortHigh},
		}},
	}, {
		SubagentType: "child", AgentHarness: "codex", Profile: "acp/shared",
		Credential: loop.CredentialGatewayBacked, DefaultModel: "luna",
		Models: []loop.RuntimeModelOption{{
			Alias: "luna", Target: validModel("luna-target"),
			DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow, model.EffortHigh},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	registry := &foreign.BuilderRegistry{}
	if err := registry.Register("acp/shared", builder.build, builder.buildRestored); err != nil {
		t.Fatal(err)
	}
	s := newDelegationSession(t, parent, []Option{WithRuntimeCatalog(catalog), WithForeignBuilderRegistry(registry)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	result, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true,
		Runtime: &tool.DelegateRuntime{
			Harness: "codex", Profile: "acp/shared", Source: "gateway", SelectionKind: "explicit",
			Model: "luna", Effort: "high", Explicit: tool.DelegateRuntimeExplicit{Harness: true, Model: true, Effort: true},
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	s.loopsMu.RLock()
	handle := s.loops[result.AgentID]
	s.loopsMu.RUnlock()
	if handle == nil {
		t.Fatal("runtime child not registered")
	}
	if got := handle.bound.RuntimeIdentity().ModelAlias; got != "luna@high" {
		t.Fatalf("durable runtime model alias = %q, want luna@high", got)
	}
	listed, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: result.AgentID})
	if err != nil || len(listed.Agents) != 1 {
		t.Fatalf("list selected runtime = %+v, %v", listed.Agents, err)
	}
	if got := listed.Agents[0].Runtime.Model; got != "luna" {
		t.Fatalf("public runtime model = %q, want stable alias luna and never concrete alias luna@high", got)
	}
	if got := listed.Agents[0].Runtime.Harness; got != "codex" {
		t.Fatalf("public runtime harness = %q, want selected harness codex", got)
	}
	scoped, ok := ctrl.(*scopedController)
	if !ok {
		t.Fatalf("controller = %T, want *scopedController", ctrl)
	}
	withoutMapping := *scoped
	withoutMapping.runtimeCatalog = loop.RuntimeCatalog{}
	if got := withoutMapping.agentRuntime(handle).Model; got != "" {
		t.Fatalf("public runtime model without stable mapping = %q, want omitted", got)
	}
	legacyHandle := &loopHandle{bound: handle.bound}
	legacyRuntime := scoped.agentRuntime(legacyHandle)
	if legacyRuntime.Harness != "" || legacyRuntime.Model != "" {
		t.Fatalf("ambiguous legacy public runtime = %+v, want harness and model omitted", legacyRuntime)
	}
}

func TestControllerValidatesHarnessManagedRuntimeSourceAndSelectors(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "child", AgentHarness: "codex", Profile: "acp/codex",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged, Default: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	base := tool.DelegateRuntime{
		Harness: "codex", Profile: "acp/codex", Source: "native", SelectionKind: "harness-managed",
	}
	if !validControllerRuntimeSelection(catalog, "child", base) {
		t.Fatal("validControllerRuntimeSelection() rejected harness-managed native runtime")
	}
	for _, invalid := range []tool.DelegateRuntime{
		{Harness: "codex", Profile: "acp/codex", Source: "native", SelectionKind: "harness-managed", Model: "luna", Effort: "none"},
		{Harness: "codex", Profile: "acp/codex", Source: "native", SelectionKind: "harness-managed", Effort: "high"},
		{Harness: "codex", Profile: "acp/codex", Source: "gateway", SelectionKind: "harness-managed", Effort: "none"},
	} {
		if validControllerRuntimeSelection(catalog, "child", invalid) {
			t.Fatalf("validControllerRuntimeSelection() accepted invalid runtime %+v", invalid)
		}
	}
}

func TestDelegateRuntimeProjectsSelectedHarnessManagedHarnessWhenProfileIsShared(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "runtime child")
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "child", AgentHarness: "alpha", Profile: "acp/shared", Default: true,
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
	}, {
		SubagentType: "child", AgentHarness: "codex", Profile: "acp/shared",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
	}})
	if err != nil {
		t.Fatal(err)
	}
	builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	registry := &foreign.BuilderRegistry{}
	if err := registry.Register("acp/shared", builder.build, builder.buildRestored); err != nil {
		t.Fatal(err)
	}
	s := newDelegationSession(t, parent, []Option{WithRuntimeCatalog(catalog), WithForeignBuilderRegistry(registry)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	result, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true,
		Runtime: &tool.DelegateRuntime{
			Harness: "codex", Profile: "acp/shared", Source: "native", SelectionKind: "harness-managed",
			Explicit: tool.DelegateRuntimeExplicit{Harness: true},
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	listed, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: result.AgentID})
	if err != nil || len(listed.Agents) != 1 {
		t.Fatalf("list selected runtime = %+v, %v", listed.Agents, err)
	}
	if got := listed.Agents[0].Runtime.Harness; got != "codex" {
		t.Fatalf("public runtime harness = %q, want selected harness codex", got)
	}
}

func TestControllerValidatesPerModelSourceWithinOneEntry(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "child", AgentHarness: "codex", Profile: "acp/codex-mixed",
		Credential: loop.CredentialGatewayBacked, Source: loop.RuntimeSourceGateway, Default: true,
		DefaultModel: "gateway",
		Models: []loop.RuntimeModelOption{
			{Alias: "gateway", Source: loop.RuntimeSourceGateway, Credential: loop.CredentialGatewayBacked, Target: validModel("gateway"), DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh}},
			{Alias: "native", Source: loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth, Target: validModel("native"), DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	for _, runtime := range []tool.DelegateRuntime{
		{Harness: "codex", Profile: "acp/codex-mixed", Source: "gateway", SelectionKind: "explicit", Model: "gateway", Effort: "high", Explicit: tool.DelegateRuntimeExplicit{Source: true}},
		{Harness: "codex", Profile: "acp/codex-mixed", Source: "native", SelectionKind: "explicit", Model: "native", Effort: "medium", Explicit: tool.DelegateRuntimeExplicit{Source: true}},
	} {
		if !validControllerRuntimeSelection(catalog, "child", runtime) {
			t.Fatalf("validControllerRuntimeSelection() rejected per-model runtime %+v", runtime)
		}
	}
	for _, runtime := range []tool.DelegateRuntime{
		{Harness: "codex", Profile: "acp/codex-mixed", Source: "native", SelectionKind: "explicit", Effort: "medium", Explicit: tool.DelegateRuntimeExplicit{Source: true}},
		{Harness: "codex", Profile: "acp/codex-mixed", Source: "native", SelectionKind: "explicit", Model: "gateway", Effort: "high", Explicit: tool.DelegateRuntimeExplicit{Source: true}},
	} {
		if validControllerRuntimeSelection(catalog, "child", runtime) {
			t.Fatalf("validControllerRuntimeSelection() accepted invalid per-model runtime %+v", runtime)
		}
	}
}

func TestControllerRejectsEffectiveSourceOverrideWithoutAgentSource(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "child", AgentHarness: "codex", Profile: "acp/codex-mixed",
		Credential: loop.CredentialGatewayBacked, Source: loop.RuntimeSourceGateway, Default: true,
		DefaultModel: "gateway",
		Models: []loop.RuntimeModelOption{
			{Alias: "gateway", Source: loop.RuntimeSourceGateway, Credential: loop.CredentialGatewayBacked, Target: validModel("gateway"), DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh}},
			{Alias: "native", Source: loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth, Target: validModel("native"), DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	runtime := tool.DelegateRuntime{
		Harness: "codex", Profile: "acp/codex-mixed", SelectionKind: "explicit",
		Model: "native", Effort: "medium",
	}
	if validControllerRuntimeSelection(catalog, "child", runtime) {
		t.Fatal("validControllerRuntimeSelection() accepted a native override without agent_source")
	}
}

func TestDelegateCatalogDerivesAllowedModes(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	topology := Topology{Definitions: []loop.Definition{parent, delegateChildWithModes("child", "final")}}
	manager := newDelegationManager(topology)
	defs := delegateExtraTools(parent, manager)
	if len(defs) != 1 {
		t.Fatalf("delegateExtraTools length = %d, want 1", len(defs))
	}
	built, err := defs[0].Build(context.Background(), tool.Bindings{
		SessionID: mustUUID(), LoopID: mustUUID(), Delegate: manager.controllerFor(mustUUID(), parent),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var startAgent tool.InvokableTool
	for _, candidate := range built {
		candidateInfo, infoErr := candidate.Info(context.Background())
		if infoErr != nil {
			t.Fatalf("Info: %v", infoErr)
		}
		if candidateInfo.Name == "StartAgent" {
			startAgent = candidate
			break
		}
	}
	if startAgent == nil {
		t.Fatal("built bundle has no StartAgent tool")
	}
	info, err := startAgent.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	for _, want := range []string{`"child"`, `"build"`, `"review"`} {
		if !strings.Contains(string(info.Schema), want) {
			t.Errorf("schema missing %s: %s", want, info.Schema)
		}
	}
}

func TestDelegateExtraToolsInjectsOneAtomicAgentBundle(t *testing.T) {
	t.Parallel()

	want := []string{"ListAgents", "MessageAgent", "StartAgent", "StopAgent"}
	parent := delegateParent(loop.DelegationManaged, "child")
	manager := newDelegationManager(Topology{Definitions: []loop.Definition{parent, delegateChild("child", "final")}})
	definitions := delegateExtraTools(parent, manager)
	if len(definitions) != 1 {
		t.Fatalf("delegateExtraTools(admitted parent) length = %d, want 1", len(definitions))
	}
	if got := definitions[0].ProducedToolNames(); !slices.Equal(got, want) {
		t.Fatalf("ProducedToolNames() = %q, want %q", got, want)
	}
	for _, name := range definitions[0].ProducedToolNames() {
		if name == "Subagent" {
			t.Fatal("delegateExtraTools produced removed Subagent tool")
		}
	}
	built, err := definitions[0].Build(context.Background(), tool.Bindings{
		SessionID: mustUUID(), LoopID: mustUUID(), Delegate: manager.controllerFor(mustUUID(), parent),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(built) != len(want) {
		t.Fatalf("Build() returned %d tools, want %d", len(built), len(want))
	}
	for i, builtTool := range built {
		info, infoErr := builtTool.Info(context.Background())
		if infoErr != nil {
			t.Fatalf("built[%d].Info() error = %v", i, infoErr)
		}
		if info.Name != want[i] {
			t.Errorf("built[%d].Info().Name = %q, want %q", i, info.Name, want[i])
		}
	}

	withoutDelegates := delegateParent(loop.DelegationManaged)
	if got := delegateExtraTools(withoutDelegates, newDelegationManager(Topology{Definitions: []loop.Definition{withoutDelegates}})); len(got) != 0 {
		t.Fatalf("delegateExtraTools(parent without delegates) length = %d, want 0", len(got))
	}
}

func TestDelegateRuntimeCatalogProviderIsParentScoped(t *testing.T) {
	t.Parallel()
	parentA := mustDefine(
		loop.WithName("parent-a"),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("parent-a")}}, validModel("parent-a")),
		loop.WithDelegates("child"),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
	)
	parentB := mustDefine(
		loop.WithName("parent-b"),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("parent-b")}}, validModel("parent-b")),
		loop.WithDelegates("child"),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
	)
	child := delegateChild("child", "child")
	catalog := func(harness loop.AgentHarnessName, profile loop.RuntimeProfileName) loop.RuntimeCatalog {
		result, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
			SubagentType: "child", AgentHarness: harness, Profile: profile, Credential: loop.CredentialGatewayBacked,
			Default: true, DefaultModel: "model", Models: []loop.RuntimeModelOption{{
				Alias: "model", Target: validModel(string(harness)), DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh},
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	catalogA := catalog("claude-code", "acp/claude-code")
	catalogB := catalog("codex", "acp/codex")
	manager := newDelegationManagerWithCatalogProvider(
		Topology{Definitions: []loop.Definition{parentA, parentB, child}},
		func(parent loop.Definition) (loop.RuntimeCatalog, bool) {
			switch parent.Name() {
			case "parent-a":
				return catalogA, true
			case "parent-b":
				return catalogB, true
			default:
				return loop.RuntimeCatalog{}, false
			}
		},
	)
	controllerA := manager.controllerFor(mustUUID(), parentA).(*scopedController)
	controllerB := manager.controllerFor(mustUUID(), parentB).(*scopedController)
	if !controllerA.hasRuntimeCatalog || controllerA.runtimeCatalog.Digest() != catalogA.Digest() {
		t.Fatal("parent-a did not receive its catalog")
	}
	if !controllerB.hasRuntimeCatalog || controllerB.runtimeCatalog.Digest() != catalogB.Digest() {
		t.Fatal("parent-b did not receive its catalog")
	}
	if controllerA.runtimeCatalog.Digest() == controllerB.runtimeCatalog.Digest() {
		t.Fatal("parent-specific catalogs collapsed to one snapshot")
	}
}

func TestFoldDelegateTerminalUsesOnlyTurnDoneMessage(t *testing.T) {
	t.Parallel()
	requestID, turnID, childID := mustUUID(), mustUUID(), mustUUID()
	index := foldDelegateTerminals([]event.Event{
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
		event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Messages: content.AgenticMessages{aiMessage("progress")}},
		event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}},
	})
	got, ok := index[requestID]
	if !ok {
		t.Fatal("correlated terminal missing")
	}
	if got.text != "" || got.status != tool.DelegateStatusCompleted {
		t.Fatalf("terminal = %+v, want empty completed answer", got)
	}
}

func TestCrashClosureReseedsInterruptedDelegateRequest(t *testing.T) {
	t.Parallel()
	requestID, turnID, childID := mustUUID(), mustUUID(), mustUUID()
	original := []event.Event{
		event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}}, InitialRequestID: requestID},
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
	}
	closure := event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}}
	manager := newDelegationManager(Topology{})
	cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: childID}
	if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, cmd)}, original, []event.Event{closure}); err != nil {
		t.Fatal(err)
	}
	got, ok := manager.getResolved(requestID)
	if !ok || got.childID != childID || got.status != tool.DelegateStatusInterrupted {
		t.Fatalf("resolved = %+v, %v; want interrupted child %v", got, ok, childID)
	}
}

func TestRestoreIgnoresUnacceptedDelegateIntent(t *testing.T) {
	t.Parallel()
	requestID, childID := mustUUID(), mustUUID()
	cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: childID}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), uuid.UUID{}, cmd)}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.getResolved(requestID); ok {
		t.Fatalf("unaccepted intent admitted: %+v", got)
	}
}

func TestRestoreDoesNotAdmitOrdinaryTurnTerminalAsDelegateRequest(t *testing.T) {
	t.Parallel()
	requestID, turnID, childID := mustUUID(), mustUUID(), mustUUID()
	ordinary := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyUser}}
	events := []event.Event{
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
		event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Message: aiMessage("ordinary answer")},
	}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, ordinary)}, events, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.getResolved(requestID); ok {
		t.Fatalf("ordinary user request was admitted as delegate result: %+v", got)
	}
}

func TestRestoreOverlaysAdmittedQueuedCancellationReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reason event.CancelReason
		status tool.DelegateStatusValue
	}{
		{name: "interrupted", reason: event.CancelTurnInterrupted, status: tool.DelegateStatusInterrupted},
		{name: "failed", reason: event.CancelTurnFailed, status: tool.DelegateStatusFailed},
		{name: "client retracted", reason: event.CancelClientRetracted, status: tool.DelegateStatusInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestID, childID := mustUUID(), mustUUID()
			cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: childID}
			events := []event.Event{
				event.DelegateRequestAccepted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}},
				event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: tt.reason},
			}
			manager := newDelegationManager(Topology{})
			if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, cmd)}, events, nil); err != nil {
				t.Fatal(err)
			}
			got, ok := manager.getResolved(requestID)
			if !ok || got.childID != childID || got.status != tt.status {
				t.Fatalf("resolved = %+v, %v; want child=%v status=%v", got, ok, childID, tt.status)
			}
		})
	}
}

func TestRestoreIgnoresUnadmittedQueuedCancellation(t *testing.T) {
	t.Parallel()
	requestID, childID := mustUUID(), mustUUID()
	ordinary := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyUser}}
	events := []event.Event{event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.CancelTurnFailed}}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, ordinary)}, events, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.getResolved(requestID); ok {
		t.Fatalf("ordinary cancellation entered delegate index: %+v", got)
	}
}

func TestRestoreRejectsDelegateIntentRouteMismatch(t *testing.T) {
	t.Parallel()
	requestID, target, wrong := mustUUID(), mustUUID(), mustUUID()
	cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: target}
	manager := newDelegationManager(Topology{})
	err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), wrong, cmd)}, nil, nil)
	var mismatch *journal.CommandRouteMismatchError
	if !errors.As(err, &mismatch) || mismatch.RecordLoopID != wrong || mismatch.TargetLoopID != target {
		t.Fatalf("error = %T %+v, want typed route mismatch", err, err)
	}
}

// TestDelegateWaitFalseThenWaitResolves proves the request correlation: a wait:false
// start returns a queued request id, and a later wait for that id resolves the SAME
// request's answer.
func TestDelegateWaitFalseThenWaitResolves(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "async final"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	if err != nil {
		t.Fatalf("start wait:false: %v", err)
	}
	if queued.State != tool.AgentStateWorking {
		t.Fatalf("state = %v, want working", queued.State)
	}
	if queued.CorrelationID.IsZero() || queued.AgentID.IsZero() {
		t.Fatalf("queued handle missing ids: %+v", queued)
	}
	pending, ok := s.delegation.getPending(queued.CorrelationID)
	if !ok {
		t.Fatal("queued request was not registered")
	}
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not resolve")
	}
	status, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: queued.AgentID})
	if err != nil {
		t.Fatalf("status before collection: %v", err)
	}
	if len(status.Agents) != 1 || status.Agents[0].State != tool.AgentStateIdle || status.Agents[0].QueuedMessages != 0 {
		t.Fatalf("resolved-uncollected agents = %+v, want one idle agent with no queued messages", status.Agents)
	}

	resolved, err := waitResponse(delegateCtx(t), ctrl, s, queued.AgentID, queued.CorrelationID, nil)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if resolved.ResponseStatus != tool.DelegateResponseCompleted {
		t.Errorf("response status = %v, want Completed", resolved.ResponseStatus)
	}
	if resolved.Response != "async final" {
		t.Errorf("output = %q, want %q", resolved.Response, "async final")
	}

	// An unknown request id for the same owned child is rejected.
	_, err = waitResponse(delegateCtx(t), ctrl, s, queued.AgentID, mustUUID(), nil)
	var de *DelegateError
	if !errors.As(err, &de) || de.Kind != DelegateUnknownRequest {
		t.Fatalf("wait unknown request error = %v, want DelegateUnknownRequest", err)
	}
}

func TestDelegateStatusCountsOnlyRequestsQueuedBehindActiveTurn(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: active.AgentID})
	}()
	waitDelegateMechanicalStatus(t, ctrl, active.AgentID, tool.DelegateStatusRunning)
	waitDelegateQueuedMessages(t, ctrl, active.AgentID, 0)

	if _, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: active.AgentID, Message: "B", WaitForResponse: false}); err != nil {
		t.Fatal(err)
	}
	waitDelegateQueuedMessages(t, ctrl, active.AgentID, 1)
}

func TestDelegateStatusDoesNotCountStartedRequestBeforeDrainObservesTurnStarted(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	appender := &blockingChildTurnStartedAppender{reached: make(chan struct{}), release: make(chan struct{})}
	s := newDelegationSession(t, parent, []Option{WithEventAppender(appender)}, delegateBlockingChild("child"))
	appender.enabled.Store(true)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(appender.release)
		_, _ = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: active.AgentID})
	}()
	select {
	case <-appender.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("TurnStarted did not reach blocked appender")
	}

	status, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: active.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Agents) != 1 || status.Agents[0].QueuedMessages != 0 {
		t.Fatalf("active request agents = %+v, want queued_messages 0", status.Agents)
	}
}

func TestDelegateStatusTracksForegroundRequestQueuedBehindActiveTurn(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	providerErr := errors.New("released provider")
	client := &releasedFailureLLM{started: make(chan struct{}), release: make(chan struct{}), err: providerErr}
	child := mustDefine(
		loop.WithName("child"),
		loop.WithInference(client, validModel("child")),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
	s := newDelegationSession(t, parent, nil, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	background, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background request did not start")
	}
	waitDelegateQueuedMessages(t, ctrl, background.AgentID, 0)

	foregroundResult := make(chan tool.DelegateResult, 1)
	foregroundErr := make(chan error, 1)
	go func() {
		result, executeErr := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: background.AgentID, Message: "B", WaitForResponse: true})
		foregroundResult <- result
		foregroundErr <- executeErr
	}()
	waitDelegateQueuedMessages(t, ctrl, background.AgentID, 1)

	close(client.release)
	select {
	case result := <-foregroundResult:
		if executeErr := <-foregroundErr; executeErr != nil || result.ResponseStatus != tool.DelegateResponseFailed {
			t.Fatalf("foreground result = %+v, %v; want failed response", result, executeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground request did not finish")
	}
	waitDelegateQueuedMessages(t, ctrl, background.AgentID, 0)

	backgroundRequest, ok := s.delegation.getPending(background.CorrelationID)
	if !ok {
		t.Fatal("completed background request was not retained")
	}
	select {
	case <-backgroundRequest.done:
	case <-time.After(2 * time.Second):
		t.Fatal("background request did not reach terminal state")
	}
	s.delegation.mu.Lock()
	tracked := len(s.delegation.requests)
	s.delegation.mu.Unlock()
	if tracked != 1 {
		t.Fatalf("tracked requests after foreground return = %d, want only retained background request", tracked)
	}

	if _, err := waitResponse(delegateCtx(t), ctrl, s, background.AgentID, background.CorrelationID, nil); err != nil {
		t.Fatal(err)
	}
	s.delegation.mu.Lock()
	tracked = len(s.delegation.requests)
	s.delegation.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("tracked requests after background collection = %d, want 0", tracked)
	}
}

func TestDelegateWaitTimeoutInterruptsRunningChild(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	result, err := waitResponse(context.Background(), ctrl, s, queued.AgentID, queued.CorrelationID, &zero)
	if err != nil || result.ResponseStatus != tool.DelegateResponseTimedOut {
		t.Fatalf("timed wait = %+v, %v; want TimedOut", result, err)
	}
	pending, ok := s.delegation.getPending(queued.CorrelationID)
	if !ok {
		t.Fatal("pending request disappeared after timeout")
	}
	select {
	case <-pending.done:
		_, status := pending.result()
		if status != tool.DelegateStatusInterrupted {
			t.Fatalf("post-timeout terminal = %v, want Interrupted", status)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timed-out wait did not interrupt the child")
	}
}

func TestDelegateQueuedRequestTimeoutCancelsOnlyThatRequest(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateMechanicalStatus(t, ctrl, active.AgentID, tool.DelegateStatusRunning)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: active.AgentID, Message: "B", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	timed, err := waitResponse(context.Background(), ctrl, s, active.AgentID, queued.CorrelationID, &zero)
	if err != nil || timed.ResponseStatus != tool.DelegateResponseTimedOut {
		t.Fatalf("timed wait = %+v, %v; want TimedOut", timed, err)
	}
	pending, ok := s.delegation.getPending(queued.CorrelationID)
	if !ok {
		t.Fatal("timed request was not left collectable")
	}
	select {
	case <-pending.done:
		_, status := pending.result()
		if status != tool.DelegateStatusInterrupted {
			t.Fatalf("queued cancellation status = %v, want Interrupted", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request-specific timeout did not resolve")
	}
	status, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: active.AgentID})
	if err != nil || len(status.Agents) != 1 || status.Agents[0].State != tool.AgentStateWorking {
		t.Fatalf("A status after B timeout = %+v, %v; want Running", status, err)
	}
	_, _ = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: active.AgentID})
}

func TestDelegateQueuedRequestCallerCancellationTargetsOnlyThatRequest(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateMechanicalStatus(t, ctrl, active.AgentID, tool.DelegateStatusRunning)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: active.AgentID, Message: "B", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := waitResponse(cancelledCtx, ctrl, s, active.AgentID, queued.CorrelationID, nil)
	if err != nil || result.ResponseStatus != tool.DelegateResponseInterrupted {
		t.Fatalf("cancelled wait = %+v, %v; want Interrupted", result, err)
	}
	pending, _ := s.delegation.getPending(queued.CorrelationID)
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("caller cancellation did not resolve targeted queued request")
	}
	status, _ := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: active.AgentID})
	if len(status.Agents) != 1 || status.Agents[0].State != tool.AgentStateWorking {
		t.Fatalf("A status after B caller cancellation = %+v, want working", status.Agents)
	}
	_, _ = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: active.AgentID})
}

func TestDelegateRunningRequestTimeoutStartsNextWithoutCancellingIt(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	running, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "B", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateMechanicalStatus(t, ctrl, running.AgentID, tool.DelegateStatusRunning)
	next, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: running.AgentID, Message: "C", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	result, err := waitResponse(context.Background(), ctrl, s, running.AgentID, running.CorrelationID, &zero)
	if err != nil || result.ResponseStatus != tool.DelegateResponseTimedOut {
		t.Fatalf("running timeout = %+v, %v; want TimedOut", result, err)
	}
	runningPending, _ := s.delegation.getPending(running.CorrelationID)
	select {
	case <-runningPending.done:
		_, status := runningPending.result()
		if status != tool.DelegateStatusInterrupted {
			t.Fatalf("running request terminal = %v, want Interrupted", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running request did not resolve after targeted timeout")
	}
	waitDelegateMechanicalStatus(t, ctrl, running.AgentID, tool.DelegateStatusRunning)
	nextPending, ok := s.delegation.getPending(next.CorrelationID)
	if !ok {
		t.Fatal("next request handle missing")
	}
	select {
	case <-nextPending.done:
		t.Fatal("targeting running B also resolved/cancelled C")
	default:
	}
	_, _ = s.cancelDelegateRequest(running.AgentID, next.CorrelationID)
}

func TestDelegateTerminalWinsCancelledWaitBeforeControlDispatch(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "B done"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	request, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "B", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := s.delegation.getPending(request.CorrelationID)
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("B did not reach terminal barrier")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := waitResponse(cancelled, ctrl, s, request.AgentID, request.CorrelationID, nil)
	if err != nil || result.ResponseStatus != tool.DelegateResponseCompleted || result.Response != "B done" {
		t.Fatalf("terminal/deadline race = %+v, %v; want completed B", result, err)
	}
	next, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: request.AgentID, Message: "C", WaitForResponse: true})
	if err != nil || next.ResponseStatus != tool.DelegateResponseCompleted {
		t.Fatalf("C after terminal race = %+v, %v", next, err)
	}
}

func TestNativeQueuedDelegateInterruptResolvesWithoutWaitTimeout(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateMechanicalStatus(t, ctrl, active.AgentID, tool.DelegateStatusRunning)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: active.AgentID, Message: "B", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: active.AgentID}); err != nil {
		t.Fatal(err)
	}
	result := make(chan tool.DelegateResult, 1)
	errCh := make(chan error, 1)
	go func() {
		got, waitErr := waitResponse(context.Background(), ctrl, s, active.AgentID, queued.CorrelationID, nil)
		result <- got
		errCh <- waitErr
	}()
	select {
	case got := <-result:
		if waitErr := <-errCh; waitErr != nil || got.ResponseStatus != tool.DelegateResponseInterrupted {
			t.Fatalf("wait = %+v, %v; want Interrupted", got, waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background wait depended on a context timeout after queued cancellation")
	}
}

func TestNativeQueuedDelegateFailedTurnResolvesFailedLiveAndRestore(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("injected active-turn failure")
	client := &releasedFailureLLM{started: make(chan struct{}), release: make(chan struct{}), err: providerErr}
	parent := delegateParent(loop.DelegationManaged, "child")
	child := mustDefine(
		loop.WithName("child"),
		loop.WithInference(client, validModel("child")),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
	events := &recordingEventAppender{}
	commands := &fakeCommandAppender{}
	s := newDelegationSession(t, parent, []Option{WithEventAppender(events), WithCommandAppender(commands)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("active turn never reached injected provider")
	}
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: active.AgentID, Message: "B", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	close(client.release)

	live, err := waitResponse(context.Background(), ctrl, s, active.AgentID, queued.CorrelationID, nil)
	if err != nil || live.ResponseStatus != tool.DelegateResponseFailed {
		t.Fatalf("live wait = %+v, %v; want Failed", live, err)
	}

	records := commands.snapshot()
	journalRecords := make([]journal.JournalRecord, len(records))
	for i := range records {
		journalRecords[i] = records[i]
	}
	restored := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(restored, journalRecords, events.snapshot(), nil); err != nil {
		t.Fatal(err)
	}
	resolved, ok := restored.getResolved(queued.CorrelationID)
	if !ok || resolved.childID != active.AgentID || resolved.status != tool.DelegateStatusFailed {
		t.Fatalf("restored cancellation = %+v, %v; want Failed child %v", resolved, ok, active.AgentID)
	}
}

func waitDelegateMechanicalStatus(t *testing.T, ctrl tool.DelegateController, delegateID uuid.UUID, want tool.DelegateStatusValue) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		status, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: delegateID})
		if err != nil {
			t.Fatal(err)
		}
		if len(status.Agents) == 1 && agentStateMatchesLegacy(status.Agents[0].State, want) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("delegate agents = %+v, want legacy state %v", status.Agents, want)
		case <-time.After(time.Millisecond):
		}
	}
}

func waitDelegateQueuedMessages(t *testing.T, ctrl tool.DelegateController, delegateID uuid.UUID, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		status, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: delegateID})
		if err != nil {
			t.Fatal(err)
		}
		if len(status.Agents) == 1 && status.Agents[0].QueuedMessages == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("delegate agents = %+v, want queued_messages %d", status.Agents, want)
		case <-time.After(time.Millisecond):
		}
	}
}

func agentStateMatchesLegacy(state tool.AgentState, status tool.DelegateStatusValue) bool {
	if status == tool.DelegateStatusRunning {
		return state == tool.AgentStateWorking
	}
	if status == tool.DelegateStatusIdle {
		return state == tool.AgentStateIdle
	}
	return state == tool.AgentStateUnavailable
}

// TestDelegateStatusReportsMechanicalState proves status returns bounded mechanical
// state + pending counts for a single owned child and for all owned children.
func TestDelegateStatusReportsMechanicalState(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "final"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Name: "api_planner", Message: "go", WaitForResponse: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	single, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: res.AgentID})
	if err != nil {
		t.Fatalf("status one: %v", err)
	}
	if len(single.Agents) != 1 || single.Agents[0].State != tool.AgentStateIdle || single.Agents[0].QueuedMessages != 0 {
		t.Errorf("single agents = %+v, want one idle child with no queued messages", single.Agents)
	}
	if single.Agents[0].Name != "api_planner" || single.Agents[0].AgentType != "child" {
		t.Errorf("single agent identity = %+v, want durable api_planner/child", single.Agents[0])
	}

	all, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus})
	if err != nil {
		t.Fatalf("status all: %v", err)
	}
	if len(all.Agents) != 1 || all.Agents[0].AgentID != res.AgentID {
		t.Errorf("agents = %+v, want exactly the one owned child", all.Agents)
	}
}

func TestDelegateStatusReportsWaitTrueChildRunning(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	startCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ctrl.Execute(startCtx, tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if len(status.Agents) == 1 {
			if status.Agents[0].State != tool.AgentStateWorking {
				t.Fatalf("active wait:true child state = %v, want working", status.Agents[0].State)
			}
			cancel()
			<-done
			return
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("child was never registered")
}

func TestDelegateStartSetupFailuresLeaveNoChildQuotaOrDurablePhantom(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("injected delegate setup failure")
	tests := []struct {
		name   string
		inject func(*Session)
	}{
		{name: "subscribe failure", inject: func(s *Session) {
			s.delegateSubscribe = func(event.EventFilter) (event.Subscription, error) { return nil, sentinel }
		}},
		{name: "initial enqueue failure", inject: func(s *Session) {
			s.delegateEnqueue = func(context.Context, loop.Backend, command.UserInput) error { return sentinel }
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingEventAppender{}
			parent := delegateParent(loop.DelegationManaged, "child")
			s := newDelegationSession(t, parent, []Option{WithEventAppender(rec)}, delegateChild("child", "answer"))
			tt.inject(s)
			beforeQuota := s.spawnedCount()
			ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent).(*scopedController)
			beforeLoops := len(ctrl.ownedChildren(s))
			_, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
			if !errors.Is(err, sentinel) {
				t.Fatalf("start error = %v, want injected sentinel", err)
			}
			if got := s.spawnedCount(); got != beforeQuota {
				t.Fatalf("spawned quota = %d, want rolled back %d", got, beforeQuota)
			}
			if got := len(ctrl.ownedChildren(s)); got != beforeLoops {
				t.Fatalf("owned children = %d, want %d", got, beforeLoops)
			}
			for _, ev := range rec.snapshot() {
				if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates.LoopID == s.ActiveLoopID() {
					t.Fatalf("failed spawn durably published child LoopStarted: %+v", started)
				}
			}
		})
	}
}

func TestDelegateStartCommitsLoopStartedBeforeTurnEvents(t *testing.T) {
	t.Parallel()
	rec := &recordingEventAppender{}
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, []Option{WithEventAppender(rec)}, delegateChild("child", "answer"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events := rec.snapshot()
		started, turn := -1, -1
		for i, ev := range events {
			switch e := ev.(type) {
			case event.LoopStarted:
				if e.LoopID == queued.AgentID {
					started = i
				}
			case event.TurnStarted:
				if e.LoopID == queued.AgentID {
					turn = i
				}
			}
		}
		if turn >= 0 {
			if started < 0 || started >= turn {
				t.Fatalf("event order started=%d turn=%d: %#v", started, turn, events)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("child TurnStarted not observed")
}

func TestDelegateStartAppendFailureRollsBackPreparedChild(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("child LoopStarted append failed")
	parent := delegateParent(loop.DelegationManaged, "child")
	appender := &failChildStartAppender{err: sentinel}
	s := newDelegationSession(t, parent, []Option{WithEventAppender(appender)}, delegateChild("child", "answer"))
	// The root LoopStarted has already committed. Fail exactly the next child creation
	// commit without replacing the live session hub beneath running loop publishers.
	appender.enabled.Store(true)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent).(*scopedController)
	beforeQuota := s.spawnedCount()
	_, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	if !errors.Is(err, sentinel) {
		t.Fatalf("start error = %v, want append sentinel", err)
	}
	if s.spawnedCount() != beforeQuota || len(ctrl.ownedChildren(s)) != 0 {
		t.Fatalf("failed durable commit left quota=%d children=%d", s.spawnedCount(), len(ctrl.ownedChildren(s)))
	}
}

func TestDelegateRequiredIntentAppendFailureDoesNotDispatch(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("delegate intent append failed")
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "answer"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent).(*scopedController)
	failing := &fakeCommandAppender{err: sentinel}
	s.cmdAppender = failing
	before := s.spawnedCount()
	_, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionDelegateIntentAppendFailed || !errors.Is(err, sentinel) {
		t.Fatalf("start error = %T %v, want typed required-intent failure", err, err)
	}
	if s.spawnedCount() != before || len(ctrl.ownedChildren(s)) != 0 {
		t.Fatalf("failed start left quota=%d children=%d", s.spawnedCount(), len(ctrl.ownedChildren(s)))
	}

	s.cmdAppender = &fakeCommandAppender{}
	started, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	s.cmdAppender = failing
	_, err = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: started.AgentID, Message: "queued", WaitForResponse: false})
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionDelegateIntentAppendFailed || !errors.Is(err, sentinel) {
		t.Fatalf("send error = %T %v, want typed required-intent failure", err, err)
	}
	if got := s.delegation.pendingCount(started.AgentID); got != 0 {
		t.Fatalf("failed send pending count = %d, want 0", got)
	}
}

func TestDelegateAcceptanceAppendFailureReturnsNoHandle(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("delegate acceptance append failed")
	parent := delegateParent(loop.DelegationManaged, "child")
	appender := &failDelegateAcceptanceAppender{err: sentinel}
	s := newDelegationSession(t, parent, []Option{WithEventAppender(appender)}, delegateChild("child", "answer"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	started, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	appender.enabled.Store(true)
	result, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: started.AgentID, Message: "B", WaitForResponse: false})
	var sessionErr *SessionError
	if err == nil || !result.CorrelationID.IsZero() || !errors.Is(err, sentinel) || !errors.As(err, &sessionErr) || sessionErr.Kind != SessionDelegateAdmissionCommitFailed {
		t.Fatalf("send = %+v, %v; want no handle and acceptance failure", result, err)
	}
	if got := s.delegation.pendingCount(started.AgentID); got != 0 {
		t.Fatalf("pending=%d, want 0", got)
	}
}

// TestDelegateQuotaReservedBeforeConstruction proves the cumulative spawn quota is
// enforced by the shared NewLoop reservation (before the child is constructed), and that
// a pre-spawn refusal (invalid mode) does not consume a quota slot.
func TestDelegateQuotaReservedBeforeConstruction(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, []Option{WithLimits(Limits{Depth: 3, Quota: 1})}, delegateChildWithModes("child", "final"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	// An invalid mode is refused BEFORE reserving quota.
	if _, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", AgentMode: "ghost", Message: "m", WaitForResponse: true}); err == nil {
		t.Fatal("expected an invalid-mode refusal")
	}

	// The first real spawn consumes the sole quota slot.
	if _, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "m", WaitForResponse: true}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// The second exceeds the quota.
	_, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "m", WaitForResponse: true})
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopQuotaExceeded {
		t.Fatalf("second start error = %v, want SessionLoopQuotaExceeded", err)
	}
}

func (s *Session) spawnedCount() int {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	return s.spawned
}

// waitTurnDoneOnLoop reads the observer until a TurnDone for loopID arrives (the child's
// turn completed and durably persisted) or the deadline elapses.
func waitTurnDoneOnLoop(t *testing.T, sub interface {
	Events() <-chan event.Delivery
}, loopID [16]byte) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				return false
			}
			if td, ok := d.Event.(event.TurnDone); ok && td.Coordinates.LoopID == loopID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestDelegateSendResolvesDistinctTurns proves each `send` produces its OWN distinct,
// request-correlated turn on an owned child (never a fold): two sequential sends each
// resolve their own answer with a distinct request id. The non-folding guarantee at a
// live tool-continuation boundary is proven at the loop-actor level by
// TestNonFoldingInputStartsOwnTurn.
func TestDelegateSendResolvesDistinctTurns(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "answer"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	start, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	childID := start.AgentID

	seen := map[uuid.UUID]struct{}{start.CorrelationID: {}}
	for i := 0; i < 2; i++ {
		res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: childID, Message: "again", WaitForResponse: true})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if res.ResponseStatus != tool.DelegateResponseCompleted || res.Response != "answer" {
			t.Fatalf("send %d = %v/%q, want Completed/answer", i, res.ResponseStatus, res.Response)
		}
		if _, dup := seen[res.CorrelationID]; dup || res.CorrelationID.IsZero() {
			t.Fatalf("send %d request id %v not distinct", i, res.CorrelationID)
		}
		seen[res.CorrelationID] = struct{}{}
	}
}

// TestDelegateWaitResolvesAfterRestore proves the plan Step-1 requirement that a
// wait:false request resolves via a later wait INCLUDING after restore: the in-memory
// pending handle does not survive the restart, but restore reconstructs the durable
// request→terminal index so the same request id resolves the same answer.
func TestDelegateWaitResolvesAfterRestore(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "durable answer")
	topo := Topology{Definitions: []loop.Definition{parent, child}, Primers: []identity.AgentName{parent.Name()}, ActivePrimer: parent.Name()}
	lc, err := NewTopologyLifecycle(topo, store, WithLifecycleFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatalf("NewTopologyLifecycle: %v", err)
	}

	ctx := delegateCtx(t)
	s, err := lc.NewSession(ctx, "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Observe all loops BEFORE the spawn so the child's TurnDone (no hub replay) is caught.
	obs, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = obs.Close() }()

	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	queued, err := ctrl.Execute(ctx, tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	if err != nil {
		t.Fatalf("start wait:false: %v", err)
	}
	childID, reqID := queued.AgentID, queued.CorrelationID

	// Wait until the child's turn is durably done before shutdown (so its terminal is on
	// the durable stream the restore reads).
	if !waitTurnDoneOnLoop(t, obs, childID) {
		t.Fatal("child turn never completed before shutdown")
	}
	sid := s.SessionID()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Restore: the in-memory pending map is empty; wait must resolve from the durable index.
	r, err := lc.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	rctrl := r.delegation.controllerFor(r.ActiveLoopID(), parent)
	res, err := waitResponse(context.Background(), rctrl, r, childID, reqID, nil)
	if err != nil {
		t.Fatalf("wait after restore: %v", err)
	}
	if res.ResponseStatus != tool.DelegateResponseCompleted || res.Response != "durable answer" {
		t.Fatalf("post-restore wait = %v/%q, want Completed/durable answer", res.ResponseStatus, res.Response)
	}
}

func TestDelegateQueuedRequestRestoresInterruptedWithoutReplay(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateBlockingChild("child")
	topo := Topology{Definitions: []loop.Definition{parent, child}, Primers: []identity.AgentName{parent.Name()}, ActivePrimer: parent.Name()}
	lc, err := NewTopologyLifecycle(topo, store, WithLifecycleFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatal(err)
	}
	s, err := lc.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	obs, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatal(err)
	}
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	a, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "A", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	if !waitTurnStartedRequest(t, obs, a.CorrelationID) {
		t.Fatal("turn A never started")
	}
	b, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: a.AgentID, Message: "B", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	if !waitInputQueuedRequest(t, obs, b.CorrelationID) {
		t.Fatal("request B never durably queued")
	}
	sid := s.SessionID()
	s.sessionCancel() // crash: no graceful queue flush or shutdown command
	// A real crash kills the process, so no predecessor writer survives to race the
	// successor; graceful Shutdown likewise awaits every loop actor before releasing the
	// lease. This in-process crash sim must do the same: wait for the cancelled loop actors
	// to fully unwind (their teardown terminal/InputCancelled appends land under the still-held
	// lease) BEFORE releasing it and restoring, so no live predecessor append races restore's
	// opening LeaseFence on the shared stream. Without this barrier the sim models a scenario
	// (successor restoring while a predecessor actor still writes) that neither a real crash nor
	// a graceful handoff ever produces.
	waitLoopsExited(t, s)
	s.releaseLease(context.Background())
	_ = obs.Close()

	restored, err := lc.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	restoredCtrl := restored.delegation.controllerFor(restored.ActiveLoopID(), parent)
	result, err := waitResponse(context.Background(), restoredCtrl, restored, a.AgentID, b.CorrelationID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResponseStatus != tool.DelegateResponseInterrupted {
		t.Fatalf("restored queued request B status = %v, want Interrupted", result.ResponseStatus)
	}
	status, err := restoredCtrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: a.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Agents) != 1 || status.Agents[0].State != tool.AgentStateIdle {
		t.Fatalf("restored child agents = %+v, want idle (B not replayed)", status.Agents)
	}
}

// waitLoopsExited blocks until every registered loop actor of s has fully unwound (its
// backend DoneChan closed). The loop actor publishes its cancel-path terminal and returns
// its queued inbox BEFORE close(cfg.done), so once DoneChan is closed no further
// loop-actor append can land. It is the deterministic (sleep-free) barrier a crash
// simulation needs so the predecessor's writers are quiesced before a successor restore
// acquires the shared stream.
func waitLoopsExited(t *testing.T, s *Session) {
	t.Helper()
	s.loopsMu.RLock()
	dones := make([]<-chan struct{}, 0, len(s.loops))
	for _, h := range s.loops {
		dones = append(dones, h.backend.DoneChan())
	}
	s.loopsMu.RUnlock()
	for _, done := range dones {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("loop actor did not exit after crash cancel")
		}
	}
}

func waitTurnStartedRequest(t *testing.T, sub interface{ Events() <-chan event.Delivery }, requestID uuid.UUID) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case delivery, ok := <-sub.Events():
			if !ok {
				return false
			}
			if started, ok := delivery.Event.(event.TurnStarted); ok && started.Cause.CommandID == requestID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func waitInputQueuedRequest(t *testing.T, sub interface{ Events() <-chan event.Delivery }, requestID uuid.UUID) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	accepted := false
	for {
		select {
		case delivery, ok := <-sub.Events():
			if !ok {
				return false
			}
			if eventAccepted, ok := delivery.Event.(event.DelegateRequestAccepted); ok && eventAccepted.Cause.CommandID == requestID {
				accepted = true
			}
			if queued, ok := delivery.Event.(event.InputQueued); ok && queued.Cause.CommandID == requestID {
				return accepted
			}
		case <-deadline:
			return false
		}
	}
}
