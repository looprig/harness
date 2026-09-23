// Package sessionwire projects Harness events onto Core's public session wire.
package sessionwire

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/looprig/core/content"
	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/event"
)

// Projection is a canonical public event before an enduring append assigns its
// journal sequence. EventID is empty for ephemeral events, matching Core's
// EphemeralPublication contract. Body is owned by the returned value.
type Projection struct {
	TenantID  coresessionwire.TenantID
	SessionID coresessionwire.SessionID
	EventID   coresessionwire.EventID
	Class     EventClass
	Body      json.RawMessage
}

// ProjectionErrorReason is a stable failure category for callers that must
// distinguish a private value from malformed public data.
type ProjectionErrorReason string

const (
	ProjectionRejected  ProjectionErrorReason = "rejected"
	ProjectionMalformed ProjectionErrorReason = "malformed"
	ProjectionEncoding  ProjectionErrorReason = "encoding"
)

// ProjectionError reports a failed public projection without including event
// payloads, credentials, grant material, or gate answers in its text.
type ProjectionError struct {
	Type   string
	Reason ProjectionErrorReason
	Cause  error
}

func (e *ProjectionError) Error() string {
	return fmt.Sprintf("sessionwire: cannot project %s: %s", e.Type, e.Reason)
}

func (e *ProjectionError) Unwrap() error { return e.Cause }

// Project converts one explicitly admitted Harness event to a canonical Core
// public body. It deliberately accepts any so private persistence records and
// live command payloads reach a typed rejection instead of a generic JSON path.
// It never assigns a journal sequence; only a successful append may do that.
//
// It is deliberately NARROWER than event.ValidateEvent on one rule, and that
// difference is load-bearing to state: every public event now reaches the durable
// append through this function, so an event Harness's own validator calls VALID
// can be refused here. The rule is reply correlation — an event.Reply whose
// ReplyTo() (its Header.Cause.CommandID) is zero is rejected as malformed, while
// ValidateEvent imposes no such requirement on the Reply set. A caller sees the
// refusal as a *journal.MarshalRecordError from the append, not as a validation
// failure at construction.
//
// The members of that set are deliberately NOT enumerated here. The set is
// sealed by event.Reply's isReply method, Project matches on the interface, and
// TestReplyProjectionCasesMatchSealedReplyUnion derives the union from pkg/event
// source — so the interface is the authority and a prose list here can only
// drift out of it. One did: it named five members while seven implement isReply.
// Read the members off event.Reply (pkg/event/README.md lists them).
//
// Every production construction site of those events sets the causing command id,
// so no live path emits a zero today. A NEW emitter that leaves
// Header.Cause.CommandID unset will pass ValidateEvent and fail the append:
// correlate the reply with the command it answers, or relax this rule — do not
// discover the difference from a fixture.
func Project(tenantID coresessionwire.TenantID, sessionID coresessionwire.SessionID, value any) (Projection, error) {
	typeName := fmt.Sprintf("%T", value)
	class := classify(value)
	if class == PrivateRejected {
		return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionRejected}
	}
	ev, ok := value.(event.Event)
	if !ok || ev.Visibility() != event.Public {
		return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionRejected}
	}
	if err := tenantID.Validate(); err != nil {
		return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionMalformed, Cause: err}
	}
	if err := sessionID.Validate(); err != nil {
		return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionMalformed, Cause: err}
	}
	if reply, ok := ev.(event.Reply); ok && reply.ReplyTo().IsZero() {
		cause := &event.InvalidEventError{
			Event: event.EventName(reflect.TypeOf(ev).Name()),
			Field: event.FieldCommandID,
			Rule:  event.RuleRequired,
		}
		return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionMalformed, Cause: cause}
	}
	if err := validateForProjection(ev, class); err != nil {
		return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionMalformed, Cause: err}
	}

	body, err := projectBody(ev, class)
	if err != nil {
		return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionEncoding, Cause: err}
	}
	projection := Projection{TenantID: tenantID, SessionID: sessionID, Class: class, Body: body}
	if class == PublicEnduring {
		projection.EventID = coresessionwire.EventID(ev.EventHeader().EventID.String())
		// A dummy nonzero sequence validates only body/identity canonicality. It
		// does not escape and therefore cannot be mistaken for an append result.
		if err := (coresessionwire.JournalEvent{EventID: projection.EventID, JournalSeq: 1, Body: body}).Validate(); err != nil {
			return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionEncoding, Cause: err}
		}
	} else if err := (coresessionwire.EphemeralPublication{TenantID: tenantID, SessionID: sessionID, Body: body}).Validate(); err != nil {
		return Projection{}, &ProjectionError{Type: typeName, Reason: ProjectionEncoding, Cause: err}
	}
	return projection, nil
}

