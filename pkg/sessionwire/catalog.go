package sessionwire

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// ReadAuthority is the caller-owned authorization scope used for multi-session
// catalog projections. Tenant identity is deliberately required even though a
// Core SessionSummary does not repeat it: the adapter must never silently
// manufacture tenancy from a Harness UUID.
type ReadAuthority struct {
	TenantID coresessionwire.TenantID
	AgentID  coresessionwire.AgentID
}

// ReadScope binds one durable Harness session to its caller-owned Core identity
// and current residency. Execution State and Residency remain independent: a
// running durable projection can be read while cold without restoring it.
type ReadScope struct {
	TenantID  coresessionwire.TenantID
	SessionID coresessionwire.SessionID
	AgentID   coresessionwire.AgentID
	Residency coresessionwire.SessionResidency
	// RuntimeSessionID is the Harness session (rig) id the scoped events were
	// produced under, when that is not the Core SessionID's rendering. It is
	// OPTIONAL. Zero keeps the original rule exactly: an event is in scope when
	// its SessionID renders as SessionID. Non-zero REPLACES that rule: an event is
	// in scope when its SessionID equals RuntimeSessionID, and SessionID is then
	// only the Core identity the projection is answered under.
	//
	// It exists because a Core SessionID is opaque and a Factory binds the rig to a
	// DERIVED id, so under a Factory the two never match and no event could be
	// projected. It affects which events are in scope and nothing else: a page
	// projected with it set is byte-identical to one projected without it over the
	// same events. Only the event projections consult it (ProjectJournalPage,
	// ProjectGatePage); the catalog projections compare catalog records, which carry
	// the Core id.
	RuntimeSessionID uuid.UUID
}

// CatalogState is the closed Harness catalog vocabulary accepted by this
// projection boundary. It is local to the adapter so pkg/sessionwire remains
// below pkg/sessionstore and can later be called from the persistence path.
type CatalogState string

const (
	CatalogStateRunning       CatalogState = "running"
	CatalogStateWaitingOnGate CatalogState = "waiting_on_gate"
	CatalogStateIdle          CatalogState = "idle"
	CatalogStateFailed        CatalogState = "failed"
	CatalogStateInterrupted   CatalogState = "interrupted"
	CatalogStateStopped       CatalogState = "stopped"
)

// CatalogRecord is the narrow Harness read-domain record needed to produce
// Core summaries/status. It intentionally excludes runtime and storage types.
type CatalogRecord struct {
	SessionID      coresessionwire.SessionID
	State          CatalogState
	Title          string
	CreatedAt      time.Time
	LastActiveAt   time.Time
	LastJournalSeq uint64
	WaitingGateID  coresessionwire.GateID
}

// ReadProjectionError reports a malformed or mismatched read projection. Its
// text names only the projection and field; it never includes catalog, event,
// prompt, tool, or authorization payload bytes.
type ReadProjectionError struct {
	Projection string
	Field      string
	Cause      error
}

func (e *ReadProjectionError) Error() string {
	return fmt.Sprintf("sessionwire: cannot project %s: invalid %s", e.Projection, e.Field)
}

func (e *ReadProjectionError) Unwrap() error { return e.Cause }

// JournalRecord pairs a public Harness event with the sequence assigned by its
// successful enduring append.
type JournalRecord struct {
	JournalSeq uint64
	Event      event.Event
}

// OpenGate supplies the read-plane facts not owned by a GateOpened event: the
// append sequence, effective deadline, and current answerability.
type OpenGate struct {
	Event         event.GateOpened
	JournalSeq    uint64
	Deadline      time.Time
	Answerability coresessionwire.GateAnswerability
}

// ValidateReadAuthority validates caller-owned multi-session authority before a
// catalog provider is touched.
func ValidateReadAuthority(authority ReadAuthority) error {
	return validateAuthority("read authority", authority)
}

