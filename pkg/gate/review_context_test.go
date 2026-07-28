package gate_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

func TestReviewContextEnumsAreClosed(t *testing.T) {
	t.Parallel()

	origins := []string{"user", "assistant", "tool", "runtime", "external", "omission"}
	wantOrigins := []gate.ReviewContextOrigin{
		gate.ReviewContextOriginUser,
		gate.ReviewContextOriginAssistant,
		gate.ReviewContextOriginTool,
		gate.ReviewContextOriginRuntime,
		gate.ReviewContextOriginExternal,
		gate.ReviewContextOriginOmission,
	}
	gotOrigins := make([]gate.ReviewContextOrigin, 0, len(origins))
	for _, raw := range origins {
		origin, ok := gate.ParseReviewContextOrigin(raw)
		if !ok {
			t.Fatalf("ParseReviewContextOrigin(%q) ok = false, want true", raw)
		}
		gotOrigins = append(gotOrigins, origin)
	}
	if !reflect.DeepEqual(gotOrigins, wantOrigins) {
		t.Errorf("origins = %v, want %v", gotOrigins, wantOrigins)
	}
	for _, raw := range []string{"", "USER", "unknown"} {
		if got, ok := gate.ParseReviewContextOrigin(raw); ok || got != "" {
			t.Errorf("ParseReviewContextOrigin(%q) = (%q, %t), want zero, false", raw, got, ok)
		}
	}

	kinds := []string{
		"user_message",
		"assistant_message",
		"assistant_tool_request",
		"tool_result",
		"runtime_context",
		"external_content",
		"omission",
	}
	wantKinds := []gate.ReviewContextKind{
		gate.ReviewContextKindUserMessage,
		gate.ReviewContextKindAssistantMessage,
		gate.ReviewContextKindAssistantToolRequest,
		gate.ReviewContextKindToolResult,
		gate.ReviewContextKindRuntimeContext,
		gate.ReviewContextKindExternalContent,
		gate.ReviewContextKindOmission,
	}
	gotKinds := make([]gate.ReviewContextKind, 0, len(kinds))
	for _, raw := range kinds {
		kind, ok := gate.ParseReviewContextKind(raw)
		if !ok {
			t.Fatalf("ParseReviewContextKind(%q) ok = false, want true", raw)
		}
		gotKinds = append(gotKinds, kind)
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Errorf("kinds = %v, want %v", gotKinds, wantKinds)
	}
	for _, raw := range []string{"", "USER_MESSAGE", "unknown"} {
		if got, ok := gate.ParseReviewContextKind(raw); ok || got != "" {
			t.Errorf("ParseReviewContextKind(%q) = (%q, %t), want zero, false", raw, got, ok)
		}
	}
}

func TestReviewContextBuildsAuthorityLabeledSnapshot(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	got, err := gate.BuildReviewContext(input, validReviewContextPolicy())
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(got.Entries))
	}
	if got.Entries[0].Origin != gate.ReviewContextOriginUser ||
		got.Entries[0].Kind != gate.ReviewContextKindUserMessage {
		t.Errorf("first entry = (%q, %q), want user/user_message", got.Entries[0].Origin, got.Entries[0].Kind)
	}
	if got.Entries[1].Origin != gate.ReviewContextOriginAssistant ||
		got.Entries[1].Kind != gate.ReviewContextKindAssistantToolRequest {
		t.Errorf("second entry = (%q, %q), want assistant/assistant_tool_request", got.Entries[1].Origin, got.Entries[1].Kind)
	}

	input.Entries[0].Content = "mutated after build"
	if got.Entries[0].Content == input.Entries[0].Content {
		t.Fatal("BuildReviewContext() output aliases input entries")
	}
	clone := got.Clone()
	clone.Entries[0].Content = "mutated clone"
	if got.Entries[0].Content == clone.Entries[0].Content {
		t.Fatal("ReviewContext.Clone() aliases receiver entries")
	}
}

func TestReviewContextAcceptsOnlyAuthorityKindPairs(t *testing.T) {
	t.Parallel()

	valid := []gate.ReviewContextEntry{
		{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage, Content: "user"},
		{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantMessage, Content: "assistant"},
		{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest, Content: "action"},
		{Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult, Content: "tool"},
		{Origin: gate.ReviewContextOriginRuntime, Kind: gate.ReviewContextKindRuntimeContext, Content: "runtime"},
		{Origin: gate.ReviewContextOriginExternal, Kind: gate.ReviewContextKindExternalContent, Content: "external"},
	}
	for _, entry := range valid {
		entry := entry
		t.Run(string(entry.Kind), func(t *testing.T) {
			t.Parallel()
			input := validReviewContext()
			input.Entries = append([]gate.ReviewContextEntry{entry}, input.Entries...)
			if _, err := gate.BuildReviewContext(input, validReviewContextPolicy()); err != nil {
				t.Fatalf("BuildReviewContext() valid pair error = %v", err)
			}
		})
	}

	allOrigins := []gate.ReviewContextOrigin{
		gate.ReviewContextOriginUser,
		gate.ReviewContextOriginAssistant,
		gate.ReviewContextOriginTool,
		gate.ReviewContextOriginRuntime,
		gate.ReviewContextOriginExternal,
		gate.ReviewContextOriginOmission,
	}
	wantOrigin := map[gate.ReviewContextKind]gate.ReviewContextOrigin{
		gate.ReviewContextKindUserMessage:          gate.ReviewContextOriginUser,
		gate.ReviewContextKindAssistantMessage:     gate.ReviewContextOriginAssistant,
		gate.ReviewContextKindAssistantToolRequest: gate.ReviewContextOriginAssistant,
		gate.ReviewContextKindToolResult:           gate.ReviewContextOriginTool,
		gate.ReviewContextKindRuntimeContext:       gate.ReviewContextOriginRuntime,
		gate.ReviewContextKindExternalContent:      gate.ReviewContextOriginExternal,
		gate.ReviewContextKindOmission:             gate.ReviewContextOriginOmission,
	}
	for kind, expected := range wantOrigin {
		for _, origin := range allOrigins {
			if origin == expected && kind != gate.ReviewContextKindOmission {
				continue
			}
			kind, origin := kind, origin
			t.Run(string(kind)+"/"+string(origin), func(t *testing.T) {
				t.Parallel()
				input := validReviewContext()
				input.Entries = append([]gate.ReviewContextEntry{{
					Origin: origin, Kind: kind, Content: "untrusted-entry",
				}}, input.Entries...)
				assertReviewBuildRejected(t, input, validReviewContextPolicy())
			})
		}
	}
}

