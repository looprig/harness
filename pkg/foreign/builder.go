// Package foreign defines the composition seams for foreign loop backends.
package foreign

import (
	"context"
	"errors"
	"sync"

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

var (
	errBuilderRegistryNil      = errors.New("foreign: builder registry unavailable")
	errEmptyBuilderProfile     = errors.New("foreign: builder profile required")
	errDuplicateBuilderProfile = errors.New("foreign: builder profile already registered")
	errNilBuilder              = errors.New("foreign: builder callbacks required")
)

// UnknownProfileError reports a profile that is not registered. Its message is
// intentionally bounded and does not include the requested profile or any
// construction detail.
type UnknownProfileError struct{}

func (*UnknownProfileError) Error() string {
	return "foreign: unknown builder profile"
}

type builderPair struct {
	build    Builder
	restored RestoredBuilder
}

// BuilderRegistry routes foreign-loop construction by the stable runtime
// profile key. The zero value is ready for use. Registration is serialized and
// lookup takes a snapshot of the function pair, so a configured registry can
// be safely composed and read concurrently.
//
// A BuilderRegistry must not be copied after first use.
type BuilderRegistry struct {
	mu       sync.RWMutex
	builders map[loop.RuntimeProfileName]builderPair
}

// Register binds a live/restored builder pair to profile. Empty profiles and
// duplicate registrations fail closed; an existing binding is never replaced.
func (r *BuilderRegistry) Register(profile loop.RuntimeProfileName, builder Builder, restored RestoredBuilder) error {
	if r == nil {
		return errBuilderRegistryNil
	}
	if profile == "" {
		return errEmptyBuilderProfile
	}
	if builder == nil || restored == nil {
		return errNilBuilder
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builders == nil {
		r.builders = make(map[loop.RuntimeProfileName]builderPair)
	}
	if _, exists := r.builders[profile]; exists {
		return errDuplicateBuilderProfile
	}
	r.builders[profile] = builderPair{build: builder, restored: restored}
	return nil
}

// Builder returns the live and restored builders registered for profile. An
// unknown profile returns a bounded *UnknownProfileError and no builders.
func (r *BuilderRegistry) Builder(profile loop.RuntimeProfileName) (Builder, RestoredBuilder, error) {
	if r == nil {
		return nil, nil, &UnknownProfileError{}
	}

	r.mu.RLock()
	pair, exists := r.builders[profile]
	r.mu.RUnlock()
	if !exists {
		return nil, nil, &UnknownProfileError{}
	}
	return pair.build, pair.restored, nil
}
