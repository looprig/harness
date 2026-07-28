package gate

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strconv"
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
		{name: "fraction integer", data: replaceWire(valid, `"expires_at_unix_milli":1800000000000`, `"expires_at_unix_milli":1.5`)},
		{name: "exponent integer", data: replaceWire(valid, `"expires_at_unix_milli":1800000000000`, `"expires_at_unix_milli":1e3`)},
		{name: "out of range integer", data: replaceWire(valid, `"expires_at_unix_milli":1800000000000`, `"expires_at_unix_milli":9223372036854775808`)},
		{name: "string as boolean", data: replaceWire(valid, `"truncated":false`, `"truncated":"false"`)},
		{name: "integer as boolean", data: replaceWire(valid, `"truncated":false`, `"truncated":0`)},
		{name: "boolean as string", data: replaceWire(valid, `"summary":"run git status"`, `"summary":false`)},
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

func TestReviewSubjectWireRejectsInvalidUTF8BeforeJSONReplacement(t *testing.T) {
	t.Parallel()
	for _, invalidByte := range []byte{0xfe, 0xff} {
		data := append([]byte(`{"version":"`), invalidByte)
		data = append(data, []byte(`"}`)...)
		got, err := unmarshalPermissionReviewSubject(data)
		if err == nil || !reflect.DeepEqual(got, PermissionReviewSubject{}) {
			t.Fatalf("byte %x unmarshal = (%#v, %v), want zero, error", invalidByte, got, err)
		}
		if len(err.Error()) > 128 || strings.Contains(err.Error(), string([]byte{invalidByte})) {
			t.Fatalf("byte %x error = %q, want bounded and non-echoing", invalidByte, err)
		}
	}
}