func TestReviewContextRejectsInvalidRootAndPolicyState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*gate.ReviewContext, *gate.ReviewContextPolicy)
	}{
		{name: "session id", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Coordinates.SessionID = uuid.UUID{} }},
		{name: "loop id", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Coordinates.LoopID = uuid.UUID{} }},
		{name: "turn id", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Coordinates.TurnID = uuid.UUID{} }},
		{name: "step id", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Coordinates.StepID = uuid.UUID{} }},
		{name: "context revision", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.ContextRevision = "" }},
		{name: "workspace root", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkspaceRoot = "" }},
		{name: "working directory", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkingDirectory = "" }},
		{name: "security ceiling", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.SecurityCeiling = "" }},
		{name: "gate policy revision", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.GatePolicyRevision = "" }},
		{name: "review policy revision", mutate: func(_ *gate.ReviewContext, p *gate.ReviewContextPolicy) { p.Revision = "" }},
		{name: "relative root", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkspaceRoot = "workspace" }},
		{name: "unclean root", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkspaceRoot = "/workspace/.." }},
		{name: "relative cwd", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkingDirectory = "repo" }},
		{name: "unclean cwd", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkingDirectory = "/workspace/repo/.." }},
		{name: "cwd escapes root", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkingDirectory = "/workspace-other" }},
		{name: "truncated input entry", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Entries[0].Truncated = true }},
		{name: "pre-set truncation applied", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Truncation.Applied = 1 }},
		{name: "pre-set truncation material", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Truncation.Material = 1 }},
		{name: "pre-set omitted entries", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Truncation.OmittedEntries = 1 }},
		{name: "pre-set omitted bytes", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Truncation.OmittedBytes = 1 }},
	}
	limitNames := []struct {
		name string
		set  func(*gate.ReviewContextPolicy, int)
	}{
		{name: "max bytes", set: func(p *gate.ReviewContextPolicy, n int) { p.MaxBytes = n }},
		{name: "max tokens", set: func(p *gate.ReviewContextPolicy, n int) { p.MaxEstimatedTokens = n }},
		{name: "max entries", set: func(p *gate.ReviewContextPolicy, n int) { p.MaxEntries = n }},
		{name: "max user", set: func(p *gate.ReviewContextPolicy, n int) { p.MaxUserEntryBytes = n }},
		{name: "max agent", set: func(p *gate.ReviewContextPolicy, n int) { p.MaxAgentEntryBytes = n }},
		{name: "max tool", set: func(p *gate.ReviewContextPolicy, n int) { p.MaxToolEntryBytes = n }},
		{name: "max block", set: func(p *gate.ReviewContextPolicy, n int) { p.MaxBlockBytes = n }},
		{name: "max active", set: func(p *gate.ReviewContextPolicy, n int) { p.MaxActiveActionBytes = n }},
	}
	for _, limit := range limitNames {
		limit := limit
		tests = append(tests,
			struct {
				name   string
				mutate func(*gate.ReviewContext, *gate.ReviewContextPolicy)
			}{name: limit.name + " zero", mutate: func(_ *gate.ReviewContext, p *gate.ReviewContextPolicy) { limit.set(p, 0) }},
			struct {
				name   string
				mutate func(*gate.ReviewContext, *gate.ReviewContextPolicy)
			}{name: limit.name + " negative", mutate: func(_ *gate.ReviewContext, p *gate.ReviewContextPolicy) { limit.set(p, -1) }},
		)
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := validReviewContext()
			policy := validReviewContextPolicy()
			tt.mutate(&input, &policy)
			assertReviewBuildRejected(t, input, policy)
		})
	}
}

func TestReviewContextRejectsInvalidUTF8WithoutEchoing(t *testing.T) {
	t.Parallel()

	secret := string([]byte{0xff, 0xfe}) + "do-not-echo"
	mutators := []func(*gate.ReviewContext, *gate.ReviewContextPolicy){
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.ContextRevision = secret },
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkspaceRoot = "/" + secret },
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.WorkingDirectory = "/" + secret },
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.RetryReason = secret },
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.SecurityCeiling = secret },
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.GatePolicyRevision = secret },
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) {
			c.Entries[0].Origin = gate.ReviewContextOrigin(secret)
		},
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) {
			c.Entries[0].Kind = gate.ReviewContextKind(secret)
		},
		func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy) { c.Entries[0].Content = secret },
		func(_ *gate.ReviewContext, p *gate.ReviewContextPolicy) { p.Revision = secret },
	}
	for i, mutate := range mutators {
		input := validReviewContext()
		policy := validReviewContextPolicy()
		mutate(&input, &policy)
		_, err := gate.BuildReviewContext(input, policy)
		var validationErr *gate.ReviewValidationError
		if !errors.As(err, &validationErr) {
			t.Fatalf("case %d error = %T, want *ReviewValidationError", i, err)
		}
		if strings.Contains(err.Error(), "do-not-echo") {
			t.Fatalf("case %d error leaks input: %q", i, err)
		}
		if len(err.Error()) > 128 {
			t.Fatalf("case %d error has unbounded text length %d", i, len(err.Error()))
		}
	}
}

