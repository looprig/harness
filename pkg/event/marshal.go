package event

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// schemaVersion is the current wire-envelope schema version stamped into every
// encoded event under the "v" key. It starts at 1; bump it (never reuse a number)
// when the on-disk envelope shape changes incompatibly, so a future reader can
// branch on the version of an old journal record. It is part of the durable wire
// contract.
const schemaVersion = 1

// maxEventBytes caps the serialized size UnmarshalEvent accepts at the untrusted
// restore boundary. An event's payload is small (header ids + capped strings) plus
// at most a committed step group, which is bounded by the block codec's own caps;
// this envelope cap fails closed on absurd top-level input before any delegated
// decode runs. Conservative starting value; tune to real history sizes later.
const maxEventBytes = 16 << 20 // 16 MiB

// EphemeralNotPersistableError is returned by MarshalEvent when handed an
// Ephemeral event (TokenDelta, ToolCallStarted, ToolCallCompleted, InputQueued).
// The Ephemeral set is never persisted — it self-heals from a later authoritative
// event and TokenDelta.Chunk has no durable codec — so the marshaler fails closed
// rather than emit a lossy record. Type is the classify name of the rejected event
// so a caller learns exactly which event it tried to persist.
type EphemeralNotPersistableError struct{ Type string }

func (e *EphemeralNotPersistableError) Error() string {
	return fmt.Sprintf("event: %s is Ephemeral and not persistable", e.Type)
}

// UnknownEventTypeError is returned by UnmarshalEvent when the envelope's "type"
// tag names no concrete event (including the empty/missing tag), or by
// MarshalEvent when a foreign concrete type is handed in (one not in classify's
// sealed union). The restore path is an untrusted boundary; callers fail secure on
// this error rather than guess a concrete event to reconstruct.
type UnknownEventTypeError struct{ Type string }

func (e *UnknownEventTypeError) Error() string {
	return fmt.Sprintf("event: unknown event type %q", e.Type)
}

// EventEncodeError wraps a failure to marshal an event's payload (a json.Marshal
// failure, or a delegated content/tool codec failure on the marshal path).
type EventEncodeError struct {
	Type  string
	Cause error
}

func (e *EventEncodeError) Error() string {
	return fmt.Sprintf("event: encode %s: %v", e.Type, e.Cause)
}
func (e *EventEncodeError) Unwrap() error { return e.Cause }

// EventDecodeError wraps a failure to unmarshal serialized event bytes (malformed
// JSON, wrong field types) once a known "type" tag has been read.
type EventDecodeError struct {
	Type  string
	Cause error
}

func (e *EventDecodeError) Error() string {
	return fmt.Sprintf("event: decode %s: %v", e.Type, e.Cause)
}
func (e *EventDecodeError) Unwrap() error { return e.Cause }

// EventLimitError is returned when serialized input exceeds the envelope byte cap.
type EventLimitError struct {
	Got int
	Max int
}

func (e *EventLimitError) Error() string {
	return fmt.Sprintf("event: input exceeds byte cap (%d > %d)", e.Got, e.Max)
}

// MarshalEvent encodes an Enduring event into the durable wire envelope: a JSON
// object carrying a "type" discriminator (== the classify name, the package's
// single naming source of truth), a "v" schema version, the embedded Header
// fields, and the type-specific payload. It fails closed on an Ephemeral event
// (EphemeralNotPersistableError) and on a type outside the sealed union
// (UnknownEventTypeError). The interface-valued fields that have no general codec
// (PermissionRequested.Request, TurnFailed.Err, RestoreErrored.Err) are projected
// onto durable forms; every other field round-trips through encoding/json.
func MarshalEvent(ev Event) ([]byte, error) {
	name, _, ok := classify(ev)
	if !ok {
		return nil, &UnknownEventTypeError{Type: name}
	}
	if ev.Class() == Ephemeral {
		return nil, &EphemeralNotPersistableError{Type: name}
	}
	// Unknown checkpoint metadata exists solely as the in-memory projection of a
	// legacy record. Never emit it into a current journal (nor emit a checkpoint
	// whose trigger/cause pairing is malformed).
	if _, checkpoint := ev.(WorkspaceCheckpointed); checkpoint {
		if err := validateEventBody(ev); err != nil {
			return nil, err
		}
	}
	payload, err := encodePayload(ev)
	if err != nil {
		return nil, err
	}
	return mergeEnvelope(name, payload)
}

