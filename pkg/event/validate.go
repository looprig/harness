package event

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

// EventName is the concrete event type name an InvalidEventError points at.
type EventName string

// FieldName is the identity/body field an InvalidEventError points at.
type FieldName string

// Rule is the human-readable invariant an InvalidEventError records, so the
// caller learns WHY the field is wrong (required vs must-be-zero), not just which.
type Rule string

const (
	// RuleRequired: the field must be non-zero for this event.
	RuleRequired Rule = "must be set"
	// RuleMustBeZero: the field must be zero for this event's scope.
	RuleMustBeZero Rule = "must be zero"
	// RuleUnknownType: the event's concrete type is not in the sealed union, so it
	// fails fail-secure (the journal/restore caller must reject it rather than guess
	// an identity contract for it).
	RuleUnknownType Rule = "is not a known event type"
	// RuleInvalid: the field contains a value outside its closed domain.
	RuleInvalid Rule = "is invalid"
)

// Identity / body field names, named so an InvalidEventError reads precisely.
const (
	FieldEventID         FieldName = "EventID"
	FieldSessionID       FieldName = "SessionID"
	FieldLoopID          FieldName = "LoopID"
	FieldTurnID          FieldName = "TurnID"
	FieldStepID          FieldName = "StepID"
	FieldToolExecutionID FieldName = "ToolExecutionID"
	FieldConsistency     FieldName = "Consistency"
	FieldTrigger         FieldName = "Trigger"
	FieldCause           FieldName = "Cause"
	FieldCommandID       FieldName = "CommandID"
	FieldActiveLoopID    FieldName = "ActiveLoopID"
	FieldModel           FieldName = "Model"
	FieldEffort          FieldName = "Effort"
	// FieldType names the whole event (not one coordinate) on the fail-secure
	// unknown-type path, paired with RuleUnknownType.
	FieldType FieldName = "Type"
)

// InvalidEventError reports that an event violates the ID fill matrix: Field names
// the offending identity/body field and Rule says whether it was required or had to
// be zero. It is a typed package-API error so a journal/test can errors.As it to
// inspect the exact violation rather than parse a string.
type InvalidEventError struct {
	Event EventName
	Field FieldName
	Rule  Rule
}

func (e *InvalidEventError) Error() string {
	return "event: invalid " + string(e.Event) + ": " + string(e.Field) + " " + string(e.Rule)
}

// idProfile is one event type's STATIC identity contract from the fill matrix:
// which Coordinates fields must be set, which must be zero, whether a
// ToolExecutionID is required (the four tool/gate events), and whether TurnID is
// OPTIONAL (only InputCancelled, whose TurnID is the returned turn for an abnormal
// return but zero for a pure client retract). A field that is neither required nor
// forbidden is unconstrained. It holds no per-instance value, so every event type's
// profile is a plain constant; ValidateEvent reads the runtime ToolExecutionID off
// the concrete event body when requireTool is set.
type idProfile struct {
	requireSession bool
	requireLoop    bool
	requireTurn    bool
	requireStep    bool

	forbidLoop bool
	forbidTurn bool
	forbidStep bool

	requireTool bool // ToolExecutionID must be non-zero
}

// ValidateEvent checks ev against the ID fill matrix and returns a typed
// *InvalidEventError on the first violation, nil when ev satisfies every invariant.
// EventID is required on every event; the per-type profile then pins the required
// and must-be-zero coordinates (and ToolExecutionID for the four tool/gate events).
// Fail-secure: an event whose concrete type is not in the sealed union is invalid
// with FieldType/RuleUnknownType — the caller learns the type is unknown, not that
// some coordinate is missing.
func ValidateEvent(ev Event) error {
	if err := validateEventIdentity(ev); err != nil {
		return err
	}
	return validateEventBody(ev)
}