func TestReviewContextRequiresCurrentIntentAndActiveAction(t *testing.T) {
	t.Parallel()

	t.Run("missing user", func(t *testing.T) {
		input := validReviewContext()
		input.Entries = input.Entries[1:]
		assertReviewBuildRejected(t, input, validReviewContextPolicy())
	})
	t.Run("missing active action", func(t *testing.T) {
		input := validReviewContext()
		input.Entries = input.Entries[:1]
		assertReviewBuildRejected(t, input, validReviewContextPolicy())
	})
	t.Run("oversized active action", func(t *testing.T) {
		input := validReviewContext()
		input.Entries[1].Content = strings.Repeat("x", 11)
		policy := validReviewContextPolicy()
		policy.MaxActiveActionBytes = 10
		assertReviewBuildRejected(t, input, policy)
	})
}

func TestReviewContextPathsUseComponentContainment(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	input.WorkspaceRoot = filepath.Clean("/workspace")
	input.WorkingDirectory = filepath.Clean("/workspace/repository")
	if _, err := gate.BuildReviewContext(input, validReviewContextPolicy()); err != nil {
		t.Fatalf("nested clean path error = %v", err)
	}
	input.WorkingDirectory = filepath.Clean("/workspace-other")
	assertReviewBuildRejected(t, input, validReviewContextPolicy())
}

func TestReviewContextAppliesPerEntryLimitsDeterministically(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		entry        gate.ReviewContextEntry
		setLimit     func(*gate.ReviewContextPolicy, int)
		wantApplied  gate.ReviewTruncationMask
		wantMaterial gate.ReviewTruncationMask
	}{
		{
			name:         "user",
			entry:        gate.ReviewContextEntry{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage},
			setLimit:     func(p *gate.ReviewContextPolicy, n int) { p.MaxUserEntryBytes = n },
			wantApplied:  gate.ReviewTruncationUserEntry,
			wantMaterial: gate.ReviewTruncationUserEntry,
		},
		{
			name:        "assistant",
			entry:       gate.ReviewContextEntry{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantMessage},
			setLimit:    func(p *gate.ReviewContextPolicy, n int) { p.MaxAgentEntryBytes = n },
			wantApplied: gate.ReviewTruncationAssistantEntry,
		},
		{
			name:         "tool",
			entry:        gate.ReviewContextEntry{Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult},
			setLimit:     func(p *gate.ReviewContextPolicy, n int) { p.MaxToolEntryBytes = n },
			wantApplied:  gate.ReviewTruncationToolEntry,
			wantMaterial: gate.ReviewTruncationToolEntry,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const limit = 40
			entry := tt.entry
			entry.Content = strings.Repeat("x", limit)
			input := validReviewContext()
			input.Entries = append([]gate.ReviewContextEntry{entry}, input.Entries...)
			policy := validReviewContextPolicy()
			tt.setLimit(&policy, limit)

			exact, err := gate.BuildReviewContext(input, policy)
			if err != nil {
				t.Fatalf("exact BuildReviewContext() error = %v", err)
			}
			if exact.Entries[0].Truncated || exact.Entries[0].Content != entry.Content {
				t.Errorf("exact entry = %#v, want unchanged", exact.Entries[0])
			}
			if exact.Truncation != (gate.ReviewTruncation{}) {
				t.Errorf("exact truncation = %#v, want zero", exact.Truncation)
			}

			input.Entries[0].Content += "y"
			over, err := gate.BuildReviewContext(input, policy)
			if err != nil {
				t.Fatalf("one-over BuildReviewContext() error = %v", err)
			}
			got := over.Entries[0]
			if !got.Truncated {
				t.Fatal("one-over entry Truncated = false, want true")
			}
			if len(got.Content) > limit {
				t.Errorf("one-over content bytes = %d, want <= %d", len(got.Content), limit)
			}
			if !strings.Contains(got.Content, "[review context truncated]") {
				t.Errorf("one-over content %q lacks fixed truncation marker", got.Content)
			}
			if over.Truncation.Applied != tt.wantApplied {
				t.Errorf("Applied = %#x, want %#x", over.Truncation.Applied, tt.wantApplied)
			}
			if over.Truncation.Material != tt.wantMaterial {
				t.Errorf("Material = %#x, want %#x", over.Truncation.Material, tt.wantMaterial)
			}
		})
	}
}

func TestReviewContextTruncationIsUTF8SafePrefixAndSuffix(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	content := "prefix-" + strings.Repeat("αβγδ", 10) + "-suffix"
	input.Entries = append([]gate.ReviewContextEntry{{
		Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult, Content: content,
	}}, input.Entries...)
	policy := validReviewContextPolicy()
	policy.MaxToolEntryBytes = 40

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	truncated := got.Entries[0].Content
	if !got.Entries[0].Truncated || !strings.Contains(truncated, "[review context truncated]") {
		t.Fatalf("entry was not explicitly truncated: %#v", got.Entries[0])
	}
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncated content is invalid UTF-8: %q", truncated)
	}
	if !strings.HasPrefix(truncated, "p") || !strings.HasSuffix(truncated, "x") {
		t.Errorf("truncated content = %q, want retained prefix and suffix", truncated)
	}
	if len(truncated) > policy.MaxToolEntryBytes {
		t.Errorf("truncated bytes = %d, want <= %d", len(truncated), policy.MaxToolEntryBytes)
	}
}

func TestReviewContextAppliesBlockLimitAndRejectsMisleadingTinyLimit(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	input.Entries[0].Content = "intent"
	input.Entries[1].Content = "action"
	input.Entries = append([]gate.ReviewContextEntry{{
		Origin: gate.ReviewContextOriginRuntime, Kind: gate.ReviewContextKindRuntimeContext,
		Content: strings.Repeat("r", 41),
	}}, input.Entries...)
	policy := validReviewContextPolicy()
	policy.MaxBlockBytes = 40

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	if got.Truncation.Applied != gate.ReviewTruncationBlock {
		t.Errorf("Applied = %#x, want block %#x", got.Truncation.Applied, gate.ReviewTruncationBlock)
	}
	if got.Truncation.Material != gate.ReviewTruncationBlock {
		t.Errorf("Material = %#x, want block %#x", got.Truncation.Material, gate.ReviewTruncationBlock)
	}

	policy.MaxBlockBytes = 10
	assertReviewBuildRejected(t, input, policy)
}

