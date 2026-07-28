package gate

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

func TestReviewSubjectWireRoundTripsStrictly(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	data, err := marshalPermissionReviewSubject(subject)
	if err != nil {
		t.Fatalf("marshalPermissionReviewSubject() error = %v", err)
	}
	got, err := unmarshalPermissionReviewSubject(data)
	if err != nil {
		t.Fatalf("unmarshalPermissionReviewSubject() error = %v", err)
	}
	gotWire, err := marshalPermissionReviewSubject(got)
	if err != nil {
		t.Fatalf("marshalPermissionReviewSubject(round trip) error = %v", err)
	}
	if !bytes.Equal(gotWire, data) {
		t.Fatalf("round-trip wire differs:\n got %s\nwant %s", gotWire, data)
	}

	pretty := bytes.ReplaceAll(data, []byte(`,"`), []byte(", \n \""))
	reordered := reorderReviewSubjectRoot(t, pretty)
	equivalent, err := unmarshalPermissionReviewSubject(reordered)
	if err != nil {
		t.Fatalf("unmarshalPermissionReviewSubject(reordered) error = %v", err)
	}
	equivalentWire, err := marshalPermissionReviewSubject(equivalent)
	if err != nil {
		t.Fatalf("marshalPermissionReviewSubject(equivalent) error = %v", err)
	}
	if !bytes.Equal(equivalentWire, data) {
		t.Fatalf("equivalent input canonicalized to %s, want %s", equivalentWire, data)
	}
}

func TestReviewSubjectWireRejectsUntrustedInput(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	valid, err := marshalPermissionReviewSubject(subject)
	if err != nil {
		t.Fatalf("marshalPermissionReviewSubject() error = %v", err)
	}
	secretKey := "unique_attacker_secret_key"
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "null", data: []byte("null")},
		{name: "oversize", data: bytes.Repeat([]byte(" "), MaxPermissionReviewSubjectWireBytes+1)},
		{name: "trailing", data: append(append([]byte(nil), valid...), []byte(`{}`)...)},
		{name: "duplicate root", data: replaceWire(valid, `"version":`, `"version":"permission_review_subject.v1","version":`)},
		{name: "duplicate nested", data: replaceWire(valid, `"gate_id":`, `"gate_id":"123e4567-e89b-12d3-a456-426614174109","gate_id":`)},
		{name: "unknown root", data: replaceWire(valid, `"version":`, `"`+secretKey+`":true,"version":`)},
		{name: "unknown nested", data: replaceWire(valid, `"context_revision":`, `"`+secretKey+`":true,"context_revision":`)},
		{name: "missing version", data: replaceWire(valid, `"version":"permission_review_subject.v1",`, ``)},
		{name: "version", data: replaceWire(valid, `"permission_review_subject.v1"`, `"permission_review_subject.v2"`)},
		{name: "kind", data: replaceWire(valid, `"harness.permission"`, `"harness.ask_user"`)},
		{name: "uppercase id", data: replaceWire(valid, subject.Basis.GateID.String(), strings.ToUpper(subject.Basis.GateID.String()))},
		{name: "uppercase digest", data: replaceWire(valid, hex.EncodeToString(subject.Basis.SubjectDigest[:]), strings.ToUpper(hex.EncodeToString(subject.Basis.SubjectDigest[:])))},
		{name: "digest mismatch", data: replaceWire(valid, hex.EncodeToString(subject.Basis.SubjectDigest[:]), strings.Repeat("0", 64))},
		{name: "unsupported origin", data: replaceWire(valid, `"origin":"user"`, `"origin":"attacker"`)},
		{name: "unsupported kind", data: replaceWire(valid, `"kind":"user_message"`, `"kind":"attacker"`)},
		{name: "unsupported mask", data: replaceWire(valid, `"applied":0`, `"applied":32768`)},
		{name: "negative counter", data: replaceWire(valid, `"omitted_entries":0`, `"omitted_entries":-1`)},
		{name: "missing user", data: replaceWire(valid, `"origin":"user"`, `"origin":"assistant"`)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := unmarshalPermissionReviewSubject(tt.data)
			if err == nil {
				t.Fatal("unmarshalPermissionReviewSubject() error = nil")
			}
			if !reflect.DeepEqual(got, PermissionReviewSubject{}) {
				t.Fatalf("subject = %#v, want zero", got)
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Fatalf("error %q echoes attacker key", err)
			}
		})
	}
}

