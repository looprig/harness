package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

func testHustleLimits() HustleLimits {
	return HustleLimits{
		BlockingConcurrent: 1, BlockingQueued: 1,
		BackgroundConcurrent: 1, BackgroundQueued: 1,
		AuditTimeout: time.Second, FinalizationTimeout: time.Second,
		WorkerDrainTimeout: time.Second,
	}
}

func TestSessionHustleFinalizerContextPreservesTrustedContext(t *testing.T) {
	t.Parallel()
	type contextValueKey struct{}
	tests := []struct {
		name string
	}{
		{name: "deadline cancellation values and owner marker survive decoration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			session := &Session{}
			deadline := time.Now().Add(time.Minute)
			root := context.WithValue(context.Background(), contextValueKey{}, "preserved")
			ctx, cancel := context.WithDeadline(root, deadline)
			decorated := (sessionHustleFinalizerContext{session: session}).DecorateFinalizerContext(ctx)
			gotDeadline, ok := decorated.Deadline()
			if !ok || !gotDeadline.Equal(deadline) {
				t.Fatalf("decorated deadline = (%v,%t), want %v,true", gotDeadline, ok, deadline)
			}
			if got := decorated.Value(contextValueKey{}); got != "preserved" {
				t.Fatalf("decorated value = %v, want preserved", got)
			}
			if !hustleFinalizerOwnsSession(decorated, session) {
				t.Fatal("decorated context lacks exact session owner marker")
			}
			cancel()
			if !errors.Is(decorated.Err(), context.Canceled) {
				t.Fatalf("decorated context error = %v, want cancellation", decorated.Err())
			}
		})
	}
}

func testHustleDefinition(t *testing.T, name hustle.Name) hustle.Definition {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName(name),
		hustle.WithParticipation(hustle.ParticipationBackground),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 1024}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("summarize", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	return definition
}

func TestSessionHustleModelResolverUsesExactLiveLoop(t *testing.T) {
	t.Parallel()
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	missingID, _ := uuid.New()
	client := &stubLLM{}
	definition := cfg(client)
	bound := bindCfg(definition, sessionID, loopID)
	done := make(chan struct{})
	backend := &channelBackend{Commands: make(chan command.Command), Done: done}
	s := &Session{sessionID: sessionID, loops: make(map[uuid.UUID]*loopHandle)}
	handle := &loopHandle{
		id: loopID, owner: s, bound: bound, backend: backend,
		liveModel: validModel("initial"),
	}
	s.loops[loopID] = handle
	resolver := sessionHustleModelResolver{session: s}

	tests := []struct {
		name       string
		ctx        context.Context
		loopID     uuid.UUID
		prepare    func()
		wantModel  string
		wantReason HustleModelResolveReason
	}{
		{name: "exact loop returns current model", ctx: context.Background(), loopID: loopID, wantModel: "initial"},
		{name: "committed model change is visible", ctx: context.Background(), loopID: loopID, prepare: func() {
			handle.setLiveView("", validModel("changed"))
		}, wantModel: "changed"},
		{name: "nil context is rejected", loopID: loopID, wantReason: HustleModelResolveInvalidContext},
		{name: "zero loop id is rejected", ctx: context.Background(), wantReason: HustleModelResolveInvalidLoopID},
		{name: "missing loop is rejected", ctx: context.Background(), loopID: missingID, wantReason: HustleModelResolveLoopNotFound},
		{name: "foreign registry entry is rejected", ctx: context.Background(), loopID: loopID, prepare: func() {
			handle.owner = &Session{}
		}, wantReason: HustleModelResolveForeignLoop},
		{name: "exited loop is rejected", ctx: context.Background(), loopID: loopID, prepare: func() {
			handle.owner = s
			close(done)
		}, wantReason: HustleModelResolveLoopExited},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepare != nil {
				tt.prepare()
			}
			binding, err := resolver.ResolveHustleModel(tt.ctx, tt.loopID)
			if tt.wantReason != "" {
				var target *HustleModelResolveError
				if !errors.As(err, &target) || target.Reason != tt.wantReason {
					t.Fatalf("ResolveHustleModel error = %T %v, want reason %q", err, err, tt.wantReason)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveHustleModel: %v", err)
			}
			if binding.Client != client {
				t.Fatal("ResolveHustleModel returned a different inference client")
			}
			if got := binding.Model.Name; got != tt.wantModel {
				t.Fatalf("resolved model = %q, want %q", got, tt.wantModel)
			}
			binding.Model.Name = "caller-mutated"
			if got := handle.Model().Name; got != tt.wantModel {
				t.Fatalf("caller mutation changed live model to %q", got)
			}
		})
	}
}

