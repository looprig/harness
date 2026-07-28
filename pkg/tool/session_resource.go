package tool

import (
	"context"
	"reflect"
)

// SessionResource is session-owned state shared by tool definitions. Activate
// late-binds live session services after construction and restore planning;
// Shutdown releases the resource during session teardown.
type SessionResource interface {
	Activate(context.Context, SessionResourceServices) error
	Shutdown(context.Context) error
}

// SessionResourceRegistry atomically resolves one session-owned resource by
// key. The factory receives a private storage directory reserved for that key.
type SessionResourceRegistry interface {
	GetOrCreate(context.Context, string, func(string) (SessionResource, error)) (SessionResource, error)
}

// ProcessLifecyclePublisher durably publishes bounded, metadata-only process
// lifecycle transitions.
type ProcessLifecyclePublisher interface {
	PublishProcessLifecycle(context.Context, ProcessLifecycleMetadata) error
}

// ProcessCompletionNotifier submits a bounded process terminal notification to
// the process owner's loop.
type ProcessCompletionNotifier interface {
	NotifyProcessCompletion(context.Context, ProcessCompletionNotification) error
}

// SessionResourceServices is the immutable late-bound service set supplied to
// every session resource after the live session has been constructed. Its zero
// value is invalid; use NewSessionResourceServices.
type SessionResourceServices struct {
	processLifecyclePublisher ProcessLifecyclePublisher
	processCompletionNotifier ProcessCompletionNotifier
}

// SessionResourceServicesValidationError reports a missing or typed-nil
// late-bound session service.
type SessionResourceServicesValidationError struct {
	Field string
}

func (e *SessionResourceServicesValidationError) Error() string {
	return "tool: invalid session resource services: " + e.Field
}

// NewSessionResourceServices constructs an immutable, fully populated service
// set and rejects both nil interfaces and interfaces containing typed nils.
func NewSessionResourceServices(
	publisher ProcessLifecyclePublisher,
	notifier ProcessCompletionNotifier,
) (SessionResourceServices, error) {
	services := SessionResourceServices{
		processLifecyclePublisher: publisher,
		processCompletionNotifier: notifier,
	}
	if err := services.Validate(); err != nil {
		return SessionResourceServices{}, err
	}
	return services, nil
}

// Validate reports whether the service set is safe to give to a session
// resource.
func (s SessionResourceServices) Validate() error {
	if s.processLifecyclePublisher == nil ||
		nilReflectValue(reflect.ValueOf(s.processLifecyclePublisher)) {
		return &SessionResourceServicesValidationError{Field: "process_lifecycle_publisher"}
	}
	if s.processCompletionNotifier == nil ||
		nilReflectValue(reflect.ValueOf(s.processCompletionNotifier)) {
		return &SessionResourceServicesValidationError{Field: "process_completion_notifier"}
	}
	return nil
}

// ProcessLifecyclePublisher returns the validated lifecycle publisher.
func (s SessionResourceServices) ProcessLifecyclePublisher() ProcessLifecyclePublisher {
	return s.processLifecyclePublisher
}

// ProcessCompletionNotifier returns the validated completion notifier.
func (s SessionResourceServices) ProcessCompletionNotifier() ProcessCompletionNotifier {
	return s.processCompletionNotifier
}

// ProcessBinding contains the session-scoped capabilities supplied to process
// tool definitions.
type ProcessBinding struct {
	Registry SessionResourceRegistry
}
