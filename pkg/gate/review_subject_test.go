package gate_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

func TestPermissionReviewSubjectConstructsAndClones(t *testing.T) {
	t.Parallel()

	basis, request, context := validPermissionReviewSubjectInput()
	got, err := gate.NewPermissionReviewSubject(basis, request, context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject() error = %v", err)
	}
	if got.Basis.SubjectDigest == ([32]byte{}) {
		t.Fatal("SubjectDigest is zero")
	}

	request.Requirements[0].Description = "mutated input"
	request.Requirements[0].Candidates[0].Description = "mutated input candidate"
	context.Entries[0].Content = "mutated input context"
	if got.Request.Requirements[0].Description == "mutated input" ||
		got.Request.Requirements[0].Candidates[0].Description == "mutated input candidate" ||
		got.Context.Entries[0].Content == "mutated input context" {
		t.Fatal("constructed subject aliases input")
	}

	clone := got.Clone()
	clone.Request.Requirements[0].Description = "mutated clone"
	clone.Request.Requirements[0].Candidates[0].Description = "mutated clone candidate"
	clone.Context.Entries[0].Content = "mutated clone context"
	if got.Request.Requirements[0].Description == clone.Request.Requirements[0].Description ||
		got.Request.Requirements[0].Candidates[0].Description == clone.Request.Requirements[0].Candidates[0].Description ||
		got.Context.Entries[0].Content == clone.Context.Entries[0].Content {
		t.Fatal("Clone() aliases receiver")
	}
}

func TestPermissionReviewSubjectRejectsInvalidBasisAndRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*gate.ReviewBasis, *tool.Request, *gate.ReviewContext)
	}{
		{name: "gate id", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.GateID = uuid.UUID{} }},
		{name: "tool execution id", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.ToolExecutionID = uuid.UUID{} }},
		{name: "incoming digest", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.SubjectDigest[0] = 1 }},
		{name: "context revision", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.ContextRevision = "" }},
		{name: "gate policy revision", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.GatePolicyRevision = "" }},
		{name: "classifier revision", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.ClassifierRevision = "" }},
		{name: "security ceiling", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.SecurityCeiling = "" }},
		{name: "context revision mismatch", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.ContextRevision = "other" }},
		{name: "gate policy mismatch", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.GatePolicyRevision = "other" }},
		{name: "security ceiling mismatch", mutate: func(b *gate.ReviewBasis, _ *tool.Request, _ *gate.ReviewContext) { b.SecurityCeiling = "other" }},
		{name: "invalid request", mutate: func(_ *gate.ReviewBasis, r *tool.Request, _ *gate.ReviewContext) { r.Requirements[0].Match = "" }},
		{name: "execution mismatch", mutate: func(_ *gate.ReviewBasis, r *tool.Request, _ *gate.ReviewContext) {
			r.ExecutionID = "123e4567-e89b-12d3-a456-426614174111"
		}},
		{name: "noncanonical execution id", mutate: func(_ *gate.ReviewBasis, r *tool.Request, _ *gate.ReviewContext) {
			r.ExecutionID = strings.ToUpper(r.ExecutionID)
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			basis, request, context := validPermissionReviewSubjectInput()
			tt.mutate(&basis, &request, &context)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if err == nil {
				t.Fatal("NewPermissionReviewSubject() error = nil")
			}
			if !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
				t.Fatalf("NewPermissionReviewSubject() subject = %#v, want zero", got)
			}
			var validationErr *gate.ReviewValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T, want *ReviewValidationError", err)
			}
		})
	}
}

