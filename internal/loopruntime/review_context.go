package loopruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

const reviewContextRevisionDomain = "harness.loop.permission-review-context/v1"

// reviewContextMetadata is private runtime wiring. A configured permission
// reviewer must supply every required field; omission fails before a tool is
// authorized. Keeping it private prevents ordinary tools from acquiring a
// review-context capability.
type reviewContextMetadata struct {
	WorkspaceRoot      string
	WorkingDirectory   string
	RetryReason        string
	SecurityCeiling    string
	GatePolicyRevision string
}

type reviewContextConfiguration struct {
	Metadata reviewContextMetadata
	Policy   gate.ReviewContextPolicy
}

// reviewContextCapture is the complete caller-owned state visible at the tool
// batch boundary. Base contains genuine committed conversation. Retained is
// generated/derived retained intent (for example a compaction summary), which
// is evidence but never direct user authority.
type reviewContextCapture struct {
	Coordinates identity.Coordinates
	Base        content.AgenticMessages
	Retained    content.AgenticMessages
	Staged      content.AgenticMessages
	Active      *content.AIMessage
	RuntimeTail *content.UserMessage
	Metadata    reviewContextMetadata
	Policy      gate.ReviewContextPolicy
}

type reviewContextCaptureError struct {
	Field string
	Cause error
}

func (e *reviewContextCaptureError) Error() string {
	return "loop: permission review context capture failed: " + e.Field
}

func (e *reviewContextCaptureError) Unwrap() error { return e.Cause }

type permissionReviewContextKey struct{}

func withPermissionReviewContext(ctx context.Context, review gate.ReviewContext) context.Context {
	return context.WithValue(ctx, permissionReviewContextKey{}, review.Clone())
}

func permissionReviewContextFromContext(ctx context.Context) (gate.ReviewContext, bool) {
	review, ok := ctx.Value(permissionReviewContextKey{}).(gate.ReviewContext)
	if !ok || review.ContextRevision == "" {
		return gate.ReviewContext{}, false
	}
	return review.Clone(), true
}

func capturePermissionReviewContext(input reviewContextCapture) (gate.ReviewContext, error) {
	if input.Metadata.WorkspaceRoot == "" ||
		input.Metadata.WorkingDirectory == "" ||
		input.Metadata.SecurityCeiling == "" ||
		input.Metadata.GatePolicyRevision == "" {
		return gate.ReviewContext{}, &reviewContextCaptureError{Field: "metadata"}
	}
	if err := preflightReviewContextCapture(input); err != nil {
		return gate.ReviewContext{}, err
	}
	entries := make([]gate.ReviewContextEntry, 0, reviewMessageCapacity(input))
	var err error
	entries, err = appendReviewMessages(entries, input.Base, reviewMessageAuthorityConversation)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	entries, err = appendReviewMessages(entries, input.Retained, reviewMessageAuthorityDerived)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	entries, err = appendReviewMessages(entries, input.Staged, reviewMessageAuthorityConversation)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	if input.RuntimeTail != nil {
		entries, err = appendReviewMessage(entries, input.RuntimeTail, reviewMessageAuthorityRuntime)
		if err != nil {
			return gate.ReviewContext{}, err
		}
	}
	if input.Active == nil {
		return gate.ReviewContext{}, &reviewContextCaptureError{Field: "active_message"}
	}
	entries, err = appendReviewMessage(entries, input.Active, reviewMessageAuthorityConversation)
	if err != nil {
		return gate.ReviewContext{}, err
	}

	context := gate.ReviewContext{
		Coordinates:        input.Coordinates,
		WorkspaceRoot:      input.Metadata.WorkspaceRoot,
		WorkingDirectory:   input.Metadata.WorkingDirectory,
		RetryReason:        input.Metadata.RetryReason,
		SecurityCeiling:    input.Metadata.SecurityCeiling,
		GatePolicyRevision: input.Metadata.GatePolicyRevision,
		Entries:            entries,
	}
	context.ContextRevision, err = permissionReviewContextRevision(context, input.Policy)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	bounded, err := gate.BuildReviewContext(context, input.Policy)
	if err != nil {
		return gate.ReviewContext{}, &reviewContextCaptureError{Field: "bounded_context", Cause: err}
	}
	return bounded, nil
}

