package event

import (
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	model "github.com/looprig/inference/model"
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
	FieldEventID          FieldName = "EventID"
	FieldSessionID        FieldName = "SessionID"
	FieldLoopID           FieldName = "LoopID"
	FieldTurnID           FieldName = "TurnID"
	FieldStepID           FieldName = "StepID"
	FieldToolExecutionID  FieldName = "ToolExecutionID"
	FieldConsistency      FieldName = "Consistency"
	FieldTrigger          FieldName = "Trigger"
	FieldCause            FieldName = "Cause"
	FieldCommandID        FieldName = "CommandID"
	FieldActiveLoopID     FieldName = "ActiveLoopID"
	FieldModel            FieldName = "Model"
	FieldModelKey         FieldName = "ModelKey"
	FieldContextLimits    FieldName = "ContextLimits"
	FieldEffort           FieldName = "Effort"
	FieldUsage            FieldName = "Usage"
	FieldMessages         FieldName = "Messages"
	FieldVisibility       FieldName = "Visibility"
	FieldDefinition       FieldName = "Definition"
	FieldRunID            FieldName = "RunID"
	FieldRuntime          FieldName = "Runtime"
	FieldDuration         FieldName = "Duration"
	FieldStage            FieldName = "Stage"
	FieldReasonCode       FieldName = "ReasonCode"
	FieldAttemptID        FieldName = "AttemptID"
	FieldReason           FieldName = "Reason"
	FieldRejectReason     FieldName = "RejectReason"
	FieldWaiterCommandIDs FieldName = "WaiterCommandIDs"
	FieldSummary          FieldName = "Summary"
	FieldPostContext      FieldName = "PostContext"
	FieldCommittedEventID FieldName = "CommittedEventID"
	FieldSource           FieldName = "Source"
	FieldActor            FieldName = "Actor"
	FieldGeneration       FieldName = "Generation"
	FieldTools            FieldName = "Tools"
	// FieldIntegrationName names IntegrationStatus.Name. It is not spelled
	// "FieldName": that identifier is this file's FieldName TYPE.
	FieldIntegrationName    FieldName = "Name"
	FieldState              FieldName = "State"
	FieldDetail             FieldName = "Detail"
	FieldEpoch              FieldName = "Epoch"
	FieldAdoptedFingerprint FieldName = "AdoptedFingerprint"
	FieldManifest           FieldName = "Manifest"
	FieldDrift              FieldName = "Drift"
	FieldMessage            FieldName = "Message"
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
// ToolExecutionID is required (the five tool-interaction events), and whether TurnID is
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
// and must-be-zero coordinates (and ToolExecutionID for the five tool-interaction events).
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
	if !h.EventVisibility.Valid() {
		return &InvalidEventError{Event: name, Field: FieldVisibility, Rule: RuleInvalid}
	}
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
		return validateModelRuntime("LoopInferenceChanged", e.Runtime)
	case LoopModeChanged:
		return validateModelRuntime("LoopModeChanged", e.Runtime)
	case LoopExternalToolsetChanged:
		return validateExternalToolset(e)
	case IntegrationStatus:
		return validateIntegrationStatus(e)
	case LoopStarted:
		return validateModelRuntime("LoopStarted", e.Runtime)
	case ContextMeasured:
		if e.Visibility() != Public {
			return &InvalidEventError{Event: "ContextMeasured", Field: FieldVisibility, Rule: RuleInvalid}
		}
		return e.Measurement.Validate()
	case ContextPressure:
		if e.Visibility() != Public {
			return &InvalidEventError{Event: "ContextPressure", Field: FieldVisibility, Rule: RuleInvalid}
		}
		return validateContextPressure(e)
	case CompactionStarted:
		return validateCompactionStarted(e)
	case CompactionCommitted:
		return validateCompactionCommitted(e)
	case CompactionRejected:
		return validateCompactionRejected(e)
	case CompactWaiterResolved:
		return validateCompactWaiterResolved(e)
	case CompactWaiterRejected:
		return validateCompactWaiterRejected(e)
	case HustleStarted:
		if e.Visibility() != Internal {
			return invalidHustle("HustleStarted", FieldVisibility)
		}
		if err := validateHustleRun("HustleStarted", e.Run); err != nil {
			return err
		}
		if !zeroModelRuntime(e.Run.Runtime) {
			return invalidHustle("HustleStarted", FieldRuntime)
		}
	case HustleCompleted:
		if e.Visibility() != Internal {
			return invalidHustle("HustleCompleted", FieldVisibility)
		}
		if err := validateHustleRun("HustleCompleted", e.Run); err != nil {
			return err
		}
		if e.Duration < 0 {
			return invalidHustle("HustleCompleted", FieldDuration)
		}
		if err := validateModelRuntime("HustleCompleted", e.Run.Runtime); err != nil {
			return invalidHustle("HustleCompleted", FieldRuntime)
		}
		if err := validateOptionalUsage("HustleCompleted", e.Usage); err != nil {
			return err
		}
	case HustleFailed:
		return validateHustleFailed(e)
	case StepDone:
		return validateStepDoneMessages(e.Messages)
	case TurnDone:
		if err := e.Usage.Validate(); err != nil {
			return &InvalidEventError{Event: "TurnDone", Field: FieldUsage, Rule: RuleInvalid}
		}
	case ConfigurationAdopted:
		return validateConfigurationAdopted(e)
	}
	return nil
}

