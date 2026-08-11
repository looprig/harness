//go:build integration

package sessionruntime_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

const processIntegrationIdentity = "process-integration-owner-v1"

var processIntegrationDefinitionNames = []string{
	"ProcessOutput",
	"ProcessInput",
	"ProcessStop",
	"Bash",
}

type processIntegrationLLM struct{}

func (processIntegrationLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("process integration: Invoke is unused")
}

func (processIntegrationLLM) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("process integration: Stream is unused")
}

type processIntegrationTool struct {
	name string
}

func (t processIntegrationTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: t.name}, nil
}

func (processIntegrationTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("unused"), nil
}

type processIntegrationStorageCall struct {
	sessionID uuid.UUID
	path      string
	identity  string
}

type processIntegrationStorageProvider struct {
	baseDir string

	mu    sync.Mutex
	calls []processIntegrationStorageCall
}

func (p *processIntegrationStorageProvider) StorageForSession(
	_ context.Context,
	sessionID uuid.UUID,
) (rig.SessionResourceStorage, error) {
	path := filepath.Join(p.baseDir, sessionID.String())
	call := processIntegrationStorageCall{
		sessionID: sessionID,
		path:      path,
		identity:  processIntegrationIdentity,
	}
	p.mu.Lock()
	p.calls = append(p.calls, call)
	p.mu.Unlock()
	return rig.SessionResourceStorage{Path: path, Identity: call.identity}, nil
}

func (p *processIntegrationStorageProvider) snapshot() []processIntegrationStorageCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]processIntegrationStorageCall(nil), p.calls...)
}

type processIntegrationResource struct {
	capture    *processIntegrationCapture
	generation int
	name       string
	path       string
	services   tool.SessionResourceServices
}

func (r *processIntegrationResource) Activate(
	_ context.Context,
	services tool.SessionResourceServices,
) error {
	err := services.Validate()
	r.capture.mu.Lock()
	defer r.capture.mu.Unlock()
	construction := r.capture.constructions[r.generation]
	if construction == nil || construction.resources[r.name] == nil {
		return fmt.Errorf(
			"process integration: activation lost resource %q in construction %d",
			r.name,
			r.generation,
		)
	}
	resource := construction.resources[r.name]
	resource.activations++
	resource.activationErr = err
	resource.resource.services = services
	return err
}

func (r *processIntegrationResource) Shutdown(context.Context) error {
	r.capture.mu.Lock()
	defer r.capture.mu.Unlock()
	construction := r.capture.constructions[r.generation]
	if construction == nil || construction.resources[r.name] == nil {
		return fmt.Errorf(
			"process integration: shutdown lost resource %q in construction %d",
			r.name,
			r.generation,
		)
	}
	construction.resources[r.name].shutdowns++
	return nil
}

type processIntegrationResourceState struct {
	resource      *processIntegrationResource
	path          string
	activations   int
	shutdowns     int
	activationErr error
}

type processIntegrationConstruction struct {
	shared      *processIntegrationResource
	resources   map[string]*processIntegrationResourceState
	definitions map[string]int
	coordinator tool.WorkspaceLifetimeCoordinator
	workspace   string
}

type processIntegrationCapture struct {
	mu             sync.Mutex
	nextGeneration int
	constructions  map[int]*processIntegrationConstruction
}

func newProcessIntegrationCapture() *processIntegrationCapture {
	return &processIntegrationCapture{
		constructions: make(map[int]*processIntegrationConstruction),
	}
}

func (c *processIntegrationCapture) createShared(path string) tool.SessionResource {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextGeneration++
	generation := c.nextGeneration
	resource := &processIntegrationResource{
		capture:    c,
		generation: generation,
		name:       processIntegrationDefinitionNames[0],
		path:       path,
	}
	c.constructions[generation] = &processIntegrationConstruction{
		shared: resource,
		resources: map[string]*processIntegrationResourceState{
			resource.name: {
				resource: resource,
				path:     path,
			},
		},
		definitions: make(map[string]int),
	}
	return resource
}

func (c *processIntegrationCapture) createResource(
	generation int,
	name string,
	path string,
) (tool.SessionResource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	construction := c.constructions[generation]
	if construction == nil {
		return nil, fmt.Errorf("process integration: missing construction %d", generation)
	}
	if _, exists := construction.resources[name]; exists {
		return nil, fmt.Errorf("process integration: duplicate resource %q", name)
	}
	resource := &processIntegrationResource{
		capture:    c,
		generation: generation,
		name:       name,
		path:       path,
	}
	construction.resources[name] = &processIntegrationResourceState{
		resource: resource,
		path:     path,
	}
	return resource, nil
}