func preflightReviewContextCapture(input reviewContextCapture) error {
	count, total := 0, 0
	accumulate := func(messages content.AgenticMessages) error {
		for _, message := range messages {
			blocks, err := reviewMessageBlocks(message)
			if err != nil {
				return err
			}
			for _, block := range blocks {
				size, err := reviewBlockInputBytes(block, 0)
				if err != nil {
					return err
				}
				count++
				if count > gate.MaxReviewContextInputEntries ||
					size > gate.MaxReviewContextEntryInputBytes ||
					size > gate.MaxReviewContextInputBytes-total {
					return &reviewContextCaptureError{Field: "input_bounds"}
				}
				total += size
			}
		}
		return nil
	}
	for _, messages := range []content.AgenticMessages{
		input.Base, input.Retained, input.Staged,
	} {
		if err := accumulate(messages); err != nil {
			return err
		}
	}
	if input.RuntimeTail != nil {
		if err := accumulate(content.AgenticMessages{input.RuntimeTail}); err != nil {
			return err
		}
	}
	if input.Active != nil {
		if err := accumulate(content.AgenticMessages{input.Active}); err != nil {
			return err
		}
	}
	return nil
}

func reviewMessageBlocks(message content.Conversation) ([]content.Block, error) {
	if message == nil {
		return nil, &reviewContextCaptureError{Field: "message"}
	}
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed != nil {
			return typed.Blocks, nil
		}
	case *content.AIMessage:
		if typed != nil {
			return typed.Blocks, nil
		}
	case *content.ToolResultMessage:
		if typed != nil {
			return typed.Blocks, nil
		}
	case *content.SystemMessage:
		if typed != nil {
			return typed.Blocks, nil
		}
	}
	return nil, &reviewContextCaptureError{Field: "message"}
}

func reviewBlockInputBytes(block content.Block, depth int) (int, error) {
	if depth > 64 {
		return 0, &reviewContextCaptureError{Field: "content_block"}
	}
	switch typed := block.(type) {
	case *content.TextBlock:
		if typed != nil {
			return len(typed.Text), nil
		}
	case *content.ImageBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.MediaType), len(typed.Source.URL), len(typed.Source.Data))
		}
	case *content.AudioBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.MediaType), len(typed.Data))
		}
	case *content.DocumentBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.MediaType), len(typed.Name), len(typed.Data), len(typed.Text))
		}
	case *content.ThinkingBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.Thinking), len(typed.Signature))
		}
	case *content.ToolUseBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.ID), len(typed.Name), len(typed.Input))
		}
	case *content.ToolResultBlock:
		if typed != nil {
			total, err := checkedReviewInputSize(len(typed.ToolUseID))
			if err != nil {
				return 0, err
			}
			for _, nested := range typed.Content {
				size, err := reviewBlockInputBytes(nested, depth+1)
				if err != nil {
					return 0, err
				}
				if size > gate.MaxReviewContextInputBytes-total {
					return 0, &reviewContextCaptureError{Field: "input_bounds"}
				}
				total += size
			}
			return total, nil
		}
	}
	return 0, &reviewContextCaptureError{Field: "content_block"}
}

func checkedReviewInputSize(sizes ...int) (int, error) {
	total := 0
	for _, size := range sizes {
		if size < 0 || size > gate.MaxReviewContextInputBytes-total {
			return 0, &reviewContextCaptureError{Field: "input_bounds"}
		}
		total += size
	}
	return total, nil
}

func reviewMessageCapacity(input reviewContextCapture) int {
	return len(input.Base) + len(input.Retained) + len(input.Staged) + 2
}

type reviewMessageAuthority uint8

const (
	reviewMessageAuthorityConversation reviewMessageAuthority = iota
	reviewMessageAuthorityDerived
	reviewMessageAuthorityRuntime
)

