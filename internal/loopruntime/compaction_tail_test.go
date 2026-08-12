package loopruntime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

func tailUser(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: text}}}}
}

func tailAI(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: text}}}}
}

func tailToolAI(calls ...string) *content.AIMessage {
	blocks := make([]content.Block, 0, len(calls))
	for _, id := range calls {
		blocks = append(blocks, &content.ToolUseBlock{ID: id, Name: "tool", Input: json.RawMessage(`{}`)})
	}
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}
}

func tailToolResult(id string) *content.ToolResultMessage {
	return &content.ToolResultMessage{Message: content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: "ok"}}}, ToolUseID: id}
}

func tailTokenEstimate(t *testing.T, message content.Conversation) content.TokenCount {
	t.Helper()
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", message, err)
	}
	return content.TokenCount((len(raw) + 3) / 4)
}

func tailTokenSum(t *testing.T, messages content.AgenticMessages) content.TokenCount {
	t.Helper()
	var total content.TokenCount
	for _, message := range messages {
		total += tailTokenEstimate(t, message)
	}
	return total
}

func tailTexts(messages content.AgenticMessages) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		switch typed := message.(type) {
		case *content.UserMessage:
			if len(typed.Blocks) > 0 {
				if text, ok := typed.Blocks[0].(*content.TextBlock); ok && text != nil {
					texts = append(texts, text.Text)
				}
			}
		case *content.AIMessage:
			if len(typed.Blocks) > 0 {
				if text, ok := typed.Blocks[0].(*content.TextBlock); ok && text != nil {
					texts = append(texts, text.Text)
				}
			}
		case *content.ToolResultMessage:
			texts = append(texts, "result:"+typed.ToolUseID)
		}
	}
	return texts
}

func TestSelectCompactionTailShortTranscriptIsNoOp(t *testing.T) {
	transcript := content.AgenticMessages{tailUser("one"), tailAI("answer"), tailUser("two"), tailAI("answer")}
	selection, err := selectCompactionTail(transcript, 0, 2, 1000)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if len(selection.Head) != 0 || !reflect.DeepEqual(selection.Retained, transcript) {
		t.Fatalf("selection = head:%v retained:%v, want no-op retained transcript", tailTexts(selection.Head), tailTexts(selection.Retained))
	}
}

func TestSelectCompactionTailRetainsNewestUserAnchoredSegments(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("first"), tailAI("first-answer"),
		tailUser("second"), tailAI("second-answer"),
		tailUser("third"), tailAI("third-answer"),
	}
	selection, err := selectCompactionTail(transcript, 0, 2, 1000)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if got, want := tailTexts(selection.Head), []string{"first", "first-answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("head = %v, want %v", got, want)
	}
	if got, want := tailTexts(selection.Retained), []string{"second", "second-answer", "third", "third-answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained = %v, want %v", got, want)
	}
}

func TestSelectCompactionTailTreatsFoldedUsersAsSegmentAnchors(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("turn-start"), tailAI("tool request"),
		tailUser("folded-one"), tailAI("folded answer"),
		tailUser("folded-two"), tailAI("final answer"),
	}
	selection, err := selectCompactionTail(transcript, 0, 2, 1000)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if got, want := tailTexts(selection.Head), []string{"turn-start", "tool request"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("head = %v, want %v", got, want)
	}
	if got, want := tailTexts(selection.Retained), []string{"folded-one", "folded answer", "folded-two", "final answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained = %v, want %v", got, want)
	}
}

func TestSelectCompactionTailStopsAtTokenTarget(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("old"), tailAI("old answer"),
		tailUser("middle"), tailAI("middle answer"),
		tailUser("new"), tailAI("new answer"),
	}
	newest := transcript[4:]
	selection, err := selectCompactionTail(transcript, 0, 10, tailTokenSum(t, newest))
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if got, want := tailTexts(selection.Retained), []string{"new", "new answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained = %v, want %v", got, want)
	}
}