// ValidateReadScope validates caller-owned single-session authority and
// residency before a session provider is touched.
func ValidateReadScope(scope ReadScope) error {
	return validateScope("read scope", scope)
}

func validateAuthority(projection string, authority ReadAuthority) error {
	if err := authority.TenantID.Validate(); err != nil {
		return &ReadProjectionError{Projection: projection, Field: "tenant_id", Cause: err}
	}
	if err := authority.AgentID.Validate(); err != nil {
		return &ReadProjectionError{Projection: projection, Field: "agent_id", Cause: err}
	}
	return nil
}

func validateScope(projection string, scope ReadScope) error {
	if err := validateAuthority(projection, ReadAuthority{TenantID: scope.TenantID, AgentID: scope.AgentID}); err != nil {
		return err
	}
	if err := scope.SessionID.Validate(); err != nil {
		return &ReadProjectionError{Projection: projection, Field: "session_id", Cause: err}
	}
	switch scope.Residency {
	case coresessionwire.SessionResidencyCold,
		coresessionwire.SessionResidencyAttaching,
		coresessionwire.SessionResidencyResident,
		coresessionwire.SessionResidencyReleasing:
		return nil
	default:
		return &ReadProjectionError{Projection: projection, Field: "residency"}
	}
}

func projectState(projection string, state CatalogState) (coresessionwire.SessionState, error) {
	switch state {
	case CatalogStateRunning:
		return coresessionwire.SessionStateRunning, nil
	case CatalogStateWaitingOnGate:
		return coresessionwire.SessionStateWaitingOnGate, nil
	case CatalogStateIdle:
		return coresessionwire.SessionStateIdle, nil
	case CatalogStateFailed:
		return coresessionwire.SessionStateFailed, nil
	case CatalogStateInterrupted:
		return coresessionwire.SessionStateInterrupted, nil
	case CatalogStateStopped:
		return coresessionwire.SessionStateStopped, nil
	default:
		return "", &ReadProjectionError{Projection: projection, Field: "state"}
	}
}

func validateCatalogSession(projection string, scope ReadScope, record CatalogRecord) error {
	if record.SessionID == "" || record.SessionID != scope.SessionID {
		return &ReadProjectionError{Projection: projection, Field: "session_id"}
	}
	return nil
}

func validateEventScope(projection string, scope ReadScope, value event.Event) error {
	if value == nil {
		return &ReadProjectionError{Projection: projection, Field: "event"}
	}
	reflected := reflect.ValueOf(value)
	if reflected.Kind() == reflect.Pointer && reflected.IsNil() {
		return &ReadProjectionError{Projection: projection, Field: "event"}
	}
	header := value.EventHeader()
	if header.SessionID.IsZero() {
		return &ReadProjectionError{Projection: projection, Field: "session_id"}
	}
	if !scope.RuntimeSessionID.IsZero() {
		if header.SessionID != scope.RuntimeSessionID {
			return &ReadProjectionError{Projection: projection, Field: "session_id"}
		}
		return nil
	}
	if coresessionwire.SessionID(header.SessionID.String()) != scope.SessionID {
		return &ReadProjectionError{Projection: projection, Field: "session_id"}
	}
	return nil
}

// ProjectSessionSummary converts one durable catalog entry to Core's recent
// session record without consulting or restoring a runtime.
func ProjectSessionSummary(scope ReadScope, record CatalogRecord) (coresessionwire.SessionSummary, error) {
	const projection = "session summary"
	if err := validateScope(projection, scope); err != nil {
		return coresessionwire.SessionSummary{}, err
	}
	if err := validateCatalogSession(projection, scope, record); err != nil {
		return coresessionwire.SessionSummary{}, err
	}
	state, err := projectState(projection, record.State)
	if err != nil {
		return coresessionwire.SessionSummary{}, err
	}
	out := coresessionwire.SessionSummary{
		SessionID: scope.SessionID, AgentID: scope.AgentID, State: state,
		Title: record.Title, CreatedAt: record.CreatedAt, LastActiveAt: projectedActivityTime(record),
	}
	if err := out.Validate(); err != nil {
		return coresessionwire.SessionSummary{}, &ReadProjectionError{Projection: projection, Field: "record", Cause: err}
	}
	return out, nil
}