func (c *processIntegrationCapture) observeDefinition(
	generation int,
	name string,
	shared *processIntegrationResource,
	coordinator tool.WorkspaceLifetimeCoordinator,
	workspace string,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	construction := c.constructions[generation]
	if construction == nil {
		return fmt.Errorf("process integration: missing construction %d", generation)
	}
	if construction.shared != shared {
		return fmt.Errorf("process integration: definition %q received a different registry resource", name)
	}
	construction.definitions[name]++
	if construction.coordinator == nil {
		construction.coordinator = coordinator
	}
	if construction.workspace == "" {
		construction.workspace = workspace
	}
	return nil
}

func (c *processIntegrationCapture) lifetimeBindings(
	generation int,
) (tool.WorkspaceLifetimeCoordinator, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	construction := c.constructions[generation]
	if construction == nil || construction.coordinator == nil {
		return nil, "", fmt.Errorf("process integration: construction %d has no lifetime coordinator", generation)
	}
	if construction.workspace == "" {
		return nil, "", fmt.Errorf("process integration: construction %d has no workspace root", generation)
	}
	return construction.coordinator, construction.workspace, nil
}

func (c *processIntegrationCapture) assertActivated(t *testing.T, generation int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	construction := c.constructions[generation]
	if construction == nil {
		t.Fatalf("construction %d was not created", generation)
	}
	if len(construction.resources) != len(processIntegrationDefinitionNames) {
		t.Fatalf(
			"construction %d resources = %d, want %d",
			generation,
			len(construction.resources),
			len(processIntegrationDefinitionNames),
		)
	}
	for _, name := range processIntegrationDefinitionNames {
		if construction.definitions[name] == 0 {
			t.Errorf("construction %d definition %q did not observe the shared registry resource", generation, name)
		}
		state := construction.resources[name]
		if state == nil {
			t.Errorf("construction %d resource %q is missing", generation, name)
			continue
		}
		if state.activations != 1 || state.activationErr != nil {
			t.Errorf(
				"construction %d resource %q activation = (%d, %v), want (1, nil)",
				generation,
				name,
				state.activations,
				state.activationErr,
			)
		}
	}
}

func (c *processIntegrationCapture) assertStableFreshRestore(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.constructions) != 2 {
		t.Fatalf("live constructions = %d, want 2", len(c.constructions))
	}
	fresh := c.constructions[1]
	restored := c.constructions[2]
	if fresh == nil || restored == nil {
		t.Fatalf("construction generations = %v, want 1 and 2", c.constructions)
	}
	if fresh.shared == restored.shared {
		t.Fatal("new and restored live constructions reused the same registry resource instance")
	}
	for _, name := range processIntegrationDefinitionNames {
		newResource := fresh.resources[name]
		restoredResource := restored.resources[name]
		if newResource == nil || restoredResource == nil {
			t.Errorf("stable-path comparison missing resource %q", name)
			continue
		}
		if newResource.resource == restoredResource.resource {
			t.Errorf("resource %q instance was reused across live constructions", name)
		}
		if newResource.path != restoredResource.path {
			t.Errorf(
				"resource %q path changed across restore: %q != %q",
				name,
				newResource.path,
				restoredResource.path,
			)
		}
	}
}

func (c *processIntegrationCapture) assertShutdown(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for generation, construction := range c.constructions {
		for name, state := range construction.resources {
			if state.shutdowns != 1 {
				t.Errorf(
					"construction %d resource %q shutdowns = %d, want 1",
					generation,
					name,
					state.shutdowns,
				)
			}
		}
	}
}

func (c *processIntegrationCapture) workflowActivityPublisher(generation int) tool.WorkflowActivityPublisher {
	c.mu.Lock()
	defer c.mu.Unlock()
	construction := c.constructions[generation]
	if construction == nil {
		return nil
	}
	state := construction.resources[processIntegrationDefinitionNames[0]]
	if state == nil || state.resource == nil {
		return nil
	}
	return state.resource.services.WorkflowActivityPublisher()
}