// encodePayload marshals the concrete event's wire form (header + type-specific
// fields, with interface fields projected). It is the single per-type marshal
// dispatch; classify has already proven ev is in the union, so the default arm is
// unreachable but fails secure with UnknownEventTypeError rather than panicking.
func encodePayload(ev Event) ([]byte, error) {
	switch e := ev.(type) {
	case StepDone:
		return marshalStepDone(e)
	case PermissionRequested:
		return marshalPermissionRequested(e)
	case TurnFailed:
		return marshalTurnFailed(e)
	case RestoreErrored:
		return marshalRestoreErrored(e)
	case GateResolved:
		return marshalGateResolved(e)
	case SessionStarted, SessionActive, SessionIdle, SessionStopped,
		RestoreStarted, RestoreDone, WorkspaceCheckpointed, WorkspaceRestored,
		ActiveLoopChanged, SecurityCeilingChanged,
		LoopIdle, LoopStarted, LoopInferenceChanged, LoopModeChanged,
		ForeignSessionBound, TurnRejected,
		UserInputRequested, TurnInterrupted,
		TurnStarted, TurnFoldedInto, InputCancelled, TurnDone,
		PermissionDecided, GatePrepared, GateOpened:
		// Every field round-trips through encoding/json directly: header + scalars/
		// strings/slices, and for the Message-bearing four (TurnStarted/TurnFoldedInto/
		// InputCancelled/TurnDone) the content.Message codec tags nested blocks. The
		// four interface-field events (StepDone/PermissionRequested/TurnFailed/
		// RestoreErrored) are handled above; this arm is the lossless-plain remainder.
		return marshalPlain(ev)
	default:
		name, _, _ := classify(ev)
		return nil, &UnknownEventTypeError{Type: name}
	}
}

// marshalPlain marshals an event whose fields all round-trip through encoding/json
// directly. The Message-bearing events (TurnStarted/TurnFoldedInto/InputCancelled/
// TurnDone) qualify: content.Message/ToolResultMessage define their own MarshalJSON
// that tags nested blocks, so a *content.UserMessage / *content.AIMessage value
// serializes losslessly here.
func marshalPlain(ev Event) ([]byte, error) {
	name, _, _ := classify(ev)
	out, err := json.Marshal(ev)
	if err != nil {
		return nil, &EventEncodeError{Type: name, Cause: err}
	}
	return out, nil
}

// stepDoneWire is StepDone's wire form: the header is promoted and Messages is
// pre-encoded by the message-slice codec (content.AgenticMessages is a []sealed-
// interface with no general struct codec, so it cannot ride as a plain field).
type stepDoneWire struct {
	Header
	Messages json.RawMessage `json:"messages,omitempty"`
}

func marshalStepDone(e StepDone) ([]byte, error) {
	var msgs json.RawMessage
	if len(e.Messages) > 0 {
		m, err := marshalMessages(e.Messages)
		if err != nil {
			return nil, &EventEncodeError{Type: "StepDone", Cause: err}
		}
		msgs = m
	}
	out, err := json.Marshal(stepDoneWire{Header: e.Header, Messages: msgs})
	if err != nil {
		return nil, &EventEncodeError{Type: "StepDone", Cause: err}
	}
	return out, nil
}

// permissionRequestedWire is PermissionRequested's wire form: the Request sealed
// interface is persisted IN FULL via tool.MarshalPermissionRequest (header-only
// would panic on TUI replay), pre-encoded into a sibling "request" key.
type permissionRequestedWire struct {
	Header
	ToolExecutionID uuid.UUID       `json:"tool_execution_id,omitzero"`
	Request         json.RawMessage `json:"request,omitempty"`
}

