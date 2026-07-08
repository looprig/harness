package gate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

type payloadKind string

const (
	payloadKindOpen        payloadKind = "open"
	payloadKindPermission  payloadKind = "permission"
	payloadKindAskUser     payloadKind = "ask_user"
	payloadKindResumeInput payloadKind = "resume_input"
)

var (
	errMissingPayloadData = errors.New("missing payload data")
	errNullPayloadData    = errors.New("null payload data")
)

// Payload is the sealed union of durable gate payload records.
type Payload interface {
	payload()
}

// OpenPayload records a gate opening and its nested scenario payload.
type OpenPayload struct {
	GateID  ID      `json:"gate_id,omitzero"`
	Payload Payload `json:"payload,omitempty"`
}

// PermissionPayload carries a sealed tool permission request.
type PermissionPayload struct {
	Request tool.PermissionRequest `json:"request,omitempty"`
}

// AskUserPayload carries an explicit question and optional fixed choices.
type AskUserPayload struct {
	Question string   `json:"question,omitempty"`
	Choices  []string `json:"choices,omitempty"`
}

// ResumeInputPayload records input made available when a gate resumes work.
type ResumeInputPayload struct {
	InputID uuid.UUID `json:"input_id,omitzero"`
	Preview string    `json:"preview,omitempty"`
}

func (OpenPayload) payload()        {}
func (PermissionPayload) payload()  {}
func (AskUserPayload) payload()     {}
func (ResumeInputPayload) payload() {}

// UnknownPayloadKindError is returned when a payload wrapper names no known kind.
type UnknownPayloadKindError struct {
	Kind string
}

func (e *UnknownPayloadKindError) Error() string {
	return fmt.Sprintf("gate: unknown payload kind %q", e.Kind)
}

// NilPayloadError is returned when MarshalPayload receives a nil payload.
type NilPayloadError struct{}

func (e *NilPayloadError) Error() string {
	return "gate: nil payload"
}

// PayloadEncodeError wraps failures while encoding a payload wrapper or data.
type PayloadEncodeError struct {
	Kind  string
	Cause error
}

func (e *PayloadEncodeError) Error() string {
	return fmt.Sprintf("gate: encode payload %q: %v", e.Kind, e.Cause)
}
func (e *PayloadEncodeError) Unwrap() error { return e.Cause }

// PayloadDecodeError wraps malformed payload JSON or malformed payload data.
type PayloadDecodeError struct {
	Kind  string
	Cause error
}

func (e *PayloadDecodeError) Error() string {
	if e.Kind == "" {
		return "gate: decode payload: " + e.Cause.Error()
	}
	return fmt.Sprintf("gate: decode payload %q: %v", e.Kind, e.Cause)
}
func (e *PayloadDecodeError) Unwrap() error { return e.Cause }

