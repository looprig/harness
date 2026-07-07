package gate

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
)

func TestGateShape(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	tests := []struct {
		name string
		gate Gate
		want bool
	}{
		{
			name: "zero gate is incomplete",
			gate: Gate{},
			want: false,
		},
		{
			name: "valid gate has core envelope fields",
			gate: Gate{
				ID:          ID(id),
				Kind:        KindPermission,
				Resolver:    ResolverLoop,
				Blocks:      BlocksToolCall,
				Effect:      EffectResume,
				Criticality: GateCritical,
				Subject: Subject{
					ToolExecutionID: ID(id),
					ToolUseID:       "toolu_1",
					TurnID:          ID(id),
					StepID:          ID(id),
					InputID:         ID(id),
				},
				Prompt: Prompt{
					Title: "Approve tool call",
					Schema: PromptSchema{
						Fields: []Field{{Name: "reason", Kind: FieldText}},
					},
					Controls: []Control{{Action: "approve", Label: "Approve"}},
				},
				ResponsePolicy: ResponsePolicy{Timeout: time.Second, OnTimeout: PolicyRespond},
				Restorable:     true,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasCoreEnvelope(tt.gate); got != tt.want {
				t.Fatalf("hasCoreEnvelope(%+v) = %v, want %v", tt.gate, got, tt.want)
			}
		})
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

func hasCoreEnvelope(g Gate) bool {
	return !uuid.UUID(g.ID).IsZero() &&
		g.Kind != "" &&
		g.Resolver != "" &&
		g.Blocks != "" &&
		g.Effect != "" &&
		g.Criticality != ""
}
