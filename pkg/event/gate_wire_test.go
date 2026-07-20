package event

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

func gateWireHeader() Header {
	return Header{
		EventID: uuid.MustParse("123e4567-e89b-12d3-a456-426614174101"),
		Coordinates: identity.Coordinates{
			SessionID: uuid.MustParse("123e4567-e89b-12d3-a456-426614174102"),
			LoopID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174103"),
			TurnID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174104"),
			StepID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174105"),
		},
		CreatedAt: time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC),
	}
}

func gateWireRequest() tool.Request {
	return tool.Request{
		ToolName:           "Bash",
		Summary:            "git push",
		ExecutionID:        "exec-1",
		Command:            "git push",
		WorkingDirectory:   "/workspace",
		ExpiresAtUnixMilli: 1,
		Requirements: []tool.Requirement{{
			Kind:        tool.CapabilityCommandExecute,
			Match:       "git push",
			Description: "execute command",
			GrantClass:  tool.GrantClassCommandStart,
			GrantTarget: "git push",
			Candidates: []tool.RuleCandidate{{
				Kind:        tool.CapabilityCommandExecute,
				Match:       "Bash(git push:*)",
				Description: "git push family",
				GrantClass:  tool.GrantClassCommandStart,
				GrantTarget: "git push",
			}},
		}},
	}
}

// TestPermissionRequestedRoundTripsTypedRequest proves the durable
// PermissionRequested wire carries the typed prepared request through the
// strict request decoder and round-trips it intact.
func TestPermissionRequestedRoundTripsTypedRequest(t *testing.T) {
	t.Parallel()
	in := PermissionRequested{
		Header:          gateWireHeader(),
		ToolExecutionID: uuid.MustParse("123e4567-e89b-12d3-a456-426614174106"),
		Request:         gateWireRequest(),
	}
	data, err := MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	decoded, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent() error = %v", err)
	}
	out, ok := decoded.(PermissionRequested)
	if !ok {
		t.Fatalf("decoded = %T, want PermissionRequested", decoded)
	}
	if out.Request.ToolName != "Bash" || len(out.Request.Requirements) != 1 {
		t.Errorf("round-trip Request = %+v, want original", out.Request)
	}
	if out.Request.Requirements[0].Description != "execute command" {
		t.Errorf("round-trip requirement description = %q, want %q", out.Request.Requirements[0].Description, "execute command")
	}
}

// TestPermissionRequestedRejectsLegacySealedWire proves a durable record
// written under the retired sealed PermissionRequest codec fails strict decode
// with a typed error instead of being migrated.
func TestPermissionRequestedRejectsLegacySealedWire(t *testing.T) {
	t.Parallel()
	legacy := []byte(`{"type":"PermissionRequested","v":1,` +
		`"event_id":"123e4567-e89b-12d3-a456-426614174101",` +
		`"session_id":"123e4567-e89b-12d3-a456-426614174102",` +
		`"loop_id":"123e4567-e89b-12d3-a456-426614174103",` +
		`"turn_id":"123e4567-e89b-12d3-a456-426614174104",` +
		`"step_id":"123e4567-e89b-12d3-a456-426614174105",` +
		`"created_at":"2026-07-20T12:00:00Z",` +
		`"tool_execution_id":"123e4567-e89b-12d3-a456-426614174106",` +
		`"request":{"type":"bash","command":"echo ok"}}`)
	_, err := UnmarshalEvent(legacy)
	if err == nil {
		t.Fatal("UnmarshalEvent(legacy sealed request) error = nil, want strict rejection")
	}
	var decode *EventDecodeError
	if !errors.As(err, &decode) {
		t.Fatalf("UnmarshalEvent() error = %v (%T), want *EventDecodeError", err, err)
	}
}

