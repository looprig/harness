package foreign

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

func TestBuilderRegistryRejectsInvalidEntryShape(t *testing.T) {
	t.Parallel()

	live := Builder(func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, EventPublisher,
		loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory) (loop.Backend, string, error) {
		return nil, "", nil
	})
	services := ServicesBuilder(func(context.Context, uuid.UUID, uuid.UUID, loop.Provenance, EventPublisher,
		loop.BoundDefinition, func() (uuid.UUID, error), *event.Factory, Services) (loop.Backend, string, error) {
		return nil, "", nil
	})

	registry := &BuilderRegistry{builders: map[loop.RuntimeProfileName]builderPair{
		"mixed": {build: live, servicesBuild: services},
		"empty": {},
	}}
	for _, profile := range []loop.RuntimeProfileName{"mixed", "empty"} {
		if gotLive, gotRestored, err := registry.Builder(profile); gotLive != nil || gotRestored != nil || err == nil {
			t.Fatalf("Builder(%q) = (%v, %v, %v), want invalid-shape error and nil builders", profile, gotLive, gotRestored, err)
		}
		if gotLive, gotRestored, err := registry.ServicesBuilder(profile); gotLive != nil || gotRestored != nil || err == nil {
			t.Fatalf("ServicesBuilder(%q) = (%v, %v, %v), want invalid-shape error and nil builders", profile, gotLive, gotRestored, err)
		}
	}
}
