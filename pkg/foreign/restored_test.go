package foreign_test

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

func TestServicesRestoredBuilderReceivesServices(t *testing.T) {
	var got foreign.Services
	builder := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID,
		_ loop.Provenance, _ foreign.EventPublisher, _ loop.BoundDefinition,
		_ func() (uuid.UUID, error), _ *event.Factory, _ foreign.RestoredForeign,
		services foreign.Services) (loop.Backend, error) {
		got = services.Clone()
		return nil, nil
	})

	descriptor := foreign.NewBrokerDescriptor("endpoint", []byte("secret"))
	want := foreign.Services{Broker: descriptor}
	if _, err := builder(context.Background(), uuid.UUID{}, uuid.UUID{}, loop.Provenance{}, nil, nil, nil, nil,
		foreign.RestoredForeign{}, want); err != nil {
		t.Fatalf("services restored builder: %v", err)
	}
	if got.Broker.Endpoint() != "endpoint" {
		t.Fatalf("restored services endpoint = %q, want endpoint", got.Broker.Endpoint())
	}
}
