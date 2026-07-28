package gate_test

import (
	"errors"
	"reflect"
	"strconv"
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

func TestPermissionReviewSubjectBasisRevisionByteBounds(t *testing.T) {
	t.Parallel()

	classifierExact := strings.Repeat("é", gate.MaxPermissionClassifierRevisionBytes/2)
	classifierOneOver := classifierExact + "x"
	policyExact := strings.Repeat("é", gate.MaxPermissionReviewPolicyRevisionBytes/2)
	policyOneOver := policyExact + "x"
	if len(classifierExact) != gate.MaxPermissionClassifierRevisionBytes ||
		len(classifierOneOver) != gate.MaxPermissionClassifierRevisionBytes+1 ||
		len(policyExact) != gate.MaxPermissionReviewPolicyRevisionBytes ||
		len(policyOneOver) != gate.MaxPermissionReviewPolicyRevisionBytes+1 {
		t.Fatal("test fixture does not exercise exact byte bounds")
	}

	tests := []struct {
		name    string
		exact   string
		oneOver string
		set     func(*gate.ReviewBasis, *gate.ReviewContext, string)
	}{
		{
			name:    "classifier revision",
			exact:   classifierExact,
			oneOver: classifierOneOver,
			set: func(basis *gate.ReviewBasis, _ *gate.ReviewContext, value string) {
				basis.ClassifierRevision = value
			},
		},
		{
			name:    "gate policy revision",
			exact:   policyExact,
			oneOver: policyOneOver,
			set: func(basis *gate.ReviewBasis, context *gate.ReviewContext, value string) {
				basis.GatePolicyRevision = value
				context.GatePolicyRevision = value
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			basis, request, context := validPermissionReviewSubjectInput()
			tt.set(&basis, &context, tt.exact)
			if _, err := gate.NewPermissionReviewSubject(basis, request, context); err != nil {
				t.Fatalf("NewPermissionReviewSubject(exact) error = %v", err)
			}

			basis, request, context = validPermissionReviewSubjectInput()
			tt.set(&basis, &context, tt.oneOver)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if err == nil || !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
				t.Fatalf("NewPermissionReviewSubject(one over) = (%#v, %v), want zero, error", got, err)
			}
			if strings.Contains(err.Error(), tt.oneOver) || len(err.Error()) > 128 {
				t.Fatalf("error = %q, want bounded and non-echoing", err)
			}
			digest, digestErr := gate.SubjectDigest(gate.PermissionReviewSubject{
				Basis: basis, Request: request, Context: context,
			})
			if digestErr == nil || digest != ([32]byte{}) {
				t.Fatalf("SubjectDigest(one over) = (%x, %v), want zero, error", digest, digestErr)
			}
		})
	}
}

func TestPermissionReviewSubjectRejectsInvalidUTF8InEverySerializedRequestString(t *testing.T) {
	t.Parallel()
	invalid := string([]byte{0xff})
	tests := []struct {
		name   string
		mutate func(*tool.Request)
	}{
		{name: "tool name", mutate: func(r *tool.Request) { r.ToolName = invalid }},
		{name: "summary", mutate: func(r *tool.Request) { r.Summary = invalid }},
		{name: "execution id", mutate: func(r *tool.Request) { r.ExecutionID = invalid }},
		{name: "command", mutate: func(r *tool.Request) { r.Command = invalid }},
		{name: "working directory", mutate: func(r *tool.Request) { r.WorkingDirectory = invalid }},
		{name: "requirement kind", mutate: func(r *tool.Request) { r.Requirements[0].Kind = invalid }},
		{name: "requirement scope", mutate: func(r *tool.Request) { r.Requirements[0].Scope = invalid }},
		{name: "requirement match", mutate: func(r *tool.Request) { r.Requirements[0].Match = invalid }},
		{name: "requirement description", mutate: func(r *tool.Request) { r.Requirements[0].Description = invalid }},
		{name: "requirement grant class", mutate: func(r *tool.Request) { r.Requirements[0].GrantClass = invalid }},
		{name: "requirement grant target", mutate: func(r *tool.Request) { r.Requirements[0].GrantTarget = invalid }},
		{name: "candidate kind", mutate: func(r *tool.Request) { r.Requirements[0].Candidates[0].Kind = invalid }},
		{name: "candidate match", mutate: func(r *tool.Request) { r.Requirements[0].Candidates[0].Match = invalid }},
		{name: "candidate description", mutate: func(r *tool.Request) { r.Requirements[0].Candidates[0].Description = invalid }},
		{name: "candidate grant class", mutate: func(r *tool.Request) { r.Requirements[0].Candidates[0].GrantClass = invalid }},
		{name: "candidate grant target", mutate: func(r *tool.Request) { r.Requirements[0].Candidates[0].GrantTarget = invalid }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			basis, request, context := validPermissionReviewSubjectInput()
			tt.mutate(&request)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if err == nil || !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
				t.Fatalf("NewPermissionReviewSubject() = (%#v, %v), want zero, error", got, err)
			}
			if strings.Contains(err.Error(), invalid) || len(err.Error()) > 128 {
				t.Fatalf("error = %q, want bounded and non-echoing", err)
			}
		})
	}
}