func TestPermissionReviewSubjectRejectsInvalidBuiltContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*gate.ReviewContext)
	}{
		{name: "zero coordinate", mutate: func(c *gate.ReviewContext) { c.Coordinates.SessionID = uuid.UUID{} }},
		{name: "relative root", mutate: func(c *gate.ReviewContext) { c.WorkspaceRoot = "workspace" }},
		{name: "cwd escape", mutate: func(c *gate.ReviewContext) { c.WorkingDirectory = "/outside" }},
		{name: "unsupported mask", mutate: func(c *gate.ReviewContext) { c.Truncation.Applied = 1 << 15 }},
		{name: "material not applied", mutate: func(c *gate.ReviewContext) { c.Truncation.Material = gate.ReviewTruncationUserEntry }},
		{name: "negative omitted entries", mutate: func(c *gate.ReviewContext) { c.Truncation.OmittedEntries = -1 }},
		{name: "negative omitted bytes", mutate: func(c *gate.ReviewContext) { c.Truncation.OmittedBytes = -1 }},
		{name: "unsupported pair", mutate: func(c *gate.ReviewContext) { c.Entries[0].Origin = gate.ReviewContextOriginExternal }},
		{name: "missing user", mutate: func(c *gate.ReviewContext) { c.Entries = c.Entries[1:] }},
		{name: "missing active action", mutate: func(c *gate.ReviewContext) { c.Entries = c.Entries[:1] }},
		{name: "unexplained truncated entry", mutate: func(c *gate.ReviewContext) { c.Entries[0].Truncated = true }},
		{name: "truncated entry wrong mask", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationToolEntry
		}},
		{name: "budget mask without omission", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationEntryCount
		}},
		{name: "unexplained omission counts", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationEntryCount
			c.Truncation.OmittedEntries = 1
			c.Truncation.OmittedBytes = 5
		}},
		{name: "forged omission marker", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationEntryCount
			c.Truncation.OmittedEntries = 1
			c.Truncation.OmittedBytes = 5
			c.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginOmission, Kind: gate.ReviewContextKindOmission, Content: "forged",
			}}, c.Entries...)
		}},
		{name: "omission without budget mask", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationUserEntry
			c.Truncation.OmittedEntries = 1
			c.Truncation.OmittedBytes = 5
			c.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginOmission, Kind: gate.ReviewContextKindOmission,
				Content: "omitted_entries=1 omitted_bytes=5",
			}}, c.Entries...)
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			basis, request, context := validPermissionReviewSubjectInput()
			tt.mutate(&context)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if err == nil || !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
				t.Fatalf("NewPermissionReviewSubject() = (%#v, %v), want zero, error", got, err)
			}
		})
	}
}

func TestPermissionReviewSubjectErrorsDoNotEchoContents(t *testing.T) {
	t.Parallel()

	secret := "unique-secret-that-must-not-echo"
	basis, request, context := validPermissionReviewSubjectInput()
	request.Requirements[0].Match = secret + "\x00"
	_, err := gate.NewPermissionReviewSubject(basis, request, context)
	if err == nil {
		t.Fatal("NewPermissionReviewSubject() error = nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q echoes request content", err)
	}

	basis, request, context = validPermissionReviewSubjectInput()
	context.Entries[0].Content = string([]byte{0xff}) + secret
	_, err = gate.NewPermissionReviewSubject(basis, request, context)
	if err == nil {
		t.Fatal("NewPermissionReviewSubject() invalid context error = nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q echoes context content", err)
	}
}

func TestPermissionReviewSubjectAcceptsGeneratedTruncation(t *testing.T) {
	t.Parallel()

	basis, request, base := validPermissionReviewSubjectInput()
	policy := gate.ReviewContextPolicy{
		Revision:             "review-policy-v1",
		MaxBytes:             4096,
		MaxEstimatedTokens:   1024,
		MaxEntries:           32,
		MaxUserEntryBytes:    1024,
		MaxAgentEntryBytes:   64,
		MaxToolEntryBytes:    1024,
		MaxBlockBytes:        128,
		MaxActiveActionBytes: 1024,
	}
	truncatedInput := base.Clone()
	truncatedInput.Entries = append([]gate.ReviewContextEntry{{
		Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantMessage,
		Content: strings.Repeat("assistant context ", 20),
	}}, truncatedInput.Entries...)
	truncated, err := gate.BuildReviewContext(truncatedInput, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext(truncated) error = %v", err)
	}
	if _, err := gate.NewPermissionReviewSubject(basis, request, truncated); err != nil {
		t.Fatalf("NewPermissionReviewSubject(truncated) error = %v", err)
	}

	omittedInput := base.Clone()
	omittedInput.Entries = append([]gate.ReviewContextEntry{
		{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantMessage, Content: "older assistant"},
		{Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult, Content: "older tool"},
	}, omittedInput.Entries...)
	policy.MaxAgentEntryBytes = 1024
	policy.MaxEntries = 3
	omitted, err := gate.BuildReviewContext(omittedInput, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext(omitted) error = %v", err)
	}
	if _, err := gate.NewPermissionReviewSubject(basis, request, omitted); err != nil {
		t.Fatalf("NewPermissionReviewSubject(omitted) error = %v", err)
	}
}

