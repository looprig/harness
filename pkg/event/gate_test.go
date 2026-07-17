package event

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// sampleGate builds a representative permission gate for the round-trip table.
func sampleGate() gate.Gate {
	return gate.Gate{
		ID:       gate.ID(seededUUID(0x70)),
		Kind:     gate.KindPermission,
		Resolver: gate.ResolverLoop,
		Blocks:   gate.BlocksToolCall,
		Effect:   gate.EffectResume,
		Subject:  gate.Subject{ToolExecutionID: gate.ID(seededUUID(0x71))},
		Prompt: gate.Prompt{
			Title: "Approve tool call",
			Body:  "Run go test",
			Controls: []gate.Control{
				{Action: "approve", Label: "Approve"},
				{Action: "deny", Label: "Deny"},
			},
		},
		ResponsePolicy: gate.ResponsePolicy{
			Timeout:   300000000000,
			OnTimeout: gate.PolicyRespond,
		},
	}
}

// TestGateEventRoundTrip proves GatePrepared, GateOpened, and GateResolved
// round-trip through MarshalEvent/UnmarshalEvent deep-equal to the original.
// GateResolved.Audit (a sealed interface) is projected through
// gate.MarshalResponseAudit and reconstructed on decode.
func TestGateEventRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   Event
	}{
		{
			name: "GatePrepared",
			ev:   GatePrepared{Header: fullHeader(), Gate: sampleGate()},
		},
		{
			name: "GateOpened",
			ev:   GateOpened{Header: fullHeader(), Gate: sampleGate()},
		},
		{
			name: "GateResolved with permission audit",
			ev: GateResolved{
				Header:        fullHeader(),
				GateID:        gate.ID(seededUUID(0x70)),
				Reason:        gate.CloseAnswered,
				Action:        "approve",
				ApprovalScope: tool.ScopeSession,
				Source:        gate.ResponseSource{Kind: gate.ResponseFromUser, Reason: "human"},
				Audit:         gate.PermissionAudit{AcceptedGrantDescriptions: []string{"network egress"}},
			},
		},
		{
			name: "GateResolved with ask-user audit",
			ev: GateResolved{
				Header: fullHeader(),
				GateID: gate.ID(seededUUID(0x70)),
				Reason: gate.CloseAnswered,
				Action: "answer",
				Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
				Audit:  gate.AskUserAudit{AnswerPreview: "yes"},
			},
		},
		{
			name: "GateResolved with form audit",
			ev: GateResolved{
				Header: fullHeader(),
				GateID: gate.ID(seededUUID(0x70)),
				Reason: gate.CloseAnswered,
				Action: gate.FormActionAccept,
				Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
				Audit: gate.FormAudit{Values: map[string]string{
					"note": "free text, journaled as user content",
					"env":  "prod",
				}},
			},
		},
		{
			// An open-url gate resolves with NO audit: its action target must never
			// be journaled and its outcome is already in Action. This is the
			// open-url answer's durable shape, not a placeholder.
			name: "GateResolved for an open-url completion carries no audit",
			ev: GateResolved{
				Header: fullHeader(),
				GateID: gate.ID(seededUUID(0x70)),
				Reason: gate.CloseAnswered,
				Action: gate.FormActionAccept,
				Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
			},
		},
		{
			name: "GateResolved nil audit boundary",
			ev: GateResolved{
				Header: fullHeader(),
				GateID: gate.ID(seededUUID(0x70)),
				Reason: gate.CloseAbandoned,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalEvent(tt.ev)
			if err != nil {
				t.Fatalf("MarshalEvent(%s) error = %v", tt.name, err)
			}
			got, err := UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent(%s) error = %v\nwire: %s", tt.name, err, data)
			}
			if !reflect.DeepEqual(got, tt.ev) {
				t.Errorf("round-trip(%s) mismatch:\n got = %#v\nwant = %#v\nwire: %s", tt.name, got, tt.ev, data)
			}
		})
	}
}

// TestGateOpenedCarriesNoPrivatePayload proves GateOpened fans out only the
// public envelope — the wire JSON must not carry a "payload" key (the private
// payload lives in GatePreparedRecord, never on the public event).
func TestGateOpenedCarriesNoPrivatePayload(t *testing.T) {
	t.Parallel()
	ev := GateOpened{Header: fullHeader(), Gate: sampleGate()}
	data, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent(GateOpened) error = %v", err)
	}
	keys := topLevelKeys(t, data)
	if hasKey(keys, "payload") {
		t.Errorf("GateOpened wire must not carry a payload key\nraw: %s", data)
	}
	if hasKey(keys, "audit") {
		t.Errorf("GateOpened wire must not carry an audit key\nraw: %s", data)
	}
}

// TestGateResolvedReMarshalFixedPoint proves a decoded GateResolved is a fixed
// point under re-marshal: the Audit interface reconstructs to the same concrete
// type and value, so re-marshaling a decoded event reproduces the same wire.
func TestGateResolvedReMarshalFixedPoint(t *testing.T) {
	t.Parallel()
	ev := GateResolved{
		Header:        fullHeader(),
		GateID:        gate.ID(seededUUID(0x70)),
		Reason:        gate.CloseAnswered,
		Action:        "approve",
		ApprovalScope: tool.ScopeWorkspace,
		Source:        gate.ResponseSource{Kind: gate.ResponseFromPolicy, Reason: "timeout"},
		Audit:         gate.PermissionAudit{AcceptedGrantDescriptions: []string{"write to /out", "network egress"}},
	}
	data1, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent #1 error = %v", err)
	}
	e1, err := UnmarshalEvent(data1)
	if err != nil {
		t.Fatalf("UnmarshalEvent #1 error = %v\nwire: %s", err, data1)
	}
	data2, err := MarshalEvent(e1)
	if err != nil {
		t.Fatalf("MarshalEvent #2 error = %v", err)
	}
	if !reflect.DeepEqual(data1, data2) {
		t.Errorf("re-marshal not a fixed point:\n data1 = %s\n data2 = %s", data1, data2)
	}
}

// TestGateEventWireTypeTag proves each gate event's wire envelope carries the
// stable "type" discriminator matching its classify name.
func TestGateEventWireTypeTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ev      Event
		wantTag string
	}{
		{"GatePrepared", GatePrepared{Header: fullHeader(), Gate: sampleGate()}, "GatePrepared"},
		{"GateOpened", GateOpened{Header: fullHeader(), Gate: sampleGate()}, "GateOpened"},
		{"GateResolved", GateResolved{Header: fullHeader(), GateID: gate.ID(seededUUID(0x70))}, "GateResolved"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalEvent(tt.ev)
			if err != nil {
				t.Fatalf("MarshalEvent(%s) error = %v", tt.name, err)
			}
			keys := topLevelKeys(t, data)
			var got string
			if err := json.Unmarshal(keys["type"], &got); err != nil {
				t.Fatalf("unmarshal type tag: %v", err)
			}
			if got != tt.wantTag {
				t.Errorf("wire type tag = %q, want %q\nraw: %s", got, tt.wantTag, data)
			}
		})
	}
}