func TestPermissionReviewSubjectRejectsInvalidUTF8GrantFreeOptionalFieldsWithoutDigestCollision(t *testing.T) {
	t.Parallel()
	basis, request, context := validPermissionReviewSubjectInput()
	request.Requirements = nil
	request.ExecutionID = ""
	request.ExpiresAtUnixMilli = 0
	for _, field := range []string{"command", "working_directory"} {
		field := field
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			for _, invalidByte := range []byte{0xfe, 0xff} {
				candidate := request.Clone()
				if field == "command" {
					candidate.Command = string([]byte{invalidByte})
				} else {
					candidate.WorkingDirectory = string([]byte{invalidByte})
				}
				subject, err := gate.NewPermissionReviewSubject(basis, candidate, context)
				if err == nil || !reflect.DeepEqual(subject, gate.PermissionReviewSubject{}) {
					t.Fatalf("byte %x subject = (%#v, %v), want zero, error", invalidByte, subject, err)
				}
				digest, digestErr := gate.SubjectDigest(gate.PermissionReviewSubject{
					Basis: basis, Request: candidate, Context: context,
				})
				if digestErr == nil || digest != ([32]byte{}) {
					t.Fatalf("byte %x digest = (%x, %v), want zero, error", invalidByte, digest, digestErr)
				}
			}
		})
	}
}

func TestPermissionReviewSubjectRejectsRequestHardBoundsWithZeroResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*tool.Request)
	}{
		{
			name: "requirements one over",
			mutate: func(request *tool.Request) {
				request.Requirements = make(
					[]tool.Requirement,
					gate.MaxPermissionReviewRequestRequirements+1,
				)
			},
		},
		{
			name: "aggregate candidates one over",
			mutate: func(request *tool.Request) {
				request.Requirements = []tool.Requirement{
					{Candidates: make([]tool.RuleCandidate, gate.MaxPermissionReviewRequestCandidates/2)},
					{Candidates: make([]tool.RuleCandidate, gate.MaxPermissionReviewRequestCandidates/2+1)},
				}
			},
		},
		{
			name: "single string one over",
			mutate: func(request *tool.Request) {
				request.Command = strings.Repeat(
					"x",
					gate.MaxPermissionReviewRequestStringBytes+1,
				)
			},
		},
		{
			name: "aggregate strings one over",
			mutate: func(request *tool.Request) {
				request.ToolName = strings.Repeat(
					"x",
					gate.MaxPermissionReviewRequestInputBytes/2,
				)
				request.Summary = strings.Repeat(
					"y",
					gate.MaxPermissionReviewRequestInputBytes/2+1,
				)
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			basis, request, context := validPermissionReviewSubjectInput()
			tt.mutate(&request)

			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if err == nil || !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
				t.Fatalf(
					"NewPermissionReviewSubject() = (%#v, %v), want zero, error",
					got,
					err,
				)
			}
			if len(err.Error()) > 128 ||
				strings.Contains(err.Error(), strings.Repeat("x", 64)) ||
				strings.Contains(err.Error(), strings.Repeat("y", 64)) {
				t.Fatalf("error = %q, want bounded and non-echoing", err)
			}

			digest, digestErr := gate.SubjectDigest(gate.PermissionReviewSubject{
				Basis: basis, Request: request, Context: context,
			})
			if digestErr == nil || digest != ([32]byte{}) {
				t.Fatalf("SubjectDigest() = (%x, %v), want zero, error", digest, digestErr)
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
				t.Fatalf(
					"NewPermissionReviewSubject() returned zero=%t, error=%v; want zero, error",
					reflect.DeepEqual(got, gate.PermissionReviewSubject{}),
					err,
				)
			}
		})
	}
}

