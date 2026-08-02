package event

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAgentRuntimeRoundTripOnLoopStarted(t *testing.T) {
	t.Parallel()

	in := LoopStarted{
		Header:  fullHeaderLoop(),
		Runtime: sampleRuntime(),
		AgentRuntime: &AgentRuntime{
			Harness:         "codex",
			Profile:         "acp/codex",
			CredentialMode:  "gateway-backed",
			ModelAlias:      "gpt-5.6-luna",
			SmallModelAlias: "sonnet-5",
		},
	}

	data, err := MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("wire JSON: %v", err)
	}
	if _, ok := wire["agent_runtime"]; !ok {
		t.Fatalf("wire = %s, want agent_runtime", data)
	}

	got, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v\nwire: %s", err, data)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %#v, want %#v", got, in)
	}
}

func TestLoopAgentSessionBoundRoundTripAndClassification(t *testing.T) {
	t.Parallel()

	in := LoopAgentSessionBound{
		Header:       fullHeaderLoop(),
		ACPSessionID: "acp-session-1",
	}
	data, err := MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("wire JSON: %v", err)
	}
	if got := string(wire["type"]); got != `"LoopAgentSessionBound"` {
		t.Errorf("type = %s, want LoopAgentSessionBound", got)
	}
	if _, ok := wire["acp_session_id"]; !ok {
		t.Fatalf("wire = %s, want acp_session_id", data)
	}

	got, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v\nwire: %s", err, data)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %#v, want %#v", got, in)
	}
	if in.Class() != Enduring || in.Scope() != ScopeLoop || in.EndsTurn() {
		t.Fatalf("LoopAgentSessionBound lifecycle/scope = %v/%v/%v, want Enduring/ScopeLoop/false", in.Class(), in.Scope(), in.EndsTurn())
	}
}

func TestLegacyLoopStartedWithoutAgentRuntimeDecodesNilAndValidates(t *testing.T) {
	t.Parallel()

	legacy := struct {
		Header
		Type string `json:"type"`
		V    uint32 `json:"v"`
	}{
		Header: fullHeaderLoop(),
		Type:   "LoopStarted",
		V:      1,
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("legacy marshal: %v", err)
	}
	if strings.Contains(string(data), `"runtime"`) || strings.Contains(string(data), `"agent_runtime"`) {
		t.Fatalf("legacy wire unexpectedly contains additive runtime fields: %s", data)
	}

	got, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent legacy: %v\nwire: %s", err, data)
	}
	started, ok := got.(LoopStarted)
	if !ok {
		t.Fatalf("decoded type = %T, want LoopStarted", got)
	}
	if started.AgentRuntime != nil {
		t.Fatalf("legacy AgentRuntime = %#v, want nil", started.AgentRuntime)
	}
	if err := ValidateEvent(got); err != nil {
		t.Fatalf("ValidateEvent(legacy) = %v, want nil", err)
	}
}

func TestAgentRuntimeValidationRejectsUnboundedAndPathLikeIdentity(t *testing.T) {
	t.Parallel()

	base := AgentRuntime{
		Harness:         "codex",
		Profile:         "acp/codex",
		CredentialMode:  "gateway-backed",
		ModelAlias:      "gpt-5.6-luna",
		SmallModelAlias: "sonnet-5",
		ACPSessionID:    "acp-session-1",
	}
	tests := []struct {
		name   string
		mutate func(*AgentRuntime)
	}{
		{name: "harness path", mutate: func(v *AgentRuntime) { v.Harness = "/tmp/codex" }},
		{name: "profile URL", mutate: func(v *AgentRuntime) { v.Profile = "https://gateway.invalid/acp" }},
		{name: "model alias traversal", mutate: func(v *AgentRuntime) { v.ModelAlias = "../luna" }},
		{name: "session URL", mutate: func(v *AgentRuntime) { v.ACPSessionID = "https://agent.invalid/session" }},
		{name: "alias overlong", mutate: func(v *AgentRuntime) { v.SmallModelAlias = strings.Repeat("x", maxAgentRuntimeIdentityBytes+1) }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value := base
			tt.mutate(&value)
			err := ValidateEvent(LoopStarted{Header: fullHeaderLoop(), Runtime: sampleRuntime(), AgentRuntime: &value})
			var invalid *InvalidEventError
			if !errors.As(err, &invalid) || invalid.Rule != RuleInvalid {
				t.Fatalf("ValidateEvent = %T %v, want InvalidEventError/invalid", err, err)
			}
		})
	}

	for _, value := range []string{"", "/tmp/session", "https://agent.invalid/session", strings.Repeat("s", maxAgentRuntimeIdentityBytes+1)} {
		value := value
		t.Run("bound "+value, func(t *testing.T) {
			t.Parallel()
			err := ValidateEvent(LoopAgentSessionBound{Header: fullHeaderLoop(), ACPSessionID: value})
			if err == nil {
				t.Fatalf("ValidateEvent accepted ACPSessionID %q", value)
			}
		})
	}
}