// ProjectSessionStatus converts replay-free catalog state to Core's durable
// status while preserving the caller-supplied residency dimension.
func ProjectSessionStatus(scope ReadScope, record CatalogRecord) (coresessionwire.SessionStatus, error) {
	const projection = "session status"
	if err := validateScope(projection, scope); err != nil {
		return coresessionwire.SessionStatus{}, err
	}
	if err := validateCatalogSession(projection, scope, record); err != nil {
		return coresessionwire.SessionStatus{}, err
	}
	state, err := projectState(projection, record.State)
	if err != nil {
		return coresessionwire.SessionStatus{}, err
	}
	out := coresessionwire.SessionStatus{
		SessionID: scope.SessionID, AgentID: scope.AgentID, State: state,
		Residency: scope.Residency, JournalTip: record.LastJournalSeq, UpdatedAt: projectedActivityTime(record),
	}
	if record.WaitingGateID != "" {
		out.WaitingGateID = record.WaitingGateID
	}
	if err := out.Validate(); err != nil {
		return coresessionwire.SessionStatus{}, &ReadProjectionError{Projection: projection, Field: "record", Cause: err}
	}
	return out, nil
}

func projectedActivityTime(record CatalogRecord) time.Time {
	if !record.LastActiveAt.IsZero() {
		return record.LastActiveAt
	}
	return record.CreatedAt
}

// ProjectSessionPage converts an already bounded, recent-first catalog window.
// Cursor creation remains the storage/read provider's responsibility.
func ProjectSessionPage(authority ReadAuthority, records []CatalogRecord, next, previous coresessionwire.Cursor) (coresessionwire.SessionPage, error) {
	const projection = "session page"
	if err := validateAuthority(projection, authority); err != nil {
		return coresessionwire.SessionPage{}, err
	}
	out := coresessionwire.SessionPage{Sessions: make([]coresessionwire.SessionSummary, 0, len(records)), NextCursor: next, PreviousCursor: previous}
	for _, record := range records {
		summary, err := ProjectSessionSummary(ReadScope{
			TenantID: authority.TenantID, SessionID: record.SessionID,
			AgentID: authority.AgentID, Residency: coresessionwire.SessionResidencyCold,
		}, record)
		if err != nil {
			return coresessionwire.SessionPage{}, err
		}
		out.Sessions = append(out.Sessions, summary)
	}
	if err := out.Validate(); err != nil {
		return coresessionwire.SessionPage{}, &ReadProjectionError{Projection: projection, Field: "record", Cause: err}
	}
	return out, nil
}

// ProjectJournalPage converts public Harness events to canonical Core records.
// CapturedTip and CoveredThrough are authenticated provider metadata and are
// never inferred from the visible event count.
func ProjectJournalPage(scope ReadScope, records []JournalRecord, capturedTip, coveredThrough uint64, next, previous coresessionwire.Cursor) (coresessionwire.JournalPage, error) {
	const projection = "journal page"
	if err := validateScope(projection, scope); err != nil {
		return coresessionwire.JournalPage{}, err
	}
	out := coresessionwire.JournalPage{
		Events: make([]coresessionwire.JournalEvent, 0, len(records)), CapturedTip: capturedTip,
		CoveredThrough: coveredThrough, NextCursor: next, PreviousCursor: previous,
	}
	for _, record := range records {
		if err := validateEventScope(projection, scope, record.Event); err != nil {
			return coresessionwire.JournalPage{}, err
		}
		projected, err := Project(scope.TenantID, scope.SessionID, record.Event)
		if err != nil {
			return coresessionwire.JournalPage{}, &ReadProjectionError{Projection: projection, Field: "event", Cause: err}
		}
		item := coresessionwire.JournalEvent{EventID: projected.EventID, JournalSeq: record.JournalSeq, Body: projected.Body}
		if err := item.Validate(); err != nil {
			return coresessionwire.JournalPage{}, &ReadProjectionError{Projection: projection, Field: "event", Cause: err}
		}
		out.Events = append(out.Events, item)
	}
	if err := out.Validate(); err != nil {
		return coresessionwire.JournalPage{}, &ReadProjectionError{Projection: projection, Field: "record", Cause: err}
	}
	return out, nil
}