func TestReviewContextTruncationMaskHasClosedSupportedBits(t *testing.T) {
	t.Parallel()

	want := gate.ReviewTruncationUserEntry |
		gate.ReviewTruncationAssistantEntry |
		gate.ReviewTruncationToolEntry |
		gate.ReviewTruncationBlock |
		gate.ReviewTruncationEntryCount |
		gate.ReviewTruncationTotalBytes |
		gate.ReviewTruncationEstimatedTokens |
		gate.ReviewTruncationActiveAction
	if gate.SupportedReviewTruncationMask != want {
		t.Errorf("SupportedReviewTruncationMask = %#x, want %#x", gate.SupportedReviewTruncationMask, want)
	}
	unknown := gate.ReviewTruncationMask(1 << 15)
	if unknown&^gate.SupportedReviewTruncationMask == 0 {
		t.Fatal("unknown truncation bit is not identifiable")
	}
}

func TestReviewContextEntryBudgetRetainsRequiredAndRecentContext(t *testing.T) {
	t.Parallel()

	input := budgetReviewContext(10)
	policy := validReviewContextPolicy()
	policy.MaxEntries = 4

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	assertBudgetedReviewContext(t, got, gate.ReviewTruncationEntryCount, 2, 20, []gate.ReviewContextKind{
		gate.ReviewContextKindOmission,
		gate.ReviewContextKindRuntimeContext,
		gate.ReviewContextKindUserMessage,
		gate.ReviewContextKindAssistantToolRequest,
	})
	if got.Entries[0].Content != "omitted_entries=2 omitted_bytes=20" {
		t.Errorf("omission marker = %q, want stable bounded counts", got.Entries[0].Content)
	}
}

func TestReviewContextTotalByteBudgetUsesCanonicalContextBytes(t *testing.T) {
	t.Parallel()

	input := budgetReviewContext(30)
	policy := validReviewContextPolicy()
	policy.MaxEntries = 4
	entryBounded, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext(entry bound) error = %v", err)
	}
	entryBounded.Truncation.Applied = gate.ReviewTruncationTotalBytes
	entryBounded.Truncation.Material = gate.ReviewTruncationTotalBytes
	policy.MaxEntries = 32
	policy.MaxBytes = canonicalContextBytes(t, entryBounded)

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	if size := canonicalContextBytes(t, got); size > policy.MaxBytes {
		t.Fatalf("canonical bytes = %d, want <= %d", size, policy.MaxBytes)
	}
	assertBudgetedReviewContext(t, got, gate.ReviewTruncationTotalBytes, 2, 60, []gate.ReviewContextKind{
		gate.ReviewContextKindOmission,
		gate.ReviewContextKindRuntimeContext,
		gate.ReviewContextKindUserMessage,
		gate.ReviewContextKindAssistantToolRequest,
	})
}

func TestReviewContextEstimatedTokenBudgetUsesCeilingBytesOverFour(t *testing.T) {
	t.Parallel()

	t.Run("exact estimate is unchanged", func(t *testing.T) {
		input := validReviewContext()
		input.Entries[0].Content = "12345"
		input.Entries[1].Content = "6789"
		policy := validReviewContextPolicy()
		policy.MaxEstimatedTokens = 3 // ceil(9/4)
		got, err := gate.BuildReviewContext(input, policy)
		if err != nil {
			t.Fatalf("BuildReviewContext() error = %v", err)
		}
		if got.Truncation != (gate.ReviewTruncation{}) {
			t.Errorf("Truncation = %#v, want zero", got.Truncation)
		}
	})

	t.Run("one token over omits optional entries", func(t *testing.T) {
		input := budgetReviewContext(30)
		policy := validReviewContextPolicy()
		policy.MaxEstimatedTokens = 20
		got, err := gate.BuildReviewContext(input, policy)
		if err != nil {
			t.Fatalf("BuildReviewContext() error = %v", err)
		}
		if estimatedReviewTokens(got.Entries) > policy.MaxEstimatedTokens {
			t.Fatalf("estimated tokens = %d, want <= %d", estimatedReviewTokens(got.Entries), policy.MaxEstimatedTokens)
		}
		assertBudgetedReviewContext(t, got, gate.ReviewTruncationEstimatedTokens, 2, 60, []gate.ReviewContextKind{
			gate.ReviewContextKindOmission,
			gate.ReviewContextKindRuntimeContext,
			gate.ReviewContextKindUserMessage,
			gate.ReviewContextKindAssistantToolRequest,
		})
	})
}

func TestReviewContextBudgetFailsWhenRequiredEntriesAndMarkerCannotFit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*gate.ReviewContextPolicy)
	}{
		{name: "entry count", mutate: func(p *gate.ReviewContextPolicy) { p.MaxEntries = 2 }},
		{name: "bytes", mutate: func(p *gate.ReviewContextPolicy) {
			input := budgetReviewContext(30)
			input.Entries = []gate.ReviewContextEntry{
				{
					Origin: gate.ReviewContextOriginOmission, Kind: gate.ReviewContextKindOmission,
					Content: "omitted_entries=3 omitted_bytes=90",
				},
				input.Entries[3],
				input.Entries[4],
			}
			input.Truncation.Applied = gate.ReviewTruncationTotalBytes
			input.Truncation.Material = gate.ReviewTruncationTotalBytes
			input.Truncation.OmittedEntries = 3
			input.Truncation.OmittedBytes = 90
			p.MaxBytes = canonicalContextBytes(t, input) - 1
		}},
		{name: "tokens", mutate: func(p *gate.ReviewContextPolicy) { p.MaxEstimatedTokens = 10 }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := budgetReviewContext(30)
			policy := validReviewContextPolicy()
			tt.mutate(&policy)
			assertReviewBuildRejected(t, input, policy)
		})
	}
}