func TestPermissionReviewSubjectDigestStableAndSensitive(t *testing.T) {
	t.Parallel()

	basis, request, context := validPermissionReviewSubjectInput()
	subject, err := gate.NewPermissionReviewSubject(basis, request, context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject() error = %v", err)
	}
	first, err := gate.SubjectDigest(subject)
	if err != nil {
		t.Fatalf("SubjectDigest() error = %v", err)
	}
	for i := 0; i < 10; i++ {
		got, err := gate.SubjectDigest(subject.Clone())
		if err != nil || got != first {
			t.Fatalf("SubjectDigest() iteration %d = (%x, %v), want %x", i, got, err, first)
		}
	}

	mutations := []struct {
		name   string
		mutate func(*gate.PermissionReviewSubject)
	}{
		{name: "gate id", mutate: func(s *gate.PermissionReviewSubject) { s.Basis.GateID[15]++ }},
		{name: "tool execution id", mutate: func(s *gate.PermissionReviewSubject) {
			s.Basis.ToolExecutionID[15]++
			s.Request.ExecutionID = s.Basis.ToolExecutionID.String()
		}},
		{name: "context revision", mutate: func(s *gate.PermissionReviewSubject) {
			s.Basis.ContextRevision = "context-v2"
			s.Context.ContextRevision = "context-v2"
		}},
		{name: "gate policy revision", mutate: func(s *gate.PermissionReviewSubject) {
			s.Basis.GatePolicyRevision = "gate-policy-v2"
			s.Context.GatePolicyRevision = "gate-policy-v2"
		}},
		{name: "classifier revision", mutate: func(s *gate.PermissionReviewSubject) { s.Basis.ClassifierRevision = "command-safety-v2" }},
		{name: "security ceiling", mutate: func(s *gate.PermissionReviewSubject) {
			s.Basis.SecurityCeiling = "read-only"
			s.Context.SecurityCeiling = "read-only"
		}},
		{name: "request summary", mutate: func(s *gate.PermissionReviewSubject) { s.Request.Summary = "different summary" }},
		{name: "request candidate", mutate: func(s *gate.PermissionReviewSubject) {
			s.Request.Requirements[0].Candidates[0].Description = "different candidate"
		}},
		{name: "context coordinate", mutate: func(s *gate.PermissionReviewSubject) { s.Context.Coordinates.StepID[15]++ }},
		{name: "context path", mutate: func(s *gate.PermissionReviewSubject) { s.Context.WorkingDirectory = "/workspace/other" }},
		{name: "retry reason", mutate: func(s *gate.PermissionReviewSubject) { s.Context.RetryReason = "different retry" }},
		{name: "entry content", mutate: func(s *gate.PermissionReviewSubject) { s.Context.Entries[0].Content = "different intent" }},
	}
	for _, tt := range mutations {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			changed := subject.Clone()
			tt.mutate(&changed)
			got, err := gate.SubjectDigest(changed)
			if err != nil {
				t.Fatalf("SubjectDigest() error = %v", err)
			}
			if got == first {
				t.Fatal("SubjectDigest() unchanged")
			}
		})
	}
}

func validPermissionReviewSubjectInput() (gate.ReviewBasis, tool.Request, gate.ReviewContext) {
	toolExecutionID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174110")
	context := gate.ReviewContext{
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
		Entries: []gate.ReviewContextEntry{
			{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage, Content: "inspect the repository"},
			{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest, Content: `{"command":"git status"}`},
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
				Kind:        tool.CapabilityCommandExecute,
				Match:       "Bash(git status)",
				Description: "Bash(git status)",
				GrantClass:  tool.GrantClassCommandStart,
				GrantTarget: "git status",
			}},
		}},
	}
	basis := gate.ReviewBasis{
		GateID:             uuid.MustParse("123e4567-e89b-12d3-a456-426614174109"),
		ToolExecutionID:    toolExecutionID,
		ContextRevision:    context.ContextRevision,
		GatePolicyRevision: context.GatePolicyRevision,
		ClassifierRevision: "command-safety-v1",
		SecurityCeiling:    context.SecurityCeiling,
	}
	return basis, request, context
}
