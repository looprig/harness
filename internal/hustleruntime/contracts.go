package hustleruntime

import (
	"context"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
)

const maxLaneQueued = 10_000

// LaneLimits bounds one participation lane. Concurrent limits executing runs;
// Queued adds waiting ownership capacity. Their sum is the total ownership cap.
type LaneLimits struct {
	Concurrent int
	Queued     int
}

// RunIDFactory mints one candidate identifier before ownership commits.
type RunIDFactory func() (uuid.UUID, error)

// Config owns only the two scheduler lanes and the identifier seam needed at the
// ownership boundary. Task-specific execution and audit collaborators are layered
// onto Controller separately.
type Config struct {
	Blocking   LaneLimits
	Background LaneLimits
	NewRunID   RunIDFactory
	Runtime    *RuntimeConfig
}

// RuntimeConfig supplies the immutable definitions and narrow controller-owned
// capabilities used by RunAndFinalize. Nil preserves the Task 18 ownership-only
// construction seam.
type RuntimeConfig struct {
	SessionID           uuid.UUID
	Definitions         []hustle.BoundDefinition
	AuditTimeout        time.Duration
	FinalizationTimeout time.Duration
	WorkerDrainTimeout  time.Duration
	Stamper             HeaderStamper
	Audit               AuditPublisher
	Faults              FaultReporter
	Activity            ActivityTracker
	FinalizerContext    FinalizerContextDecorator
}

// HeaderStamper mints the identity fields of one internal lifecycle event.
type HeaderStamper interface {
	Stamp(event.Header) (event.Header, error)
}

// AuditPublisher owns the checked private durable lifecycle path.
type AuditPublisher interface {
	PublishInternalEventChecked(context.Context, event.Event) error
}

// FaultReporter receives bounded typed controller faults.
type FaultReporter interface {
	ReportFault(context.Context, error)
}

// ActivityTracker acquires blocking session activity for one owned run.
type ActivityTracker interface {
	AcquireHustleActivity(context.Context, hustle.RunID) (ActivityLease, error)
}

// ActivityLease retains blocking activity through finalization.
type ActivityLease interface {
	Release(context.Context) error
}

// FinalizerContextDecorator adds consumer-owned, non-capability metadata to the
// trusted finalizer context. The runtime never receives the consumer object that
// interprets the marker.
type FinalizerContextDecorator interface {
	DecorateFinalizerContext(context.Context) context.Context
}

// ValidateResult performs consumer-owned decoding and domain validation before
// HustleCompleted can commit.
type ValidateResult func(context.Context, hustle.Result) error

// Finalizer is the consumer-owned product commit callback. Every owned run calls
// it exactly once; a pre-ownership rejection never calls it.
type Finalizer func(context.Context, hustle.Outcome) error
