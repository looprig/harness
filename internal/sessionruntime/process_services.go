package sessionruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

var errSessionProcessServicesUnavailable = errors.New("session: process services unavailable")

// sessionProcessServiceBridge is the stable, session-owned indirection captured
// by process resources during activation. Task 24 attaches checked durable
// publication and completion delivery behind this object without replacing it:
// the bridge's pointer identity never changes across probe/restore/live
// activation, only its unexported delegate fields do, under mu. Until a delegate
// is attached, every call keeps returning the explicit unavailable error a
// resource sees before Task 24 lands.
type sessionProcessServiceBridge struct {
	mu                 sync.Mutex
	lifecyclePublisher tool.ProcessLifecyclePublisher // nil until Task 24B attaches it
}

func newSessionProcessServices() (
	*sessionProcessServiceBridge,
	tool.SessionResourceServices,
	error,
) {
	bridge := &sessionProcessServiceBridge{}
	services, err := tool.NewSessionResourceServices(bridge, bridge)
	if err != nil {
		return nil, tool.SessionResourceServices{}, err
	}
	return bridge, services, nil
}

// attachProcessLifecyclePublisher installs the checked durable publisher (Task
// 24B's implementation) behind the bridge WITHOUT replacing the bridge value
// itself: every resource that already captured this *sessionProcessServiceBridge
// through SessionResourceServices observes the checked behavior on its very next
// call, because the bridge's identity never changes. A nil publisher is ignored
// so an accidental nil injection can never erase an already-attached delegate and
// silently fall back to the unavailable stub.
func (b *sessionProcessServiceBridge) attachProcessLifecyclePublisher(p tool.ProcessLifecyclePublisher) {
	if p == nil {
		return
	}
	b.mu.Lock()
	b.lifecyclePublisher = p
	b.mu.Unlock()
}

func (b *sessionProcessServiceBridge) PublishProcessLifecycle(
	ctx context.Context,
	metadata tool.ProcessLifecycleMetadata,
) error {
	b.mu.Lock()
	publisher := b.lifecyclePublisher
	b.mu.Unlock()
	if publisher == nil {
		return errSessionProcessServicesUnavailable
	}
	return publisher.PublishProcessLifecycle(ctx, metadata)
}

func (*sessionProcessServiceBridge) NotifyProcessCompletion(
	context.Context,
	tool.ProcessCompletionNotification,
) error {
	return errSessionProcessServicesUnavailable
}

var (
	_ tool.ProcessLifecyclePublisher = (*sessionProcessServiceBridge)(nil)
	_ tool.ProcessCompletionNotifier = (*sessionProcessServiceBridge)(nil)
)
