package sessionruntime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestSessionProcessServiceBridgeIsValidatedAndUnavailable(t *testing.T) {
	bridge, services, err := newSessionProcessServices()
	if err != nil {
		t.Fatalf("newSessionProcessServices() error = %v", err)
	}
	if bridge == nil {
		t.Fatal("newSessionProcessServices() bridge = nil")
	}
	if err := services.Validate(); err != nil {
		t.Fatalf("SessionResourceServices.Validate() error = %v", err)
	}
	if services.ProcessLifecyclePublisher() != bridge {
		t.Fatal("lifecycle publisher did not retain the stable bridge")
	}
	if services.ProcessCompletionNotifier() != bridge {
		t.Fatal("completion notifier did not retain the stable bridge")
	}
	if services.WorkflowActivityPublisher() != bridge {
		t.Fatal("workflow activity publisher did not retain the stable bridge")
	}
	if err := bridge.PublishProcessLifecycle(context.Background(), tool.ProcessLifecycleMetadata{}); !errors.Is(err, errSessionProcessServicesUnavailable) {
		t.Fatalf("PublishProcessLifecycle() error = %v, want %v", err, errSessionProcessServicesUnavailable)
	}
	if err := bridge.NotifyProcessCompletion(context.Background(), tool.ProcessCompletionNotification{}); !errors.Is(err, errSessionProcessServicesUnavailable) {
		t.Fatalf("NotifyProcessCompletion() error = %v, want %v", err, errSessionProcessServicesUnavailable)
	}
	if err := bridge.PublishWorkflowActivity(context.Background(), tool.WorkflowActivityMetadata{}); !errors.Is(err, errSessionProcessServicesUnavailable) {
		t.Fatalf("PublishWorkflowActivity() error = %v, want %v", err, errSessionProcessServicesUnavailable)
	}
}

type blockingWorkflowActivityPublisher struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingWorkflowActivityPublisher) PublishWorkflowActivity(context.Context, tool.WorkflowActivityMetadata) error {
	close(p.started)
	<-p.release
	return nil
}