func TestBindSessionHustlesIsSingleTransactionalConstruction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		definitions []hustle.Definition
		nilFactory  bool
		nilHub      bool
		wantReason  HustleConstructionReason
	}{
		{name: "valid definitions build one controller", definitions: []hustle.Definition{testHustleDefinition(t, "compact")}},
		{name: "invalid definition fails binding", definitions: []hustle.Definition{{}}, wantReason: HustleConstructionBindFailed},
		{name: "missing factory fails closed", definitions: []hustle.Definition{testHustleDefinition(t, "factory")}, nilFactory: true, wantReason: HustleConstructionMissingCollaborator},
		{name: "missing hub fails closed", definitions: []hustle.Definition{testHustleDefinition(t, "hub")}, nilHub: true, wantReason: HustleConstructionMissingCollaborator},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionID, _ := uuid.New()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			factory := event.NewFactory(uuid.New, time.Now)
			s := &Session{
				sessionID: sessionID, sessionCtx: ctx, sessionCancel: cancel,
				factory: factory, hustleDefinitions: append([]hustle.Definition(nil), tt.definitions...),
				hustleLimits: testHustleLimits(), loops: make(map[uuid.UUID]*loopHandle),
			}
			if !tt.nilHub {
				s.hub = hub.New(sessionID, hub.WithFactory(factory))
			}
			if tt.nilFactory {
				s.factory = nil
			}
			err := s.bindSessionHustles()
			if tt.wantReason != "" {
				var target *HustleConstructionError
				if !errors.As(err, &target) || target.Reason != tt.wantReason {
					t.Fatalf("bindSessionHustles error = %T %v, want reason %q", err, err, tt.wantReason)
				}
				if s.hustleController != nil {
					t.Fatal("failed construction retained a partial controller")
				}
				return
			}
			if err != nil {
				t.Fatalf("bindSessionHustles: %v", err)
			}
			if s.hustleController == nil {
				t.Fatal("bindSessionHustles did not construct the controller")
			}
			err = s.bindSessionHustles()
			var target *HustleConstructionError
			if !errors.As(err, &target) || target.Reason != HustleConstructionAlreadyBound {
				t.Fatalf("second bind error = %T %v, want already-bound", err, err)
			}
		})
	}
}

type sessionEvidenceTool struct {
	infos []*tool.ToolInfo
	calls int
}

func (t *sessionEvidenceTool) Info(context.Context) (*tool.ToolInfo, error) {
	index := t.calls
	if index >= len(t.infos) {
		index = len(t.infos) - 1
	}
	t.calls++
	info := *t.infos[index]
	info.Schema = append(json.RawMessage(nil), info.Schema...)
	return &info, nil
}

func (*sessionEvidenceTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

type sessionEvidenceCoordinator struct{}

func (sessionEvidenceCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return sessionEvidencePermit{}, nil
}
func (sessionEvidenceCoordinator) Healthy() error { return nil }

type sessionEvidencePermit struct{}

func (sessionEvidencePermit) Release() {}

func sessionEvidenceDefinition(
	t *testing.T,
	factory tool.Factory,
) hustle.Definition {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName("permission-review"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 4096, OutputBytes: 4096}),
		hustle.WithNamedInference(&stubLLM{}, validModel("reviewer")),
		hustle.WithSystemPrompt("review", "prompt-v1"),
		hustle.WithPolicyRevision("review-v1"),
		hustle.WithOutputSchema(inference.OutputSchema{
			Name: "assessment",
			Schema: json.RawMessage(`{
				"type":"object",
				"properties":{"recommendation":{"type":"string"}},
				"required":["recommendation"],
				"additionalProperties":false
			}`),
			Strict: true,
		}),
		hustle.WithEvidenceTools(hustle.EvidenceToolPolicy{
			Revision: "evidence-v1",
			Limits: hustle.ToolLoopLimits{
				MaxRounds: 1, MaxCalls: 1, MaxCallsPerRound: 1,
				MaxResultBytes: 1024, MaxEvidenceBytes: 1024,
			},
			Definitions: []tool.Definition{
				tool.NewDefinition("workspace-status", tool.RequiresWorkspace, factory),
			},
		}),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return definition
}