func TestReviewContextBudgetingIsDeterministicAndDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	input := budgetReviewContext(30)
	original := input.Clone()
	policy := validReviewContextPolicy()
	policy.MaxEntries = 4
	entryBounded, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext(entry bound) error = %v", err)
	}
	entryBounded.Truncation.Applied = gate.ReviewTruncationTotalBytes
	entryBounded.Truncation.Material = gate.ReviewTruncationTotalBytes
	policy.MaxEntries = 32
	policy.MaxBytes = canonicalContextBytes(t, entryBounded)
	first, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("first BuildReviewContext() error = %v", err)
	}
	for i := 0; i < 20; i++ {
		got, buildErr := gate.BuildReviewContext(input, policy)
		if buildErr != nil {
			t.Fatalf("run %d BuildReviewContext() error = %v", i, buildErr)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d output differs:\ngot  %#v\nwant %#v", i, got, first)
		}
	}
	if !reflect.DeepEqual(input, original) {
		t.Fatalf("input mutated:\ngot  %#v\nwant %#v", input, original)
	}
}

func TestReviewContextCurrentIntentIsNeverOmitted(t *testing.T) {
	t.Parallel()

	input := budgetReviewContext(30)
	input.Entries[3].Content = strings.Repeat("u", 41)
	policy := validReviewContextPolicy()
	policy.MaxUserEntryBytes = 40
	policy.MaxEntries = 4

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	var current *gate.ReviewContextEntry
	for i := range got.Entries {
		if got.Entries[i].Kind == gate.ReviewContextKindUserMessage {
			current = &got.Entries[i]
		}
	}
	if current == nil || !current.Truncated {
		t.Fatalf("current intent = %#v, want retained and explicitly truncated", current)
	}
	if got.Truncation.Applied&gate.ReviewTruncationUserEntry == 0 ||
		got.Truncation.Material&gate.ReviewTruncationUserEntry == 0 {
		t.Errorf("Truncation = %#v, want material user-entry bit", got.Truncation)
	}
}

func TestReviewContextActiveActionRejectsEveryApplicableBound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*gate.ReviewContextPolicy)
	}{
		{name: "active action", mutate: func(p *gate.ReviewContextPolicy) { p.MaxActiveActionBytes = 5 }},
		{name: "assistant entry", mutate: func(p *gate.ReviewContextPolicy) { p.MaxAgentEntryBytes = 5 }},
		{name: "block", mutate: func(p *gate.ReviewContextPolicy) { p.MaxBlockBytes = 5 }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := validReviewContext()
			input.Entries[1].Content = "action"
			policy := validReviewContextPolicy()
			tt.mutate(&policy)
			assertReviewBuildRejected(t, input, policy)
		})
	}
}

func TestReviewContextCombinedBudgetsRecordEveryExercisedLimit(t *testing.T) {
	t.Parallel()

	input := budgetReviewContext(30)
	policy := validReviewContextPolicy()
	policy.MaxEntries = 4
	entryBounded, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext(entry bound) error = %v", err)
	}
	want := gate.ReviewTruncationEntryCount | gate.ReviewTruncationTotalBytes
	entryBounded.Truncation.Applied = want
	entryBounded.Truncation.Material = want
	policy.MaxBytes = canonicalContextBytes(t, entryBounded)

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	if got.Truncation.Applied != want || got.Truncation.Material != want {
		t.Errorf("Truncation = %#v, want Applied and Material %#x", got.Truncation, want)
	}
}

func TestReviewContextEntryBudgetRecordsTokenConstraintExercisedByCandidate(t *testing.T) {
	t.Parallel()

	input := budgetReviewContext(10)
	policy := validReviewContextPolicy()
	policy.MaxEntries = 3
	// The unmodified five-entry context is exactly eleven estimated tokens.
	// The required entries plus the omission marker fit twelve, while adding
	// the newest optional entry alongside that marker exercises the token
	// budget even though entry count was the only initial failure.
	policy.MaxEstimatedTokens = 12

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	want := gate.ReviewTruncationEntryCount |
		gate.ReviewTruncationEstimatedTokens
	assertBudgetedReviewContext(t, got, want, 3, 30, []gate.ReviewContextKind{
		gate.ReviewContextKindOmission,
		gate.ReviewContextKindUserMessage,
		gate.ReviewContextKindAssistantToolRequest,
	})

	basis, request, _ := validPermissionReviewSubjectInput()
	subject, err := gate.NewPermissionReviewSubject(basis, request, got)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject() error = %v", err)
	}
	digest, err := gate.SubjectDigest(subject)
	if err != nil {
		t.Fatalf("SubjectDigest() error = %v", err)
	}
	if digest != subject.Basis.SubjectDigest {
		t.Fatalf("SubjectDigest() = %x, want stored %x", digest, subject.Basis.SubjectDigest)
	}
}

func TestReviewContextEntryBudgetRecordsByteConstraintExercisedByCandidate(t *testing.T) {
	t.Parallel()

	input := budgetReviewContext(0)
	input.Entries = input.Entries[1:]
	policy := validReviewContextPolicy()
	policy.MaxEntries = 3

	base := input.Clone()
	base.Entries = []gate.ReviewContextEntry{
		{
			Origin:  gate.ReviewContextOriginOmission,
			Kind:    gate.ReviewContextKindOmission,
			Content: "omitted_entries=2 omitted_bytes=0",
		},
		input.Entries[2],
		input.Entries[3],
	}
	base.Truncation = gate.ReviewTruncation{
		Applied:        gate.ReviewTruncationEntryCount,
		Material:       gate.ReviewTruncationEntryCount,
		OmittedEntries: 2,
	}
	candidate := input.Clone()
	candidate.Entries = []gate.ReviewContextEntry{
		{
			Origin:  gate.ReviewContextOriginOmission,
			Kind:    gate.ReviewContextKindOmission,
			Content: "omitted_entries=1 omitted_bytes=0",
		},
		input.Entries[1],
		input.Entries[2],
		input.Entries[3],
	}
	candidate.Truncation = gate.ReviewTruncation{
		Applied:        gate.ReviewTruncationEntryCount,
		Material:       gate.ReviewTruncationEntryCount,
		OmittedEntries: 1,
	}
	fullBytes := canonicalContextBytes(t, input)
	baseBytes := canonicalContextBytes(t, base)
	policy.MaxBytes = max(fullBytes, baseBytes)
	if candidateBytes := canonicalContextBytes(t, candidate); candidateBytes <= policy.MaxBytes {
		t.Fatalf(
			"test setup candidate bytes = %d, want > full/base limit %d",
			candidateBytes,
			policy.MaxBytes,
		)
	}

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	want := gate.ReviewTruncationEntryCount |
		gate.ReviewTruncationTotalBytes
	assertBudgetedReviewContext(t, got, want, 2, 0, []gate.ReviewContextKind{
		gate.ReviewContextKindOmission,
		gate.ReviewContextKindUserMessage,
		gate.ReviewContextKindAssistantToolRequest,
	})

	basis, request, _ := validPermissionReviewSubjectInput()
	subject, err := gate.NewPermissionReviewSubject(basis, request, got)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject() error = %v", err)
	}
	if digest, digestErr := gate.SubjectDigest(subject); digestErr != nil ||
		digest != subject.Basis.SubjectDigest {
		t.Fatalf(
			"SubjectDigest() = (%x, %v), want stored %x",
			digest,
			digestErr,
			subject.Basis.SubjectDigest,
		)
	}
}

