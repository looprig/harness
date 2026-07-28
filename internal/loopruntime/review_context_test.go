package loopruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	loopapi "github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestCapturePermissionReviewContextAtToolBatchBoundary(t *testing.T) {
	t.Parallel()

	coordinates := identity.Coordinates{
		SessionID: mustReviewUUID(t),
		LoopID:    mustReviewUUID(t),
		TurnID:    mustReviewUUID(t),
		StepID:    mustReviewUUID(t),
	}
	base := content.AgenticMessages{
		reviewUserMessage("earlier human intent"),
		reviewAIMessage(&content.TextBlock{Text: "recent assistant"}),
		&content.ToolResultMessage{
			Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: "recent result"}}},
			ToolUseID: "earlier-call",
		},
	}
	staged := content.AgenticMessages{
		reviewUserMessage("current human request"),
		reviewUserMessage("human follow-up"),
	}
	active := reviewAIMessage(
		&content.TextBlock{Text: "I will inspect it."},
		&content.ToolUseBlock{ID: "active-call", Name: "Read", Input: json.RawMessage(`{"path":"README.md"}`)},
	)
	runtimeTail := reviewUserMessage("cwd=/workspace/repo")
	base = append(base, &content.ToolResultMessage{
		Message: content.Message{Role: content.RoleTool, Blocks: []content.Block{
			&content.DocumentBlock{Name: "fetched.txt", Text: "external fetched content"},
		}},
		ToolUseID: "fetch-call",
	})

	got, err := capturePermissionReviewContext(reviewContextCapture{
		Coordinates: coordinates,
		Base:        base,
		Retained:    content.AgenticMessages{reviewUserMessage("derived retained intent summary")},
		Staged:      staged,
		Active:      active,
		RuntimeTail: runtimeTail,
		Metadata: reviewContextMetadata{
			WorkspaceRoot:      "/workspace",
			WorkingDirectory:   "/workspace/repo",
			RetryReason:        "retry after access escalation",
			SecurityCeiling:    "workspace-write; unmet-requirements=true",
			GatePolicyRevision: "gate-policy-v1",
		},
		Policy: testReviewContextPolicy(),
	})
	if err != nil {
		t.Fatalf("capturePermissionReviewContext() error = %v", err)
	}
	if got.Coordinates != coordinates {
		t.Errorf("Coordinates = %+v, want %+v", got.Coordinates, coordinates)
	}
	if got.ContextRevision == "" {
		t.Fatal("ContextRevision = empty")
	}
	if got.WorkspaceRoot != "/workspace" ||
		got.WorkingDirectory != "/workspace/repo" ||
		got.GatePolicyRevision != "gate-policy-v1" ||
		got.SecurityCeiling != "workspace-write; unmet-requirements=true" ||
		got.RetryReason != "retry after access escalation" {
		t.Fatalf("metadata = %+v, want exact configured metadata", got)
	}

	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginUser, gate.ReviewContextKindUserMessage, "earlier human intent")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginRuntime, gate.ReviewContextKindRuntimeContext, "derived retained intent summary")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginAssistant, gate.ReviewContextKindAssistantMessage, "recent assistant")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginTool, gate.ReviewContextKindToolResult, "recent result")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginUser, gate.ReviewContextKindUserMessage, "current human request")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginUser, gate.ReviewContextKindUserMessage, "human follow-up")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginRuntime, gate.ReviewContextKindRuntimeContext, "cwd=/workspace/repo")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginExternal, gate.ReviewContextKindExternalContent, "external fetched content")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginAssistant, gate.ReviewContextKindAssistantMessage, "I will inspect it.")
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginAssistant, gate.ReviewContextKindAssistantToolRequest, `"active-call"`)
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginAssistant, gate.ReviewContextKindAssistantToolRequest, `"README.md"`)

	again, err := capturePermissionReviewContext(reviewContextCapture{
		Coordinates: coordinates, Base: base,
		Retained: content.AgenticMessages{reviewUserMessage("derived retained intent summary")},
		Staged:   staged, Active: active, RuntimeTail: runtimeTail,
		Metadata: gotMetadata(got), Policy: testReviewContextPolicy(),
	})
	if err != nil {
		t.Fatalf("second capture error = %v", err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatal("capture is not deterministic for identical turn state")
	}
	staged[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "mutated source"
	assertReviewEntry(t, got.Entries, gate.ReviewContextOriginUser, gate.ReviewContextKindUserMessage, "current human request")
}

func TestPermissionReviewCaptureFailsClosedBeforeUnboundedEncoding(t *testing.T) {
	t.Parallel()

	valid := validReviewCapture(t)
	tests := []struct {
		name      string
		mutate    func(*reviewContextCapture)
		wantField string
	}{
		{
			name: "missing required metadata",
			mutate: func(input *reviewContextCapture) {
				input.Metadata.SecurityCeiling = ""
			},
			wantField: "metadata",
		},
		{
			name: "typed nil block",
			mutate: func(input *reviewContextCapture) {
				var block *content.TextBlock
				input.Staged = content.AgenticMessages{&content.UserMessage{
					Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{block}},
				}}
			},
			wantField: "content_block",
		},
		{
			name: "one entry over hard input bound",
			mutate: func(input *reviewContextCapture) {
				input.Staged = content.AgenticMessages{reviewUserMessage(
					strings.Repeat("do-not-echo", gate.MaxReviewContextEntryInputBytes/len("do-not-echo")+1),
				)}
			},
			wantField: "input_bounds",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := valid
			tt.mutate(&input)
			_, err := capturePermissionReviewContext(input)
			var captureErr *reviewContextCaptureError
			if !errors.As(err, &captureErr) || captureErr.Field != tt.wantField {
				t.Fatalf("error = %T %v, want capture field %q", err, err, tt.wantField)
			}
			if strings.Contains(err.Error(), "do-not-echo") {
				t.Fatal("bounded error echoed snapshot content")
			}
		})
	}
}

