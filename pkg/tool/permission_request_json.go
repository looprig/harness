package tool

import (
	"encoding/json"
	"fmt"
)

// requestType is the wire discriminator for a concrete PermissionRequest. It is
// part of the durable wire contract: never rename an existing value (old journals
// carry the old string); only add new constants when a new concrete request type
// joins the sealed interface.
type requestType string

const (
	typeFileWrite requestType = "file_write"
	typeBash      requestType = "bash"
	typeFetch     requestType = "fetch"
	typeWebSearch requestType = "web_search"
	typeSkill     requestType = "skill_load"
	typeUnknown   requestType = "unknown"
)

// maxPermissionRequestBytes caps the serialized size accepted at the untrusted
// restore boundary. A permission request carries only small redacted strings
// (path, command, URL, query, summary, or a skill load's rel-path + agent + size +
// hex digest — NEVER a skill body), so a generous cap still fails closed on absurd
// input. Conservative starting value; tune to real sizes later.
const maxPermissionRequestBytes = 1 << 20 // 1 MiB

// UnknownPermissionRequestError is returned by the codec when serialized bytes
// carry a tag with no concrete type (including the empty/missing tag), or when a
// foreign concrete type is handed to the marshaler. The restore path is an
// untrusted boundary; callers fail secure on this error rather than guess a
// concrete request to reconstruct.
type UnknownPermissionRequestError struct{ Type string }

func (e *UnknownPermissionRequestError) Error() string {
	return fmt.Sprintf("tool: unknown permission request type %q", e.Type)
}

// NilPermissionRequestError is returned by MarshalPermissionRequest when the
// request is a nil interface or a typed-nil pointer. Construction always uses
// non-nil concrete values, so this indicates a caller bug; the codec fails secure
// rather than emit a tagless or "null" record that could mask data loss on restore.
type NilPermissionRequestError struct{}

func (e *NilPermissionRequestError) Error() string {
	return "tool: nil permission request"
}

// PermissionRequestEncodeError wraps a failure to marshal a concrete request
// payload (a json.Marshal failure on an otherwise-known type).
type PermissionRequestEncodeError struct {
	Type  requestType
	Cause error
}

func (e *PermissionRequestEncodeError) Error() string {
	return fmt.Sprintf("tool: encode permission request %q: %v", string(e.Type), e.Cause)
}
func (e *PermissionRequestEncodeError) Unwrap() error { return e.Cause }

// PermissionRequestDecodeError wraps a failure to unmarshal serialized request
// bytes (malformed JSON, wrong field types).
type PermissionRequestDecodeError struct{ Cause error }

func (e *PermissionRequestDecodeError) Error() string {
	return "tool: decode permission request: " + e.Cause.Error()
}
func (e *PermissionRequestDecodeError) Unwrap() error { return e.Cause }

// PermissionRequestLimitError is returned when serialized input exceeds the codec
// safety cap.
type PermissionRequestLimitError struct {
	Got int
	Max int
}

func (e *PermissionRequestLimitError) Error() string {
	return fmt.Sprintf("tool: permission request input exceeds byte cap (%d > %d)", e.Got, e.Max)
}

// requestTag returns the wire discriminator for a concrete PermissionRequest. A nil
// interface, typed-nil pointer, or foreign type fails closed (mirrors blockTag).
// Both value and pointer forms of each concrete type are accepted so a caller that
// holds e.g. a *BashRequest still encodes correctly.
func requestTag(r PermissionRequest) (requestType, error) {
	switch v := r.(type) {
	case nil:
		return "", &NilPermissionRequestError{}
	case FileWriteRequest:
		return typeFileWrite, nil
	case *FileWriteRequest:
		if v == nil {
			return "", &NilPermissionRequestError{}
		}
		return typeFileWrite, nil
	case BashRequest:
		return typeBash, nil
	case *BashRequest:
		if v == nil {
			return "", &NilPermissionRequestError{}
		}
		return typeBash, nil
	case FetchRequest:
		return typeFetch, nil
	case *FetchRequest:
		if v == nil {
			return "", &NilPermissionRequestError{}
		}
		return typeFetch, nil
	case WebSearchRequest:
		return typeWebSearch, nil
	case *WebSearchRequest:
		if v == nil {
			return "", &NilPermissionRequestError{}
		}
		return typeWebSearch, nil
	case SkillLoadRequest:
		return typeSkill, nil
	case *SkillLoadRequest:
		if v == nil {
			return "", &NilPermissionRequestError{}
		}
		return typeSkill, nil
	case UnknownRequest:
		return typeUnknown, nil
	case *UnknownRequest:
		if v == nil {
			return "", &NilPermissionRequestError{}
		}
		return typeUnknown, nil
	default:
		return "", &UnknownPermissionRequestError{Type: r.ToolName()}
	}
}

// MarshalPermissionRequest writes {"type": <tag>, ...payload}. Each concrete type
// is persisted IN FULL — every field needed so a reconstructed value's
// ToolName()/Description()/AllowedScopes() equal the original's. The payload is
// marshaled first, then the tag is merged in as a sibling key (never via an
// embedding wrapper, which would let a payload field shadow the "type" key).
func MarshalPermissionRequest(r PermissionRequest) ([]byte, error) {
	tag, err := requestTag(r)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, &PermissionRequestEncodeError{Type: tag, Cause: err}
	}
	if string(payload) == "null" {
		// A non-nil request always marshals to a JSON object; "null" means a
		// typed-nil pointer slipped past requestTag. Fail secure.
		return nil, &NilPermissionRequestError{}
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, &PermissionRequestEncodeError{Type: tag, Cause: err}
	}
	tagJSON, _ := json.Marshal(string(tag)) // a string; cannot fail
	fields["type"] = tagJSON
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &PermissionRequestEncodeError{Type: tag, Cause: err}
	}
	return out, nil
}

// UnmarshalPermissionRequest reads the tag, allocates the concrete type, and decodes
// the same bytes into it (the extra "type" key is ignored by the struct decode). It
// returns a value (not a pointer) for each concrete type, matching how the requests
// are constructed and used elsewhere, so a round-tripped value is reflect-equal to
// the original.
func UnmarshalPermissionRequest(data []byte) (PermissionRequest, error) {
	if len(data) > maxPermissionRequestBytes {
		return nil, &PermissionRequestLimitError{Got: len(data), Max: maxPermissionRequestBytes}
	}
	var probe struct {
		Type requestType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &PermissionRequestDecodeError{Cause: err}
	}
	switch probe.Type {
	case typeFileWrite:
		return decodeRequest[FileWriteRequest](data)
	case typeBash:
		return decodeRequest[BashRequest](data)
	case typeFetch:
		return decodeRequest[FetchRequest](data)
	case typeWebSearch:
		return decodeRequest[WebSearchRequest](data)
	case typeSkill:
		return decodeRequest[SkillLoadRequest](data)
	case typeUnknown:
		return decodeRequest[UnknownRequest](data)
	default:
		return nil, &UnknownPermissionRequestError{Type: string(probe.Type)}
	}
}

// decodeRequest unmarshals data into a fresh T and returns it (by value) as a
// PermissionRequest. Each concrete request type satisfies the interface with value
// receivers, so the value form is itself a PermissionRequest.
func decodeRequest[T interface {
	PermissionRequest
}](data []byte) (PermissionRequest, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, &PermissionRequestDecodeError{Cause: err}
	}
	return v, nil
}
