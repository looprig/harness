package gate

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
)

func TestGateJSONRoundTripPreservesEnvelope(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	toolExecutionID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174001")
	turnID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174002")
	stepID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174003")
	inputID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174004")
	original := Gate{
		ID:          ID(id),
		Kind:        KindPermission,
		Resolver:    ResolverLoop,
		Blocks:      BlocksToolCall,
		Effect:      EffectResume,
		Criticality: GateCritical,
		Subject: Subject{
			ToolExecutionID: ID(toolExecutionID),
			ToolUseID:       "toolu_1",
			TurnID:          ID(turnID),
			StepID:          ID(stepID),
			InputID:         ID(inputID),
		},
		Prompt: Prompt{
			Title: "Approve tool call",
			Body:  "Bash wants to run a command.",
			Schema: PromptSchema{
				Fields: []Field{
					{
						Name:     "scope",
						Label:    "Scope",
						Kind:     FieldSelect,
						Required: true,
						Options: []Option{
							{Value: "once", Label: "Once"},
							{Value: "session", Label: "Session"},
						},
						Default: json.RawMessage(`{"value":"once","sticky":false}`),
					},
				},
			},
			Controls: []Control{
				{Action: "approve", Label: "Approve"},
				{Action: "deny", Label: "Deny"},
			},
		},
		ResponsePolicy: ResponsePolicy{
			Timeout:   2 * time.Second,
			OnTimeout: PolicyRespond,
		},
		Restorable: true,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal raw: %v", err)
	}
	if got := string(raw["id"]); got != `"`+id.String()+`"` {
		t.Fatalf("id JSON = %s, want UUID string %q", got, id.String())
	}
	var rawPolicy struct {
		Timeout   int64        `json:"timeout"`
		OnTimeout PolicyAction `json:"on_timeout"`
	}
	if err := json.Unmarshal(raw["response_policy"], &rawPolicy); err != nil {
		t.Fatalf("json.Unmarshal response_policy: %v", err)
	}
	if rawPolicy.Timeout != int64(2*time.Second) {
		t.Fatalf("timeout JSON = %d, want nanoseconds %d", rawPolicy.Timeout, int64(2*time.Second))
	}
	if rawPolicy.OnTimeout != PolicyRespond {
		t.Fatalf("on_timeout JSON = %q, want %q", rawPolicy.OnTimeout, PolicyRespond)
	}

	var roundTrip Gate
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("json.Unmarshal gate: %v", err)
	}
	if roundTrip.ID != ID(id) {
		t.Fatalf("roundTrip.ID = %s, want %s", roundTrip.ID, id)
	}
	if roundTrip.Kind != KindPermission ||
		roundTrip.Resolver != ResolverLoop ||
		roundTrip.Blocks != BlocksToolCall ||
		roundTrip.Effect != EffectResume ||
		roundTrip.Criticality != GateCritical ||
		!roundTrip.Restorable {
		t.Fatalf("roundTrip envelope = %+v, want original envelope", roundTrip)
	}
	if roundTrip.Subject.ToolExecutionID != ID(toolExecutionID) ||
		roundTrip.Subject.ToolUseID != "toolu_1" ||
		roundTrip.Subject.TurnID != ID(turnID) ||
		roundTrip.Subject.StepID != ID(stepID) ||
		roundTrip.Subject.InputID != ID(inputID) {
		t.Fatalf("roundTrip.Subject = %+v, want original subject %+v", roundTrip.Subject, original.Subject)
	}
	if len(roundTrip.Prompt.Controls) != 2 || roundTrip.Prompt.Controls[0].Action != "approve" || roundTrip.Prompt.Controls[1].Action != "deny" {
		t.Fatalf("roundTrip.Prompt.Controls = %+v, want approve/deny controls", roundTrip.Prompt.Controls)
	}
	if len(roundTrip.Prompt.Schema.Fields) != 1 {
		t.Fatalf("roundTrip fields len = %d, want 1", len(roundTrip.Prompt.Schema.Fields))
	}
	field := roundTrip.Prompt.Schema.Fields[0]
	if field.Name != "scope" || field.Kind != FieldSelect || !field.Required {
		t.Fatalf("roundTrip field = %+v, want scope select required", field)
	}
	if len(field.Options) != 2 || field.Options[0].Value != "once" || field.Options[1].Value != "session" {
		t.Fatalf("roundTrip options = %+v, want once/session", field.Options)
	}
	if got, want := string(field.Default), `{"value":"once","sticky":false}`; got != want {
		t.Fatalf("roundTrip default = %s, want %s", got, want)
	}
	if roundTrip.ResponsePolicy.Timeout != 2*time.Second || roundTrip.ResponsePolicy.OnTimeout != PolicyRespond {
		t.Fatalf("roundTrip.ResponsePolicy = %+v, want timeout/respond", roundTrip.ResponsePolicy)
	}
}