func validateEventIdentity(ev Event) error {
	nameStr, prof, ok := classify(ev)
	name := EventName(nameStr)
	if !ok {
		return &InvalidEventError{Event: name, Field: FieldType, Rule: RuleUnknownType}
	}
	h := ev.EventHeader()
	if h.EventID.IsZero() {
		return &InvalidEventError{Event: name, Field: FieldEventID, Rule: RuleRequired}
	}
	return checkProfile(name, h.Coordinates, toolExecutionID(ev), prof)
}

func validateEventBody(ev Event) error {
	switch e := ev.(type) {
	case WorkspaceCheckpointed:
		if e.Consistency != SnapshotQuiescent && e.Consistency != SnapshotFuzzy {
			return &InvalidEventError{Event: "WorkspaceCheckpointed", Field: FieldConsistency, Rule: RuleInvalid}
		}
		if e.Trigger < SnapshotTriggerManual || e.Trigger > SnapshotTriggerSeed {
			return &InvalidEventError{Event: "WorkspaceCheckpointed", Field: FieldTrigger, Rule: RuleInvalid}
		}
		if !validCheckpointCause(e.SessionID, e.Trigger, e.Cause) {
			return &InvalidEventError{Event: "WorkspaceCheckpointed", Field: FieldCause, Rule: RuleInvalid}
		}
	case ActiveLoopChanged:
		if e.ActiveLoopID.IsZero() {
			return &InvalidEventError{Event: "ActiveLoopChanged", Field: FieldActiveLoopID, Rule: RuleRequired}
		}
	case DelegateRequestAccepted:
		if e.Cause.CommandID.IsZero() {
			return &InvalidEventError{Event: "DelegateRequestAccepted", Field: FieldCommandID, Rule: RuleRequired}
		}
	case LoopInferenceChanged:
		if err := e.Model.Validate(); err != nil {
			return &InvalidEventError{Event: "LoopInferenceChanged", Field: FieldModel, Rule: RuleInvalid}
		}
		if e.Model.Origin != inference.OriginCustom && e.Model.Origin != inference.OriginCatalog {
			return &InvalidEventError{Event: "LoopInferenceChanged", Field: FieldModel, Rule: RuleInvalid}
		}
		if !e.Model.Sampling.Effort.Valid() {
			return &InvalidEventError{Event: "LoopInferenceChanged", Field: FieldModel, Rule: RuleInvalid}
		}
		if !e.Effort.Valid() {
			return &InvalidEventError{Event: "LoopInferenceChanged", Field: FieldEffort, Rule: RuleInvalid}
		}
	}
	return nil
}

func validCheckpointCause(sessionID uuid.UUID, trigger SnapshotTriggerKind, cause identity.Cause) bool {
	zero := identity.Cause{}
	if trigger == SnapshotTriggerManual || trigger == SnapshotTriggerSeed {
		return cause == zero
	}
	if cause.EventID.IsZero() || !cause.CommandID.IsZero() || !cause.ToolExecutionID.IsZero() || cause.Agency != identity.AgencyMachine {
		return false
	}
	c := cause.Coordinates
	if c.SessionID != sessionID {
		return false
	}
	switch trigger {
	case SnapshotTriggerIdle:
		return !c.SessionID.IsZero() && c.LoopID.IsZero() && c.TurnID.IsZero() && c.StepID.IsZero()
	case SnapshotTriggerInterrupt, SnapshotTriggerTurnDone:
		return !c.SessionID.IsZero() && !c.LoopID.IsZero() && !c.TurnID.IsZero() && c.StepID.IsZero()
	case SnapshotTriggerStepDone:
		return !c.SessionID.IsZero() && !c.LoopID.IsZero() && !c.TurnID.IsZero() && !c.StepID.IsZero()
	default:
		return false
	}
}