func TestRunTurnClonesOnePrivateReviewContextPerPermissionGate(t *testing.T) {
	t.Parallel()

	runTool := &fakeRunTool{name: "T", output: "ok"}
	runTool.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git status", false), nil, nil
	}
	tools := resolveToolSetCaps(ToolSet{
		Access:               interactiveEvaluator(t, gate.AccessGated, &recordingRuleWriter{}, &recordingIssuer{}),
		Registry:             []tool.InvokableTool{runTool},
		MaxToolIterations:    25,
		MaxToolCallsPerTurn:  100,
		MaxParallelToolCalls: 2,
	})
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{
			toolUseChunk(0, "active-1", "T", `{}`),
			toolUseChunk(1, "active-2", "T", `{}`),
		},
		{textChunk("done")},
	}}
	gateReg := make(chan gateRegistration)
	registrations := make(chan gateRegistration, 2)
	go func() {
		for index := 0; index < 2; index++ {
			registration := <-gateReg
			registrations <- registration
			close(registration.ack)
			registration.reply <- approveCommand(registration.callID)
		}
	}()

	cfg, state, recorder := newTurnFixture(
		[]content.Block{&content.TextBlock{Text: "current request"}},
		content.AgenticMessages{reviewUserMessage("earlier user intent")},
		tools,
		client,
		gateReg,
	)
	cfg.reviewContext = &reviewContextConfiguration{
		Metadata: reviewContextMetadata{
			WorkspaceRoot:      "/workspace",
			WorkingDirectory:   "/workspace",
			SecurityCeiling:    "workspace-write; unmet-requirements=true",
			GatePolicyRevision: "gate-policy-v1",
		},
		Policy: testReviewContextPolicy(),
	}
	if terminal := runTurn(context.Background(), cfg, state); terminal == nil {
		t.Fatal("runTurn() terminal = nil")
	}

	first := <-registrations
	second := <-registrations
	if first.reviewContext.ContextRevision == "" {
		t.Fatal("first registration review context is empty")
	}
	if first.reviewContext.ContextRevision != second.reviewContext.ContextRevision {
		t.Fatalf("context revisions differ: %q != %q", first.reviewContext.ContextRevision, second.reviewContext.ContextRevision)
	}
	if &first.reviewContext.Entries[0] == &second.reviewContext.Entries[0] {
		t.Fatal("permission registrations alias review-context entries")
	}
	before := second.reviewContext.Entries[0].Content
	first.reviewContext.Entries[0].Content = "mutated first registration"
	if second.reviewContext.Entries[0].Content != before {
		t.Fatal("mutating first registration changed second registration")
	}
	for _, durable := range []any{first.gate, first.payload, recorder.events()} {
		encoded, err := json.Marshal(durable)
		if err != nil {
			t.Fatalf("marshal durable projection: %v", err)
		}
		if strings.Contains(string(encoded), "current request") ||
			strings.Contains(string(encoded), first.reviewContext.ContextRevision) {
			t.Fatal("live review snapshot leaked into gate/event projection")
		}
	}

	toolContext := withPermissionReviewContext(context.Background(), first.reviewContext)
	if _, ok := loopapi.PreparedCallFromContext(toolContext); ok {
		t.Fatal("public prepared-call API retrieved private review context")
	}
	if _, ok := loopapi.ToolUseIDFrom(toolContext); ok {
		t.Fatal("public tool-use API retrieved private review context")
	}
}

