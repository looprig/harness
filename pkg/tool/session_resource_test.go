package tool

import (
	"context"
	"errors"
	"testing"
)

type lifecyclePublisherStub struct{}

func (*lifecyclePublisherStub) PublishProcessLifecycle(context.Context, ProcessLifecycleMetadata) error {
	return nil
}

type completionNotifierStub struct{}

func (*completionNotifierStub) NotifyProcessCompletion(context.Context, ProcessCompletionNotification) error {
	return nil
}

func TestSessionResourceServicesRejectsTypedNilLifecyclePublisher(t *testing.T) {
	t.Parallel()

	var publisher *lifecyclePublisherStub
	_, err := NewSessionResourceServices(publisher, &completionNotifierStub{})
	var validationErr *SessionResourceServicesValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "process_lifecycle_publisher" {
		t.Fatalf("NewSessionResourceServices() error = %T %v, want lifecycle publisher validation error", err, err)
	}
}

func TestSessionResourceServicesRejectsTypedNilCompletionNotifier(t *testing.T) {
	t.Parallel()

	var notifier *completionNotifierStub
	_, err := NewSessionResourceServices(&lifecyclePublisherStub{}, notifier)
	var validationErr *SessionResourceServicesValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "process_completion_notifier" {
		t.Fatalf("NewSessionResourceServices() error = %T %v, want completion notifier validation error", err, err)
	}
}

func TestSessionResourceServicesAreImmutableAndValidated(t *testing.T) {
	t.Parallel()

	publisher := &lifecyclePublisherStub{}
	notifier := &completionNotifierStub{}
	services, err := NewSessionResourceServices(publisher, notifier)
	if err != nil {
		t.Fatalf("NewSessionResourceServices() error = %v", err)
	}
	if err := services.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if services.ProcessLifecyclePublisher() != publisher {
		t.Fatal("ProcessLifecyclePublisher() did not preserve the validated service")
	}
	if services.ProcessCompletionNotifier() != notifier {
		t.Fatal("ProcessCompletionNotifier() did not preserve the validated service")
	}

	var zero SessionResourceServices
	if err := zero.Validate(); err == nil {
		t.Fatal("zero SessionResourceServices.Validate() error = nil, want failure")
	}
}
