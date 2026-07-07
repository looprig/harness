package gate

import "encoding/json"

// ResponseSourceKind identifies who produced a gate response.
type ResponseSourceKind string

const (
	// ResponseFromUser records a response supplied by a human user.
	ResponseFromUser ResponseSourceKind = "user"
	// ResponseFromPolicy records a response supplied by gate policy.
	ResponseFromPolicy ResponseSourceKind = "policy"
	// ResponseFromModel records a response supplied by model decision policy.
	ResponseFromModel ResponseSourceKind = "model"
)

// ResponseRequest is the generic action and value payload used to answer a gate.
type ResponseRequest struct {
	Action string                     `json:"action,omitempty"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
}

// GateResponse is the resolved response envelope for a gate.
type GateResponse struct {
	GateID ID                         `json:"gate_id,omitzero"`
	Action string                     `json:"action,omitempty"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
	Source ResponseSource             `json:"source,omitzero"`
}

// ResponseSource describes the origin and reason for a response.
type ResponseSource struct {
	Kind   ResponseSourceKind `json:"kind,omitempty"`
	Reason string             `json:"reason,omitempty"`
}