func TestPermissionReviewSubjectRejectsImpossibleTruncationMetadata(t *testing.T) {
	t.Parallel()

	const marker = "\n…[review context truncated]…\n"
	truncated := func(content string) string {
		return "prefix" + marker + content
	}
	tests := []struct {
		name   string
		mutate func(*gate.ReviewContext)
	}{
		{name: "final active action truncated", mutate: func(c *gate.ReviewContext) {
			c.Entries[1].Content = truncated("suffix")
			c.Entries[1].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationAssistantEntry
			c.Truncation.Material = gate.ReviewTruncationAssistantEntry
		}},
		{name: "current user material clear", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Content = truncated("suffix")
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationUserEntry
		}},
		{name: "tool result material clear", mutate: func(c *gate.ReviewContext) {
			c.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult,
				Content: truncated("suffix"), Truncated: true,
			}}, c.Entries...)
			c.Truncation.Applied = gate.ReviewTruncationToolEntry
		}},
		{name: "runtime context material clear", mutate: func(c *gate.ReviewContext) {
			c.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginRuntime, Kind: gate.ReviewContextKindRuntimeContext,
				Content: truncated("suffix"), Truncated: true,
			}}, c.Entries...)
			c.Truncation.Applied = gate.ReviewTruncationBlock
		}},
		{name: "external content material clear", mutate: func(c *gate.ReviewContext) {
			c.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginExternal, Kind: gate.ReviewContextKindExternalContent,
				Content: truncated("suffix"), Truncated: true,
			}}, c.Entries...)
			c.Truncation.Applied = gate.ReviewTruncationBlock
		}},
		{name: "earlier tool request material clear", mutate: func(c *gate.ReviewContext) {
			c.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest,
				Content: truncated("suffix"), Truncated: true,
			}}, c.Entries...)
			c.Truncation.Applied = gate.ReviewTruncationAssistantEntry
		}},
		{name: "truncated without marker", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationUserEntry
			c.Truncation.Material = gate.ReviewTruncationUserEntry
		}},
		{name: "truncated with two markers", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Content = "prefix" + marker + "middle" + marker + "suffix"
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationUserEntry
			c.Truncation.Material = gate.ReviewTruncationUserEntry
		}},
		{name: "truncated without prefix", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Content = marker + "suffix"
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationUserEntry
			c.Truncation.Material = gate.ReviewTruncationUserEntry
		}},
		{name: "truncated without suffix", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Content = "prefix" + marker
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationUserEntry
			c.Truncation.Material = gate.ReviewTruncationUserEntry
		}},
		{name: "omission budget material clear", mutate: func(c *gate.ReviewContext) {
			c.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginOmission, Kind: gate.ReviewContextKindOmission,
				Content: "omitted_entries=1 omitted_bytes=5",
			}}, c.Entries...)
			c.Truncation.Applied = gate.ReviewTruncationEntryCount
			c.Truncation.OmittedEntries = 1
			c.Truncation.OmittedBytes = 5
		}},
		{name: "omission with zero counters", mutate: func(c *gate.ReviewContext) {
			c.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginOmission, Kind: gate.ReviewContextKindOmission,
				Content: "omitted_entries=0 omitted_bytes=0",
			}}, c.Entries...)
			c.Truncation.Applied = gate.ReviewTruncationEntryCount
			c.Truncation.Material = gate.ReviewTruncationEntryCount
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