func TestPermissionReviewCommonSubjectDigestNeutralizesOnlyClassifierIdentity(t *testing.T) {
	t.Parallel()
	first := validPermissionReviewSubject(t)
	basis := first.Basis
	basis.SubjectDigest = [32]byte{}
	basis.ClassifierRevision = "command-safety-v2"
	second, err := NewPermissionReviewSubject(basis, first.Request, first.Context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject(second) error = %v", err)
	}
	if first.Basis.SubjectDigest == second.Basis.SubjectDigest {
		t.Fatal("classifier-specific full subject digests are equal")
	}
	firstCommon, err := permissionReviewCommonSubjectDigest(first)
	if err != nil {
		t.Fatalf("permissionReviewCommonSubjectDigest(first) error = %v", err)
	}
	secondCommon, err := permissionReviewCommonSubjectDigest(second)
	if err != nil {
		t.Fatalf("permissionReviewCommonSubjectDigest(second) error = %v", err)
	}
	if firstCommon != secondCommon {
		t.Fatalf("common digests differ: %x != %x", firstCommon, secondCommon)
	}

	tests := []struct {
		name   string
		mutate func(*PermissionReviewSubject)
	}{
		{name: "gate id", mutate: func(s *PermissionReviewSubject) { s.Basis.GateID[15]++ }},
		{name: "tool execution id", mutate: func(s *PermissionReviewSubject) {
			s.Basis.ToolExecutionID[15]++
			s.Request.ExecutionID = s.Basis.ToolExecutionID.String()
		}},
		{name: "request", mutate: func(s *PermissionReviewSubject) { s.Request.Summary = "different request" }},
		{name: "context coordinates", mutate: func(s *PermissionReviewSubject) {
			s.Context.Coordinates.StepID[15]++
		}},
		{name: "context revision", mutate: func(s *PermissionReviewSubject) {
			s.Basis.ContextRevision = "context-v2"
			s.Context.ContextRevision = "context-v2"
		}},
		{name: "gate policy", mutate: func(s *PermissionReviewSubject) {
			s.Basis.GatePolicyRevision = "gate-policy-v2"
			s.Context.GatePolicyRevision = "gate-policy-v2"
		}},
		{name: "security ceiling", mutate: func(s *PermissionReviewSubject) {
			s.Basis.SecurityCeiling = "restricted-v2"
			s.Context.SecurityCeiling = "restricted-v2"
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			changed := second.Clone()
			changed.Basis.SubjectDigest = [32]byte{}
			tt.mutate(&changed)
			changed, err := NewPermissionReviewSubject(
				changed.Basis,
				changed.Request,
				changed.Context,
			)
			if err != nil {
				t.Fatalf("NewPermissionReviewSubject(changed) error = %v", err)
			}
			changedCommon, err := permissionReviewCommonSubjectDigest(changed)
			if err != nil {
				t.Fatalf("permissionReviewCommonSubjectDigest(changed) error = %v", err)
			}
			if firstCommon == changedCommon {
				t.Fatalf("%s mutation did not change common subject digest", tt.name)
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

func TestReviewSubjectWireRejectsNullForEveryScalarFamily(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	basis := subject.Basis
	basis.SubjectDigest = [32]byte{}
	request := subject.Request.Clone()
	request.ToolName = ""
	request.Summary = ""
	request.ExecutionID = ""
	request.Command = ""
	request.WorkingDirectory = ""
	request.ExpiresAtUnixMilli = 0
	request.Requirements[0].Kind = "filesystem.read"
	request.Requirements[0].Scope = ""
	request.Requirements[0].GrantClass = ""
	request.Requirements[0].GrantTarget = ""
	request.Requirements[0].Candidates[0].Kind = "filesystem.read"
	request.Requirements[0].Candidates[0].GrantClass = ""
	request.Requirements[0].Candidates[0].GrantTarget = ""
	context := subject.Context.Clone()
	context.RetryReason = ""
	subject, err := NewPermissionReviewSubject(basis, request, context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject(zero scalars) error = %v", err)
	}
	valid, err := marshalPermissionReviewSubject(subject)
	if err != nil {
		t.Fatalf("marshalPermissionReviewSubject() error = %v", err)
	}
	if _, err := unmarshalPermissionReviewSubject(valid); err != nil {
		t.Fatalf("canonical empty/false/zero primitives rejected: %v", err)
	}

	tests := []struct {
		name string
		path []string
	}{
		{name: "root version", path: []string{"version"}},
		{name: "root gate kind", path: []string{"gate_kind"}},
		{name: "root basis", path: []string{"basis"}},
		{name: "root request", path: []string{"request"}},
		{name: "root context", path: []string{"context"}},
		{name: "basis gate id", path: []string{"basis", "gate_id"}},
		{name: "basis tool execution id", path: []string{"basis", "tool_execution_id"}},
		{name: "basis subject digest", path: []string{"basis", "subject_digest"}},
		{name: "basis context revision", path: []string{"basis", "context_revision"}},
		{name: "basis gate policy revision", path: []string{"basis", "gate_policy_revision"}},
		{name: "basis classifier revision", path: []string{"basis", "classifier_revision"}},
		{name: "basis security ceiling", path: []string{"basis", "security_ceiling"}},
		{name: "request tool name empty", path: []string{"request", "tool_name"}},
		{name: "request summary empty", path: []string{"request", "summary"}},
		{name: "request execution id empty", path: []string{"request", "execution_id"}},
		{name: "request command empty", path: []string{"request", "command"}},
		{name: "request working directory empty", path: []string{"request", "working_directory"}},
		{name: "request expiry zero", path: []string{"request", "expires_at_unix_milli"}},
		{name: "request requirements", path: []string{"request", "requirements"}},
		{name: "requirement kind", path: []string{"request", "requirements", "0", "kind"}},
		{name: "requirement scope empty", path: []string{"request", "requirements", "0", "scope"}},
		{name: "requirement match", path: []string{"request", "requirements", "0", "match"}},
		{name: "requirement description", path: []string{"request", "requirements", "0", "description"}},
		{name: "requirement grant class empty", path: []string{"request", "requirements", "0", "grant_class"}},
		{name: "requirement grant target empty", path: []string{"request", "requirements", "0", "grant_target"}},
		{name: "requirement candidates", path: []string{"request", "requirements", "0", "candidates"}},
		{name: "candidate kind", path: []string{"request", "requirements", "0", "candidates", "0", "kind"}},
		{name: "candidate match", path: []string{"request", "requirements", "0", "candidates", "0", "match"}},
		{name: "candidate description", path: []string{"request", "requirements", "0", "candidates", "0", "description"}},
		{name: "candidate grant class empty", path: []string{"request", "requirements", "0", "candidates", "0", "grant_class"}},
		{name: "candidate grant target empty", path: []string{"request", "requirements", "0", "candidates", "0", "grant_target"}},
		{name: "context coordinates", path: []string{"context", "coordinates"}},
		{name: "context session id", path: []string{"context", "coordinates", "session_id"}},
		{name: "context loop id", path: []string{"context", "coordinates", "loop_id"}},
		{name: "context turn id", path: []string{"context", "coordinates", "turn_id"}},
		{name: "context step id", path: []string{"context", "coordinates", "step_id"}},
		{name: "context revision", path: []string{"context", "context_revision"}},
		{name: "context workspace root", path: []string{"context", "workspace_root"}},
		{name: "context working directory", path: []string{"context", "working_directory"}},
		{name: "context retry reason empty", path: []string{"context", "retry_reason"}},
		{name: "context security ceiling", path: []string{"context", "security_ceiling"}},
		{name: "context gate policy revision", path: []string{"context", "gate_policy_revision"}},
		{name: "context entries", path: []string{"context", "entries"}},
		{name: "context truncation", path: []string{"context", "truncation"}},
		{name: "entry origin", path: []string{"context", "entries", "0", "origin"}},
		{name: "entry kind", path: []string{"context", "entries", "0", "kind"}},
		{name: "entry content", path: []string{"context", "entries", "0", "content"}},
		{name: "entry truncated false", path: []string{"context", "entries", "0", "truncated"}},
		{name: "truncation applied zero", path: []string{"context", "truncation", "applied"}},
		{name: "truncation material zero", path: []string{"context", "truncation", "material"}},
		{name: "truncation omitted entries zero", path: []string{"context", "truncation", "omitted_entries"}},
		{name: "truncation omitted bytes zero", path: []string{"context", "truncation", "omitted_bytes"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mutated := replaceReviewWirePathWithNull(t, valid, tt.path)
			got, err := unmarshalPermissionReviewSubject(mutated)
			if err == nil || !reflect.DeepEqual(got, PermissionReviewSubject{}) {
				t.Fatalf("unmarshalPermissionReviewSubject() = (%#v, %v), want zero, error", got, err)
			}
		})
	}
}

func TestReviewSubjectWireRejectsUnexplainedTruncationMasks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*ReviewContext)
	}{
		{name: "active action", mutate: func(c *ReviewContext) {
			c.Truncation.Applied = ReviewTruncationActiveAction
		}},
		{name: "user", mutate: func(c *ReviewContext) {
			c.Truncation.Applied = ReviewTruncationUserEntry
		}},
		{name: "assistant", mutate: func(c *ReviewContext) {
			c.Truncation.Applied = ReviewTruncationAssistantEntry
		}},
		{name: "tool", mutate: func(c *ReviewContext) {
			c.Truncation.Applied = ReviewTruncationToolEntry
		}},
		{name: "block", mutate: func(c *ReviewContext) {
			c.Truncation.Applied = ReviewTruncationBlock
		}},
		{name: "partial material", mutate: func(c *ReviewContext) {
			c.Entries[0].Content = "p\n…[review context truncated]…\ns"
			c.Entries[0].Truncated = true
			c.Truncation.Applied = ReviewTruncationUserEntry | ReviewTruncationBlock
			c.Truncation.Material = ReviewTruncationUserEntry
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subject := validPermissionReviewSubject(t)
			tt.mutate(&subject.Context)
			data := marshalUncheckedPermissionReviewSubject(t, subject)
			got, err := unmarshalPermissionReviewSubject(data)
			if err == nil || !reflect.DeepEqual(got, PermissionReviewSubject{}) {
				t.Fatalf("unmarshalPermissionReviewSubject() = (%#v, %v), want zero, error", got, err)
			}
		})
	}
}

func TestReviewSubjectWireRoundTripsBuilderTruncationAndZeroByteOmission(t *testing.T) {
	t.Parallel()

	t.Run("per-kind and block bits", func(t *testing.T) {
		base := validPermissionReviewSubject(t)
		input := base.Context.Clone()
		input.Entries = append([]ReviewContextEntry{{
			Origin: ReviewContextOriginTool, Kind: ReviewContextKindToolResult,
			Content: strings.Repeat("tool-evidence", 40),
		}}, input.Entries...)
		policy := reviewContextWireTestPolicy()
		policy.MaxToolEntryBytes = 96
		policy.MaxBlockBytes = 64
		built, err := BuildReviewContext(input, policy)
		if err != nil {
			t.Fatalf("BuildReviewContext() error = %v", err)
		}
		want := ReviewTruncationToolEntry | ReviewTruncationBlock
		if built.Truncation.Applied != want || built.Truncation.Material != want {
			t.Fatalf("Truncation = %#v, want both masks %#x", built.Truncation, want)
		}
		assertPermissionReviewContextWireRoundTrip(t, base, built)
	})

	t.Run("zero-byte omission", func(t *testing.T) {
		base := validPermissionReviewSubject(t)
		input := base.Context.Clone()
		input.Entries = append([]ReviewContextEntry{
			{
				Origin: ReviewContextOriginAssistant, Kind: ReviewContextKindAssistantMessage,
				Content: "",
			},
			{
				Origin: ReviewContextOriginAssistant, Kind: ReviewContextKindAssistantMessage,
				Content: "",
			},
		}, input.Entries...)
		policy := reviewContextWireTestPolicy()
		policy.MaxEntries = 3
		built, err := BuildReviewContext(input, policy)
		if err != nil {
			t.Fatalf("BuildReviewContext() error = %v", err)
		}
		if built.Truncation.OmittedEntries != 2 ||
			built.Truncation.OmittedBytes != 0 ||
			built.Truncation.Applied != ReviewTruncationEntryCount ||
			built.Truncation.Material != ReviewTruncationEntryCount {
			t.Fatalf("Truncation = %#v, want two zero-byte material omissions", built.Truncation)
		}
		if built.Entries[0].Content != "omitted_entries=2 omitted_bytes=0" {
			t.Fatalf("omission marker = %q", built.Entries[0].Content)
		}
		assertPermissionReviewContextWireRoundTrip(t, base, built)
	})
}

func TestReviewSubjectDigestKnownFixture(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	const want = "d3143d47f3e68cd386b0e28d7d701633feec42651cce18bea8237b06b90d0e08"
	if got := hex.EncodeToString(subject.Basis.SubjectDigest[:]); got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
}

func TestReviewContextMaxBytesIncludesCompleteCanonicalProjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*ReviewContext)
	}{
		{name: "context revision", mutate: func(c *ReviewContext) {
			c.ContextRevision = strings.Repeat("r", 1024)
		}},
		{name: "workspace root", mutate: func(c *ReviewContext) {
			c.WorkspaceRoot = "/" + strings.Repeat("r", 1024)
			c.WorkingDirectory = c.WorkspaceRoot
		}},
		{name: "working directory", mutate: func(c *ReviewContext) {
			c.WorkingDirectory = c.WorkspaceRoot + "/" + strings.Repeat("r", 1024)
		}},
		{name: "retry reason", mutate: func(c *ReviewContext) {
			c.RetryReason = strings.Repeat("r", 1024)
		}},
		{name: "security ceiling", mutate: func(c *ReviewContext) {
			c.SecurityCeiling = strings.Repeat("r", 1024)
		}},
		{name: "gate policy revision", mutate: func(c *ReviewContext) {
			c.GatePolicyRevision = strings.Repeat("r", 1024)
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := validPermissionReviewSubject(t).Context
			input.Entries[0].Content = "u"
			input.Entries[1].Content = "a"
			tt.mutate(&input)
			policy := reviewContextWireTestPolicy()
			policy.MaxBytes = canonicalReviewContextSize(t, input) - 1
			got, err := BuildReviewContext(input, policy)
			if err == nil || !reflect.DeepEqual(got, ReviewContext{}) {
				t.Fatalf("BuildReviewContext() = (%#v, %v), want zero, error", got, err)
			}
		})
	}
}