func TestReviewContextV1OmissionIsAlwaysMaterial(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	input.Entries[0].Content = "intent"
	input.Entries[1].Content = "action"
	input.Entries = append([]gate.ReviewContextEntry{{
		Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantMessage,
		Content: strings.Repeat("a", 100),
	}}, input.Entries...)
	policy := validReviewContextPolicy()
	expected := input.Clone()
	expected.Entries = []gate.ReviewContextEntry{
		{
			Origin: gate.ReviewContextOriginOmission, Kind: gate.ReviewContextKindOmission,
			Content: "omitted_entries=1 omitted_bytes=100",
		},
		input.Entries[1],
		input.Entries[2],
	}
	expected.Truncation.Applied = gate.ReviewTruncationTotalBytes
	expected.Truncation.Material = gate.ReviewTruncationTotalBytes
	expected.Truncation.OmittedEntries = 1
	expected.Truncation.OmittedBytes = 100
	policy.MaxBytes = canonicalContextBytes(t, expected)

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	if got.Truncation.Applied != gate.ReviewTruncationTotalBytes {
		t.Errorf("Applied = %#x, want total bytes", got.Truncation.Applied)
	}
	if got.Truncation.Material != gate.ReviewTruncationTotalBytes {
		t.Errorf("Material = %#x, want total bytes for conservative v1 omission", got.Truncation.Material)
	}
}

func TestReviewContextRejectsUnknownLabels(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	input.Entries[0].Origin = "unknown-origin"
	assertReviewBuildRejected(t, input, validReviewContextPolicy())
	input = validReviewContext()
	input.Entries[0].Kind = "unknown-kind"
	assertReviewBuildRejected(t, input, validReviewContextPolicy())
}

func TestReviewContextTinyEntryLimitFailsClosed(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	input.Entries[1].Content = "a"
	input.Entries = append([]gate.ReviewContextEntry{{
		Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult,
		Content: strings.Repeat("t", 20),
	}}, input.Entries...)
	policy := validReviewContextPolicy()
	policy.MaxToolEntryBytes = 10
	assertReviewBuildRejected(t, input, policy)
}

func TestReviewContextTruncationRebalancesForWideSuffixRune(t *testing.T) {
	t.Parallel()

	const marker = "\n…[review context truncated]…\n"
	input := validReviewContext()
	input.Entries = append([]gate.ReviewContextEntry{{
		Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult,
		Content: "a" + strings.Repeat("b", 40) + "😀",
	}}, input.Entries...)
	policy := validReviewContextPolicy()
	policy.MaxToolEntryBytes = len(marker) + len("a😀")

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v, marker and useful prefix/suffix fit", err)
	}
	if got.Entries[0].Content != "a"+marker+"😀" {
		t.Errorf("truncated content = %q, want balanced wide-rune suffix", got.Entries[0].Content)
	}
}

func TestReviewContextBudgetsRetainFinalUserAndFinalToolRequest(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	input.Entries = []gate.ReviewContextEntry{
		{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage, Content: "old intent"},
		{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest, Content: "old action"},
		{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage, Content: "current intent"},
		{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest, Content: "active action"},
	}
	policy := validReviewContextPolicy()
	policy.MaxEntries = 3

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	if len(got.Entries) != 3 ||
		got.Entries[1].Content != "current intent" ||
		got.Entries[2].Content != "active action" {
		t.Fatalf("entries = %#v, want marker plus final intent and action", got.Entries)
	}
}

