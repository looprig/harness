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

func TestAgentRuntimeRejectsNativeHarnessManagedIdentityWithModelRuntime(t *testing.T) {
	t.Parallel()

	runtime := AgentRuntime{
		Harness:        "codex",
		Profile:        "acp/codex",
		CredentialMode: "native-auth",
		Source:         "native",
		SelectionKind:  "harness-managed",
	}
	event := LoopStarted{Header: fullHeaderLoop(), Runtime: sampleRuntime(), AgentRuntime: &runtime}
	err := ValidateEvent(event)
	var invalid *InvalidEventError
	if !errors.As(err, &invalid) || invalid.Rule != RuleInvalid {
		t.Fatalf("ValidateEvent() error = %T %v, want InvalidEventError/invalid", err, err)
	}

	if _, err := MarshalEvent(event); err == nil {
		t.Fatal("MarshalEvent() accepted a native/harness-managed identity with a model runtime")
	}
}

func TestNativeHarnessManagedLoopStartedRoundTripOmitsModelRuntime(t *testing.T) {
	t.Parallel()

	in := LoopStarted{
		Header: fullHeaderLoop(),
		AgentRuntime: &AgentRuntime{
			Harness:        "codex",
			Profile:        "acp/codex",
			CredentialMode: "native-auth",
			Source:         "native",
			SelectionKind:  "harness-managed",
		},
	}

	if err := ValidateEvent(in); err != nil {
		t.Fatalf("ValidateEvent() error = %v, want nil", err)
	}
	data, err := MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v, want nil", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("wire JSON: %v", err)
	}
	if _, present := wire["runtime"]; present {
		t.Fatalf("managed LoopStarted wire contains a model runtime: %s", data)
	}

	decoded, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent() error = %v", err)
	}
	started, ok := decoded.(LoopStarted)
	if !ok {
		t.Fatalf("decoded event = %T, want LoopStarted", decoded)
	}
	if started.Runtime != (ModelRuntime{}) {
		t.Fatalf("decoded Runtime = %+v, want empty model runtime", started.Runtime)
	}
	if started.AgentRuntime == nil || *started.AgentRuntime != *in.AgentRuntime {
		t.Fatalf("decoded AgentRuntime = %+v, want %+v", started.AgentRuntime, in.AgentRuntime)
	}
}

func TestLoopStartedDecodeRejectsAgentRuntimeWithoutModelRuntime(t *testing.T) {
	t.Parallel()

	in := LoopStarted{
		Header: fullHeaderLoop(),
		AgentRuntime: &AgentRuntime{
			Harness:        "codex",
			Profile:        "acp/codex",
			CredentialMode: "native-auth",
			Source:         "native",
			SelectionKind:  "harness-managed",
		},
	}
	data, err := MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}

	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("wire JSON: %v", err)
	}
	var baseAgent map[string]json.RawMessage
	if err := json.Unmarshal(wire["agent_runtime"], &baseAgent); err != nil {
		t.Fatalf("agent_runtime JSON: %v", err)
	}

	tests := []struct {
		name   string
		fields map[string]string
	}{
		{
			name: "native source with mismatched gateway credential",
			fields: map[string]string{
				"credential_mode": "gateway-backed",
			},
		},
		{
			name: "native source with invalid credential",
			fields: map[string]string{
				"credential_mode": "not-a-credential",
			},
		},
		{
			name: "gateway explicit selection",
			fields: map[string]string{
				"credential_mode": "gateway-backed",
				"source":          "gateway",
				"selection_kind":  "explicit",
				"model_alias":     "gpt-5.6-luna",
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := make(map[string]json.RawMessage, len(baseAgent)+len(tt.fields))
			for key, value := range baseAgent {
				agent[key] = value
			}
			for key, value := range tt.fields {
				raw, marshalErr := json.Marshal(value)
				if marshalErr != nil {
					t.Fatalf("marshal %s: %v", key, marshalErr)
				}
				agent[key] = raw
			}
			mutatedWire := make(map[string]json.RawMessage, len(wire))
			for key, value := range wire {
				mutatedWire[key] = value
			}
			agentJSON, marshalErr := json.Marshal(agent)
			if marshalErr != nil {
				t.Fatalf("marshal agent_runtime: %v", marshalErr)
			}
			mutatedWire["agent_runtime"] = agentJSON
			malformed, err := json.Marshal(mutatedWire)
			if err != nil {
				t.Fatalf("marshal wire: %v", err)
			}

			if _, err := UnmarshalEvent(malformed); err == nil {
				t.Fatalf("UnmarshalEvent() accepted malformed omitted-runtime record: %s", malformed)
			} else {
				var invalid *InvalidEventError
				if !errors.As(err, &invalid) {
					t.Fatalf("UnmarshalEvent() error = %T %v, want InvalidEventError", err, err)
				}
			}
		})
	}
}
