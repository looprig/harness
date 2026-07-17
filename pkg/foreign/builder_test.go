package foreign_test

import (
	"context"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

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