func TestReviewContextHardInputBounds(t *testing.T) {
	t.Parallel()

	t.Run("entry count exact", func(t *testing.T) {
		t.Parallel()
		input := validReviewContext()
		optional := make([]gate.ReviewContextEntry, gate.MaxReviewContextInputEntries-2)
		for i := range optional {
			optional[i] = gate.ReviewContextEntry{
				Origin:  gate.ReviewContextOriginAssistant,
				Kind:    gate.ReviewContextKindAssistantMessage,
				Content: "x",
			}
		}
		input.Entries = append(optional, input.Entries...)
		policy := maximumReviewContextPolicy()
		if _, err := gate.BuildReviewContext(input, policy); err != nil {
			t.Fatalf("BuildReviewContext(exact entries) error = %v", err)
		}
		input.Entries = append([]gate.ReviewContextEntry{optional[0]}, input.Entries...)
		assertReviewBuildRejected(t, input, policy)
	})

	t.Run("entry bytes exact", func(t *testing.T) {
		t.Parallel()
		input := validReviewContext()
		input.Entries = append([]gate.ReviewContextEntry{{
			Origin: gate.ReviewContextOriginAssistant,
			Kind:   gate.ReviewContextKindAssistantMessage,
			Content: strings.Repeat(
				"x",
				gate.MaxReviewContextEntryInputBytes,
			),
		}}, input.Entries...)
		policy := maximumReviewContextPolicy()
		if _, err := gate.BuildReviewContext(input, policy); err != nil {
			t.Fatalf("BuildReviewContext(exact entry bytes) error = %v", err)
		}
		input.Entries[0].Content += "x"
		assertReviewBuildRejected(t, input, policy)
	})

	t.Run("aggregate bytes exact", func(t *testing.T) {
		t.Parallel()
		input := validReviewContext()
		input.Entries[0].Content = "u"
		input.Entries[1].Content = "a"
		policy := maximumReviewContextPolicy()
		const optionalEntries = 3
		for range optionalEntries {
			input.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginAssistant,
				Kind:   gate.ReviewContextKindAssistantMessage,
			}}, input.Entries...)
		}
		remaining := gate.MaxReviewContextInputBytes - rawReviewContextTextBytes(input, policy)
		for i := 0; i < optionalEntries; i++ {
			size := min(remaining, gate.MaxReviewContextEntryInputBytes)
			input.Entries[i].Content = strings.Repeat("x", size)
			remaining -= size
		}
		if remaining != 0 || rawReviewContextTextBytes(input, policy) != gate.MaxReviewContextInputBytes {
			t.Fatalf("test setup remaining = %d, raw bytes = %d", remaining, rawReviewContextTextBytes(input, policy))
		}
		if _, err := gate.BuildReviewContext(input, policy); err != nil {
			t.Fatalf("BuildReviewContext(exact aggregate bytes) error = %v", err)
		}
		input.Entries[optionalEntries-1].Content += "x"
		assertReviewBuildRejected(t, input, policy)
	})
}

func TestReviewContextRootFieldHardBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*gate.ReviewContext, *gate.ReviewContextPolicy, int)
	}{
		{name: "context revision", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy, n int) {
			c.ContextRevision = strings.Repeat("r", n)
		}},
		{name: "workspace root", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy, n int) {
			c.WorkspaceRoot = "/" + strings.Repeat("r", n-1)
			c.WorkingDirectory = c.WorkspaceRoot
		}},
		{name: "working directory", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy, n int) {
			c.WorkingDirectory = c.WorkspaceRoot + "/" + strings.Repeat("r", n-len(c.WorkspaceRoot)-1)
		}},
		{name: "retry reason", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy, n int) {
			c.RetryReason = strings.Repeat("r", n)
		}},
		{name: "security ceiling", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy, n int) {
			c.SecurityCeiling = strings.Repeat("r", n)
		}},
		{name: "gate policy revision", mutate: func(c *gate.ReviewContext, _ *gate.ReviewContextPolicy, n int) {
			c.GatePolicyRevision = strings.Repeat("r", n)
		}},
		{name: "review policy revision", mutate: func(_ *gate.ReviewContext, p *gate.ReviewContextPolicy, n int) {
			p.Revision = strings.Repeat("r", n)
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := validReviewContext()
			policy := maximumReviewContextPolicy()
			tt.mutate(&input, &policy, gate.MaxReviewContextRootFieldBytes)
			if _, err := gate.BuildReviewContext(input, policy); err != nil {
				t.Fatalf("BuildReviewContext(exact root field) error = %v", err)
			}
			tt.mutate(&input, &policy, gate.MaxReviewContextRootFieldBytes+1)
			assertReviewBuildRejected(t, input, policy)
		})
	}
}