func integrationWorkflowActivity(sessionID uuid.UUID) tool.WorkflowActivityMetadata {
	return tool.WorkflowActivityMetadata{
		EventID:         uuid.UUID{0x91},
		SessionID:       sessionID,
		RunID:           uuid.UUID{0x92},
		WorkflowName:    "source_document_extract",
		WorkflowVersion: "v1",
		Kind:            string(event.WorkflowActivityRunStarted),
		Status:          string(event.WorkflowRunStatusRunning),
		TotalVertices:   2,
		OccurredAt:      time.Date(2026, time.August, 10, 10, 0, 0, 0, time.UTC),
		Message:         "workflow started",
	}
}

func processIntegrationDefinition(
	name string,
	capture *processIntegrationCapture,
) tool.Definition {
	return tool.NewDefinition(
		name,
		tool.RequiresWorkspace|tool.RequiresProcessServices,
		func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
			if bindings.Process == nil || bindings.Process.Registry == nil {
				return nil, fmt.Errorf("process integration: %q missing process registry", name)
			}
			if bindings.Workspace == nil || bindings.Workspace.Coordinator == nil {
				return nil, fmt.Errorf("process integration: %q missing workspace coordinator", name)
			}
			lifetime, ok := bindings.Workspace.Coordinator.(tool.WorkspaceLifetimeCoordinator)
			if !ok {
				return nil, fmt.Errorf("process integration: %q missing lifetime coordinator", name)
			}

			sharedRaw, err := bindings.Process.Registry.GetOrCreate(
				ctx,
				"process-integration-"+processIntegrationDefinitionNames[0],
				func(path string) (tool.SessionResource, error) {
					return capture.createShared(path), nil
				},
			)
			if err != nil {
				return nil, err
			}
			shared, ok := sharedRaw.(*processIntegrationResource)
			if !ok {
				return nil, fmt.Errorf("process integration: %q received unexpected shared resource %T", name, sharedRaw)
			}
			if err := capture.observeDefinition(
				shared.generation,
				name,
				shared,
				lifetime,
				bindings.Workspace.Root,
			); err != nil {
				return nil, err
			}

			if name != processIntegrationDefinitionNames[0] {
				resourceRaw, err := bindings.Process.Registry.GetOrCreate(
					ctx,
					"process-integration-"+name,
					func(path string) (tool.SessionResource, error) {
						return capture.createResource(shared.generation, name, path)
					},
				)
				if err != nil {
					return nil, err
				}
				resource, ok := resourceRaw.(*processIntegrationResource)
				if !ok || resource.generation != shared.generation || resource.name != name {
					return nil, fmt.Errorf("process integration: %q received unexpected own resource %T", name, resourceRaw)
				}
			}
			return []tool.InvokableTool{processIntegrationTool{name: name}}, nil
		},
	)
}

type processIntegrationObservedContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
}