func TestReviewContextMaxBytesExactCanonicalBoundary(t *testing.T) {
	t.Parallel()

	input := validPermissionReviewSubject(t).Context
	policy := reviewContextWireTestPolicy()
	exact := canonicalReviewContextSize(t, input)
	policy.MaxBytes = exact
	got, err := BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext(exact) error = %v", err)
	}
	if size := canonicalReviewContextSize(t, got); size != exact || size > policy.MaxBytes {
		t.Fatalf("canonical size = %d, want %d and <= %d", size, exact, policy.MaxBytes)
	}

	policy.MaxBytes = exact - 1
	over, err := BuildReviewContext(input, policy)
	if err == nil || !reflect.DeepEqual(over, ReviewContext{}) {
		t.Fatalf("BuildReviewContext(one over) = (%#v, %v), want zero, error", over, err)
	}
}

func canonicalReviewContextSize(t testing.TB, context ReviewContext) int {
	t.Helper()
	subject := validPermissionReviewSubject(t)
	subject.Context = context
	data, err := json.Marshal(
		permissionReviewSubjectToWire(subject, zeroPermissionReviewDigestHex).Context,
	)
	if err != nil {
		t.Fatalf("json.Marshal(context projection) error = %v", err)
	}
	return len(data)
}

func reviewContextWireTestPolicy() ReviewContextPolicy {
	return ReviewContextPolicy{
		Revision:             "review-policy-v1",
		MaxBytes:             MaxPermissionReviewSubjectWireBytes,
		MaxEstimatedTokens:   MaxPermissionReviewSubjectWireBytes,
		MaxEntries:           32,
		MaxUserEntryBytes:    4096,
		MaxAgentEntryBytes:   4096,
		MaxToolEntryBytes:    4096,
		MaxBlockBytes:        4096,
		MaxActiveActionBytes: 4096,
	}
}