func TestSelectCompactionTailRetainsNewestOversizedSegment(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("old"), tailAI("old answer"),
		tailUser("new"), tailAI("this segment is deliberately larger than its target"),
	}
	newestTokens := tailTokenSum(t, transcript[2:])
	if newestTokens == 0 {
		t.Fatal("newest segment token estimate is zero")
	}
	selection, err := selectCompactionTail(transcript, 0, 1, newestTokens-1)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if got, want := tailTexts(selection.Head), []string{"old", "old answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("head = %v, want %v", got, want)
	}
	if got, want := tailTexts(selection.Retained), []string{"new", "this segment is deliberately larger than its target"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained = %v, want %v", got, want)
	}
	if got := tailTokenSum(t, selection.Retained); got <= newestTokens-1 {
		t.Fatalf("retained token estimate = %d, want it to exceed target %d", got, newestTokens-1)
	}
}

func TestSelectCompactionTailForcesPreviousSummaryIntoHead(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("previous summary"), tailUser("old user"), tailAI("old answer"),
		tailUser("new user"), tailAI("new answer"),
	}
	selection, err := selectCompactionTail(transcript, 1, 1, 1000)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if got, want := tailTexts(selection.Head), []string{"previous summary", "old user", "old answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("head = %v, want %v", got, want)
	}
	if got, want := tailTexts(selection.Retained), []string{"new user", "new answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained = %v, want %v", got, want)
	}
}

func TestSelectCompactionTailKeepsParallelToolPairsTogether(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("old"),
		tailToolAI("call-a", "call-b"),
		tailToolResult("call-b"), tailToolResult("call-a"),
		tailUser("new"), tailAI("new answer"),
	}
	selection, err := selectCompactionTail(transcript, 0, 1, 1000)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if got, want := tailTexts(selection.Head), []string{"old", "result:call-b", "result:call-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("head = %v, want %v", got, want)
	}
	if got, want := tailTexts(selection.Retained), []string{"new", "new answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained = %v, want %v", got, want)
	}
}

func TestSelectCompactionTailMovesCutToKeepCrossSegmentToolPairTogether(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("old"), tailToolAI("call-a"),
		tailUser("folded"), tailToolResult("call-a"),
	}
	const maxTokens = content.TokenCount(1)
	selection, err := selectCompactionTail(transcript, 0, 1, maxTokens)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if len(selection.Head) != 0 || len(selection.Retained) != len(transcript) {
		t.Fatalf("selection split tool pair: head:%v retained:%v", tailTexts(selection.Head), tailTexts(selection.Retained))
	}
	if got := tailTokenSum(t, selection.Retained); got <= maxTokens {
		t.Fatalf("retained token estimate = %d, want complete tool pair to exceed target %d", got, maxTokens)
	}
}

func TestSelectCompactionTailRejectsMalformedHistory(t *testing.T) {
	tests := []struct {
		name       string
		transcript content.AgenticMessages
		derived    int
	}{
		{name: "nil message", transcript: content.AgenticMessages{nil}},
		{name: "typed nil message", transcript: content.AgenticMessages{(*content.UserMessage)(nil)}},
		{name: "nil block", transcript: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{nil}}}}},
		{name: "typed nil block", transcript: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{(*content.TextBlock)(nil)}}}}},
		{name: "invalid message role", transcript: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleAssistant}}}},
		{name: "orphan result", transcript: content.AgenticMessages{tailUser("user"), tailToolResult("missing")}},
		{name: "orphan call", transcript: content.AgenticMessages{tailUser("user"), tailToolAI("missing")}},
		{name: "duplicate tool call ids", transcript: content.AgenticMessages{tailUser("user"), tailToolAI("duplicate", "duplicate"), tailToolResult("duplicate")}},
		{name: "duplicate tool result", transcript: content.AgenticMessages{tailUser("user"), tailToolAI("once"), tailToolResult("once"), tailToolResult("once")}},
		{
			name: "tool pair crosses derived prefix",
			transcript: content.AgenticMessages{
				tailUser("derived"), tailToolAI("cross"), tailUser("retained"), tailToolResult("cross"),
			},
			derived: 2,
		},
		{name: "derived prefix out of bounds", transcript: content.AgenticMessages{tailUser("user")}, derived: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if selection, err := selectCompactionTail(tt.transcript, tt.derived, 1, 100); err == nil || selection.Head != nil || selection.Retained != nil {
				t.Fatalf("selectCompactionTail() = %#v, %v, want fail closed", selection, err)
			}
		})
	}
}