func TestPermissionReviewCaptureUsesCanonicalDeterministicTruncation(t *testing.T) {
	t.Parallel()

	input := validReviewCapture(t)
	input.Base = content.AgenticMessages{
		reviewUserMessage("earlier human"),
		reviewAIMessage(&content.TextBlock{Text: strings.Repeat("assistant evidence ", 100)}),
	}
	input.Policy.MaxAgentEntryBytes = 128
	first, err := capturePermissionReviewContext(input)
	if err != nil {
		t.Fatalf("capturePermissionReviewContext() error = %v", err)
	}
	second, err := capturePermissionReviewContext(input)
	if err != nil {
		t.Fatalf("second capture error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("truncated snapshots differ for identical input")
	}
	found := false
	for _, entry := range first.Entries {
		if entry.Truncated && strings.Contains(entry.Content, "review context truncated") {
			found = true
		}
	}
	if !found || first.Truncation.Applied == 0 {
		t.Fatalf("truncation = %+v entries=%+v, want explicit deterministic marker", first.Truncation, first.Entries)
	}
}

func TestConfiguredReviewCaptureFailureStopsBeforeGateAndTool(t *testing.T) {
	t.Parallel()

	runTool := &fakeRunTool{name: "T", output: "must not run"}
	runTool.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git status", false), nil, nil
	}
	tools := resolveToolSetCaps(ToolSet{
		Access:   interactiveEvaluator(t, gate.AccessGated, &recordingRuleWriter{}, &recordingIssuer{}),
		Registry: []tool.InvokableTool{runTool},
	})
	client := &scriptedLLM{scripts: [][]content.Chunk{{
		toolUseChunk(0, "active", "T", `{}`),
	}}}
	gateReg := make(chan gateRegistration, 1)
	cfg, state, _ := newTurnFixture(
		[]content.Block{&content.TextBlock{Text: "current request"}},
		nil, tools, client, gateReg,
	)
	cfg.reviewContext = &reviewContextConfiguration{
		Metadata: reviewContextMetadata{
			WorkspaceRoot: "/workspace", WorkingDirectory: "/workspace",
			GatePolicyRevision: "gate-policy-v1",
			// Missing SecurityCeiling must fail closed.
		},
		Policy: testReviewContextPolicy(),
	}
	if _, ok := runTurn(context.Background(), cfg, state).(event.TurnFailed); !ok {
		t.Fatal("runTurn() did not fail when configured review metadata was missing")
	}
	if atomic.LoadInt32(&runTool.totalRuns) != 0 {
		t.Fatal("tool executed after review capture failure")
	}
	select {
	case <-gateReg:
		t.Fatal("permission gate opened after review capture failure")
	default:
	}
}

func reviewUserMessage(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: text}}}}
}

func reviewAIMessage(blocks ...content.Block) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}
}

func mustReviewUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

func testReviewContextPolicy() gate.ReviewContextPolicy {
	return gate.ReviewContextPolicy{
		Revision:             "review-context-v1",
		MaxBytes:             64 << 10,
		MaxEstimatedTokens:   16 << 10,
		MaxEntries:           64,
		MaxUserEntryBytes:    8 << 10,
		MaxAgentEntryBytes:   8 << 10,
		MaxToolEntryBytes:    8 << 10,
		MaxBlockBytes:        8 << 10,
		MaxActiveActionBytes: 8 << 10,
	}
}

func validReviewCapture(t *testing.T) reviewContextCapture {
	t.Helper()
	return reviewContextCapture{
		Coordinates: identity.Coordinates{
			SessionID: mustReviewUUID(t),
			LoopID:    mustReviewUUID(t),
			TurnID:    mustReviewUUID(t),
			StepID:    mustReviewUUID(t),
		},
		Staged: content.AgenticMessages{reviewUserMessage("current user")},
		Active: reviewAIMessage(&content.ToolUseBlock{
			ID: "active", Name: "Read", Input: json.RawMessage(`{"path":"README.md"}`),
		}),
		Metadata: reviewContextMetadata{
			WorkspaceRoot:      "/workspace",
			WorkingDirectory:   "/workspace",
			SecurityCeiling:    "workspace-write; unmet-requirements=true",
			GatePolicyRevision: "gate-policy-v1",
		},
		Policy: testReviewContextPolicy(),
	}
}

func gotMetadata(context gate.ReviewContext) reviewContextMetadata {
	return reviewContextMetadata{
		WorkspaceRoot: context.WorkspaceRoot, WorkingDirectory: context.WorkingDirectory,
		RetryReason: context.RetryReason, SecurityCeiling: context.SecurityCeiling,
		GatePolicyRevision: context.GatePolicyRevision,
	}
}

func assertReviewEntry(
	t *testing.T,
	entries []gate.ReviewContextEntry,
	origin gate.ReviewContextOrigin,
	kind gate.ReviewContextKind,
	contains string,
) {
	t.Helper()
	for _, entry := range entries {
		if entry.Origin == origin && entry.Kind == kind && containsText(entry.Content, contains) {
			return
		}
	}
	t.Errorf("missing entry origin=%q kind=%q containing %q in %+v", origin, kind, contains, entries)
}

func containsText(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}

func approveCommand(callID uuid.UUID) command.Command {
	return command.ApproveToolCall{
		GateRoute: command.GateRoute{ToolExecutionID: callID},
		Action:    gate.ApprovalApprove,
	}
}