func appendReviewMessages(
	entries []gate.ReviewContextEntry,
	messages content.AgenticMessages,
	authority reviewMessageAuthority,
) ([]gate.ReviewContextEntry, error) {
	for _, message := range messages {
		var err error
		entries, err = appendReviewMessage(entries, message, authority)
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func appendReviewMessage(
	entries []gate.ReviewContextEntry,
	message content.Conversation,
	authority reviewMessageAuthority,
) ([]gate.ReviewContextEntry, error) {
	if message == nil {
		return nil, &reviewContextCaptureError{Field: "message"}
	}
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed == nil {
			return nil, &reviewContextCaptureError{Field: "message"}
		}
		origin, kind := gate.ReviewContextOriginUser, gate.ReviewContextKindUserMessage
		if authority != reviewMessageAuthorityConversation {
			origin, kind = gate.ReviewContextOriginRuntime, gate.ReviewContextKindRuntimeContext
		}
		return appendReviewBlocks(entries, typed.Blocks, origin, kind)
	case *content.AIMessage:
		if typed == nil {
			return nil, &reviewContextCaptureError{Field: "message"}
		}
		return appendAssistantReviewBlocks(entries, typed.Blocks)
	case *content.ToolResultMessage:
		if typed == nil {
			return nil, &reviewContextCaptureError{Field: "message"}
		}
		return appendReviewBlocks(entries, typed.Blocks, gate.ReviewContextOriginTool, gate.ReviewContextKindToolResult)
	case *content.SystemMessage:
		if typed == nil {
			return nil, &reviewContextCaptureError{Field: "message"}
		}
		return appendReviewBlocks(entries, typed.Blocks, gate.ReviewContextOriginRuntime, gate.ReviewContextKindRuntimeContext)
	default:
		return nil, &reviewContextCaptureError{Field: "message"}
	}
}

func appendAssistantReviewBlocks(
	entries []gate.ReviewContextEntry,
	blocks []content.Block,
) ([]gate.ReviewContextEntry, error) {
	for _, block := range blocks {
		kind := gate.ReviewContextKindAssistantMessage
		if _, ok := block.(*content.ToolUseBlock); ok {
			kind = gate.ReviewContextKindAssistantToolRequest
		}
		var err error
		entries, err = appendReviewBlock(entries, block, gate.ReviewContextOriginAssistant, kind)
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func appendReviewBlocks(
	entries []gate.ReviewContextEntry,
	blocks []content.Block,
	origin gate.ReviewContextOrigin,
	kind gate.ReviewContextKind,
) ([]gate.ReviewContextEntry, error) {
	for _, block := range blocks {
		entryOrigin, entryKind := origin, kind
		switch block.(type) {
		case *content.ImageBlock, *content.AudioBlock, *content.DocumentBlock:
			entryOrigin, entryKind = gate.ReviewContextOriginExternal, gate.ReviewContextKindExternalContent
		}
		var err error
		entries, err = appendReviewBlock(entries, block, entryOrigin, entryKind)
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func appendReviewBlock(
	entries []gate.ReviewContextEntry,
	block content.Block,
	origin gate.ReviewContextOrigin,
	kind gate.ReviewContextKind,
) ([]gate.ReviewContextEntry, error) {
	encoded, err := content.MarshalBlock(block)
	if err != nil {
		return nil, &reviewContextCaptureError{Field: "content_block", Cause: err}
	}
	return append(entries, gate.ReviewContextEntry{
		Origin: origin, Kind: kind, Content: string(encoded),
	}), nil
}

func permissionReviewContextRevision(
	context gate.ReviewContext,
	policy gate.ReviewContextPolicy,
) (string, error) {
	projection := struct {
		Domain               string                    `json:"domain"`
		Coordinates          identity.Coordinates      `json:"coordinates"`
		WorkspaceRoot        string                    `json:"workspace_root"`
		WorkingDirectory     string                    `json:"working_directory"`
		RetryReason          string                    `json:"retry_reason"`
		SecurityCeiling      string                    `json:"security_ceiling"`
		GatePolicyRevision   string                    `json:"gate_policy_revision"`
		ReviewPolicyRevision string                    `json:"review_policy_revision"`
		Entries              []gate.ReviewContextEntry `json:"entries"`
	}{
		Domain: reviewContextRevisionDomain, Coordinates: context.Coordinates,
		WorkspaceRoot: context.WorkspaceRoot, WorkingDirectory: context.WorkingDirectory,
		RetryReason: context.RetryReason, SecurityCeiling: context.SecurityCeiling,
		GatePolicyRevision: context.GatePolicyRevision, ReviewPolicyRevision: policy.Revision,
		Entries: context.Entries,
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", &reviewContextCaptureError{Field: "context_revision", Cause: err}
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