func TestPermissionReviewSubjectRejectsContextBeyondBuilderHardBounds(t *testing.T) {
	t.Parallel()

	omission := func(entries int, bytes int) func(*gate.ReviewContext) {
		return func(context *gate.ReviewContext) {
			context.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginOmission,
				Kind:   gate.ReviewContextKindOmission,
				Content: "omitted_entries=" + strconv.Itoa(entries) +
					" omitted_bytes=" + strconv.Itoa(bytes),
			}}, context.Entries...)
			context.Truncation = gate.ReviewTruncation{
				Applied:        gate.ReviewTruncationEntryCount,
				Material:       gate.ReviewTruncationEntryCount,
				OmittedEntries: entries,
				OmittedBytes:   bytes,
			}
		}
	}
	tests := []struct {
		name      string
		wantField gate.ReviewValidationField
		mutate    func(*gate.ReviewContext)
	}{
		{
			name:      "entry count",
			wantField: gate.ReviewValidationFieldContextEntry,
			mutate: func(context *gate.ReviewContext) {
				entries := make(
					[]gate.ReviewContextEntry,
					gate.MaxReviewContextInputEntries+1,
				)
				for index := range entries {
					entries[index] = gate.ReviewContextEntry{
						Origin: gate.ReviewContextOriginAssistant,
						Kind:   gate.ReviewContextKindAssistantMessage,
					}
				}
				copy(entries[len(entries)-2:], context.Entries)
				context.Entries = entries
			},
		},
		{
			name:      "omitted entry count",
			wantField: gate.ReviewValidationFieldContext,
			mutate: omission(
				gate.MaxReviewContextInputEntries+1,
				1,
			),
		},
		{
			name:      "omitted bytes",
			wantField: gate.ReviewValidationFieldContext,
			mutate: omission(
				1,
				gate.MaxReviewContextInputBytes+1,
			),
		},
		{
			name:      "entry content",
			wantField: gate.ReviewValidationFieldContextEntry,
			mutate: func(context *gate.ReviewContext) {
				context.Entries[0].Content = strings.Repeat(
					"x",
					gate.MaxReviewContextEntryInputBytes+1,
				)
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			basis, request, context := validPermissionReviewSubjectInput()
			tt.mutate(&context)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if err == nil || !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
				t.Fatalf("NewPermissionReviewSubject() = (%#v, %v), want zero, error", got, err)
			}
			var validationErr *gate.ReviewValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T, want *ReviewValidationError", err)
			}
			if validationErr.Field != tt.wantField ||
				validationErr.Reason != gate.ReviewValidationOutOfBounds {
				t.Fatalf(
					"error = %#v, want field %q reason %q",
					validationErr,
					tt.wantField,
					gate.ReviewValidationOutOfBounds,
				)
			}
		})
	}
}

func TestPermissionReviewSubjectEnforcesReconstructedOriginalEntryCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		omittedEntries int
		wantErr        bool
	}{
		{
			name:           "exact",
			omittedEntries: gate.MaxReviewContextInputEntries - 2,
		},
		{
			name:           "one over",
			omittedEntries: gate.MaxReviewContextInputEntries - 1,
			wantErr:        true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			basis, request, context := validPermissionReviewSubjectInput()
			addPermissionReviewOmission(&context, tt.omittedEntries, 0)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if tt.wantErr {
				if err == nil || !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
					t.Fatalf("NewPermissionReviewSubject() = (%#v, %v), want zero, error", got, err)
				}
				assertReviewSubjectOutOfBounds(t, err, gate.ReviewValidationFieldContextEntry)
				return
			}
			if err != nil {
				t.Fatalf("NewPermissionReviewSubject() exact bound error = %v", err)
			}
		})
	}
}

