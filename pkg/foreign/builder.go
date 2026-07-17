// Package foreign defines the composition seams for foreign loop backends.
package foreign

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// EventPublisher is the foreign loop's narrow consumer of the session event
// fan-in. A session satisfies it via PublishEvent.
type EventPublisher interface {
	PublishEvent(context.Context, event.Event) error
	PublishEventChecked(context.Context, event.Event) error
}

// Builder is the composition-root seam a session uses to construct a foreign loop.
// It returns the Backend and the minted ForeignSID, which the caller records.
type Builder func(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
) (loop.Backend, string, error)
