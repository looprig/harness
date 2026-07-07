package gate

import (
	"encoding/json"
	"time"
)

type PolicyAction string

const (
	PolicyWait           PolicyAction = "wait"
	PolicyRespond        PolicyAction = "respond"
	PolicySuspendSession PolicyAction = "suspend_session"
	PolicyModelDecide    PolicyAction = "model_decide"
)

type ResponsePolicy struct {
	Timeout   time.Duration `json:"timeout,omitzero"`
	OnTimeout PolicyAction  `json:"on_timeout,omitempty"`
}

func (p ResponsePolicy) EffectiveAction() PolicyAction {
	if p.OnTimeout == "" {
		return PolicyWait
	}
	return p.OnTimeout
}

type ResponseTemplate struct {
	Action string                     `json:"action,omitempty"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
}

type ModelDecisionPolicy struct {
	Prompt         string           `json:"prompt,omitempty"`
	AllowedActions []string         `json:"allowed_actions,omitempty"`
	Default        ResponseTemplate `json:"default,omitzero"`
	Metadata       json.RawMessage  `json:"metadata,omitempty"`
}