func TestPermissionReviewSubjectEnforcesReconstructedOriginalRawInputBytes(t *testing.T) {
	t.Parallel()

	basis, request, base := validPermissionReviewSubjectInput()
	// Two omitted entries can genuinely carry the near-4 MiB content total
	// while each remains within the builder's 2 MiB per-entry input ceiling.
	const omittedEntries = 2
	retainedRawBytes := len(base.ContextRevision) +
		len(base.WorkspaceRoot) +
		len(base.WorkingDirectory) +
		len(base.RetryReason) +
		len(base.SecurityCeiling) +
		len(base.GatePolicyRevision) +
		1 // minimum valid unretained ReviewContextPolicy.Revision
	for _, entry := range base.Entries {
		retainedRawBytes += len(entry.Origin) + len(entry.Kind) + len(entry.Content)
	}
	minimumOmittedLabelBytes := minimumOriginalReviewContextEntryLabelBytesForTest()
	exactOmittedBytes := gate.MaxReviewContextInputBytes -
		retainedRawBytes -
		minimumOmittedLabelBytes*omittedEntries
	if exactOmittedBytes <= 0 {
		t.Fatalf("test setup exact omitted bytes = %d, want positive", exactOmittedBytes)
	}

	tests := []struct {
		name         string
		omittedBytes int
		wantErr      bool
	}{
		{name: "exact", omittedBytes: exactOmittedBytes},
		{name: "one over", omittedBytes: exactOmittedBytes + 1, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			context := base.Clone()
			addPermissionReviewOmission(&context, omittedEntries, tt.omittedBytes)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if tt.wantErr {
				if err == nil || !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
					t.Fatalf("NewPermissionReviewSubject() = (%#v, %v), want zero, error", got, err)
				}
				assertReviewSubjectOutOfBounds(t, err, gate.ReviewValidationFieldContextEntry)
				return
			}
			if err != nil {
				t.Fatalf("NewPermissionReviewSubject() exact bound error = %v", err)
			}
		})
	}
}

func TestPermissionReviewSubjectEnforcesOmittedByteDistribution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		omittedEntries int
		omittedBytes   int
		wantErr        bool
	}{
		{
			name:           "one entry exact",
			omittedEntries: 1,
			omittedBytes:   gate.MaxReviewContextEntryInputBytes,
		},
		{
			name:           "one entry one over",
			omittedEntries: 1,
			omittedBytes:   gate.MaxReviewContextEntryInputBytes + 1,
			wantErr:        true,
		},
		{
			name:           "two entries distribute one over single entry",
			omittedEntries: 2,
			omittedBytes:   gate.MaxReviewContextEntryInputBytes + 1,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			basis, request, context := validPermissionReviewSubjectInput()
			addPermissionReviewOmission(
				&context,
				tt.omittedEntries,
				tt.omittedBytes,
			)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if tt.wantErr {
				if err == nil ||
					!reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
					t.Fatalf(
						"NewPermissionReviewSubject() = (%#v, %v), want zero, error",
						got,
						err,
					)
				}
				assertReviewSubjectOutOfBounds(
					t,
					err,
					gate.ReviewValidationFieldContextEntry,
				)
				if len(err.Error()) > 128 ||
					strings.Contains(err.Error(), strconv.Itoa(tt.omittedBytes)) {
					t.Fatalf("error = %q, want bounded and non-echoing", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPermissionReviewSubject() error = %v", err)
			}
			if digest, digestErr := gate.SubjectDigest(got); digestErr != nil ||
				digest != got.Basis.SubjectDigest {
				t.Fatalf(
					"SubjectDigest() = (%x, %v), want stored %x",
					digest,
					digestErr,
					got.Basis.SubjectDigest,
				)
			}
		})
	}
}