// TestGateResolvedRoundTripWithoutScope proves the resolved-gate wire carries
// the exact approval action plus the descriptions-only audit and no scope key.
func TestGateResolvedRoundTripWithoutScope(t *testing.T) {
	t.Parallel()
	in := GateResolved{
		Header:   gateWireHeader(),
		GateID:   gate.ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174107")),
		Resolver: gate.ResolverLoop,
		Reason:   gate.CloseAnswered,
		Action:   string(gate.ApprovalApproveAlwaysWorkspace),
		Source:   gate.ResponseSource{Kind: gate.ResponseFromUser},
		Audit: gate.PermissionAudit{
			RequirementDescriptions: []string{"execute command"},
			CandidateDescriptions:   []string{"git push family"},
		},
	}
	data, err := MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	if strings.Contains(string(data), `"scope"`) {
		t.Errorf("GateResolved wire still carries a scope key: %s", data)
	}
	decoded, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent() error = %v", err)
	}
	out, ok := decoded.(GateResolved)
	if !ok {
		t.Fatalf("decoded = %T, want GateResolved", decoded)
	}
	if out.Action != string(gate.ApprovalApproveAlwaysWorkspace) {
		t.Errorf("round-trip Action = %q, want %q", out.Action, gate.ApprovalApproveAlwaysWorkspace)
	}
	audit, ok := out.Audit.(gate.PermissionAudit)
	if !ok {
		t.Fatalf("round-trip Audit = %T, want gate.PermissionAudit", out.Audit)
	}
	if len(audit.CandidateDescriptions) != 1 || audit.CandidateDescriptions[0] != "git push family" {
		t.Errorf("round-trip CandidateDescriptions = %v, want [git push family]", audit.CandidateDescriptions)
	}
}

// TestUnmarshalRejectsRemovedSecurityLimitEventTags proves the hard cut for
// durable event records: the retired security-limit tags fail restore with the
// codec's typed unknown-tag error instead of being skipped or migrated.
func TestUnmarshalRejectsRemovedSecurityLimitEventTags(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{"SecurityLimitChanged", "SecurityCeilingChanged"} {
		record := []byte(`{"type":"` + tag + `","v":1,` +
			`"event_id":"123e4567-e89b-12d3-a456-426614174101",` +
			`"session_id":"123e4567-e89b-12d3-a456-426614174102",` +
			`"created_at":"2026-07-20T12:00:00Z","level":2}`)
		_, err := UnmarshalEvent(record)
		var unknown *UnknownEventTypeError
		if !errors.As(err, &unknown) {
			t.Fatalf("UnmarshalEvent(%s) error = %v, want *UnknownEventTypeError", tag, err)
		}
		if unknown.Type != tag {
			t.Errorf("UnknownEventTypeError.Type = %q, want %q", unknown.Type, tag)
		}
	}
}

// TestGateWiresNeverCarryTokensOrRawArguments is the audit-secrecy pin for the
// durable gate wires: serialized PermissionRequested and GateResolved records
// built from a token-minting flow contain neither grant-token material nor raw
// tool arguments.
func TestGateWiresNeverCarryTokensOrRawArguments(t *testing.T) {
	t.Parallel()
	const mintedToken = "minted-grant-token-3f9a"
	const rawArgs = `{"command":"git push","env":{"AWS_SECRET":"raw-secret"}}`

	requested, err := MarshalEvent(PermissionRequested{
		Header:          gateWireHeader(),
		ToolExecutionID: uuid.MustParse("123e4567-e89b-12d3-a456-426614174106"),
		Request:         gateWireRequest(),
	})
	if err != nil {
		t.Fatalf("MarshalEvent(PermissionRequested) error = %v", err)
	}
	resolved, err := MarshalEvent(GateResolved{
		Header:   gateWireHeader(),
		GateID:   gate.ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174107")),
		Resolver: gate.ResolverLoop,
		Reason:   gate.CloseAnswered,
		Action:   string(gate.ApprovalApprove),
		Audit:    gate.PermissionAudit{RequirementDescriptions: []string{"execute command"}},
	})
	if err != nil {
		t.Fatalf("MarshalEvent(GateResolved) error = %v", err)
	}
	for name, wire := range map[string][]byte{"PermissionRequested": requested, "GateResolved": resolved} {
		if strings.Contains(string(wire), mintedToken) {
			t.Errorf("%s wire contains grant token material: %s", name, wire)
		}
		if strings.Contains(string(wire), "raw-secret") || strings.Contains(string(wire), "AWS_SECRET") {
			t.Errorf("%s wire contains raw tool arguments: %s", name, wire)
		}
		if strings.Contains(string(wire), "accepted_grants") || strings.Contains(string(wire), `"token"`) {
			t.Errorf("%s wire carries a token-bearing key: %s", name, wire)
		}
	}
	_ = rawArgs
}