func (c *processIntegrationObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestProcessServicesIntegrationNewRestoreAndLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	capture := newProcessIntegrationCapture()
	definitions := make([]tool.Definition, 0, len(processIntegrationDefinitionNames))
	for _, name := range processIntegrationDefinitionNames {
		definitions = append(definitions, processIntegrationDefinition(name, capture))
	}
	loopDefinition, err := loop.Define(
		loop.WithName("process-integration"),
		loop.WithInference(
			processIntegrationLLM{},
			model.Model{
				Provider:  "test",
				APIFormat: model.APIFormatOpenAI,
				BaseURL:   "http://localhost",
				Name:      "process-integration",
			},
		),
		loop.WithTools(definitions...),
	)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}
	sessionDiskRoot := t.TempDir()
	sessionDisk, err := fsstore.Open(fsstore.Options{Root: sessionDiskRoot})
	if err != nil {
		t.Fatalf("fsstore.Open(session) error = %v", err)
	}
	t.Cleanup(func() { _ = sessionDisk.Close() })
	sessionStore, err := sessionstore.Open(sessionDisk.Backend())
	if err != nil {
		t.Fatalf("sessionstore.Open() error = %v", err)
	}
	workspaceDiskRoot := t.TempDir()
	workspaceDisk, err := fsstore.Open(fsstore.Options{Root: workspaceDiskRoot})
	if err != nil {
		t.Fatalf("fsstore.Open(workspace) error = %v", err)
	}
	t.Cleanup(func() { _ = workspaceDisk.Close() })
	workspaceStore, err := workspacestore.Open(workspaceDisk.Backend().Blobs)
	if err != nil {
		t.Fatalf("workspacestore.Open() error = %v", err)
	}
	workspaceRoot := t.TempDir()
	provider := &processIntegrationStorageProvider{baseDir: t.TempDir()}
	defined, err := rig.Define(
		rig.WithLoops(loopDefinition),
		rig.WithPrimers("process-integration"),
		rig.WithSessionStore(sessionStore),
		rig.WithSessionWorkspaces(workspaceStore, workspaceRoot),
		rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotManual}),
		rig.WithSessionResourceStorage(provider),
	)
	if err != nil {
		t.Fatalf("rig.Define() error = %v", err)
	}

	live, err := defined.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	liveShutdown := processIntegrationShutdownCleanup(t, live)
	sessionID := live.SessionID()
	capture.assertActivated(t, 1)
	activityPublisher := capture.workflowActivityPublisher(1)
	if activityPublisher == nil {
		t.Fatal("live workflow activity publisher = nil")
	}
	activitySubscription, err := live.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}
	defer activitySubscription.Close()
	activityMetadata := integrationWorkflowActivity(sessionID)
	if err := activityPublisher.PublishWorkflowActivity(ctx, activityMetadata); err != nil {
		t.Fatalf("live PublishWorkflowActivity() error = %v", err)
	}
	var firstActivity event.Delivery
	select {
	case firstActivity = <-activitySubscription.Events():
	case <-ctx.Done():
		t.Fatalf("timed out waiting for live WorkflowActivity: %v", ctx.Err())
	}
	if got, ok := firstActivity.Event.(event.WorkflowActivity); !ok || got.EventID != activityMetadata.EventID {
		t.Fatalf("live activity delivery = %#v, want WorkflowActivity %v", firstActivity.Event, activityMetadata.EventID)
	}
	if firstActivity.JournalSeq == 0 {
		t.Fatal("live activity delivery has zero journal sequence")
	}

	checkpoint, err := live.CheckpointWorkspace(ctx)
	if err != nil {
		t.Fatalf("CheckpointWorkspace() error = %v", err)
	}
	coordinator, workspaceRoot, err := capture.lifetimeBindings(1)
	if err != nil {
		t.Fatal(err)
	}
	scopedWritePath := filepath.Join(workspaceRoot, "process-integration-output.txt")
	lifetime, err := coordinator.AcquireLifetime(
		ctx,
		tool.NewWorkspaceAccess(tool.WorkspaceAccessScopedWrite, []string{scopedWritePath}, nil),
	)
	if err != nil {
		t.Fatalf("AcquireLifetime() error = %v", err)
	}
	defer lifetime.Release()

	restoreBase, restoreCancel := context.WithTimeout(ctx, 10*time.Second)
	defer restoreCancel()
	restoreContext := &processIntegrationObservedContext{
		Context:  restoreBase,
		observed: make(chan struct{}),
	}
	restoreResult := make(chan error, 1)
	go func() {
		restoreResult <- live.RestoreWorkspace(restoreContext, checkpoint)
	}()

	select {
	case <-restoreContext.observed:
	case err := <-restoreResult:
		t.Fatalf("RestoreWorkspace() completed before waiting on its checkpoint permit: %v", err)
	case <-ctx.Done():
		t.Fatalf("RestoreWorkspace() did not reach checkpoint-permit wait: %v", ctx.Err())
	}
	select {
	case err := <-restoreResult:
		t.Fatalf("RestoreWorkspace() completed while writable lifetime lease was held: %v", err)
	default:
	}
	lifetime.Release()
	select {
	case err := <-restoreResult:
		if err != nil {
			t.Fatalf("RestoreWorkspace() after lifetime release error = %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("RestoreWorkspace() did not complete after lifetime release: %v", ctx.Err())
	}

	liveShutdown()
	if err := sessionDisk.Close(); err != nil {
		t.Fatalf("close first session fsstore: %v", err)
	}
	if err := workspaceDisk.Close(); err != nil {
		t.Fatalf("close first workspace fsstore: %v", err)
	}
	sessionDisk2, err := fsstore.Open(fsstore.Options{Root: sessionDiskRoot})
	if err != nil {
		t.Fatalf("reopen session fsstore: %v", err)
	}
	t.Cleanup(func() { _ = sessionDisk2.Close() })
	sessionStore2, err := sessionstore.Open(sessionDisk2.Backend())
	if err != nil {
		t.Fatalf("reopen sessionstore: %v", err)
	}
	workspaceDisk2, err := fsstore.Open(fsstore.Options{Root: workspaceDiskRoot})
	if err != nil {
		t.Fatalf("reopen workspace fsstore: %v", err)
	}
	t.Cleanup(func() { _ = workspaceDisk2.Close() })
	workspaceStore2, err := workspacestore.Open(workspaceDisk2.Backend().Blobs)
	if err != nil {
		t.Fatalf("reopen workspacestore: %v", err)
	}
	defined2, err := rig.Define(
		rig.WithLoops(loopDefinition),
		rig.WithPrimers("process-integration"),
		rig.WithSessionStore(sessionStore2),
		rig.WithSessionWorkspaces(workspaceStore2, workspaceRoot),
		rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotManual}),
		rig.WithSessionResourceStorage(provider),
		rig.WithRestoreDecider(session.AcceptAllDecider{}),
	)
	if err != nil {
		t.Fatalf("restored rig.Define() error = %v", err)
	}
	restored, err := defined2.RestoreSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("RestoreSession() error = %v", err)
	}
	restoredShutdown := processIntegrationShutdownCleanup(t, restored)
	if restored.SessionID() != sessionID {
		t.Fatalf("restored SessionID = %v, want %v", restored.SessionID(), sessionID)
	}
	capture.assertActivated(t, 2)
	capture.assertStableFreshRestore(t)
	restoredActivityPublisher := capture.workflowActivityPublisher(2)
	if restoredActivityPublisher == nil {
		t.Fatal("restored workflow activity publisher = nil")
	}
	restoredSubscription, err := restored.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("restored SubscribeEvents() error = %v", err)
	}
	defer restoredSubscription.Close()
	if err := restoredActivityPublisher.PublishWorkflowActivity(ctx, activityMetadata); err != nil {
		t.Fatalf("restored duplicate PublishWorkflowActivity() error = %v", err)
	}
	select {
	case delivery := <-restoredSubscription.Events():
		t.Fatalf("restored duplicate delivered %T at sequence %d", delivery.Event, delivery.JournalSeq)
	case <-time.After(100 * time.Millisecond):
	}
	replayer, err := sessionStore2.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: firstActivity.JournalSeq})
	if err != nil {
		t.Fatalf("restored OpenEventReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: sessionID, From: journal.FromSeq(firstActivity.JournalSeq)})
	if err != nil {
		t.Fatalf("restored EventReplayer.Open() error = %v", err)
	}
	defer cursor.Close()
	workflowReplayCount := 0
	for {
		replayed, sequence, nextErr := cursor.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("restored replay Next() error = %v", nextErr)
		}
		got, ok := replayed.(event.WorkflowActivity)
		if !ok {
			continue
		}
		workflowReplayCount++
		if sequence != firstActivity.JournalSeq || got.EventID != activityMetadata.EventID || got.Message != activityMetadata.Message {
			t.Fatalf("restored replay activity = (%#v, %d), want event %v at sequence %d", got, sequence, activityMetadata.EventID, firstActivity.JournalSeq)
		}
	}
	if workflowReplayCount != 1 {
		t.Fatalf("restored replay WorkflowActivity count = %d, want 1", workflowReplayCount)
	}

	storageCalls := provider.snapshot()
	if len(storageCalls) != 2 {
		t.Fatalf("resource storage provider calls = %d, want 2", len(storageCalls))
	}
	wantStoragePath := filepath.Join(provider.baseDir, sessionID.String())
	for i, call := range storageCalls {
		if call.sessionID != sessionID || call.path != wantStoragePath || call.identity != processIntegrationIdentity {
			t.Errorf(
				"resource storage call[%d] = (%v, %q, %q), want (%v, %q, %q)",
				i,
				call.sessionID,
				call.path,
				call.identity,
				sessionID,
				wantStoragePath,
				processIntegrationIdentity,
			)
		}
	}

	restoredShutdown()
	capture.assertShutdown(t)
}

func processIntegrationShutdownCleanup(
	t *testing.T,
	controller session.SessionController,
) func() {
	t.Helper()
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := controller.Shutdown(ctx); err != nil {
				t.Errorf("Session.Shutdown() error = %v", err)
			}
		})
	}
	t.Cleanup(shutdown)
	return shutdown
}