func TestPermissionReviewSubjectAcceptsBuilderExactInputBoundWithDistinctPolicyRevisions(t *testing.T) {
	t.Parallel()

	basis, request, input := validPermissionReviewSubjectInput()
	input.GatePolicyRevision = strings.Repeat("g", gate.MaxPermissionReviewPolicyRevisionBytes)
	basis.GatePolicyRevision = input.GatePolicyRevision
	policy := gate.ReviewContextPolicy{
		Revision:             "r",
		MaxBytes:             gate.MaxPermissionReviewSubjectWireBytes,
		MaxEstimatedTokens:   gate.MaxReviewContextInputBytes / 4,
		MaxEntries:           3,
		MaxUserEntryBytes:    gate.MaxReviewContextEntryInputBytes,
		MaxAgentEntryBytes:   gate.MaxReviewContextEntryInputBytes,
		MaxToolEntryBytes:    gate.MaxReviewContextEntryInputBytes,
		MaxBlockBytes:        gate.MaxReviewContextEntryInputBytes,
		MaxActiveActionBytes: gate.MaxReviewContextEntryInputBytes,
	}
	const omittedEntries = 2
	input.Entries = append([]gate.ReviewContextEntry{
		{Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult},
		{Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult},
	}, input.Entries...)
	remaining := gate.MaxReviewContextInputBytes -
		rawPermissionReviewContextTextBytes(input, policy)
	for i := 0; i < omittedEntries; i++ {
		size := min(remaining, gate.MaxReviewContextEntryInputBytes)
		input.Entries[i].Content = strings.Repeat("x", size)
		remaining -= size
	}
	if remaining != 0 ||
		rawPermissionReviewContextTextBytes(input, policy) != gate.MaxReviewContextInputBytes {
		t.Fatalf(
			"test setup remaining = %d, raw bytes = %d",
			remaining,
			rawPermissionReviewContextTextBytes(input, policy),
		)
	}

	built, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext(exact aggregate bound) error = %v", err)
	}
	if built.Truncation.OmittedEntries != omittedEntries {
		t.Fatalf(
			"BuildReviewContext().Truncation.OmittedEntries = %d, want %d",
			built.Truncation.OmittedEntries,
			omittedEntries,
		)
	}
	if _, err := gate.NewPermissionReviewSubject(basis, request, built); err != nil {
		t.Fatalf("NewPermissionReviewSubject(builder output) error = %v", err)
	}

	input.Entries[omittedEntries-1].Content += "x"
	if _, err := gate.BuildReviewContext(input, policy); err == nil {
		t.Fatal("BuildReviewContext(one over aggregate bound) error = nil")
	} else {
		assertReviewSubjectOutOfBounds(t, err, gate.ReviewValidationFieldContextEntry)
	}
}

func TestPermissionReviewSubjectReceivesNoAmbiguousBuilderTruncationMarker(t *testing.T) {
	t.Parallel()

	const marker = "\n…[review context truncated]…\n"
	basis, request, context := validPermissionReviewSubjectInput()
	context.Entries[0].Content = "prefix" + marker + "suffix"
	policy := validReviewContextPolicy()

	built, err := gate.BuildReviewContext(context, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext(non-truncated source marker) error = %v", err)
	}
	if _, err := gate.NewPermissionReviewSubject(basis, request, built); err != nil {
		t.Fatalf("NewPermissionReviewSubject(builder output) error = %v", err)
	}

	context.Entries[0].Content = "prefix-" + marker + strings.Repeat("x", 128)
	policy.MaxUserEntryBytes = 64
	got, err := gate.BuildReviewContext(context, policy)
	if err == nil || !reflect.DeepEqual(got, gate.ReviewContext{}) {
		t.Fatalf(
			"BuildReviewContext(ambiguous truncation marker) = (%#v, %v), want zero, error",
			got,
			err,
		)
	}
	var validationErr *gate.ReviewValidationError
	if !errors.As(err, &validationErr) ||
		validationErr.Field != gate.ReviewValidationFieldContextEntry ||
		validationErr.Reason != gate.ReviewValidationReserved {
		t.Fatalf("BuildReviewContext() error = %T %v, want reserved context-entry error", err, err)
	}
}

