package gate

import (
	"errors"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// typedPermissionRequest builds a representative prepared request with one
// command-backed requirement and one reusable candidate, the shape the runner
// journals for a permission gate.
func typedPermissionRequest() tool.Request {
	return tool.Request{
		ToolName:           "Bash",
		Summary:            "git push",
		ExecutionID:        "exec-1",
		Command:            "git push",
		WorkingDirectory:   "/workspace",
		ExpiresAtUnixMilli: 1,
		Requirements: []tool.Requirement{
			{
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
			},
		},
	}
}

// TestPermissionPayloadRoundTripsTypedRequest proves the durable permission
// payload wire is the typed prepared request: it survives a marshal/unmarshal
// cycle intact, decoded through the strict request decoder.
func TestPermissionPayloadRoundTripsTypedRequest(t *testing.T) {
	t.Parallel()
	in := PermissionPayload{Request: typedPermissionRequest()}

	data, err := MarshalPayload(in)
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	decoded, err := UnmarshalPayload(data)
	if err != nil {
		t.Fatalf("UnmarshalPayload() error = %v", err)
	}
	out, ok := decoded.(PermissionPayload)
	if !ok {
		t.Fatalf("decoded payload = %T, want PermissionPayload", decoded)
	}
	if out.Request.ToolName != "Bash" || out.Request.Command != "git push" {
		t.Errorf("round-trip request = %+v, want original", out.Request)
	}
	if len(out.Request.Requirements) != 1 {
		t.Fatalf("round-trip requirements = %d, want 1", len(out.Request.Requirements))
	}
	req := out.Request.Requirements[0]
	if req.Kind != tool.CapabilityCommandExecute || req.GrantClass != tool.GrantClassCommandStart {
		t.Errorf("round-trip requirement = %+v, want original", req)
	}
	if len(req.Candidates) != 1 || req.Candidates[0].Match != "Bash(git push:*)" {
		t.Errorf("round-trip candidates = %+v, want original", req.Candidates)
	}
}

// TestPermissionPayloadRejectsInvalidRequest proves a payload carrying a
// request that violates the prepared-request invariants cannot be journaled.
func TestPermissionPayloadRejectsInvalidRequest(t *testing.T) {
	t.Parallel()
	request := typedPermissionRequest()
	request.Requirements[0].GrantTarget = "" // break the grant pair invariant
	if _, err := MarshalPayload(PermissionPayload{Request: request}); err == nil {
		t.Fatal("MarshalPayload() error = nil, want invalid request rejection")
	}
}

// TestPermissionPayloadRejectsLegacySealedRequestWire proves the hard cut: a
// journal record written under the retired sealed PermissionRequest codec
// (a {type,...} discriminator wire) fails strict decode with a typed error
// instead of being silently skipped or migrated.
func TestPermissionPayloadRejectsLegacySealedRequestWire(t *testing.T) {
	t.Parallel()
	legacy := []byte(`{"kind":"permission","data":{"type":"bash","command":"echo ok","grants":[{"token":"t1","description":"network egress"}]}}`)
	_, err := UnmarshalPayload(legacy)
	if err == nil {
		t.Fatal("UnmarshalPayload() error = nil, want legacy wire rejection")
	}
	var decodeErr *PayloadDecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("UnmarshalPayload() error = %v (%T), want *PayloadDecodeError", err, err)
	}
}

// TestPermissionAuditStoresDescriptionsOnly pins the new audit wire: the
// durable permission audit carries requirement and candidate DESCRIPTIONS and
// round-trips them; a legacy accepted_grant_descriptions record fails strict
// decode; and no token-like material appears on the wire.
func TestPermissionAuditStoresDescriptionsOnly(t *testing.T) {
	t.Parallel()
	in := PermissionAudit{
		RequirementDescriptions: []string{"execute command", "network egress"},
		CandidateDescriptions:   []string{"git push family"},
	}
	data, err := MarshalResponseAudit(in)
	if err != nil {
		t.Fatalf("MarshalResponseAudit() error = %v", err)
	}
	decoded, err := UnmarshalResponseAudit(data)
	if err != nil {
		t.Fatalf("UnmarshalResponseAudit() error = %v", err)
	}
	out, ok := decoded.(PermissionAudit)
	if !ok {
		t.Fatalf("decoded audit = %T, want PermissionAudit", decoded)
	}
	if len(out.RequirementDescriptions) != 2 || out.RequirementDescriptions[0] != "execute command" {
		t.Errorf("RequirementDescriptions = %v, want originals", out.RequirementDescriptions)
	}
	if len(out.CandidateDescriptions) != 1 || out.CandidateDescriptions[0] != "git push family" {
		t.Errorf("CandidateDescriptions = %v, want originals", out.CandidateDescriptions)
	}

	legacy := []byte(`{"kind":"permission","data":{"accepted_grant_descriptions":["network egress"]}}`)
	if _, err := UnmarshalResponseAudit(legacy); err == nil {
		t.Fatal("UnmarshalResponseAudit(legacy) error = nil, want strict rejection")
	}
}

// TestParseApprovalAction pins the exact three approval actions as the only
// parsable action strings — the single validation source DecodeApprovalAction
// and the session gate route share.
func TestParseApprovalAction(t *testing.T) {
	t.Parallel()
	for _, want := range []ApprovalAction{ApprovalApprove, ApprovalApproveAlwaysWorkspace, ApprovalDeny} {
		got, ok := ParseApprovalAction(string(want))
		if !ok || got != want {
			t.Errorf("ParseApprovalAction(%q) = (%q, %v), want (%q, true)", want, got, ok, want)
		}
	}
	for _, invalid := range []string{"", "approve", "deny", "Approve always", "APPROVE", "Approve always for this session"} {
		if _, ok := ParseApprovalAction(invalid); ok {
			t.Errorf("ParseApprovalAction(%q) ok = true, want false", invalid)
		}
	}
}

// TestPermissionPayloadWireNeverCarriesTokens proves no field of the durable
// permission payload wire can smuggle grant-token material: the wire is the
// validated typed request, which has no token field, and a wire document
// carrying an unknown token-bearing key is rejected.
func TestPermissionPayloadWireNeverCarriesTokens(t *testing.T) {
	t.Parallel()
	data, err := MarshalPayload(PermissionPayload{Request: typedPermissionRequest()})
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	if strings.Contains(string(data), "token") || strings.Contains(string(data), "grants\"") {
		t.Errorf("permission payload wire contains token-like keys: %s", data)
	}
	smuggled := []byte(`{"kind":"permission","data":{"tool_name":"Bash","grant_tokens":["mint-me"]}}`)
	if _, err := UnmarshalPayload(smuggled); err == nil {
		t.Fatal("UnmarshalPayload(smuggled tokens) error = nil, want strict rejection")
	}
}