func TestReviewContextRejectsConsumerLimitsOverHardBounds(t *testing.T) {
	t.Parallel()

	if _, err := gate.BuildReviewContext(validReviewContext(), maximumReviewContextPolicy()); err != nil {
		t.Fatalf("BuildReviewContext(exact maximum policy) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*gate.ReviewContextPolicy)
	}{
		{name: "canonical bytes", mutate: func(p *gate.ReviewContextPolicy) {
			p.MaxBytes = gate.MaxPermissionReviewSubjectWireBytes + 1
		}},
		{name: "estimated tokens", mutate: func(p *gate.ReviewContextPolicy) {
			p.MaxEstimatedTokens = gate.MaxReviewContextInputBytes/4 + 1
		}},
		{name: "entries", mutate: func(p *gate.ReviewContextPolicy) {
			p.MaxEntries = gate.MaxReviewContextInputEntries + 1
		}},
		{name: "user entry", mutate: func(p *gate.ReviewContextPolicy) {
			p.MaxUserEntryBytes = gate.MaxReviewContextEntryInputBytes + 1
		}},
		{name: "assistant entry", mutate: func(p *gate.ReviewContextPolicy) {
			p.MaxAgentEntryBytes = gate.MaxReviewContextEntryInputBytes + 1
		}},
		{name: "tool entry", mutate: func(p *gate.ReviewContextPolicy) {
			p.MaxToolEntryBytes = gate.MaxReviewContextEntryInputBytes + 1
		}},
		{name: "block", mutate: func(p *gate.ReviewContextPolicy) {
			p.MaxBlockBytes = gate.MaxReviewContextEntryInputBytes + 1
		}},
		{name: "active action", mutate: func(p *gate.ReviewContextPolicy) {
			p.MaxActiveActionBytes = gate.MaxReviewContextEntryInputBytes + 1
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := maximumReviewContextPolicy()
			tt.mutate(&policy)
			assertReviewBuildRejected(t, validReviewContext(), policy)
		})
	}
}

func TestReviewContextReducesIntermediateHistoryOverSubjectCeiling(t *testing.T) {
	t.Parallel()

	input := validReviewContext()
	input.Entries = append([]gate.ReviewContextEntry{
		{
			Origin:  gate.ReviewContextOriginAssistant,
			Kind:    gate.ReviewContextKindAssistantMessage,
			Content: strings.Repeat("a", 600<<10),
		},
		{
			Origin:  gate.ReviewContextOriginTool,
			Kind:    gate.ReviewContextKindToolResult,
			Content: strings.Repeat("t", 600<<10),
		},
	}, input.Entries...)
	policy := maximumReviewContextPolicy()
	policy.MaxBytes = 128 << 10

	got, err := gate.BuildReviewContext(input, policy)
	if err != nil {
		t.Fatalf("BuildReviewContext() reducible >1 MiB history error = %v", err)
	}
	if got.Truncation.OmittedEntries != 2 {
		t.Fatalf("OmittedEntries = %d, want 2", got.Truncation.OmittedEntries)
	}
	if size := canonicalContextBytes(t, got); size > policy.MaxBytes ||
		size > gate.MaxPermissionReviewSubjectWireBytes {
		t.Fatalf("canonical bytes = %d, want <= %d and <= subject ceiling", size, policy.MaxBytes)
	}
}

func maximumReviewContextPolicy() gate.ReviewContextPolicy {
	return gate.ReviewContextPolicy{
		Revision:             "review-policy-v1",
		MaxBytes:             gate.MaxPermissionReviewSubjectWireBytes,
		MaxEstimatedTokens:   gate.MaxReviewContextInputBytes / 4,
		MaxEntries:           gate.MaxReviewContextInputEntries,
		MaxUserEntryBytes:    gate.MaxReviewContextEntryInputBytes,
		MaxAgentEntryBytes:   gate.MaxReviewContextEntryInputBytes,
		MaxToolEntryBytes:    gate.MaxReviewContextEntryInputBytes,
		MaxBlockBytes:        gate.MaxReviewContextEntryInputBytes,
		MaxActiveActionBytes: gate.MaxReviewContextEntryInputBytes,
	}
}

func rawReviewContextTextBytes(input gate.ReviewContext, policy gate.ReviewContextPolicy) int {
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

func validReviewContext() gate.ReviewContext {
	return gate.ReviewContext{
		Coordinates: identity.Coordinates{
			SessionID: uuid.MustParse("123e4567-e89b-12d3-a456-426614174101"),
			LoopID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174102"),
			TurnID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174103"),
			StepID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174104"),
		},
		ContextRevision:    "context-v1",
		WorkspaceRoot:      "/workspace",
		WorkingDirectory:   "/workspace/repo",
		SecurityCeiling:    "workspace-write",
		GatePolicyRevision: "gate-policy-v1",
		Entries: []gate.ReviewContextEntry{
			{
				Origin:  gate.ReviewContextOriginUser,
				Kind:    gate.ReviewContextKindUserMessage,
				Content: "please inspect the repository",
			},
			{
				Origin:  gate.ReviewContextOriginAssistant,
				Kind:    gate.ReviewContextKindAssistantToolRequest,
				Content: `{"command":"rg TODO"}`,
			},
		},
	}
}

func validReviewContextPolicy() gate.ReviewContextPolicy {
	return gate.ReviewContextPolicy{
		Revision:             "review-policy-v1",
		MaxBytes:             4096,
		MaxEstimatedTokens:   1024,
		MaxEntries:           32,
		MaxUserEntryBytes:    1024,
		MaxAgentEntryBytes:   1024,
		MaxToolEntryBytes:    1024,
		MaxBlockBytes:        2048,
		MaxActiveActionBytes: 1024,
	}
}

func budgetReviewContext(optionalBytes int) gate.ReviewContext {
	input := validReviewContext()
	input.Entries[0].Content = "intent"
	input.Entries[1].Content = "action"
	input.Entries = append([]gate.ReviewContextEntry{
		{
			Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantMessage,
			Content: strings.Repeat("a", optionalBytes),
		},
		{
			Origin: gate.ReviewContextOriginTool, Kind: gate.ReviewContextKindToolResult,
			Content: strings.Repeat("t", optionalBytes),
		},
		{
			Origin: gate.ReviewContextOriginRuntime, Kind: gate.ReviewContextKindRuntimeContext,
			Content: strings.Repeat("r", optionalBytes),
		},
	}, input.Entries...)
	return input
}

func assertBudgetedReviewContext(
	t *testing.T,
	got gate.ReviewContext,
	wantMask gate.ReviewTruncationMask,
	wantOmittedEntries int,
	wantOmittedBytes int,
	wantKinds []gate.ReviewContextKind,
) {
	t.Helper()
	if got.Truncation.Applied != wantMask {
		t.Errorf("Applied = %#x, want %#x", got.Truncation.Applied, wantMask)
	}
	if got.Truncation.Material != wantMask {
		t.Errorf("Material = %#x, want %#x", got.Truncation.Material, wantMask)
	}
	if got.Truncation.OmittedEntries != wantOmittedEntries {
		t.Errorf("OmittedEntries = %d, want %d", got.Truncation.OmittedEntries, wantOmittedEntries)
	}
	if got.Truncation.OmittedBytes != wantOmittedBytes {
		t.Errorf("OmittedBytes = %d, want %d", got.Truncation.OmittedBytes, wantOmittedBytes)
	}
	kinds := make([]gate.ReviewContextKind, len(got.Entries))
	for i := range got.Entries {
		kinds[i] = got.Entries[i].Kind
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("entry kinds = %v, want %v", kinds, wantKinds)
	}
}

func contentBytes(entries []gate.ReviewContextEntry) int {
	total := 0
	for _, entry := range entries {
		total += len(entry.Content)
	}
	return total
}

func estimatedReviewTokens(entries []gate.ReviewContextEntry) int {
	return (contentBytes(entries) + 3) / 4
}

func canonicalContextBytes(t testing.TB, context gate.ReviewContext) int {
	t.Helper()
	size, err := gate.CanonicalReviewContextSizeForTest(context)
	if err != nil {
		t.Fatalf("canonical context size error = %v", err)
	}
	return size
}

func assertReviewBuildRejected(t *testing.T, input gate.ReviewContext, policy gate.ReviewContextPolicy) {
	t.Helper()
	got, err := gate.BuildReviewContext(input, policy)
	if !reflect.DeepEqual(got, gate.ReviewContext{}) {
		t.Errorf("BuildReviewContext() output on error = %#v, want zero value", got)
	}
	var validationErr *gate.ReviewValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("BuildReviewContext() error = %T %v, want *ReviewValidationError", err, err)
	}
}
