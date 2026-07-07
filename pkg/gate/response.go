package gate

import "encoding/json"

type ResponseSourceKind string

const (
	ResponseFromUser   ResponseSourceKind = "user"
	ResponseFromPolicy ResponseSourceKind = "policy"
	ResponseFromModel  ResponseSourceKind = "model"
)

type ResponseRequest struct {
	Action string                     `json:"action,omitempty"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
}

type GateResponse struct {
	GateID ID                         `json:"gate_id,omitzero"`
	Action string                     `json:"action,omitempty"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
	Source ResponseSource             `json:"source,omitzero"`
}

type ResponseSource struct {
	Kind   ResponseSourceKind `json:"kind,omitempty"`
	Reason string             `json:"reason,omitempty"`
}