func TestRouteJSONRoundTripPreservesIDs(t *testing.T) {
	t.Parallel()

	route := Route{
		GateID:          ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174010")),
		LoopID:          ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174011")),
		ToolExecutionID: ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174012")),
	}

	data, err := json.Marshal(route)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var roundTrip Route
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("json.Unmarshal route: %v", err)
	}
	if roundTrip != route {
		t.Fatalf("roundTrip route = %+v, want %+v", roundTrip, route)
	}
}

func TestResponsePolicyEffectiveAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy ResponsePolicy
		want   PolicyAction
	}{
		{
			name: "zero policy waits",
			want: PolicyWait,
		},
		{
			name:   "explicit respond is preserved",
			policy: ResponsePolicy{OnTimeout: PolicyRespond},
			want:   PolicyRespond,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.policy.EffectiveAction(); got != tt.want {
				t.Fatalf("EffectiveAction() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResponseValuesPreserveRawJSON(t *testing.T) {
	t.Parallel()

	request := ResponseRequest{
		Action: "answer",
		Values: map[string]json.RawMessage{
			"object": json.RawMessage(`{"nested":[1,true,"x"]}`),
			"text":   json.RawMessage(`"hello"`),
		},
	}
	reqData, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("json.Marshal request: %v", err)
	}
	var reqRoundTrip ResponseRequest
	if err := json.Unmarshal(reqData, &reqRoundTrip); err != nil {
		t.Fatalf("json.Unmarshal request: %v", err)
	}
	if got := string(reqRoundTrip.Values["object"]); got != `{"nested":[1,true,"x"]}` {
		t.Fatalf("request object value = %s, want raw object", got)
	}
	if got := string(reqRoundTrip.Values["text"]); got != `"hello"` {
		t.Fatalf("request text value = %s, want raw string", got)
	}

	response := GateResponse{
		GateID: ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174020")),
		Action: "approve",
		Values: map[string]json.RawMessage{
			"scope": json.RawMessage(`{"value":"session"}`),
		},
		Source: ResponseSource{Kind: ResponseFromPolicy, Reason: "timeout"},
	}
	respData, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("json.Marshal response: %v", err)
	}
	var respRoundTrip GateResponse
	if err := json.Unmarshal(respData, &respRoundTrip); err != nil {
		t.Fatalf("json.Unmarshal response: %v", err)
	}
	if got := string(respRoundTrip.Values["scope"]); got != `{"value":"session"}` {
		t.Fatalf("response scope value = %s, want raw object", got)
	}
	if respRoundTrip.Source.Kind != ResponseFromPolicy || respRoundTrip.Source.Reason != "timeout" {
		t.Fatalf("response source = %+v, want policy timeout", respRoundTrip.Source)
	}
}

func TestGateIDJSONRoundTrip(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	original := Gate{
		ID:          ID(id),
		Kind:        KindAskUser,
		Resolver:    ResolverSession,
		Blocks:      BlocksSession,
		Effect:      EffectControl,
		Criticality: GateNonCritical,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("marshaled gate is invalid JSON: %s", data)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal raw: %v", err)
	}
	if got := string(raw["id"]); got != `"`+id.String()+`"` {
		t.Fatalf("id JSON = %s, want UUID string %q", got, id.String())
	}

	var roundTrip Gate
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("json.Unmarshal gate: %v", err)
	}
	if roundTrip.ID != ID(id) {
		t.Fatalf("roundTrip.ID = %s, want %s", roundTrip.ID, id)
	}
}

func TestDecodeApprovalActionAcceptsExactlyThreeActions(t *testing.T) {
	tests := []struct {
		data string
		want ApprovalAction
	}{
		{data: `{"action":"Approve"}`, want: ApprovalApprove},
		{data: `{"action":"Approve always for this workspace"}`, want: ApprovalApproveAlwaysWorkspace},
		{data: `{"action":"Deny"}`, want: ApprovalDeny},
	}
	for _, tt := range tests {
		action, err := DecodeApprovalAction([]byte(tt.data))
		if err != nil {
			t.Fatalf("DecodeApprovalAction(%s) error = %v", tt.data, err)
		}
		if action != tt.want {
			t.Fatalf("DecodeApprovalAction(%s) = %q, want %q", tt.data, action, tt.want)
		}
	}
}

func TestDecodeApprovalActionRejectsMalformedAndNonExactInput(t *testing.T) {
	tests := []string{
		`{"action":"approve"}`,
		`{"action":"Approve always"}`,
		`{"action":""}`,
		`{"action":"Approve","action":"Deny"}`,
		`{"action":"Approve","scope":"workspace"}`,
		`{"action":"Approve"}{}`,
		`null`,
		`[]`,
		`not json`,
	}
	for _, data := range tests {
		_, err := DecodeApprovalAction([]byte(data))
		var decodeErr *ApprovalActionDecodeError
		if !errors.As(err, &decodeErr) {
			t.Fatalf("DecodeApprovalAction(%q) error = %T %v, want *ApprovalActionDecodeError", data, err, err)
		}
	}
}