type payloadWrapper struct {
	Kind payloadKind     `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type openPayloadData struct {
	GateID  ID              `json:"gate_id,omitzero"`
	Payload json.RawMessage `json:"payload"`
}

// MarshalPayload encodes a sealed payload as a {kind,data} discriminator wrapper.
func MarshalPayload(payload Payload) ([]byte, error) {
	kind, err := payloadTag(payload)
	if err != nil {
		return nil, err
	}
	data, err := marshalPayloadData(kind, payload)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(payloadWrapper{Kind: kind, Data: data})
	if err != nil {
		return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
	}
	return out, nil
}

// UnmarshalPayload decodes a {kind,data} payload wrapper and fails closed on unknown kinds.
func UnmarshalPayload(data []byte) (Payload, error) {
	var wrapper payloadWrapper
	if err := decodeStrict(data, &wrapper); err != nil {
		return nil, &PayloadDecodeError{Cause: err}
	}
	if wrapper.Kind == "" {
		return nil, &UnknownPayloadKindError{Kind: ""}
	}
	if len(wrapper.Data) == 0 {
		return nil, &PayloadDecodeError{Kind: string(wrapper.Kind), Cause: errMissingPayloadData}
	}
	if isExplicitJSONNull(wrapper.Data) {
		return nil, &PayloadDecodeError{Kind: string(wrapper.Kind), Cause: errNullPayloadData}
	}
	return unmarshalPayloadData(wrapper.Kind, wrapper.Data)
}

func payloadTag(payload Payload) (payloadKind, error) {
	switch v := payload.(type) {
	case nil:
		return "", &NilPayloadError{}
	case OpenPayload:
		return payloadKindOpen, nil
	case *OpenPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindOpen, nil
	case PermissionPayload:
		return payloadKindPermission, nil
	case *PermissionPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindPermission, nil
	case AskUserPayload:
		return payloadKindAskUser, nil
	case *AskUserPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindAskUser, nil
	case ResumeInputPayload:
		return payloadKindResumeInput, nil
	case *ResumeInputPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindResumeInput, nil
	default:
		return "", &UnknownPayloadKindError{Kind: fmt.Sprintf("%T", payload)}
	}
}

func marshalPayloadData(kind payloadKind, payload Payload) (json.RawMessage, error) {
	switch kind {
	case payloadKindOpen:
		v := openPayloadValue(payload)
		if v.Payload == nil {
			return nil, &PayloadEncodeError{Kind: string(kind), Cause: &NilPayloadError{}}
		}
		nested, err := MarshalPayload(v.Payload)
		if err != nil {
			return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
		}
		return marshalPayloadJSON(kind, openPayloadData{GateID: v.GateID, Payload: nested})
	case payloadKindPermission:
		v := permissionPayloadValue(payload)
		data, err := tool.MarshalPermissionRequest(v.Request)
		if err != nil {
			return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
		}
		return data, nil
	case payloadKindAskUser:
		return marshalPayloadJSON(kind, askUserPayloadValue(payload))
	case payloadKindResumeInput:
		return marshalPayloadJSON(kind, resumeInputPayloadValue(payload))
	default:
		return nil, &UnknownPayloadKindError{Kind: string(kind)}
	}
}

func unmarshalPayloadData(kind payloadKind, data json.RawMessage) (Payload, error) {
	switch kind {
	case payloadKindOpen:
		var raw openPayloadData
		if err := decodeStrict(data, &raw); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		if len(raw.Payload) == 0 {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: errMissingPayloadData}
		}
		nested, err := UnmarshalPayload(raw.Payload)
		if err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return OpenPayload{GateID: raw.GateID, Payload: nested}, nil
	case payloadKindPermission:
		request, err := tool.UnmarshalPermissionRequest(data)
		if err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return PermissionPayload{Request: request}, nil
	case payloadKindAskUser:
		var payload AskUserPayload
		if err := decodeStrict(data, &payload); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return payload, nil
	case payloadKindResumeInput:
		var payload ResumeInputPayload
		if err := decodeStrict(data, &payload); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return payload, nil
	default:
		return nil, &UnknownPayloadKindError{Kind: string(kind)}
	}
}

func marshalPayloadJSON(kind payloadKind, value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
	}
	return data, nil
}

func openPayloadValue(payload Payload) OpenPayload {
	switch v := payload.(type) {
	case OpenPayload:
		return v
	case *OpenPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func permissionPayloadValue(payload Payload) PermissionPayload {
	switch v := payload.(type) {
	case PermissionPayload:
		return v
	case *PermissionPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func askUserPayloadValue(payload Payload) AskUserPayload {
	switch v := payload.(type) {
	case AskUserPayload:
		return v
	case *AskUserPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func resumeInputPayloadValue(payload Payload) ResumeInputPayload {
	switch v := payload.(type) {
	case ResumeInputPayload:
		return v
	case *ResumeInputPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func decodeStrict(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func isExplicitJSONNull(data json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
