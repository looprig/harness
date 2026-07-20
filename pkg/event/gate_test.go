package event

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
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
				Header: fullHeader(),
				GateID: gate.ID(seededUUID(0x70)),
				Reason: gate.CloseAnswered,
				Action: string(gate.ApprovalApprove),
				Source: gate.ResponseSource{Kind: gate.ResponseFromUser, Reason: "human"},
				Audit:  gate.PermissionAudit{RequirementDescriptions: []string{"network egress"}},
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

// TestGateIdentityProfileByResolver is the profile-selection proof. A host-owned
// gate (gate.ResolverSession) needs only a SessionID — a loop-attributed
// elicitation may carry a LoopID, an open-url gate a turn but no step, and a
// startup elicitation neither loop nor turn — while a loop-owned gate
// (gate.ResolverLoop) keeps the full step profile and a missing step STILL fails.
// It is the anti-regression guard for both directions of the fix at once.
func TestGateIdentityProfileByResolver(t *testing.T) {
	t.Parallel()

	sess := seededUUID(0x11)
	loop := seededUUID(0x22)
	turn := seededUUID(0x33)
	step := seededUUID(0x44)
	evID := seededUUID(0x55)

	hdr := func(c identity.Coordinates) Header { return Header{Coordinates: c, EventID: evID} }
	host := func(k gate.Kind) gate.Gate {
		return gate.Gate{ID: gate.ID(seededUUID(0x70)), Kind: k, Resolver: gate.ResolverSession}
	}
	loopGate := gate.Gate{ID: gate.ID(seededUUID(0x70)), Kind: gate.KindPermission, Resolver: gate.ResolverLoop}

	valid := []struct {
		name string
		ev   Event
	}{
		// Host-owned: SessionID is the only required coordinate.
		{"host form startup (session only)", GateOpened{Header: hdr(identity.Coordinates{SessionID: sess}), Gate: host(gate.KindForm)}},
		{"host form loop-attributed", GateOpened{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop}), Gate: host(gate.KindForm)}},
		{"host open-url turn but no step", GateOpened{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn}), Gate: host(gate.KindOpenURL)}},
		{"host form full quartet", GatePrepared{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn, StepID: step}), Gate: host(gate.KindForm)}},
		{"host resolved session only", GateResolved{Header: hdr(identity.Coordinates{SessionID: sess}), GateID: gate.ID(seededUUID(0x70)), Resolver: gate.ResolverSession}},
		// Loop-owned still passes with the full step quartet.
		{"loop permission full quartet", GateOpened{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn, StepID: step}), Gate: loopGate}},
		{"loop resolved full quartet", GateResolved{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn, StepID: step}), GateID: gate.ID(seededUUID(0x70)), Resolver: gate.ResolverLoop}},
	}
	for _, tt := range valid {
		tt := tt
		t.Run("valid/"+tt.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateEvent(tt.ev); err != nil {
				t.Errorf("ValidateEvent(%s) = %v, want nil", tt.name, err)
			}
		})
	}

	invalid := []struct {
		name      string
		ev        Event
		wantField FieldName
	}{
		// Loop-owned discipline is NOT weakened: a permission gate with no step fails,
		// and this is the exact shape a real loopruntime permission gate produces.
		{"loop permission missing step", GateOpened{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn}), Gate: loopGate}, FieldStepID},
		{"loop permission missing turn+step", GateOpened{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop}), Gate: loopGate}, FieldTurnID},
		{"loop resolved missing step", GateResolved{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn}), GateID: gate.ID(seededUUID(0x70)), Resolver: gate.ResolverLoop}, FieldStepID},
		// An empty resolver (a pre-discriminator record) fails SECURE to the strict profile.
		{"resolved empty resolver falls back to strict", GateResolved{Header: hdr(identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn}), GateID: gate.ID(seededUUID(0x70))}, FieldStepID},
		// Host-owned still requires a SessionID.
		{"host missing session", GateOpened{Header: hdr(identity.Coordinates{}), Gate: host(gate.KindForm)}, FieldSessionID},
	}
	for _, tt := range invalid {
		tt := tt
		t.Run("invalid/"+tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEvent(tt.ev)
			var ve *InvalidEventError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidateEvent(%s) = %v (%T), want *InvalidEventError", tt.name, err, err)
			}
			if ve.Field != tt.wantField {
				t.Errorf("ValidateEvent(%s) field = %q, want %q", tt.name, ve.Field, tt.wantField)
			}
		})
	}
}

// TestGateResolvedResolverRoundTrips proves the additive Resolver discriminator
// survives MarshalEvent/UnmarshalEvent, so a decoded GateResolved can select its
// own identity profile without the prepared record.
func TestGateResolvedResolverRoundTrips(t *testing.T) {
	t.Parallel()
	for _, resolver := range []gate.ResolverKind{gate.ResolverSession, gate.ResolverLoop} {
		resolver := resolver
		t.Run(string(resolver), func(t *testing.T) {
			t.Parallel()
			// A coordinate shape that is valid for THIS resolver's profile.
			hdr := fullHeader()
			if resolver == gate.ResolverSession {
				hdr = fullHeaderSession()
			}
			ev := GateResolved{Header: hdr, GateID: gate.ID(seededUUID(0x70)), Resolver: resolver, Reason: gate.CloseAnswered}
			data, err := MarshalEvent(ev)
			if err != nil {
				t.Fatalf("MarshalEvent: %v", err)
			}
			got, err := UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent: %v\nwire: %s", err, data)
			}
			if !reflect.DeepEqual(got, ev) {
				t.Errorf("round-trip mismatch:\n got = %#v\nwant = %#v\nwire: %s", got, ev, data)
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
		Header: fullHeader(),
		GateID: gate.ID(seededUUID(0x70)),
		Reason: gate.CloseAnswered,
		Action: string(gate.ApprovalApproveAlwaysWorkspace),
		Source: gate.ResponseSource{Kind: gate.ResponseFromPolicy, Reason: "timeout"},
		Audit:  gate.PermissionAudit{RequirementDescriptions: []string{"write to /out", "network egress"}, CandidateDescriptions: []string{"git push family"}},
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