// Bounds for ConfigurationAdopted's durable, partly user-authored payload: a
// hostile or buggy decision must not be able to append an unbounded record to
// the journal, and a legacy (SchemaVersion 0) manifest projection is never
// persisted.
const (
	// The manifest is decoded from untrusted journal input, so its collections
	// are capped defense-in-depth. The caps are generous: they never trip a
	// legitimate configuration, only an abusive one.
	maxConfigManifestTools     = 4096
	maxConfigManifestAppFields = 1024
	// maxConfigDriftChanges must never reject a drift summary a VALID manifest
	// comparison can legitimately produce, or a large-but-legitimate change would
	// brick every restore. A schema-1↔schema-1 assessment can emit one change per
	// tool (up to maxConfigManifestTools) plus one per app field (up to
	// maxConfigManifestAppFields) plus the ~dozen scalar-field categories; the +64
	// covers those scalars with slack. It still bounds a decoded hostile event.
	maxConfigDriftChanges = maxConfigManifestTools + maxConfigManifestAppFields + 64
	// MaxConfigMessageLen and MaxConfigActorLen bound the durable, partly
	// user-authored audit fields. They are exported so the restore constructor can
	// TRUNCATE a decider's over-long Message/Actor before building the adoption (a
	// long audit note must never brick a restore); the validator here still rejects
	// an over-long field on a hand-crafted, decoded journal record.
	MaxConfigMessageLen = 4096
	MaxConfigActorLen   = 1024
)

// validateConfigurationAdopted enforces the config-epoch invariants: epoch 1
// belongs to SessionStarted so an adoption is always >= 2, the adopted
// fingerprint is required, the source is one of the four closed DecisionSource
// values, the drift summary, message, and actor are length-capped, and a legacy
// (SchemaVersion 0) manifest projection is refused.
func validateConfigurationAdopted(e ConfigurationAdopted) error {
	const name EventName = "ConfigurationAdopted"
	if e.Epoch < 2 {
		return &InvalidEventError{Event: name, Field: FieldEpoch, Rule: RuleInvalid}
	}
	if e.AdoptedFingerprint == "" {
		return &InvalidEventError{Event: name, Field: FieldAdoptedFingerprint, Rule: RuleRequired}
	}
	if !e.Source.Valid() {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleInvalid}
	}
	if len(e.Drift) > maxConfigDriftChanges {
		return &InvalidEventError{Event: name, Field: FieldDrift, Rule: RuleInvalid}
	}
	if len(e.Message) > MaxConfigMessageLen {
		return &InvalidEventError{Event: name, Field: FieldMessage, Rule: RuleInvalid}
	}
	if len(e.Actor) > MaxConfigActorLen {
		return &InvalidEventError{Event: name, Field: FieldActor, Rule: RuleInvalid}
	}
	if e.Manifest.SchemaVersion == 0 {
		return &InvalidEventError{Event: name, Field: FieldManifest, Rule: RuleInvalid}
	}
	// A persisted manifest's recorded fingerprint must match the manifest itself,
	// so a durable baseline can never carry a fingerprint that disagrees with the
	// configuration it describes.
	if e.Manifest.Fingerprint() != e.AdoptedFingerprint {
		return &InvalidEventError{Event: name, Field: FieldAdoptedFingerprint, Rule: RuleInvalid}
	}
	if len(e.Manifest.Tools) > maxConfigManifestTools {
		return &InvalidEventError{Event: name, Field: FieldManifest, Rule: RuleInvalid}
	}
	if len(e.Manifest.AppFields) > maxConfigManifestAppFields {
		return &InvalidEventError{Event: name, Field: FieldManifest, Rule: RuleInvalid}
	}
	return nil
}

