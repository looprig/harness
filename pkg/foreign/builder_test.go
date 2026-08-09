package foreign_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

type contractDeliveryHook struct{}

func (contractDeliveryHook) CreateIntent(context.Context, foreign.DeliveryIntent) error {
	return nil
}

func (contractDeliveryHook) Reserve(context.Context, foreign.DeliveryReservation) error {
	return nil
}

func (contractDeliveryHook) QueueFallback(context.Context, foreign.DeliveryFallback) error {
	return nil
}

func (contractDeliveryHook) Resolve(context.Context, foreign.DeliveryResolution) error {
	return nil
}

func TestBuilderContracts(t *testing.T) {
	var builder foreign.Builder
	if builder != nil {
		t.Fatal("Builder zero value is non-nil")
	}

	var restoredBuilder foreign.RestoredBuilder
	if restoredBuilder != nil {
		t.Fatal("RestoredBuilder zero value is non-nil")
	}

	builder = func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance,
		foreign.EventPublisher, loop.BoundDefinition, func() (uuid.UUID, error),
		*event.Factory) (loop.Backend, string, error) {
		return nil, "", nil
	}
	restoredBuilder = func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance,
		foreign.EventPublisher, loop.BoundDefinition, func() (uuid.UUID, error),
		*event.Factory, foreign.RestoredForeign) (loop.Backend, error) {
		return nil, nil
	}

	if builder == nil || restoredBuilder == nil {
		t.Fatal("typed builder assignment produced nil")
	}
}

func TestServicesBuilderContractsAndDescriptorDefensiveCopy(t *testing.T) {
	var builder foreign.ServicesBuilder
	if builder != nil {
		t.Fatal("ServicesBuilder zero value is non-nil")
	}

	var restoredBuilder foreign.ServicesRestoredBuilder
	if restoredBuilder != nil {
		t.Fatal("ServicesRestoredBuilder zero value is non-nil")
	}

	builder = func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance,
		foreign.EventPublisher, loop.BoundDefinition, func() (uuid.UUID, error),
		*event.Factory, foreign.Services) (loop.Backend, string, error) {
		return nil, "", nil
	}
	restoredBuilder = func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance,
		foreign.EventPublisher, loop.BoundDefinition, func() (uuid.UUID, error),
		*event.Factory, foreign.RestoredForeign, foreign.Services) (loop.Backend, error) {
		return nil, nil
	}
	if builder == nil || restoredBuilder == nil {
		t.Fatal("typed services builder assignment produced nil")
	}

	capability := []byte("capability")
	descriptor := foreign.NewBrokerDescriptor("unix:///tmp/broker", capability)
	capability[0] = 'X'
	if got := descriptor.Capability(); !bytes.Equal(got, []byte("capability")) {
		t.Fatalf("descriptor retained caller capability backing: %q", got)
	}
	returned := descriptor.Capability()
	returned[0] = 'Y'
	if got := descriptor.Capability(); !bytes.Equal(got, []byte("capability")) {
		t.Fatalf("descriptor accessor exposed capability backing: %q", got)
	}
	if got := descriptor.Endpoint(); got != "unix:///tmp/broker" {
		t.Fatalf("descriptor endpoint = %q, want unix:///tmp/broker", got)
	}

	services := foreign.Services{Broker: descriptor, Delivery: contractDeliveryHook{}}
	cloned := services.Clone()
	if cloned.Broker.Endpoint() != services.Broker.Endpoint() ||
		!bytes.Equal(cloned.Broker.Capability(), services.Broker.Capability()) ||
		cloned.Delivery == nil {
		t.Fatalf("services clone = %#v, want equivalent independent services", cloned)
	}
}

func TestRestoredForeignRetainsAssignedValues(t *testing.T) {
	msg := &content.AIMessage{}
	msgs := content.AgenticMessages{msg}
	seed := foreign.RestoredForeign{
		ForeignSID: "foreign-session-17",
		TurnIndex:  event.TurnIndex(23),
		Msgs:       msgs,
	}

	if seed.ForeignSID != "foreign-session-17" {
		t.Fatalf("ForeignSID = %q, want %q", seed.ForeignSID, "foreign-session-17")
	}
	if seed.TurnIndex != event.TurnIndex(23) {
		t.Fatalf("TurnIndex = %d, want %d", seed.TurnIndex, event.TurnIndex(23))
	}
	if len(seed.Msgs) != 1 || seed.Msgs[0] != msg {
		t.Fatalf("Msgs = %#v, want exact assigned messages %#v", seed.Msgs, msgs)
	}
}