func marshalPermissionRequested(e PermissionRequested) ([]byte, error) {
	var req json.RawMessage
	if e.Request != nil {
		r, err := tool.MarshalPermissionRequest(e.Request)
		if err != nil {
			return nil, &EventEncodeError{Type: "PermissionRequested", Cause: err}
		}
		req = r
	}
	out, err := json.Marshal(permissionRequestedWire{
		Header:          e.Header,
		ToolExecutionID: e.ToolExecutionID,
		Request:         req,
	})
	if err != nil {
		return nil, &EventEncodeError{Type: "PermissionRequested", Cause: err}
	}
	return out, nil
}

// turnFailedWire is TurnFailed's wire form: the Err interface has no general codec,
// so it is projected to a stable {kind,message} pair (RestoredError) — kind from
// ErrKind, message from Err.Error() — preserving the human-readable cause even when
// the concrete type cannot survive.
type turnFailedWire struct {
	Header
	TurnIndex TurnIndex      `json:"turn_index,omitzero"`
	Err       *RestoredError `json:"err,omitempty"`
}

func marshalTurnFailed(e TurnFailed) ([]byte, error) {
	out, err := json.Marshal(turnFailedWire{
		Header:    e.Header,
		TurnIndex: e.TurnIndex,
		Err:       projectError(e.Err),
	})
	if err != nil {
		return nil, &EventEncodeError{Type: "TurnFailed", Cause: err}
	}
	return out, nil
}

// restoreErroredWire mirrors turnFailedWire for the session-scoped restore failure:
// Err projects to the same {kind,message} RestoredError pair.
type restoreErroredWire struct {
	Header
	Err *RestoredError `json:"err,omitempty"`
}

func marshalRestoreErrored(e RestoreErrored) ([]byte, error) {
	out, err := json.Marshal(restoreErroredWire{
		Header: e.Header,
		Err:    projectError(e.Err),
	})
	if err != nil {
		return nil, &EventEncodeError{Type: "RestoreErrored", Cause: err}
	}
	return out, nil
}

// projectError projects a live error to its durable {kind,message} form. A nil
// error projects to KindUnknown with an empty message (an absent cause), so the
// restored event always carries a *RestoredError rather than a typed-nil — matching
// the reconstructed-on-unmarshal contract the round-trip test asserts. An already-
// restored *RestoredError (the decode form, re-marshaled by journal compaction /
// checkpoint re-persist) copies its fields directly rather than calling Error():
// (*RestoredError).Error() renders "<kind>: <message>", so re-projecting through it
// would accrete a "<kind>: " prefix onto Message on every cycle. Copying makes
// re-marshal a fixed point — Kind AND Message stable across any number of round-trips
// (ErrKind already keeps Kind stable the same way).
func projectError(err error) *RestoredError {
	if err == nil {
		return &RestoredError{Kind: KindUnknown, Message: ""}
	}
	var restored *RestoredError
	if errors.As(err, &restored) {
		return &RestoredError{Kind: restored.Kind, Message: restored.Message}
	}
	return &RestoredError{Kind: ErrKind(err), Message: err.Error()}
}

// mergeEnvelope merges the type discriminator and schema version into a pre-encoded
// payload object as sibling keys, mirroring the block/permission-request codecs (a
// merge — never an embedding wrapper, which would let a payload field shadow "type"
// or "v"). The payload is always a JSON object here (every event marshals to one).
func mergeEnvelope(name string, payload []byte) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, &EventEncodeError{Type: name, Cause: err}
	}
	typeJSON, _ := json.Marshal(name)             // a string; cannot fail
	versionJSON, _ := json.Marshal(schemaVersion) // an int; cannot fail
	fields["type"] = typeJSON
	fields["v"] = versionJSON
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &EventEncodeError{Type: name, Cause: err}
	}
	return out, nil
}

