package sessionwire

import "github.com/looprig/harness/pkg/event"

// EventClass is the adapter's closed publication policy for a concrete Harness
// event. PrivateRejected is also the fail-closed result for values outside the
// sealed event union.
type EventClass string

const (
	PublicEnduring  EventClass = "public_enduring"
	PublicEphemeral EventClass = "public_ephemeral"
	PrivateRejected EventClass = "private_rejected"
)

// classify is the explicit public-projection table. Keep every concrete type in
// its own arm: grouping arms would make a class change less visible in review,
// and a default marshal path would turn a newly introduced event public.
//
// RECORDED POLICY, not a defect report: TurnFoldedInto and InputCancelled each
// carry a *content.UserMessage, and a PublicEnduring body is the event's full
// marshalled form with GateResolved's audit as the ONLY redaction (see
// projectBody). So raw user message text from those two events reaches the public
// journal verbatim. That is measured behaviour, stated here because the next person
// deciding what a public journal may contain should find it written down rather
// than discover it. Changing it is a decision for whoever owns the projection
// contract, and it would be a breaking change to the public body shape.
func classify(value any) EventClass {
	switch value.(type) {
	case event.SessionStarted:
		return PublicEnduring
	case event.SessionActive:
		return PublicEnduring
	case event.SessionIdle:
		return PublicEnduring
	case event.SessionStopped:
		return PublicEnduring
	case event.SessionResidencyReleased:
		return PublicEnduring
	case event.RestoreStarted:
		return PublicEnduring
	case event.RestoreDone:
		return PublicEnduring
	case event.RestoreErrored:
		return PublicEnduring
	case event.ConfigurationAdopted:
		return PublicEnduring
	case event.WorkspaceCheckpointed:
		return PublicEnduring
	case event.WorkspaceRestored:
		return PublicEnduring
	case event.ActiveLoopChanged:
		return PublicEnduring
	case event.DelegateDeliveryStateChanged:
		return PublicEnduring
	case event.WorkflowActivity:
		return PublicEnduring
	case event.LoopRestoreTombstoned:
		return PublicEnduring
	case event.IntegrationStatus:
		return PublicEphemeral
	case event.HustleStarted:
		return PrivateRejected
	case event.HustleCompleted:
		return PrivateRejected
	case event.HustleFailed:
		return PrivateRejected
	case event.PermissionReviewStarted:
		return PrivateRejected
	case event.PermissionReviewCompleted:
		return PrivateRejected
	case event.ProcessStarted:
		return PublicEnduring
	case event.ProcessBackgrounded:
		return PublicEnduring
	case event.ProcessCompleted:
		return PublicEnduring
	case event.ProcessStopRequested:
		return PublicEnduring
	case event.ProcessLost:
		return PublicEnduring
	case event.LoopIdle:
		return PublicEnduring
	case event.LoopStarted:
		return PublicEnduring
	case event.DelegateRequestAccepted:
		return PublicEnduring
	case event.LoopInferenceChanged:
		return PublicEnduring
	case event.LoopModeChanged:
		return PublicEnduring
	case event.LoopExternalToolsetChanged:
		return PublicEnduring
	case event.ContextMeasured:
		return PublicEnduring
	case event.ContextPressure:
		return PublicEphemeral
	case event.CompactionStarted:
		return PublicEphemeral
	case event.CompactionCommitted:
		return PublicEnduring
	case event.CompactionRejected:
		return PublicEnduring
	case event.CompactWaiterResolved:
		return PublicEnduring
	case event.CompactWaiterRejected:
		return PublicEnduring
	case event.ForeignSessionBound:
		return PublicEnduring
	case event.LoopAgentSessionBound:
		return PublicEnduring
	case event.TokenDelta:
		return PublicEphemeral
	case event.TurnStarted:
		return PublicEnduring
	case event.StepDone:
		return PublicEnduring
	case event.TurnFoldedInto:
		return PublicEnduring
	case event.InputCancelled:
		return PublicEnduring
	case event.InputQueued:
		return PublicEphemeral
	case event.TurnRejected:
		return PublicEnduring
	case event.TurnDone:
		return PublicEnduring
	case event.TurnFailed:
		return PublicEnduring
	case event.TurnInterrupted:
		return PublicEnduring
	case event.PermissionRequested:
		return PublicEnduring
	case event.PermissionDecided:
		return PublicEnduring
	case event.UserInputRequested:
		return PublicEnduring
	case event.ToolCallStarted:
		return PublicEphemeral
	case event.ToolCallCompleted:
		return PublicEphemeral
	case event.GatePrepared:
		return PrivateRejected
	case event.GateOpened:
		return PublicEnduring
	case event.GateResolved:
		return PublicEnduring
	default:
		return PrivateRejected
	}
}
