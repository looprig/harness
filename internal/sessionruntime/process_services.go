package sessionruntime

import (
	"context"
	"errors"

	"github.com/looprig/harness/pkg/tool"
)

var errSessionProcessServicesUnavailable = errors.New("session: process services unavailable")

// sessionProcessServiceBridge is the stable, session-owned indirection captured
// by process resources during activation. Task 24 attaches checked durable
// publication and completion delivery behind this object without replacing it.
type sessionProcessServiceBridge struct{}

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

func (*sessionProcessServiceBridge) PublishProcessLifecycle(
	context.Context,
	tool.ProcessLifecycleMetadata,
) error {
	return errSessionProcessServicesUnavailable
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