func TestSessionBindsEvidenceToolsWithOriginAndAttenuatedWorkspace(t *testing.T) {
	t.Parallel()
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	workspaceRoot := "/managed/workspace"
	concrete := &sessionEvidenceTool{infos: []*tool.ToolInfo{{
		Name: "workspace-status", Desc: "read workspace status",
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}}}
	builds := 0
	definition := sessionEvidenceDefinition(t, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		builds++
		if bindings.SessionID != sessionID || bindings.LoopID != loopID {
			t.Fatalf("tool bindings IDs = %v/%v, want %v/%v", bindings.SessionID, bindings.LoopID, sessionID, loopID)
		}
		if bindings.Workspace == nil || bindings.Workspace.Root != workspaceRoot {
			t.Fatalf("workspace binding = %#v", bindings.Workspace)
		}
		if bindings.Delegate != nil || bindings.ExtraTools != nil {
			t.Fatalf("evidence binding was not attenuated: %#v", bindings)
		}
		return []tool.InvokableTool{concrete}, nil
	})
	s := &Session{
		sessionID: sessionID, activeLoopID: loopID,
		sessionCtx: context.Background(), hustleDefinitions: []hustle.Definition{definition},
		wsRoot: workspaceRoot, wsCoordinator: sessionEvidenceCoordinator{},
	}
	bound, err := s.bindHustleDefinitions()
	if err != nil {
		t.Fatalf("bindHustleDefinitions() error = %v", err)
	}
	if builds != 1 || len(bound) != 1 || len(bound[0].EvidenceTools()) != 1 {
		t.Fatalf("bound evidence = builds:%d definitions:%d tools:%d", builds, len(bound), len(bound[0].EvidenceTools()))
	}
}

func TestSessionConstructionRejectsEvidenceSchemaDriftBeforeRun(t *testing.T) {
	t.Parallel()
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	// tool.Definition.Build consumes the first entry before the Hustle binder's
	// two-read drift check.
	concrete := &sessionEvidenceTool{infos: []*tool.ToolInfo{
		{Name: "workspace-status", Desc: "read workspace", Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "workspace-status", Desc: "read workspace", Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "workspace-status", Desc: "read workspace", Schema: json.RawMessage(`{"type":"object","properties":{"changed":{"type":"boolean"}},"required":["changed"],"additionalProperties":false}`)},
	}}
	definition := sessionEvidenceDefinition(t, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{concrete}, nil
	})
	factory := event.NewFactory(uuid.New, time.Now)
	s := &Session{
		sessionID: sessionID, activeLoopID: loopID,
		sessionCtx: context.Background(), hustleDefinitions: []hustle.Definition{definition},
		hustleLimits: testHustleLimits(), factory: factory,
		hub:    hub.New(sessionID, hub.WithFactory(factory)),
		wsRoot: "/workspace", wsCoordinator: sessionEvidenceCoordinator{},
	}
	err := s.bindSessionHustles()
	var construction *HustleConstructionError
	if !errors.As(err, &construction) || construction.Reason != HustleConstructionBindFailed {
		t.Fatalf("bindSessionHustles() error = %T %v, want bind failure", err, err)
	}
	if s.hustleController != nil {
		t.Fatal("schema drift retained a runnable controller")
	}
}

func TestLifecycleHustleBindingFailureReleasesConstructionOwnership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		restore bool
	}{
		{name: "new session abort releases lease"},
		{name: "restore abort releases lease", restore: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, leaser := newRecordingStore(t)
			definition := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful")
			var sessionID uuid.UUID
			if tt.restore {
				sessionID = runAndShutdown(t, store, definition)
			}
			lifecycle, err := newTestLifecycle(definition, store, WithLifecycleHustles([]hustle.Definition{{}}, testHustleLimits()))
			if err != nil {
				t.Fatalf("NewTopologyLifecycle: %v", err)
			}
			var got *Session
			if tt.restore {
				got, err = lifecycle.RestoreSession(context.Background(), sessionID)
			} else {
				got, err = lifecycle.NewSession(context.Background(), "")
			}
			if got != nil {
				t.Fatal("construction returned a session after hustle bind failure")
			}
			var construction *HustleConstructionError
			if !errors.As(err, &construction) || construction.Reason != HustleConstructionBindFailed {
				t.Fatalf("construction error = %T %v, want hustle bind failure", err, err)
			}
			if !leaser.balanced() {
				t.Fatalf("construction ownership leaked: acquired=%d released=%d", leaser.acquired, leaser.released)
			}
		})
	}
}

func TestLifecycleBindsHustlesBeforeReturning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		restore bool
	}{
		{name: "new session returns with bound controller"},
		{name: "restored session uses current frozen definitions", restore: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			definition := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful")
			var sessionID uuid.UUID
			if tt.restore {
				sessionID = runAndShutdown(t, store, definition)
			}
			lifecycle, err := newTestLifecycle(
				definition, store,
				WithLifecycleHustles([]hustle.Definition{testHustleDefinition(t, "compact")}, testHustleLimits()),
			)
			if err != nil {
				t.Fatalf("NewTopologyLifecycle: %v", err)
			}
			var got *Session
			if tt.restore {
				got, err = lifecycle.RestoreSession(context.Background(), sessionID)
			} else {
				got, err = lifecycle.NewSession(context.Background(), "")
			}
			if err != nil {
				t.Fatalf("session construction: %v", err)
			}
			t.Cleanup(func() { _ = got.Shutdown(context.Background()) })
			if !got.hustlesBound || got.hustleController == nil {
				t.Fatal("reachable session does not own its bound hustle controller")
			}
		})
	}
}