func invalidHustle(name EventName, field FieldName) *InvalidEventError {
	return &InvalidEventError{Event: name, Field: field, Rule: RuleInvalid}
}

func validateHustleRun(name EventName, run HustleRunDescriptor) error {
	if err := run.Definition.Validate(); err != nil {
		return invalidHustle(name, FieldDefinition)
	}
	if uuid.UUID(run.RunID).IsZero() {
		return invalidHustle(name, FieldRunID)
	}
	return nil
}

func validateHustleFailed(e HustleFailed) error {
	const name EventName = "HustleFailed"
	if e.Visibility() != Internal {
		return invalidHustle(name, FieldVisibility)
	}
	if err := validateHustleRun(name, e.Run); err != nil {
		return err
	}
	if e.Duration < 0 {
		return invalidHustle(name, FieldDuration)
	}
	if !e.Stage.Valid() {
		return invalidHustle(name, FieldStage)
	}
	if !e.ReasonCode.Valid() {
		return invalidHustle(name, FieldReasonCode)
	}
	if !hustle.ReasonAllowed(e.Stage, e.ReasonCode) {
		return invalidHustle(name, FieldReasonCode)
	}
	preResolution := e.Stage == hustle.StageQueue || e.Stage == hustle.StageModelResolution
	if preResolution {
		if e.Usage != nil {
			return invalidHustle(name, FieldUsage)
		}
		if !zeroModelRuntime(e.Run.Runtime) {
			return invalidHustle(name, FieldRuntime)
		}
		return nil
	}
	if err := validateModelRuntime(name, e.Run.Runtime); err != nil {
		return invalidHustle(name, FieldRuntime)
	}
	return validateOptionalUsage(name, e.Usage)
}

func validateOptionalUsage(name EventName, usage *content.Usage) error {
	if usage == nil {
		return nil
	}
	if err := usage.Validate(); err != nil {
		return invalidHustle(name, FieldUsage)
	}
	return nil
}

func zeroModelRuntime(runtime ModelRuntime) bool {
	return runtime.Key == (model.ModelKey{}) && runtime.Limits == (model.ContextLimits{}) && runtime.Effort == model.Effort("")
}

func validateStepDoneMessages(messages content.AgenticMessages) error {
	if len(messages) == 0 {
		return &InvalidEventError{Event: "StepDone", Field: FieldMessages, Rule: RuleInvalid}
	}
	first, ok := messages[0].(*content.AIMessage)
	if !ok || first == nil || first.Role != content.RoleAssistant {
		return &InvalidEventError{Event: "StepDone", Field: FieldMessages, Rule: RuleInvalid}
	}
	for _, message := range messages[1:] {
		toolResult, toolResultOK := message.(*content.ToolResultMessage)
		if !toolResultOK || toolResult == nil || toolResult.Role != content.RoleTool {
			return &InvalidEventError{Event: "StepDone", Field: FieldMessages, Rule: RuleInvalid}
		}
	}
	return nil
}

// Bounds for LoopExternalToolsetChanged. External toolsets are third-party supplied,
// so every string that reaches the journal is length-capped and the tool list is
// count-capped: a hostile or buggy MCP server must not be able to append an unbounded
// record to the durable log.
const (
	maxExternalSourceLen     = 64
	maxExternalGenerationLen = 128
	maxExternalToolNameLen   = 128
	maxExternalTools         = 512
	schemaDigestHexLen       = 64 // hex SHA-256
)