// ProjectGatePage maps already selected open gates into Core's presentation-safe
// projection. Harness Gate.Subject and ResponsePolicy are intentionally absent.
func ProjectGatePage(scope ReadScope, gates []OpenGate, journalTip, openGateCount uint64, next, previous coresessionwire.Cursor) (coresessionwire.GatePage, error) {
	const projection = "gate page"
	if err := validateScope(projection, scope); err != nil {
		return coresessionwire.GatePage{}, err
	}
	out := coresessionwire.GatePage{
		JournalTip: journalTip, OpenGateCount: openGateCount,
		Gates: make([]coresessionwire.GateProjection, 0, len(gates)), NextCursor: next, PreviousCursor: previous,
	}
	for _, item := range gates {
		if err := validateEventScope(projection, scope, item.Event); err != nil {
			return coresessionwire.GatePage{}, err
		}
		if _, err := Project(scope.TenantID, scope.SessionID, item.Event); err != nil {
			return coresessionwire.GatePage{}, &ReadProjectionError{Projection: projection, Field: "event", Cause: err}
		}
		projected := coresessionwire.GateProjection{
			GateID: coresessionwire.GateID(item.Event.Gate.ID.String()), Kind: string(item.Event.Gate.Kind),
			Prompt:        projectGatePrompt(item.Event),
			OpenedEventID: coresessionwire.EventID(item.Event.EventID.String()), OpenedJournalSeq: item.JournalSeq,
			Deadline: item.Deadline, Answerability: item.Answerability,
		}
		if err := projected.Validate(); err != nil {
			return coresessionwire.GatePage{}, &ReadProjectionError{Projection: projection, Field: "gate", Cause: err}
		}
		out.Gates = append(out.Gates, projected)
	}
	if err := out.Validate(); err != nil {
		return coresessionwire.GatePage{}, &ReadProjectionError{Projection: projection, Field: "record", Cause: err}
	}
	return out, nil
}

func projectGatePrompt(opened event.GateOpened) coresessionwire.GatePrompt {
	prompt := opened.Gate.Prompt
	out := coresessionwire.GatePrompt{
		Title: prompt.Title, Body: prompt.Body, Origin: prompt.Origin,
		Schema:   coresessionwire.GatePromptSchema{Fields: make([]coresessionwire.GatePromptField, 0, len(prompt.Schema.Fields))},
		Controls: make([]coresessionwire.GateControl, 0, len(prompt.Controls)),
	}
	for _, field := range prompt.Schema.Fields {
		projected := coresessionwire.GatePromptField{
			Name: field.Name, Label: field.Label, Kind: coresessionwire.GateFieldKind(field.Kind), Required: field.Required,
			Options: make([]coresessionwire.GatePromptOption, 0, len(field.Options)), Default: cloneRaw(field.Default),
		}
		for _, option := range field.Options {
			projected.Options = append(projected.Options, coresessionwire.GatePromptOption{Value: option.Value, Label: option.Label})
		}
		out.Schema.Fields = append(out.Schema.Fields, projected)
	}
	for _, control := range prompt.Controls {
		out.Controls = append(out.Controls, coresessionwire.GateControl{Action: control.Action, Label: control.Label})
	}
	return out
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	if value == nil {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}
