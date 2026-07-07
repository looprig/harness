package gate

import (
	"encoding/json"
	"errors"
	"fmt"
)

type responseAuditKind string

const (
	responseAuditKindPermission responseAuditKind = "permission"
	responseAuditKindAskUser    responseAuditKind = "ask_user"
)

var errMissingResponseAuditData = errors.New("missing response audit data")

// ResponseAudit is the sealed union of durable, redacted gate response audit records.
type ResponseAudit interface {
	responseAudit()
}

// PermissionAudit stores human-readable grant descriptions accepted by a response.
type PermissionAudit struct {
	AcceptedGrantDescriptions []string `json:"accepted_grant_descriptions,omitempty"`
}

// AskUserAudit stores a redacted preview of a user answer.
type AskUserAudit struct {
	AnswerPreview string `json:"answer_preview,omitempty"`
}

func (PermissionAudit) responseAudit() {}
func (AskUserAudit) responseAudit()    {}

// UnknownResponseAuditKindError is returned when an audit wrapper names no known kind.
type UnknownResponseAuditKindError struct {
	Kind string
}

func (e *UnknownResponseAuditKindError) Error() string {
	return fmt.Sprintf("gate: unknown response audit kind %q", e.Kind)
}

// NilResponseAuditError is returned when MarshalResponseAudit receives nil audit data.
type NilResponseAuditError struct{}

func (e *NilResponseAuditError) Error() string {
	return "gate: nil response audit"
}

// ResponseAuditEncodeError wraps failures while encoding response audit data.
type ResponseAuditEncodeError struct {
	Kind  string
	Cause error
}

func (e *ResponseAuditEncodeError) Error() string {
	return fmt.Sprintf("gate: encode response audit %q: %v", e.Kind, e.Cause)
}
func (e *ResponseAuditEncodeError) Unwrap() error { return e.Cause }

// ResponseAuditDecodeError wraps malformed audit JSON or malformed audit data.
type ResponseAuditDecodeError struct {
	Kind  string
	Cause error
}

func (e *ResponseAuditDecodeError) Error() string {
	if e.Kind == "" {
		return "gate: decode response audit: " + e.Cause.Error()
	}
	return fmt.Sprintf("gate: decode response audit %q: %v", e.Kind, e.Cause)
}
func (e *ResponseAuditDecodeError) Unwrap() error { return e.Cause }

type responseAuditWrapper struct {
	Kind responseAuditKind `json:"kind"`
	Data json.RawMessage   `json:"data"`
}

// MarshalResponseAudit encodes a sealed response audit as a {kind,data} wrapper.
func MarshalResponseAudit(audit ResponseAudit) ([]byte, error) {
	kind, err := responseAuditTag(audit)
	if err != nil {
		return nil, err
	}
	data, err := marshalResponseAuditData(kind, audit)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(responseAuditWrapper{Kind: kind, Data: data})
	if err != nil {
		return nil, &ResponseAuditEncodeError{Kind: string(kind), Cause: err}
	}
	return out, nil
}

// UnmarshalResponseAudit decodes a {kind,data} wrapper and fails closed on unknown kinds.
func UnmarshalResponseAudit(data []byte) (ResponseAudit, error) {
	var wrapper responseAuditWrapper
	if err := decodeStrict(data, &wrapper); err != nil {
		return nil, &ResponseAuditDecodeError{Cause: err}
	}
	if wrapper.Kind == "" {
		return nil, &UnknownResponseAuditKindError{Kind: ""}
	}
	if len(wrapper.Data) == 0 {
		return nil, &ResponseAuditDecodeError{Kind: string(wrapper.Kind), Cause: errMissingResponseAuditData}
	}
	return unmarshalResponseAuditData(wrapper.Kind, wrapper.Data)
}

func responseAuditTag(audit ResponseAudit) (responseAuditKind, error) {
	switch v := audit.(type) {
	case nil:
		return "", &NilResponseAuditError{}
	case PermissionAudit:
		return responseAuditKindPermission, nil
	case *PermissionAudit:
		if v == nil {
			return "", &NilResponseAuditError{}
		}
		return responseAuditKindPermission, nil
	case AskUserAudit:
		return responseAuditKindAskUser, nil
	case *AskUserAudit:
		if v == nil {
			return "", &NilResponseAuditError{}
		}
		return responseAuditKindAskUser, nil
	default:
		return "", &UnknownResponseAuditKindError{Kind: fmt.Sprintf("%T", audit)}
	}
}

func marshalResponseAuditData(kind responseAuditKind, audit ResponseAudit) (json.RawMessage, error) {
	switch kind {
	case responseAuditKindPermission:
		return marshalResponseAuditJSON(kind, permissionAuditValue(audit))
	case responseAuditKindAskUser:
		return marshalResponseAuditJSON(kind, askUserAuditValue(audit))
	default:
		return nil, &UnknownResponseAuditKindError{Kind: string(kind)}
	}
}

func unmarshalResponseAuditData(kind responseAuditKind, data json.RawMessage) (ResponseAudit, error) {
	switch kind {
	case responseAuditKindPermission:
		var audit PermissionAudit
		if err := decodeStrict(data, &audit); err != nil {
			return nil, &ResponseAuditDecodeError{Kind: string(kind), Cause: err}
		}
		return audit, nil
	case responseAuditKindAskUser:
		var audit AskUserAudit
		if err := decodeStrict(data, &audit); err != nil {
			return nil, &ResponseAuditDecodeError{Kind: string(kind), Cause: err}
		}
		return audit, nil
	default:
		return nil, &UnknownResponseAuditKindError{Kind: string(kind)}
	}
}

func marshalResponseAuditJSON(kind responseAuditKind, value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, &ResponseAuditEncodeError{Kind: string(kind), Cause: err}
	}
	return data, nil
}

func permissionAuditValue(audit ResponseAudit) PermissionAudit {
	switch v := audit.(type) {
	case PermissionAudit:
		return v
	case *PermissionAudit:
		return *v
	default:
		panic("gate: internal response audit type mismatch")
	}
}

func askUserAuditValue(audit ResponseAudit) AskUserAudit {
	switch v := audit.(type) {
	case AskUserAudit:
		return v
	case *AskUserAudit:
		return *v
	default:
		panic("gate: internal response audit type mismatch")
	}
}