// checkProfile enforces one event's idProfile against its Coordinates: required
// fields must be non-zero, forbidden fields must be zero, and a tool/gate event's
// ToolExecutionID (read from the concrete event body, passed in as toolID) must be
// non-zero. The order (session ▸ loop ▸ turn ▸ step ▸ tool) reports the outermost
// violation first.
func checkProfile(name EventName, c identity.Coordinates, toolID uuid.UUID, p idProfile) error {
	checks := []struct {
		bad   bool
		field FieldName
		rule  Rule
	}{
		{p.requireSession && c.SessionID.IsZero(), FieldSessionID, RuleRequired},
		{p.requireLoop && c.LoopID.IsZero(), FieldLoopID, RuleRequired},
		{p.forbidLoop && !c.LoopID.IsZero(), FieldLoopID, RuleMustBeZero},
		{p.requireTurn && c.TurnID.IsZero(), FieldTurnID, RuleRequired},
		{p.forbidTurn && !c.TurnID.IsZero(), FieldTurnID, RuleMustBeZero},
		{p.requireStep && c.StepID.IsZero(), FieldStepID, RuleRequired},
		{p.forbidStep && !c.StepID.IsZero(), FieldStepID, RuleMustBeZero},
		// StepID requires TurnID (StepID set ⇒ TurnID set), independent of the profile.
		{!c.StepID.IsZero() && c.TurnID.IsZero(), FieldTurnID, RuleRequired},
		{p.requireTool && toolID.IsZero(), FieldToolExecutionID, RuleRequired},
	}
	for _, chk := range checks {
		if chk.bad {
			return &InvalidEventError{Event: name, Field: chk.field, Rule: chk.rule}
		}
	}
	return nil
}

// toolExecutionID returns the body ToolExecutionID for the four tool/gate events
// (the only events whose profile sets requireTool) and the zero UUID for every
// other type — checkProfile ignores it unless requireTool is set.
func toolExecutionID(ev Event) uuid.UUID {
	switch e := ev.(type) {
	case PermissionRequested:
		return e.ToolExecutionID
	case PermissionDecided:
		return e.ToolExecutionID
	case UserInputRequested:
		return e.ToolExecutionID
	case ToolCallStarted:
		return e.ToolExecutionID
	case ToolCallCompleted:
		return e.ToolExecutionID
	default:
		return uuid.UUID{}
	}
}