// UnmarshalEvent decodes a durable wire envelope back into a concrete Event. It
// fails closed on the untrusted restore boundary: input over the byte cap →
// EventLimitError; malformed envelope → EventDecodeError; an unknown or missing
// "type" tag → UnknownEventTypeError; a malformed payload for a known type →
// EventDecodeError. A successfully decoded event is validated against the ID fill
// matrix (ValidateEvent), so a structurally-valid but semantically-invalid record
// is rejected rather than resurrected.
func UnmarshalEvent(data []byte) (Event, error) {
	if len(data) > maxEventBytes {
		return nil, &EventLimitError{Got: len(data), Max: maxEventBytes}
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &EventDecodeError{Type: "", Cause: err}
	}
	ev, err := decodePayload(probe.Type, data)
	if err != nil {
		return nil, err
	}
	if err := validateDecodedEvent(ev, data); err != nil {
		return nil, err
	}
	return ev, nil
}

// validateDecodedEvent preserves the one additive compatibility exception in the
// event schema: checkpoints written before consistency/trigger existed decode with
// both enum values unknown. Explicit zero values, partial metadata, and newly
// constructed unknown-valued checkpoints remain invalid through ValidateEvent.
func validateDecodedEvent(ev Event, data []byte) error {
	if err := validateEventIdentity(ev); err != nil {
		return err
	}
	if _, ok := ev.(WorkspaceCheckpointed); ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return &EventDecodeError{Type: "WorkspaceCheckpointed", Cause: err}
		}
		_, hasConsistency := fields["consistency"]
		_, hasTrigger := fields["trigger"]
		if !hasConsistency && !hasTrigger {
			return nil
		}
	}
	return validateEventBody(ev)
}

// decodePayload dispatches on the "type" tag to the concrete decoder. An unknown or
// empty tag fails secure with UnknownEventTypeError; a malformed payload surfaces as
// EventDecodeError from the per-type decoder.
func decodePayload(tag string, data []byte) (Event, error) {
	switch tag {
	case "SessionStarted":
		return decodePlain[SessionStarted](tag, data)
	case "SessionActive":
		return decodePlain[SessionActive](tag, data)
	case "SessionIdle":
		return decodePlain[SessionIdle](tag, data)
	case "SessionStopped":
		return decodePlain[SessionStopped](tag, data)
	case "RestoreStarted":
		return decodePlain[RestoreStarted](tag, data)
	case "RestoreDone":
		return decodePlain[RestoreDone](tag, data)
	case "RestoreErrored":
		return decodeRestoreErrored(data)
	case "WorkspaceCheckpointed":
		return decodePlain[WorkspaceCheckpointed](tag, data)
	case "WorkspaceRestored":
		return decodePlain[WorkspaceRestored](tag, data)
	case "ActiveLoopChanged":
		return decodePlain[ActiveLoopChanged](tag, data)
	case "SecurityCeilingChanged":
		return decodePlain[SecurityCeilingChanged](tag, data)
	case "LoopIdle":
		return decodePlain[LoopIdle](tag, data)
	case "LoopStarted":
		return decodePlain[LoopStarted](tag, data)
	case "LoopInferenceChanged":
		return decodePlain[LoopInferenceChanged](tag, data)
	case "LoopModeChanged":
		return decodePlain[LoopModeChanged](tag, data)
	case "ForeignSessionBound":
		return decodePlain[ForeignSessionBound](tag, data)
	case "TurnStarted":
		return decodePlain[TurnStarted](tag, data)
	case "StepDone":
		return decodeStepDone(data)
	case "TurnFoldedInto":
		return decodePlain[TurnFoldedInto](tag, data)
	case "InputCancelled":
		return decodePlain[InputCancelled](tag, data)
	case "TurnRejected":
		return decodePlain[TurnRejected](tag, data)
	case "TurnDone":
		return decodePlain[TurnDone](tag, data)
	case "TurnFailed":
		return decodeTurnFailed(data)
	case "TurnInterrupted":
		return decodePlain[TurnInterrupted](tag, data)
	case "PermissionRequested":
		return decodePermissionRequested(data)
	case "PermissionDecided":
		return decodePlain[PermissionDecided](tag, data)
	case "UserInputRequested":
		return decodePlain[UserInputRequested](tag, data)
	case "GatePrepared":
		return decodePlain[GatePrepared](tag, data)
	case "GateOpened":
		return decodePlain[GateOpened](tag, data)
	case "GateResolved":
		return decodeGateResolved(data)
	default:
		return nil, &UnknownEventTypeError{Type: tag}
	}
}