func validateForProjection(ev event.Event, class EventClass) error {
	if class == PublicEnduring {
		return event.ValidateEvent(ev)
	}
	switch value := ev.(type) {
	case event.TokenDelta:
		if err := validateEphemeralHeader("TokenDelta", value.Header, ephemeralProfile{requireTurn: true, forbidStep: true}); err != nil {
			return err
		}
		_, err := projectChunk(value.Chunk)
		return err
	case event.ToolCallStarted:
		if err := validateEphemeralHeader("ToolCallStarted", value.Header, ephemeralProfile{requireTurn: true, requireStep: true}); err != nil {
			return err
		}
		if value.ToolExecutionID.IsZero() {
			return invalidEphemeral("ToolCallStarted", event.FieldToolExecutionID, event.RuleRequired)
		}
		return nil
	case event.ToolCallCompleted:
		if err := validateEphemeralHeader("ToolCallCompleted", value.Header, ephemeralProfile{requireTurn: true, requireStep: true}); err != nil {
			return err
		}
		if value.ToolExecutionID.IsZero() {
			return invalidEphemeral("ToolCallCompleted", event.FieldToolExecutionID, event.RuleRequired)
		}
		return nil
	case event.InputQueued:
		return validateEphemeralHeader("InputQueued", value.Header, ephemeralProfile{forbidTurn: true, forbidStep: true})
	case event.ContextPressure:
		if err := validateEphemeralHeader("ContextPressure", value.Header, ephemeralProfile{forbidTurn: true, forbidStep: true}); err != nil {
			return err
		}
		return validateContextPressure(value)
	default:
		// CompactionStarted is deliberately minted because its progress identity
		// correlates the later enduring result. IntegrationStatus has no Harness
		// producer and retains its declared Event validation contract.
		return event.ValidateEvent(ev)
	}
}

type ephemeralProfile struct {
	requireTurn bool
	requireStep bool
	forbidTurn  bool
	forbidStep  bool
}

func validateEphemeralHeader(name string, header event.Header, profile ephemeralProfile) error {
	checks := []struct {
		bad   bool
		field event.FieldName
		rule  event.Rule
	}{
		{header.SessionID.IsZero(), event.FieldSessionID, event.RuleRequired},
		{header.LoopID.IsZero(), event.FieldLoopID, event.RuleRequired},
		{profile.requireTurn && header.TurnID.IsZero(), event.FieldTurnID, event.RuleRequired},
		{profile.forbidTurn && !header.TurnID.IsZero(), event.FieldTurnID, event.RuleMustBeZero},
		{profile.requireStep && header.StepID.IsZero(), event.FieldStepID, event.RuleRequired},
		{profile.forbidStep && !header.StepID.IsZero(), event.FieldStepID, event.RuleMustBeZero},
		{!header.StepID.IsZero() && header.TurnID.IsZero(), event.FieldTurnID, event.RuleRequired},
	}
	for _, check := range checks {
		if check.bad {
			return invalidEphemeral(name, check.field, check.rule)
		}
	}
	return nil
}

func validateContextPressure(value event.ContextPressure) error {
	if err := value.Measurement.Validate(); err != nil {
		return err
	}
	if value.Occupancy > event.FullScaleBasisPoints {
		return &event.ContextValidationError{Field: event.ContextField("Occupancy")}
	}
	if value.Previous > event.PressureHardLimit {
		return &event.ContextValidationError{Field: event.ContextField("Previous")}
	}
	if value.Current < event.PressureNormal || value.Current > event.PressureHardLimit || value.Current == value.Previous {
		return &event.ContextValidationError{Field: event.ContextField("Current")}
	}
	return nil
}

