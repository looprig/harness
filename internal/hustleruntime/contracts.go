package hustleruntime

import (
	"context"

	"github.com/looprig/core/uuid"
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
}

// Finalizer is the consumer-owned product commit callback. Every owned run calls
// it exactly once; a pre-ownership rejection never calls it.
type Finalizer func(context.Context, hustle.Outcome) error