// decodePlain decodes an event whose fields all round-trip through encoding/json
// directly (the inverse of marshalPlain). The extra "type"/"v" envelope keys are
// ignored by the struct decode.
func decodePlain[T any](tag string, data []byte) (Event, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, &EventDecodeError{Type: tag, Cause: err}
	}
	ev, ok := any(v).(Event)
	if !ok {
		// Unreachable: every T in the dispatch is a concrete Event. Fail secure.
		return nil, &UnknownEventTypeError{Type: tag}
	}
	return ev, nil
}

func decodeStepDone(data []byte) (Event, error) {
	var w stepDoneWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "StepDone", Cause: err}
	}
	var msgs content.AgenticMessages
	if len(w.Messages) > 0 {
		m, err := unmarshalMessages(w.Messages)
		if err != nil {
			return nil, &EventDecodeError{Type: "StepDone", Cause: err}
		}
		msgs = m
	}
	return StepDone{Header: w.Header, Messages: msgs}, nil
}

func decodePermissionRequested(data []byte) (Event, error) {
	var w permissionRequestedWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "PermissionRequested", Cause: err}
	}
	ev := PermissionRequested{Header: w.Header, ToolExecutionID: w.ToolExecutionID}
	if len(w.Request) > 0 {
		req, err := tool.UnmarshalPermissionRequest(w.Request)
		if err != nil {
			return nil, &EventDecodeError{Type: "PermissionRequested", Cause: err}
		}
		ev.Request = req
	}
	return ev, nil
}

func decodeTurnFailed(data []byte) (Event, error) {
	var w turnFailedWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "TurnFailed", Cause: err}
	}
	ev := TurnFailed{Header: w.Header, TurnIndex: w.TurnIndex}
	if w.Err != nil {
		ev.Err = w.Err
	}
	return ev, nil
}

func decodeRestoreErrored(data []byte) (Event, error) {
	var w restoreErroredWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "RestoreErrored", Cause: err}
	}
	ev := RestoreErrored{Header: w.Header}
	if w.Err != nil {
		ev.Err = w.Err
	}
	return ev, nil
}

// gateResolvedWire is GateResolved's wire form: the Audit sealed interface is
// persisted IN FULL via gate.MarshalResponseAudit (a bare interface would marshal
// to lossy, un-keyed output), pre-encoded into a sibling "audit" key. A nil Audit
// serializes to an absent key.
type gateResolvedWire struct {
	Header
	GateID        gate.ID             `json:"gate_id,omitzero"`
	Reason        gate.CloseReason    `json:"reason,omitempty"`
	Action        string              `json:"action,omitempty"`
	ApprovalScope tool.ApprovalScope  `json:"scope,omitzero"`
	Source        gate.ResponseSource `json:"source,omitzero"`
	Audit         json.RawMessage     `json:"audit,omitempty"`
}

func marshalGateResolved(e GateResolved) ([]byte, error) {
	var auditJSON json.RawMessage
	if e.Audit != nil {
		a, err := gate.MarshalResponseAudit(e.Audit)
		if err != nil {
			return nil, &EventEncodeError{Type: "GateResolved", Cause: err}
		}
		auditJSON = a
	}
	out, err := json.Marshal(gateResolvedWire{
		Header:        e.Header,
		GateID:        e.GateID,
		Reason:        e.Reason,
		Action:        e.Action,
		ApprovalScope: e.ApprovalScope,
		Source:        e.Source,
		Audit:         auditJSON,
	})
	if err != nil {
		return nil, &EventEncodeError{Type: "GateResolved", Cause: err}
	}
	return out, nil
}