func TestSelectCompactionTailRejectsInvalidTargets(t *testing.T) {
	transcript := content.AgenticMessages{tailUser("user"), tailAI("answer")}
	for _, target := range []struct {
		name     string
		segments int
		tokens   content.TokenCount
	}{
		{name: "zero segments", segments: 0, tokens: 10},
		{name: "negative segments", segments: -1, tokens: 10},
		{name: "zero tokens", segments: 1, tokens: 0},
	} {
		t.Run(target.name, func(t *testing.T) {
			if _, err := selectCompactionTail(transcript, 0, target.segments, target.tokens); err == nil {
				t.Fatal("selectCompactionTail() error = nil, want invalid target error")
			}
		})
	}
}

func TestSelectCompactionTailReturnsNonAliasingClones(t *testing.T) {
	transcript := content.AgenticMessages{tailUser("old"), tailAI("old answer"), tailUser("new"), tailAI("new answer")}
	selection, err := selectCompactionTail(transcript, 0, 1, 1000)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	selection.Head[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "changed head"
	selection.Retained[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "changed retained"
	if got := transcript[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text; got != "old" {
		t.Fatalf("source head message changed to %q", got)
	}
	if got := transcript[2].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text; got != "new" {
		t.Fatalf("source retained message changed to %q", got)
	}
}

func TestSelectCompactionTailClonesNestedRetainedBlocks(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("anchor"),
		tailToolAI("call"),
		tailUser("folded user"),
		&content.ToolResultMessage{Message: content.Message{
			Role: content.RoleTool,
			Blocks: []content.Block{&content.ToolResultBlock{
				ToolUseID: "call", Content: []content.Block{&content.TextBlock{Text: "result"}},
			}},
		}, ToolUseID: "call"},
	}
	selection, err := selectCompactionTail(transcript, 0, 1, 1000)
	if err != nil {
		t.Fatalf("selectCompactionTail() error = %v", err)
	}
	if len(selection.Head) != 0 || len(selection.Retained) != len(transcript) {
		t.Fatalf("selection = head:%d retained:%d, want all retained for complete pair", len(selection.Head), len(selection.Retained))
	}
	selection.Retained[1].(*content.AIMessage).Blocks[0].(*content.ToolUseBlock).Input = json.RawMessage(`{"query":"changed"}`)
	selection.Retained[3].(*content.ToolResultMessage).Blocks[0].(*content.ToolResultBlock).Content[0].(*content.TextBlock).Text = "changed result"
	if got := string(transcript[1].(*content.AIMessage).Blocks[0].(*content.ToolUseBlock).Input); got != `{}` {
		t.Fatalf("source tool input changed to %q", got)
	}
	if got := transcript[3].(*content.ToolResultMessage).Blocks[0].(*content.ToolResultBlock).Content[0].(*content.TextBlock).Text; got != "result" {
		t.Fatalf("source nested tool result changed to %q", got)
	}
}

func TestSelectCompactionExecutionCandidateProjectsHeadAndRetainsExactTail(t *testing.T) {
	transcript := content.AgenticMessages{tailUser("old head"), tailAI("old answer"), tailUser("new tail"), tailAI("new answer")}
	measurement := event.ContextMeasurement{Basis: event.ContextBasis{Revision: 7}, RequestFingerprint: [32]byte{1}}
	request := inference.Request{System: "stable", Messages: cloneMessages(transcript)}
	candidate := compactionExecutionCandidate{
		Measurement: measurement, Request: request, Transcript: cloneMessages(transcript), derivedPrefix: 0,
	}
	policy := &loop.CompactionPolicy{KeepRecentSegments: 1, KeepRecentTokens: 1000}
	selected, reason := selectCompactionExecutionCandidate(candidate, policy)
	if reason != event.CompactRejectUnspecified {
		t.Fatalf("selection rejection = %v, want unspecified", reason)
	}
	if got, want := tailTexts(selected.Transcript), []string{"old head", "old answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("projected transcript = %v, want head %v", got, want)
	}
	if got, want := tailTexts(selected.Retained), []string{"new tail", "new answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained transcript = %v, want tail %v", got, want)
	}
	if selected.Measurement != measurement || !reflect.DeepEqual(selected.Request, request) {
		t.Fatal("selection changed full-request measurement/CAS identity")
	}
	selected.Retained[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "mutated retained"
	if got := transcript[2].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text; got != "new tail" {
		t.Fatalf("retained clone aliases source with %q", got)
	}
}

func TestSelectCompactionExecutionCandidateForcesDerivedPrefixIntoProjectedHead(t *testing.T) {
	transcript := content.AgenticMessages{tailUser("previous summary"), tailUser("old head"), tailAI("old answer"), tailUser("new tail"), tailAI("new answer")}
	candidate := compactionExecutionCandidate{Transcript: cloneMessages(transcript), derivedPrefix: 1}
	selected, reason := selectCompactionExecutionCandidate(candidate, &loop.CompactionPolicy{KeepRecentSegments: 1, KeepRecentTokens: 1000})
	if reason != event.CompactRejectUnspecified {
		t.Fatalf("selection rejection = %v, want unspecified", reason)
	}
	if got, want := tailTexts(selected.Transcript), []string{"previous summary", "old head", "old answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("projected head = %v, want %v", got, want)
	}
}

func TestSelectCompactionExecutionCandidateKeepsPriorDerivedPrefixesBeforeRetainedCut(t *testing.T) {
	transcript := content.AgenticMessages{
		tailUser("prior summary"), tailUser("latest summary"),
		tailUser("old head"), tailAI("old answer"),
		tailUser("new retained"), tailAI("new answer"),
	}
	candidate := compactionExecutionCandidate{Transcript: cloneMessages(transcript), derivedPrefix: 2}
	selected, reason := selectCompactionExecutionCandidate(candidate, &loop.CompactionPolicy{KeepRecentSegments: 1, KeepRecentTokens: 1000})
	if reason != event.CompactRejectUnspecified {
		t.Fatalf("selection rejection = %v, want unspecified", reason)
	}
	if got, want := tailTexts(selected.Transcript), []string{"prior summary", "latest summary", "old head", "old answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("projected head = %v, want %v", got, want)
	}
	if got, want := tailTexts(selected.Retained), []string{"new retained", "new answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained tail = %v, want %v", got, want)
	}
	selected.Retained[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "mutated retained"
	if got := transcript[4].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text; got != "new retained" {
		t.Fatalf("retained selection aliases candidate transcript with %q", got)
	}
}

func TestSelectCompactionExecutionCandidateDoesNotRejectOversizedNewestTail(t *testing.T) {
	transcript := content.AgenticMessages{tailUser("old head"), tailAI("old answer"), tailUser("new tail"), tailAI("new answer")}
	candidate := compactionExecutionCandidate{Transcript: cloneMessages(transcript)}
	selected, reason := selectCompactionExecutionCandidate(candidate, &loop.CompactionPolicy{KeepRecentSegments: 1, KeepRecentTokens: 1})
	if reason != event.CompactRejectUnspecified {
		t.Fatalf("selection rejection = %v, want unspecified despite target exceed", reason)
	}
	if got, want := tailTexts(selected.Retained), []string{"new tail", "new answer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained oversized tail = %v, want %v", got, want)
	}
}

func TestSelectCompactionExecutionCandidateRejectsNoMeaningfulHead(t *testing.T) {
	transcript := content.AgenticMessages{tailUser("only segment"), tailAI("answer")}
	candidate := compactionExecutionCandidate{Transcript: cloneMessages(transcript)}
	selected, reason := selectCompactionExecutionCandidate(candidate, &loop.CompactionPolicy{KeepRecentSegments: 1, KeepRecentTokens: 1000})
	if reason != event.CompactRejectUnavailable {
		t.Fatalf("selection rejection = %v, want unavailable", reason)
	}
	if !reflect.DeepEqual(selected.Transcript, candidate.Transcript) || !reflect.DeepEqual(selected.Retained, transcript) {
		t.Fatalf("no-op candidate changed transcript or lost exact retained clone: %#v", selected)
	}
}
