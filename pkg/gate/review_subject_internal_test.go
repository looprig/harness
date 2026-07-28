package gate

import (
	"math"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

func TestPermissionReviewRequestPreflightExactAndOneOverBounds(t *testing.T) {
	t.Parallel()

	nestedAggregateExact := tool.Request{
		Command: strings.Repeat("x", MaxPermissionReviewRequestInputBytes-11),
		Requirements: []tool.Requirement{{
			Kind:        "a",
			Scope:       "b",
			Match:       "c",
			Description: "d",
			GrantClass:  "e",
			GrantTarget: "f",
			Candidates: []tool.RuleCandidate{{
				Kind:        "g",
				Match:       "h",
				Description: "i",
				GrantClass:  "j",
				GrantTarget: "k",
			}},
		}},
	}
	nestedAggregateOneOver := nestedAggregateExact.Clone()
	nestedAggregateOneOver.Requirements[0].Candidates[0].GrantTarget = "kl"

	tests := []struct {
		name    string
		exact   tool.Request
		oneOver tool.Request
	}{
		{
			name: "requirements",
			exact: tool.Request{
				Requirements: make([]tool.Requirement, MaxPermissionReviewRequestRequirements),
			},
			oneOver: tool.Request{
				Requirements: make([]tool.Requirement, MaxPermissionReviewRequestRequirements+1),
			},
		},
		{
			name: "candidates aggregate across requirements",
			exact: tool.Request{Requirements: []tool.Requirement{
				{Candidates: make([]tool.RuleCandidate, MaxPermissionReviewRequestCandidates/2)},
				{Candidates: make([]tool.RuleCandidate, MaxPermissionReviewRequestCandidates/2)},
			}},
			oneOver: tool.Request{Requirements: []tool.Requirement{
				{Candidates: make([]tool.RuleCandidate, MaxPermissionReviewRequestCandidates/2)},
				{Candidates: make([]tool.RuleCandidate, MaxPermissionReviewRequestCandidates/2+1)},
			}},
		},
		{
			name: "one string",
			exact: tool.Request{
				Command: strings.Repeat("x", MaxPermissionReviewRequestStringBytes),
			},
			oneOver: tool.Request{
				Command: strings.Repeat("x", MaxPermissionReviewRequestStringBytes+1),
			},
		},
		{
			name: "aggregate strings",
			exact: tool.Request{
				ToolName: strings.Repeat("x", MaxPermissionReviewRequestInputBytes/2),
				Summary:  strings.Repeat("y", MaxPermissionReviewRequestInputBytes/2),
			},
			oneOver: tool.Request{
				ToolName: strings.Repeat("x", MaxPermissionReviewRequestInputBytes/2),
				Summary:  strings.Repeat("y", MaxPermissionReviewRequestInputBytes/2+1),
			},
		},
		{
			name:    "aggregate includes requirement and candidate strings",
			exact:   nestedAggregateExact,
			oneOver: nestedAggregateOneOver,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if reason := permissionReviewRequestPreflightReason(tt.exact); reason != "" {
				t.Fatalf("permissionReviewRequestPreflightReason(exact) = %q, want empty", reason)
			}
			if reason := permissionReviewRequestPreflightReason(tt.oneOver); reason == "" {
				t.Fatal("permissionReviewRequestPreflightReason(one over) is empty")
			}
		})
	}
}

func TestPermissionReviewRequestPreflightCoversEveryStringAndUTF8(t *testing.T) {
	t.Parallel()

	request := tool.Request{
		ToolName:         "tool",
		Summary:          "summary",
		ExecutionID:      "execution",
		Command:          "command",
		WorkingDirectory: "working-directory",
		Requirements: []tool.Requirement{{
			Kind:        "kind",
			Scope:       "scope",
			Match:       "match",
			Description: "description",
			GrantClass:  "grant-class",
			GrantTarget: "grant-target",
			Candidates: []tool.RuleCandidate{{
				Kind:        "candidate-kind",
				Match:       "candidate-match",
				Description: "candidate-description",
				GrantClass:  "candidate-grant-class",
				GrantTarget: "candidate-grant-target",
			}},
		}},
	}
	if reason := permissionReviewRequestPreflightReason(request); reason != "" {
		t.Fatalf("permissionReviewRequestPreflightReason(valid) = %q, want empty", reason)
	}

	request.Requirements[0].Candidates[0].GrantTarget = string([]byte{0xff})
	if reason := permissionReviewRequestPreflightReason(request); reason != ReviewValidationInvalid {
		t.Fatalf(
			"permissionReviewRequestPreflightReason(invalid UTF-8) = %q, want %q",
			reason,
			ReviewValidationInvalid,
		)
	}
}