// classify is the SINGLE enumeration of the sealed event union: it returns the
// concrete type name, its STATIC idProfile, and whether the type is in the union.
// Keeping name and profile in one switch means a newly added event type cannot be
// half-registered — there is exactly one place to add it. An unknown type renders
// as "Event" with ok==false (ValidateEvent rejects it fail-secure).
func classify(ev Event) (name string, profile idProfile, ok bool) {
	switch ev.(type) {
	case SessionStarted:
		return "SessionStarted", sessionProfile(), true
	case SessionActive:
		return "SessionActive", sessionProfile(), true
	case SessionIdle:
		return "SessionIdle", sessionProfile(), true
	case SessionStopped:
		return "SessionStopped", sessionProfile(), true
	case RestoreStarted:
		// Session-scoped, same shape as SessionStarted: only SessionID set.
		return "RestoreStarted", sessionProfile(), true
	case RestoreDone:
		return "RestoreDone", sessionProfile(), true
	case RestoreErrored:
		return "RestoreErrored", sessionProfile(), true
	case WorkspaceCheckpointed:
		// Session-scoped: a session-global workspace snapshot appended at quiescence
		// (same shape as RestoreDone/SessionIdle) — only SessionID set. Ref is an
		// opaque payload string the validator never constrains.
		return "WorkspaceCheckpointed", sessionProfile(), true
	case WorkspaceRestored:
		return "WorkspaceRestored", sessionProfile(), true
	case ActiveLoopChanged:
		return "ActiveLoopChanged", sessionProfile(), true
	case SecurityCeilingChanged:
		// Session-scoped: a session-global ceiling clamp appended when the operator
		// changes it (same shape as WorkspaceCheckpointed) — only SessionID set. Level is
		// an opaque ordinal the validator never constrains.
		return "SecurityCeilingChanged", sessionProfile(), true
	case LoopIdle:
		return "LoopIdle", loopProfile(), true
	case LoopStarted:
		// Loop-scoped: NEW loop in Header.Coordinates (SessionID+LoopID set, Turn/Step
		// zero). The spawning loop/turn/step rides in Header.Cause, which the validator
		// never constrains — same shape as LoopIdle.
		return "LoopStarted", loopProfile(), true
	case DelegateRequestAccepted:
		return "DelegateRequestAccepted", loopProfile(), true
	case LoopInferenceChanged:
		return "LoopInferenceChanged", loopProfile(), true
	case LoopModeChanged:
		return "LoopModeChanged", loopProfile(), true
	case ForeignSessionBound:
		return "ForeignSessionBound", loopProfile(), true
	case TokenDelta:
		return "TokenDelta", stepProfile(), true
	case TurnStarted:
		return "TurnStarted", turnProfile(), true
	case StepDone:
		return "StepDone", stepProfile(), true
	case TurnFoldedInto:
		return "TurnFoldedInto", turnProfile(), true
	case InputCancelled:
		return "InputCancelled", inputCancelledProfile(), true
	case InputQueued:
		// Loop-scoped reply event resolved before a turn exists: SessionID+LoopID set,
		// TurnID/StepID zero (same shape as LoopIdle).
		return "InputQueued", loopProfile(), true
	case TurnRejected:
		return "TurnRejected", loopProfile(), true
	case TurnDone:
		return "TurnDone", turnProfile(), true
	case TurnFailed:
		return "TurnFailed", turnProfile(), true
	case TurnInterrupted:
		return "TurnInterrupted", turnProfile(), true
	case PermissionRequested:
		return "PermissionRequested", toolProfile(), true
	case PermissionDecided:
		return "PermissionDecided", toolProfile(), true
	case UserInputRequested:
		return "UserInputRequested", toolProfile(), true
	case ToolCallStarted:
		return "ToolCallStarted", toolProfile(), true
	case ToolCallCompleted:
		return "ToolCallCompleted", toolProfile(), true
	case GatePrepared:
		return "GatePrepared", stepProfile(), true
	case GateOpened:
		return "GateOpened", stepProfile(), true
	case GateResolved:
		return "GateResolved", stepProfile(), true
	default:
		return "Event", idProfile{}, false
	}
}

// sessionProfile: ScopeSession — only SessionID set; LoopID/TurnID/StepID zero.
func sessionProfile() idProfile {
	return idProfile{requireSession: true, forbidLoop: true, forbidTurn: true, forbidStep: true}
}

// loopProfile: ScopeLoop with no turn — SessionID+LoopID set; TurnID/StepID zero.
func loopProfile() idProfile {
	return idProfile{requireSession: true, requireLoop: true, forbidTurn: true, forbidStep: true}
}

// turnProfile: turn events — SessionID+LoopID+TurnID set; StepID zero.
func turnProfile() idProfile {
	return idProfile{requireSession: true, requireLoop: true, requireTurn: true, forbidStep: true}
}

// inputCancelledProfile: SessionID+LoopID set; StepID zero; TurnID OPTIONAL (zero
// for a client retract outside a turn, the returned turn for an abnormal return).
func inputCancelledProfile() idProfile {
	return idProfile{requireSession: true, requireLoop: true, forbidStep: true}
}

// stepProfile: step events (TokenDelta/StepDone) — SessionID+LoopID+TurnID+StepID set.
func stepProfile() idProfile {
	return idProfile{requireSession: true, requireLoop: true, requireTurn: true, requireStep: true}
}

// toolProfile: the four tool/gate events — full quartet set plus a required
// ToolExecutionID (read from the event body by ValidateEvent, not stored here).
func toolProfile() idProfile {
	return idProfile{
		requireSession: true, requireLoop: true, requireTurn: true, requireStep: true,
		requireTool: true,
	}
}