func TestReviewSubjectWireRejectsStoredDigestMismatchOnMarshal(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	subject.Request.Summary = "changed after construction"
	got, err := marshalPermissionReviewSubject(subject)
	if err == nil || got != nil {
		t.Fatalf("marshalPermissionReviewSubject() = (%s, %v), want nil, error", got, err)
	}
}

func TestReviewSubjectWireRejectsMissingZeroValuedField(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	basis := subject.Basis
	basis.SubjectDigest = [32]byte{}
	request := subject.Request.Clone()
	request.Summary = ""
	subject, err := NewPermissionReviewSubject(basis, request, subject.Context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject() error = %v", err)
	}
	valid, err := marshalPermissionReviewSubject(subject)
	if err != nil {
		t.Fatalf("marshalPermissionReviewSubject() error = %v", err)
	}
	missing := replaceWire(valid, `"summary":"",`, ``)
	got, err := unmarshalPermissionReviewSubject(missing)
	if err == nil || !reflect.DeepEqual(got, PermissionReviewSubject{}) {
		t.Fatalf("unmarshalPermissionReviewSubject() = (%#v, %v), want zero, error", got, err)
	}
}

func TestReviewSubjectDigestKnownFixture(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	const want = "d3143d47f3e68cd386b0e28d7d701633feec42651cce18bea8237b06b90d0e08"
	if got := hex.EncodeToString(subject.Basis.SubjectDigest[:]); got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
}

func validPermissionReviewSubject(t testing.TB) PermissionReviewSubject {
	t.Helper()

	toolExecutionID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174110")
	context := ReviewContext{
		Coordinates: identity.Coordinates{
			SessionID: uuid.MustParse("123e4567-e89b-12d3-a456-426614174101"),
			LoopID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174102"),
			TurnID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174103"),
			StepID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174104"),
		},
		ContextRevision:    "context-v1",
		WorkspaceRoot:      "/workspace",
		WorkingDirectory:   "/workspace/repo",
		RetryReason:        "sandbox denied",
		SecurityCeiling:    "workspace-write",
		GatePolicyRevision: "gate-policy-v1",
		Entries: []ReviewContextEntry{
			{Origin: ReviewContextOriginUser, Kind: ReviewContextKindUserMessage, Content: "inspect the repository"},
			{Origin: ReviewContextOriginAssistant, Kind: ReviewContextKindAssistantToolRequest, Content: `{"command":"git status"}`},
		},
	}
	request := tool.Request{
		ToolName:           "Bash",
		Summary:            "run git status",
		ExecutionID:        toolExecutionID.String(),
		Command:            "git status",
		WorkingDirectory:   "/workspace/repo",
		ExpiresAtUnixMilli: 1800000000000,
		Requirements: []tool.Requirement{{
			Kind:        tool.CapabilityCommandExecute,
			Match:       "git status",
			Description: "run git status",
			GrantClass:  tool.GrantClassCommandStart,
			GrantTarget: "git status",
			Candidates: []tool.RuleCandidate{{
				Kind: tool.CapabilityCommandExecute, Match: "Bash(git status)",
				Description: "Bash(git status)", GrantClass: tool.GrantClassCommandStart,
				GrantTarget: "git status",
			}},
		}},
	}
	subject, err := NewPermissionReviewSubject(ReviewBasis{
		GateID:             uuid.MustParse("123e4567-e89b-12d3-a456-426614174109"),
		ToolExecutionID:    toolExecutionID,
		ContextRevision:    context.ContextRevision,
		GatePolicyRevision: context.GatePolicyRevision,
		ClassifierRevision: "command-safety-v1",
		SecurityCeiling:    context.SecurityCeiling,
	}, request, context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject() error = %v", err)
	}
	return subject
}

func replaceWire(data []byte, old, replacement string) []byte {
	return []byte(strings.Replace(string(data), old, replacement, 1))
}

func reorderReviewSubjectRoot(t *testing.T, data []byte) []byte {
	t.Helper()
	// The canonical root is a fixed struct. Moving the first field to the end
	// proves object-key allocation/order is not part of the digest.
	const prefix = `{"version":"permission_review_subject.v1",`
	compact := bytes.ReplaceAll(data, []byte(" \n "), nil)
	if !bytes.HasPrefix(compact, []byte(prefix)) {
		t.Fatalf("wire prefix = %s", compact)
	}
	rest := compact[len(prefix):]
	return append(append([]byte("{"), rest[:len(rest)-1]...), []byte(`,"version":"permission_review_subject.v1"}`)...)
}
