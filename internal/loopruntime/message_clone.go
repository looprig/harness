package loopruntime

import (
	"github.com/looprig/core/content"
	"github.com/looprig/inference"
)

// cloneInferenceRequest gives an external request collaborator independent
// ownership of every reference-backed field while preserving scalar request
// intent. The original request remains safe for the current provider call.
func cloneInferenceRequest(request inference.Request) inference.Request {
	cloned := request
	cloned.Model = request.Model.Clone()
	if request.Messages == nil {
		cloned.Messages = nil
	} else {
		cloned.Messages = cloneMessages(request.Messages)
	}
	cloned.Tools = cloneInferenceTools(request.Tools)
	cloned.Output = cloneOutputSchema(request.Output)
	if request.Override != nil {
		override := request.Override.Clone()
		cloned.Override = &override
	}
	return cloned
}

// cloneMessages gives a conversation graph one owner. It recursively copies
// messages, blocks, usage, raw JSON, and binary payloads so mutation cannot cross
// runtime boundaries through content's pointer-backed sealed unions.
func cloneMessages(messages content.AgenticMessages) content.AgenticMessages {
	cloned := make(content.AgenticMessages, len(messages))
	for i, message := range messages {
		cloned[i] = cloneConversation(message)
	}
	return cloned
}

func cloneConversation(message content.Conversation) content.Conversation {
	switch typed := message.(type) {
	case *content.UserMessage:
		return cloneUserMessage(typed)
	case *content.AIMessage:
		return cloneAIMessage(typed)
	case *content.SystemMessage:
		return cloneSystemMessage(typed)
	case *content.ToolResultMessage:
		return cloneToolResultMessage(typed)
	default:
		return nil
	}
}

func cloneUserMessage(message *content.UserMessage) *content.UserMessage {
	if message == nil {
		return nil
	}
	return &content.UserMessage{Message: cloneMessage(message.Message)}
}

func cloneAIMessage(message *content.AIMessage) *content.AIMessage {
	if message == nil {
		return nil
	}
	cloned := &content.AIMessage{Message: cloneMessage(message.Message)}
	if message.Usage != nil {
		usage := *message.Usage
		cloned.Usage = &usage
	}
	return cloned
}

func cloneSystemMessage(message *content.SystemMessage) *content.SystemMessage {
	if message == nil {
		return nil
	}
	return &content.SystemMessage{Message: cloneMessage(message.Message)}
}

func cloneToolResultMessage(message *content.ToolResultMessage) *content.ToolResultMessage {
	if message == nil {
		return nil
	}
	return &content.ToolResultMessage{
		Message:   cloneMessage(message.Message),
		ToolUseID: message.ToolUseID,
		IsError:   message.IsError,
	}
}

// cloneMessage copies the blocks through content.CloneBlocks rather than
// through a type switch of its own. The switch used to live here, one module
// away from the sealed union it enumerated, so core could add a variant and
// this file would keep compiling while dropping it — which is how ProviderState
// went missing from this clone and two others at once. Core now owns the copy,
// so a new variant and its copy arm land together.
//
// The behaviour this delegation changes, deliberately: the old arms rebuilt
// thinking and tool-use blocks through core's CONSTRUCTORS, which normalize. An
// empty non-nil Input came back nil and a ProviderStateFormat whose
// ProviderState was empty came back cleared. content.CloneBlock is faithful
// instead — see its doc comment — so this clone now hands the request build,
// the durable commit, and the restore seed exactly the value the runtime holds.
// It can only preserve more than it did.
func cloneMessage(message content.Message) content.Message {
	return content.Message{Role: message.Role, Blocks: content.CloneBlocks(message.Blocks)}
}