func TestPermissionReviewRequestPreflightIsAllocationFree(t *testing.T) {
	request := tool.Request{
		Requirements: make([]tool.Requirement, MaxPermissionReviewRequestRequirements),
	}
	for index := range request.Requirements {
		request.Requirements[index].Candidates = []tool.RuleCandidate{{}}
	}
	if reason := permissionReviewRequestPreflightReason(request); reason != "" {
		t.Fatalf("permissionReviewRequestPreflightReason() = %q, want empty", reason)
	}

	if allocations := testing.AllocsPerRun(100, func() {
		if reason := permissionReviewRequestPreflightReason(request); reason != "" {
			panic("preflight unexpectedly rejected fixed request")
		}
	}); allocations != 0 {
		t.Fatalf("preflight allocations = %f, want 0", allocations)
	}
}

func TestPermissionReviewSubjectRejectsHardBoundsBeforeDeepClone(t *testing.T) {
	base := validPermissionReviewSubject(t)
	basis := base.Basis
	basis.SubjectDigest = [32]byte{}

	tests := []struct {
		name    string
		request tool.Request
		context ReviewContext
	}{
		{
			name: "request",
			request: tool.Request{
				Requirements: make(
					[]tool.Requirement,
					MaxPermissionReviewRequestRequirements+1,
				),
			},
			context: base.Context,
		},
		{
			name:    "context",
			request: base.Request,
			context: func() ReviewContext {
				context := base.Context
				context.Entries = make(
					[]ReviewContextEntry,
					MaxReviewContextInputEntries+1,
				)
				return context
			}(),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewPermissionReviewSubject(basis, tt.request, tt.context); err == nil {
				t.Fatal("NewPermissionReviewSubject() error = nil")
			}
			allocations := testing.AllocsPerRun(100, func() {
				if _, err := NewPermissionReviewSubject(basis, tt.request, tt.context); err == nil {
					panic("hard-bound input unexpectedly accepted")
				}
			})
			if allocations > 1 {
				t.Fatalf(
					"NewPermissionReviewSubject() allocations = %f, want at most one validation error allocation",
					allocations,
				)
			}
		})
	}
}

func TestPermissionReviewTypedEncodingPathsPreflightBeforeProjection(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	subject.Request.Requirements = make(
		[]tool.Requirement,
		MaxPermissionReviewRequestRequirements+1,
	)

	if digest, err := SubjectDigest(subject); err == nil || digest != ([32]byte{}) {
		t.Fatalf("SubjectDigest() = (%x, %v), want zero, error", digest, err)
	}
	if digest, err := permissionReviewSubjectDigest(subject); err == nil ||
		digest != ([32]byte{}) {
		t.Fatalf(
			"permissionReviewSubjectDigest() = (%x, %v), want zero, error",
			digest,
			err,
		)
	}
	if digest, err := permissionReviewCommonSubjectDigest(subject); err == nil ||
		digest != ([32]byte{}) {
		t.Fatalf(
			"permissionReviewCommonSubjectDigest() = (%x, %v), want zero, error",
			digest,
			err,
		)
	}
	if data, err := marshalPermissionReviewSubject(subject); err == nil || data != nil {
		t.Fatalf("marshalPermissionReviewSubject() = (%q, %v), want nil, error", data, err)
	}
}

func TestCheckedPermissionReviewRequestAddBoundaries(t *testing.T) {
	t.Parallel()

	if got, ok := checkedPermissionReviewRequestAdd(math.MaxInt-1, 1); !ok || got != math.MaxInt {
		t.Fatalf("checked add exact = (%d, %t), want (%d, true)", got, ok, math.MaxInt)
	}
	if got, ok := checkedPermissionReviewRequestAdd(math.MaxInt, 1); ok || got != 0 {
		t.Fatalf("checked add overflow = (%d, %t), want (0, false)", got, ok)
	}
	if got, ok := checkedPermissionReviewRequestAdd(-1, 0); ok || got != 0 {
		t.Fatalf("checked add negative = (%d, %t), want (0, false)", got, ok)
	}
}