func marshalUncheckedPermissionReviewSubject(
	t testing.TB,
	subject PermissionReviewSubject,
) []byte {
	t.Helper()
	subject.Basis.SubjectDigest = [32]byte{}
	digest, err := permissionReviewSubjectDigest(subject)
	if err != nil {
		t.Fatalf("permissionReviewSubjectDigest() error = %v", err)
	}
	subject.Basis.SubjectDigest = digest
	data, err := json.Marshal(
		permissionReviewSubjectToWire(subject, hex.EncodeToString(digest[:])),
	)
	if err != nil {
		t.Fatalf("json.Marshal(subject wire) error = %v", err)
	}
	return data
}

func assertPermissionReviewContextWireRoundTrip(
	t testing.TB,
	base PermissionReviewSubject,
	context ReviewContext,
) {
	t.Helper()
	basis := base.Basis
	basis.SubjectDigest = [32]byte{}
	subject, err := NewPermissionReviewSubject(basis, base.Request, context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject() error = %v", err)
	}
	data, err := marshalPermissionReviewSubject(subject)
	if err != nil {
		t.Fatalf("marshalPermissionReviewSubject() error = %v", err)
	}
	got, err := unmarshalPermissionReviewSubject(data)
	if err != nil {
		t.Fatalf("unmarshalPermissionReviewSubject() error = %v", err)
	}
	if !reflect.DeepEqual(got, subject) {
		t.Fatalf("round trip = %#v, want %#v", got, subject)
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

func replaceReviewWirePathWithNull(
	t testing.TB,
	data []byte,
	path []string,
) []byte {
	t.Helper()
	mutated := replaceReviewWireRawPathWithNull(t, json.RawMessage(data), path)
	return []byte(mutated)
}

func replaceReviewWireRawPathWithNull(
	t testing.TB,
	raw json.RawMessage,
	path []string,
) json.RawMessage {
	t.Helper()
	if len(path) == 0 {
		return json.RawMessage("null")
	}
	if index, err := strconv.Atoi(path[0]); err == nil {
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil ||
			index < 0 || index >= len(values) {
			t.Fatalf("invalid array path %v", path)
		}
		values[index] = replaceReviewWireRawPathWithNull(t, values[index], path[1:])
		data, err := json.Marshal(values)
		if err != nil {
			t.Fatalf("json.Marshal(array path) error = %v", err)
		}
		return data
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("invalid object path %v: %v", path, err)
	}
	child, ok := object[path[0]]
	if !ok {
		t.Fatalf("missing object path %v", path)
	}
	object[path[0]] = replaceReviewWireRawPathWithNull(t, child, path[1:])
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("json.Marshal(object path) error = %v", err)
	}
	return data
}