func TestPermissionReviewSubjectEnforcesEveryContextRootFieldHardBound(t *testing.T) {
	t.Parallel()

	type rootMutation func(*gate.ReviewBasis, *gate.ReviewContext, int)
	tests := []struct {
		name      string
		limit     int
		wantField gate.ReviewValidationField
		mutate    rootMutation
	}{
		{
			name:      "context revision",
			limit:     gate.MaxReviewContextRootFieldBytes,
			wantField: gate.ReviewValidationFieldBasis,
			mutate: func(basis *gate.ReviewBasis, context *gate.ReviewContext, size int) {
				context.ContextRevision = strings.Repeat("r", size)
				basis.ContextRevision = context.ContextRevision
			},
		},
		{
			name:      "workspace root",
			limit:     gate.MaxReviewContextRootFieldBytes,
			wantField: gate.ReviewValidationFieldContext,
			mutate: func(_ *gate.ReviewBasis, context *gate.ReviewContext, size int) {
				context.WorkspaceRoot = "/" + strings.Repeat("w", size-1)
				context.WorkingDirectory = context.WorkspaceRoot
			},
		},
		{
			name:      "working directory",
			limit:     gate.MaxReviewContextRootFieldBytes,
			wantField: gate.ReviewValidationFieldContext,
			mutate: func(_ *gate.ReviewBasis, context *gate.ReviewContext, size int) {
				context.WorkingDirectory = context.WorkspaceRoot + "/" +
					strings.Repeat("w", size-len(context.WorkspaceRoot)-1)
			},
		},
		{
			name:      "retry reason",
			limit:     gate.MaxReviewContextRootFieldBytes,
			wantField: gate.ReviewValidationFieldContext,
			mutate: func(_ *gate.ReviewBasis, context *gate.ReviewContext, size int) {
				context.RetryReason = strings.Repeat("r", size)
			},
		},
		{
			name:      "security ceiling",
			limit:     gate.MaxReviewContextRootFieldBytes,
			wantField: gate.ReviewValidationFieldBasis,
			mutate: func(basis *gate.ReviewBasis, context *gate.ReviewContext, size int) {
				context.SecurityCeiling = strings.Repeat("s", size)
				basis.SecurityCeiling = context.SecurityCeiling
			},
		},
		{
			name:      "gate policy revision",
			limit:     gate.MaxPermissionReviewPolicyRevisionBytes,
			wantField: gate.ReviewValidationFieldBasis,
			mutate: func(basis *gate.ReviewBasis, context *gate.ReviewContext, size int) {
				context.GatePolicyRevision = strings.Repeat("g", size)
				basis.GatePolicyRevision = context.GatePolicyRevision
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			basis, request, context := validPermissionReviewSubjectInput()
			tt.mutate(&basis, &context, tt.limit)
			if _, err := gate.NewPermissionReviewSubject(basis, request, context); err != nil {
				t.Fatalf("NewPermissionReviewSubject(exact bound) error = %v", err)
			}

			basis, request, context = validPermissionReviewSubjectInput()
			tt.mutate(&basis, &context, tt.limit+1)
			got, err := gate.NewPermissionReviewSubject(basis, request, context)
			if err == nil || !reflect.DeepEqual(got, gate.PermissionReviewSubject{}) {
				t.Fatalf(
					"NewPermissionReviewSubject(one over) returned zero=%t, error=%v; want zero, error",
					reflect.DeepEqual(got, gate.PermissionReviewSubject{}),
					err,
				)
			}
			var validationErr *gate.ReviewValidationError
			if !errors.As(err, &validationErr) ||
				validationErr.Field != tt.wantField ||
				validationErr.Reason != gate.ReviewValidationOutOfBounds {
				t.Fatalf(
					"error = %#v, want %s out_of_bounds",
					validationErr,
					tt.wantField,
				)
			}
		})
	}
}

func TestPermissionReviewSubjectAcceptsBuilderMaximumEntryCount(t *testing.T) {
	t.Parallel()

	basis, request, context := validPermissionReviewSubjectInput()
	entries := make([]gate.ReviewContextEntry, gate.MaxReviewContextInputEntries)
	for index := range entries {
		entries[index] = gate.ReviewContextEntry{
			Origin: gate.ReviewContextOriginAssistant,
			Kind:   gate.ReviewContextKindAssistantMessage,
		}
	}
	copy(entries[len(entries)-2:], context.Entries)
	context.Entries = entries
	if _, err := gate.NewPermissionReviewSubject(basis, request, context); err != nil {
		t.Fatalf("NewPermissionReviewSubject(exact entry bound) error = %v", err)
	}
}

