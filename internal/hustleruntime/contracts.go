package hustleruntime

import (
	"context"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
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
	Evidence            *EvidenceRuntimeConfig
}

// EvidenceRuntimeConfig supplies only the headless, read-only capabilities
// needed by opt-in evidence-tool definitions.
type EvidenceRuntimeConfig struct {
	Access          EvidenceAccessEvaluator
	Containment     EvidenceContainmentVerifier
	AllowedKinds    []string
	ReadWorkspace   *tool.ReadWorkspaceBinding
	SecurityCeiling string
	NewExecutionID  EvidenceExecutionIDFactory
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

// FinalizerContextDecorator adds consumer-owned, non-capability metadata while
// preserving the supplied trusted context's values and deadline. The runtime
// never receives the consumer object that interprets the marker.
type FinalizerContextDecorator interface {
	DecorateFinalizerContext(context.Context) context.Context
}

// ValidateResult performs consumer-owned decoding and domain validation before
// HustleCompleted can commit.
type ValidateResult func(context.Context, hustle.Result) error

// Finalizer is the consumer-owned product commit callback. Every owned run calls
// it exactly once; a pre-ownership rejection never calls it. Trusted production
// adapters must preserve and honor the supplied context when delegating. They are
// built from focused product capability and must never capture a Session,
// Shutdown function, or other generic session-control capability.
type Finalizer func(context.Context, hustle.Outcome) error

// evidenceAccessEvaluator is the deliberately non-interactive access seam used
// by the evidence runner. gate.AccessBindings satisfies it without exposing
// approval, stored-rule, persistence, or grant capabilities.
type EvidenceAccessEvaluator interface {
	AccessFor(tool.Requirement) (uint8, error)
}

type evidenceAccessEvaluator = EvidenceAccessEvaluator

// EvidenceContainmentPolicy is the complete security context exposed to the
// evidence containment verifier. ReadRoot must be the canonical workspace root;
// SecurityCeiling is the trusted consumer's effective, non-widenable policy.
type EvidenceContainmentPolicy struct {
	ReadRoot        string
	SecurityCeiling string
}

// EvidenceContainmentVerifier independently resolves every prepared target,
// including symlinks and ambiguous scopes, against the canonical read root and
// enforces the configured security ceiling. It receives no session, gate,
// mutation, grant, rule, or loop-control capability. Implementations must fail
// closed when a tool-owned Requirement cannot be mapped unambiguously.
type EvidenceContainmentVerifier interface {
	VerifyEvidenceContainment(context.Context, EvidenceContainmentPolicy, tool.Request) error
}

type evidenceContainmentVerifier = EvidenceContainmentVerifier

type EvidenceExecutionIDFactory func() (uuid.UUID, error)

type evidenceExecutionIDFactory = EvidenceExecutionIDFactory

var withPreparedEvidenceCall = loop.WithPreparedCall