func TestSessionProcessServiceBridgeClosesAndDrainsWorkflowPublisher(t *testing.T) {
	bridge, _, err := newSessionProcessServices()
	if err != nil {
		t.Fatalf("newSessionProcessServices() error = %v", err)
	}
	delegate := &blockingWorkflowActivityPublisher{started: make(chan struct{}), release: make(chan struct{})}
	bridge.attachWorkflowActivityPublisher(delegate)

	publicationDone := make(chan error, 1)
	go func() {
		publicationDone <- bridge.PublishWorkflowActivity(context.Background(), tool.WorkflowActivityMetadata{})
	}()
	select {
	case <-delegate.started:
	case <-time.After(time.Second):
		t.Fatal("workflow publication did not reach its delegate")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- bridge.closeWorkflowActivityPublisher(context.Background())
	}()
	select {
	case err := <-closeDone:
		t.Fatalf("closeWorkflowActivityPublisher() returned before in-flight publication drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(delegate.release)
	if err := <-publicationDone; err != nil {
		t.Fatalf("in-flight PublishWorkflowActivity() = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("closeWorkflowActivityPublisher() = %v", err)
	}
	if err := bridge.PublishWorkflowActivity(context.Background(), tool.WorkflowActivityMetadata{}); !errors.Is(err, errSessionProcessServicesUnavailable) {
		t.Fatalf("post-close PublishWorkflowActivity() error = %v, want %v", err, errSessionProcessServicesUnavailable)
	}
}

type processServicesCaptureResource struct {
	mu       sync.Mutex
	services tool.SessionResourceServices
}

func (r *processServicesCaptureResource) Activate(_ context.Context, services tool.SessionResourceServices) error {
	if err := services.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	r.services = services
	r.mu.Unlock()
	return nil
}

func (r *processServicesCaptureResource) Shutdown(context.Context) error {
	return nil
}

func (r *processServicesCaptureResource) capturedServices() tool.SessionResourceServices {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.services
}

type processServicesCapture struct {
	bridge   *sessionProcessServiceBridge
	resource *processServicesCaptureResource
}

func TestNewAndRestoreActivateResourcesWithTheirStableProcessServiceBridge(t *testing.T) {
	var capturesMu sync.Mutex
	captures := make(map[*sessionResources]processServicesCapture)
	definition := processResourceDefinition(t, loop.EngineNative, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		registry, ok := bindings.Process.Registry.(*sessionResources)
		if !ok {
			return nil, errors.New("process registry is not *sessionResources")
		}
		resource, err := registry.GetOrCreate(ctx, "process-services-capture", func(string) (tool.SessionResource, error) {
			captured := &processServicesCaptureResource{}
			capturesMu.Lock()
			captures[registry] = processServicesCapture{
				bridge:   registry.processServiceBridge,
				resource: captured,
			}
			capturesMu.Unlock()
			return captured, nil
		})
		if err != nil {
			return nil, err
		}
		if _, ok := resource.(*processServicesCaptureResource); !ok {
			return nil, errors.New("process resource is not *processServicesCaptureResource")
		}
		return []tool.InvokableTool{primerTestTool{name: "process"}}, nil
	})
	store := processResourceStore(t)
	resourceRoot := filepath.Join(t.TempDir(), "resources")
	lifecycle, err := newTestLifecycle(
		definition,
		store,
		WithLifecycleSessionResourceStorage(func(context.Context, uuid.UUID) (string, string, error) {
			return resourceRoot, "stable-owner", nil
		}),
	)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle() error = %v", err)
	}

	live, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	sessionID := live.SessionID()
	assertCapturedProcessServices(t, &capturesMu, captures, 1)
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("new Shutdown() error = %v", err)
	}
	capturesMu.Lock()
	var liveCapture processServicesCapture
	for _, captured := range captures {
		liveCapture = captured
		break
	}
	capturesMu.Unlock()
	if err := liveCapture.resource.capturedServices().WorkflowActivityPublisher().PublishWorkflowActivity(
		context.Background(), validWorkflowActivityMetadata(sessionID),
	); !errors.Is(err, errSessionProcessServicesUnavailable) {
		t.Fatalf("retained workflow publisher after normal Shutdown() error = %v, want %v", err, errSessionProcessServicesUnavailable)
	}

	restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RestoreSession() error = %v", err)
	}
	defer func() {
		if err := restored.Shutdown(context.Background()); err != nil {
			t.Errorf("restore Shutdown() error = %v", err)
		}
	}()
	assertCapturedProcessServices(t, &capturesMu, captures, 2)
}

func assertCapturedProcessServices(
	t *testing.T,
	capturesMu *sync.Mutex,
	captures map[*sessionResources]processServicesCapture,
	want int,
) {
	t.Helper()
	capturesMu.Lock()
	defer capturesMu.Unlock()
	if len(captures) != want {
		t.Fatalf("captured registries = %d, want %d", len(captures), want)
	}
	for registry, captured := range captures {
		if captured.bridge == nil {
			t.Fatal("captured bridge = nil")
		}
		if registry.processServiceBridge != captured.bridge {
			t.Fatal("registry replaced its process service bridge")
		}
		services := captured.resource.capturedServices()
		if err := services.Validate(); err != nil {
			t.Fatalf("captured SessionResourceServices.Validate() error = %v", err)
		}
		if services.ProcessLifecyclePublisher() != captured.bridge {
			t.Fatal("activated resource captured a replacement lifecycle publisher")
		}
		if services.ProcessCompletionNotifier() != captured.bridge {
			t.Fatal("activated resource captured a replacement completion notifier")
		}
		if services.WorkflowActivityPublisher() != captured.bridge {
			t.Fatal("activated resource captured a replacement workflow activity publisher")
		}
	}
}