func TestPermissionReviewSubjectRejectsUnexplainedTruncationMasks(t *testing.T) {
	t.Parallel()

	const marker = "\n…[review context truncated]…\n"
	tests := []struct {
		name   string
		mutate func(*gate.ReviewContext)
	}{
		{name: "active action", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationActiveAction
		}},
		{name: "user", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationUserEntry
		}},
		{name: "assistant", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationAssistantEntry
		}},
		{name: "tool", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationToolEntry
		}},
		{name: "block", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationBlock
		}},
		{name: "material entry missing one exercised bit", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Content = "prefix" + marker + "suffix"
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationUserEntry |
				gate.ReviewTruncationBlock
			c.Truncation.Material = gate.ReviewTruncationUserEntry
		}},
		{name: "unexplained material bit", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Content = "prefix" + marker + "suffix"
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationUserEntry |
				gate.ReviewTruncationAssistantEntry
			c.Truncation.Material = gate.ReviewTruncationUserEntry |
				gate.ReviewTruncationAssistantEntry
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

func addPermissionReviewOmission(
	context *gate.ReviewContext,
	omittedEntries int,
	omittedBytes int,
) {
	context.Entries = append([]gate.ReviewContextEntry{{
		Origin: gate.ReviewContextOriginOmission,
		Kind:   gate.ReviewContextKindOmission,
		Content: "omitted_entries=" + strconv.Itoa(omittedEntries) +
			" omitted_bytes=" + strconv.Itoa(omittedBytes),
	}}, context.Entries...)
	context.Truncation = gate.ReviewTruncation{
		Applied:        gate.ReviewTruncationEntryCount,
		Material:       gate.ReviewTruncationEntryCount,
		OmittedEntries: omittedEntries,
		OmittedBytes:   omittedBytes,
	}
}

func minimumOriginalReviewContextEntryLabelBytesForTest() int {
	pairs := []struct {
		origin gate.ReviewContextOrigin
		kind   gate.ReviewContextKind
	}{
		{gate.ReviewContextOriginUser, gate.ReviewContextKindUserMessage},
		{gate.ReviewContextOriginAssistant, gate.ReviewContextKindAssistantMessage},
		{gate.ReviewContextOriginAssistant, gate.ReviewContextKindAssistantToolRequest},
		{gate.ReviewContextOriginTool, gate.ReviewContextKindToolResult},
		{gate.ReviewContextOriginRuntime, gate.ReviewContextKindRuntimeContext},
		{gate.ReviewContextOriginExternal, gate.ReviewContextKindExternalContent},
	}
	minimum := len(pairs[0].origin) + len(pairs[0].kind)
	for _, pair := range pairs[1:] {
		size := len(pair.origin) + len(pair.kind)
		if size < minimum {
			minimum = size
		}
	}
	return minimum
}

func rawPermissionReviewContextTextBytes(
	input gate.ReviewContext,
	policy gate.ReviewContextPolicy,
) int {
	total := len(input.ContextRevision) +
		len(input.WorkspaceRoot) +
		len(input.WorkingDirectory) +
		len(input.RetryReason) +
		len(input.SecurityCeiling) +
		len(input.GatePolicyRevision) +
		len(policy.Revision)
	for _, entry := range input.Entries {
		total += len(entry.Origin) + len(entry.Kind) + len(entry.Content)
	}
	return total
}

func assertReviewSubjectOutOfBounds(
	t *testing.T,
	err error,
	wantField gate.ReviewValidationField,
) {
	t.Helper()

	var validationErr *gate.ReviewValidationError
	if !errors.As(err, &validationErr) ||
		validationErr.Field != wantField ||
		validationErr.Reason != gate.ReviewValidationOutOfBounds {
		t.Fatalf(
			"error = %#v, want field %q reason %q",
			validationErr,
			wantField,
			gate.ReviewValidationOutOfBounds,
		)
	}
}
