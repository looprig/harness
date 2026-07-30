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
	completionNotifier tool.ProcessCompletionNotifier // nil until Task 24C attaches it
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

// attachProcessCompletionNotifier installs the checked completion notifier
// (Task 24C's implementation, *Session.NotifyProcessCompletion) behind the
// bridge WITHOUT replacing the bridge value itself — the exact same pattern
// attachProcessLifecyclePublisher established for 24B: every resource that
// already captured this *sessionProcessServiceBridge through
// SessionResourceServices observes the checked behavior on its very next
// call, because the bridge's identity never changes. A nil notifier is
// ignored so an accidental nil injection can never erase an already-attached
// delegate and silently fall back to the unavailable stub.
func (b *sessionProcessServiceBridge) attachProcessCompletionNotifier(n tool.ProcessCompletionNotifier) {
	if n == nil {
		return
	}
	b.mu.Lock()
	b.completionNotifier = n
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

func (b *sessionProcessServiceBridge) NotifyProcessCompletion(
	ctx context.Context,
	notification tool.ProcessCompletionNotification,
) error {
	b.mu.Lock()
	notifier := b.completionNotifier
	b.mu.Unlock()
	if notifier == nil {
		return errSessionProcessServicesUnavailable
	}
	return notifier.NotifyProcessCompletion(ctx, notification)
}

var (
	_ tool.ProcessLifecyclePublisher = (*sessionProcessServiceBridge)(nil)
	_ tool.ProcessCompletionNotifier = (*sessionProcessServiceBridge)(nil)
)