func decodeGateResolved(data []byte) (Event, error) {
	var w gateResolvedWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "GateResolved", Cause: err}
	}
	ev := GateResolved{
		Header:        w.Header,
		GateID:        w.GateID,
		Reason:        w.Reason,
		Action:        w.Action,
		ApprovalScope: w.ApprovalScope,
		Source:        w.Source,
	}
	if len(w.Audit) > 0 {
		audit, err := gate.UnmarshalResponseAudit(w.Audit)
		if err != nil {
			return nil, &EventDecodeError{Type: "GateResolved", Cause: err}
		}
		ev.Audit = audit
	}
	return ev, nil
}

// maxMessagesPerStep caps the element count of a committed step group accepted at
// the untrusted restore boundary. A step group is one AIMessage plus its tool
// results, so a generous cap still fails closed on absurd input. Each message's
// blocks are independently capped by the content block codec.
const maxMessagesPerStep = 10_000

// UnknownMessageRoleError is returned by the message-slice decoder when a message's
// "role" names no concrete Conversation type (including an empty/missing role). The
// Conversation union is sealed and discriminated by role; an unknown role fails
// closed rather than guess a concrete message to reconstruct.
type UnknownMessageRoleError struct{ Role string }

func (e *UnknownMessageRoleError) Error() string {
	return fmt.Sprintf("event: unknown message role %q", e.Role)
}

// marshalMessages encodes a content.AgenticMessages (an ordered []Conversation, a
// sealed interface that has no general struct codec) as a JSON array. Each element
// is marshaled by its own MarshalJSON, which stamps the "role" discriminator and
// tags nested blocks via the content block codec — so the array is self-describing
// and the decoder can reconstruct the concrete type from role alone.
func marshalMessages(msgs content.AgenticMessages) ([]byte, error) {
	raws := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		r, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		raws[i] = r
	}
	return json.Marshal(raws)
}

// unmarshalMessages decodes a JSON array of role-tagged messages back into a
// content.AgenticMessages, allocating the concrete type for each element from its
// "role" and delegating block decoding to the content message codecs. It enforces
// the element-count cap and fails closed on an unknown role.
func unmarshalMessages(data []byte) (content.AgenticMessages, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, err
	}
	if len(raws) > maxMessagesPerStep {
		return nil, &EventLimitError{Got: len(raws), Max: maxMessagesPerStep}
	}
	if len(raws) == 0 {
		// An empty array decodes to nil, matching the marshal path (omitempty drops
		// a nil/empty slice) so the codec is a stable fixed point under re-marshal.
		return nil, nil
	}
	msgs := make(content.AgenticMessages, 0, len(raws))
	for _, r := range raws {
		m, err := unmarshalMessage(r)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// unmarshalMessage reads the "role" discriminator and decodes the bytes into the
// matching concrete Conversation type. RoleTool maps to ToolResultMessage (which
// carries tool_use_id/is_error and defines its own codec); the other roles map to
// the role-only message wrappers. Pointer forms are returned because every
// Conversation implementor satisfies the interface with a pointer receiver, mirror-
// ing how the messages are constructed and committed in the loop.
func unmarshalMessage(data []byte) (content.Conversation, error) {
	var probe struct {
		Role content.Role `json:"role"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	switch probe.Role {
	case content.RoleUser:
		return decodeMessage[content.UserMessage](data)
	case content.RoleAssistant:
		return decodeMessage[content.AIMessage](data)
	case content.RoleSystem:
		return decodeMessage[content.SystemMessage](data)
	case content.RoleTool:
		return decodeMessage[content.ToolResultMessage](data)
	default:
		return nil, &UnknownMessageRoleError{Role: string(probe.Role)}
	}
}

// decodeMessage unmarshals data into a freshly allocated *T and returns it as a
// content.Conversation. Each *T satisfies Conversation, and its UnmarshalJSON
// (promoted from Message or defined on ToolResultMessage) decodes nested blocks via
// the content codec.
func decodeMessage[T any](data []byte) (content.Conversation, error) {
	v := new(T)
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	c, ok := any(v).(content.Conversation)
	if !ok {
		// Unreachable: each *T in the switch is a Conversation. Fail secure.
		return nil, &UnknownMessageRoleError{}
	}
	return c, nil
}