// isLowerHex reports whether s is exactly n lowercase-hex characters. The digest is
// produced by SchemaDigest, so anything else means a hand-built or tampered record.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateExternalToolset enforces the durable shape of LoopExternalToolsetChanged:
// a non-empty bounded Source and Generation, a bounded tool list, and per-tool a
// non-empty bounded Name plus a well-formed hex SHA-256 digest. Duplicate tool names
// are rejected — the slot's whole purpose is a collision-free registry, so a record
// claiming two identically named tools describes a state the runtime never installs.
// An EMPTY Tools list is valid and meaningful: it durably records a slot cleared to
// nothing.
// validateIntegrationStatus enforces IntegrationStatus's bounds and its closed
// State enum at the publish boundary.
//
// It is the whole reason the event is safe to define for a producer that lives
// outside this module. Source, Name, and Detail all originate in an integration
// Harness does not own, and Detail is the field an integration fills from a
// failure it observed — which is to say, from text a third-party server
// influenced. Bounding them here means a hostile or buggy integration cannot
// grow an event without limit, and rejecting an undeclared State means an unset
// or garbage state fails closed rather than rendering as whatever the zero value
// happens to sit next to.
func validateIntegrationStatus(e IntegrationStatus) error {
	const name EventName = "IntegrationStatus"
	if e.Source == "" {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleRequired}
	}
	if len(e.Source) > MaxIntegrationSourceBytes {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleInvalid}
	}
	if e.Name == "" {
		return &InvalidEventError{Event: name, Field: FieldIntegrationName, Rule: RuleRequired}
	}
	if len(e.Name) > MaxIntegrationNameBytes {
		return &InvalidEventError{Event: name, Field: FieldIntegrationName, Rule: RuleInvalid}
	}
	if !e.State.Valid() {
		return &InvalidEventError{Event: name, Field: FieldState, Rule: RuleInvalid}
	}
	if len(e.Detail) > MaxIntegrationDetailBytes {
		return &InvalidEventError{Event: name, Field: FieldDetail, Rule: RuleInvalid}
	}
	return nil
}

func validateExternalToolset(e LoopExternalToolsetChanged) error {
	const name EventName = "LoopExternalToolsetChanged"
	if e.Source == "" {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleRequired}
	}
	if len(e.Source) > maxExternalSourceLen {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleInvalid}
	}
	if e.Generation == "" {
		return &InvalidEventError{Event: name, Field: FieldGeneration, Rule: RuleRequired}
	}
	if len(e.Generation) > maxExternalGenerationLen {
		return &InvalidEventError{Event: name, Field: FieldGeneration, Rule: RuleInvalid}
	}
	if len(e.Tools) > maxExternalTools {
		return &InvalidEventError{Event: name, Field: FieldTools, Rule: RuleInvalid}
	}
	seen := make(map[string]struct{}, len(e.Tools))
	for _, t := range e.Tools {
		if t.Name == "" || len(t.Name) > maxExternalToolNameLen {
			return &InvalidEventError{Event: name, Field: FieldTools, Rule: RuleInvalid}
		}
		if !isLowerHex(t.SchemaDigest, schemaDigestHexLen) {
			return &InvalidEventError{Event: name, Field: FieldTools, Rule: RuleInvalid}
		}
		if _, dup := seen[t.Name]; dup {
			return &InvalidEventError{Event: name, Field: FieldTools, Rule: RuleInvalid}
		}
		seen[t.Name] = struct{}{}
	}
	return nil
}