func invalidEphemeral(name string, field event.FieldName, rule event.Rule) error {
	return &event.InvalidEventError{Event: event.EventName(name), Field: field, Rule: rule}
}

func projectBody(ev event.Event, class EventClass) (json.RawMessage, error) {
	if class == PublicEphemeral {
		return projectEphemeral(ev)
	}
	encoded, err := event.MarshalEvent(ev)
	if err != nil {
		return nil, err
	}
	// The public body is the native encoding minus the per-type redactions in
	// privacy.go (GateResolved's audit, the model endpoint, the Host workspace path).
	redacted, err := redactPublicBody(ev, encoded)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(redacted), nil
}

func projectEphemeral(ev event.Event) (json.RawMessage, error) {
	switch value := ev.(type) {
	case event.TokenDelta:
		chunk, err := projectChunk(value.Chunk)
		if err != nil {
			return nil, err
		}
		return marshalPublicEvent("TokenDelta", struct {
			event.Header
			TurnIndex event.TurnIndex `json:"turn_index,omitzero"`
			Chunk     json.RawMessage `json:"chunk"`
		}{Header: value.Header, TurnIndex: value.TurnIndex, Chunk: chunk})
	case event.ToolCallStarted:
		return marshalPublicEvent("ToolCallStarted", value)
	case event.ToolCallCompleted:
		return marshalPublicEvent("ToolCallCompleted", value)
	case event.InputQueued:
		return marshalPublicEvent("InputQueued", value)
	case event.CompactionStarted:
		return marshalPublicEvent("CompactionStarted", value)
	case event.ContextPressure:
		return marshalPublicEvent("ContextPressure", value)
	case event.IntegrationStatus:
		return marshalPublicEvent("IntegrationStatus", value)
	default:
		return nil, fmt.Errorf("unrecognized ephemeral event %T", ev)
	}
}

func marshalPublicEvent(name string, value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	typeBytes, _ := json.Marshal(name)
	versionBytes, _ := json.Marshal(1)
	fields["type"] = typeBytes
	fields["v"] = versionBytes
	return json.Marshal(fields)
}

func projectChunk(chunk content.Chunk) (json.RawMessage, error) {
	switch value := chunk.(type) {
	case *content.TextChunk:
		if value == nil {
			break
		}
		return json.Marshal(struct {
			ChunkType string `json:"chunk_type"`
			Text      string `json:"text"`
		}{ChunkType: "text", Text: value.Text})
	case *content.ThinkingChunk:
		if value == nil {
			break
		}
		return json.Marshal(struct {
			ChunkType string `json:"chunk_type"`
			Thinking  string `json:"thinking"`
		}{ChunkType: "thinking", Thinking: value.Thinking})
	case *content.ToolUseChunk:
		if value == nil {
			break
		}
		// InputJSON is raw tool input and remains private. Identity is enough
		// for a public client to correlate the later durable tool events.
		return json.Marshal(struct {
			ChunkType string `json:"chunk_type"`
			Index     int    `json:"index"`
			ID        string `json:"id"`
			Name      string `json:"name"`
		}{ChunkType: "tool_use", Index: value.Index, ID: value.ID, Name: value.Name})
	case *content.RefusalChunk:
		if value == nil {
			break
		}
		return json.Marshal(struct {
			ChunkType string `json:"chunk_type"`
			Text      string `json:"text"`
		}{ChunkType: "refusal", Text: value.Text})
	case *content.ImageChunk:
		if value == nil {
			break
		}
		return json.Marshal(struct {
			ChunkType string            `json:"chunk_type"`
			Index     int               `json:"index"`
			MediaType content.MediaType `json:"media_type,omitempty"`
			URL       string            `json:"url,omitempty"`
			Data      []byte            `json:"data,omitempty"`
		}{ChunkType: "image", Index: value.Index, MediaType: value.MediaType, URL: value.Source.URL, Data: value.Source.Data})
	}
	return nil, fmt.Errorf("unrecognized token chunk %T", chunk)
}
