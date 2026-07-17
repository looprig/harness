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

// Answer is the validated result of answering a HOST-OWNED gate, delivered live
// to the opener that is blocked on it.
//
// It is deliberately NOT a durable record and has no JSON codec. A gate answered
// on behalf of a loop turns into a command, which the loop consumes in memory;
// this is the same thing for a gate whose opener is the host itself. What
// survives the process is the GateResolved event and its redacted FormAudit —
// never Values.
//
// Values is the whole reason the two are separate. It carries the answers keyed
// by schema field name, exactly as ParseFormAnswers produced them, INCLUDING
// free text a human typed. That is what an opener asked for and must act on, and
// it is precisely what must not enter a journal (see FormAudit). Keeping it off
// every durable type makes that structural rather than remembered.
type Answer struct {
	GateID ID
	Action string
	// Values holds form answers keyed by field name. It is nil for any action
	// other than an accepted form.
	Values map[string]string
	Source ResponseSource
}

// ResponseSource describes the origin and reason for a response.
type ResponseSource struct {
	Kind   ResponseSourceKind `json:"kind,omitempty"`
	Reason string             `json:"reason,omitempty"`
}