func validateModelRuntime(name EventName, runtime ModelRuntime) error {
	if err := runtime.Key.Validate(); err != nil {
		return &InvalidEventError{Event: name, Field: FieldModelKey, Rule: RuleInvalid}
	}
	if err := runtime.Limits.Validate(); err != nil {
		return &InvalidEventError{Event: name, Field: FieldContextLimits, Rule: RuleInvalid}
	}
	if !runtime.Effort.Valid() {
		return &InvalidEventError{Event: name, Field: FieldEffort, Rule: RuleInvalid}
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

// toolExecutionID returns the body ToolExecutionID for the five tool-interaction events
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
	switch e := ev.(type) {
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
	case ConfigurationAdopted:
		// Session-scoped, same shape as SessionStarted: only SessionID set. The
		// SessionID rides in the Header; the event carries no standalone field.
		return "ConfigurationAdopted", sessionProfile(), true
	case WorkspaceCheckpointed:
		// Session-scoped: a session-global workspace snapshot appended at quiescence
		// (same shape as RestoreDone/SessionIdle) — only SessionID set. Ref is an
		// opaque payload string the validator never constrains.
		return "WorkspaceCheckpointed", sessionProfile(), true
	case WorkspaceRestored:
		return "WorkspaceRestored", sessionProfile(), true
	case ActiveLoopChanged:
		return "ActiveLoopChanged", sessionProfile(), true
	case IntegrationStatus:
		// Session-scoped: an integration is a session-global resource, not a
		// loop's. Same shape as WorkspaceCheckpointed — only SessionID set.
		return "IntegrationStatus", sessionProfile(), true
	case HustleStarted:
		return "HustleStarted", sessionProfile(), true
	case HustleCompleted:
		return "HustleCompleted", sessionProfile(), true
	case HustleFailed:
		return "HustleFailed", sessionProfile(), true
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
	case LoopExternalToolsetChanged:
		return "LoopExternalToolsetChanged", loopProfile(), true
	case ContextMeasured:
		return "ContextMeasured", loopProfile(), true
	case ContextPressure:
		return "ContextPressure", loopProfile(), true
	case CompactionStarted:
		return "CompactionStarted", loopProfile(), true
	case CompactionCommitted:
		return "CompactionCommitted", loopProfile(), true
	case CompactionRejected:
		return "CompactionRejected", loopProfile(), true
	case CompactWaiterResolved:
		return "CompactWaiterResolved", loopProfile(), true
	case CompactWaiterRejected:
		return "CompactWaiterRejected", loopProfile(), true
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
		// Gate identity varies by how the gate was raised, so the profile is selected
		// from the embedded gate's resolver rather than fixed per type. GatePrepared and
		// GateOpened carry the full gate.Gate; GateResolved carries its own Resolver tag.
		return "GatePrepared", gateIdentityProfile(e.Gate.Resolver), true
	case GateOpened:
		return "GateOpened", gateIdentityProfile(e.Gate.Resolver), true
	case GateResolved:
		return "GateResolved", gateIdentityProfile(e.Resolver), true
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

// gateIdentityProfile selects a gate event's identity contract from the resolver
// that owns it. The three gate events are loopScoped (Scope()==ScopeLoop) but the
// coordinates a gate legitimately carries depend on HOW it was raised, so a single
// per-type profile is wrong:
//
//   - Host-owned gates (gate.ResolverSession — a form/open-url elicitation raised by
//     an integration through GateHost.OpenHostGate) belong to no turn or step, and
//     a startup elicitation belongs to no loop either. Their only guaranteed
//     coordinate is the SessionID; LoopID/TurnID/StepID are OPTIONAL (a
//     loop-attributed elicitation carries a LoopID, startup carries none).
//   - Loop-owned gates (gate.ResolverLoop — permission/ask-user) keep the FULL step
//     profile: a permission gate that parks a tool call without a step is malformed
//     and must fail, exactly as before.
//
// An empty/unknown resolver fails SECURE to the strict loop-owned profile — the same
// contract every gate record enforced before host-owned gates were distinguished, so
// a record written before the discriminator existed is held to the stricter rule.
func gateIdentityProfile(resolver gate.ResolverKind) idProfile {
	if resolver == gate.ResolverSession {
		return hostGateProfile()
	}
	return stepProfile()
}

// hostGateProfile: host-owned gates require only a SessionID. LoopID/TurnID/StepID
// are unconstrained (neither required nor forbidden), so a loop-attributed
// elicitation may carry a LoopID and a startup one need not. The universal
// StepID⇒TurnID rule in checkProfile still applies. It mirrors the way
// inputCancelledProfile makes an inner coordinate optional rather than forbidden.
func hostGateProfile() idProfile {
	return idProfile{requireSession: true}
}

// toolProfile: the five tool-interaction events — full quartet set plus a required
// ToolExecutionID (read from the event body by ValidateEvent, not stored here).
func toolProfile() idProfile {
	return idProfile{
		requireSession: true, requireLoop: true, requireTurn: true, requireStep: true,
		requireTool: true,
	}
}
