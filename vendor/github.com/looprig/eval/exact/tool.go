package exact

import (
	"context"
	"strconv"

	"github.com/looprig/core/content"
	"github.com/looprig/eval"
)

// Config-error reason for the tool evaluators. A single reason covers an empty or
// otherwise invalid tool name, since both are the same author mistake.
const reasonInvalidToolName = "requires a valid, non-empty tool name"

// Finding codes for the tool evaluators.
const (
	codeRequiredToolAbsent   eval.FindingCode = "required_tool_absent"
	codeForbiddenToolPresent eval.FindingCode = "forbidden_tool_present"
)

// toolPresence asserts either the presence (forbid=false) or absence
// (forbid=true) of a tool call with a given name. It works over the typed
// *content.ToolUseBlock values in the conversation — including blocks nested
// inside a tool result — and never parses the tool arguments, so a malformed
// ToolUseBlock.Input can never cause a panic or a spurious verdict.
type toolPresence struct {
	desc      eval.Descriptor
	tool      string
	forbid    bool
	configErr string
}

// RequiredTool returns an evaluator asserting that a tool-use block naming tool
// appears in the conversation. An empty or invalid name is a configuration error:
// the evaluator yields Errored, never pass.
func RequiredTool(name string) eval.Evaluator {
	return newToolPresence(name, false,
		"exact/required_tool",
		"asserts a tool call with the given name was made")
}

// ForbiddenTool returns an evaluator asserting that no tool-use block naming tool
// appears in the conversation. An empty or invalid name is a configuration error:
// the evaluator yields Errored, never pass.
func ForbiddenTool(name string) eval.Evaluator {
	return newToolPresence(name, true,
		"exact/forbidden_tool",
		"asserts a tool call with the given name was not made")
}

// NoToolCall is an alias for ForbiddenTool, matching the framework design's
// spelling exact.NoToolCall("issue_refund").
func NoToolCall(name string) eval.Evaluator { return ForbiddenTool(name) }

func newToolPresence(name string, forbid bool, evalName eval.Name, desc string) eval.Evaluator {
	e := toolPresence{
		desc: eval.Descriptor{
			Name:        evalName,
			Revision:    evaluatorRevision,
			Method:      eval.MethodProgrammatic,
			Description: desc,
		},
		tool:   name,
		forbid: forbid,
	}
	// Validate the tool name at construction so it is always safe to render in a
	// finding and to use as an evidence ToolName. An invalid name is an author
	// error surfaced as Errored, never a pass.
	if err := eval.Name(name).Validate(); err != nil {
		e.configErr = reasonInvalidToolName
	}
	return e
}

func (e toolPresence) Descriptor() eval.Descriptor { return e.desc }

func (e toolPresence) Evaluate(_ context.Context, s eval.Sample) (eval.Assessment, error) {
	if e.configErr != "" {
		return configErrored(e.desc, e.configErr), nil
	}
	if a, ok := e.desc.CheckRequires(s); !ok {
		return a, nil
	}
	block, idx, found := findToolUse(s.Observation.Conversation, e.tool)
	if e.forbid {
		if !found {
			return eval.Pass(e.desc), nil
		}
		return e.failForbidden(block, idx), nil
	}
	if found {
		return eval.Pass(e.desc), nil
	}
	return e.failRequired(), nil
}

// failRequired builds the required-tool-absent failure. The tool name is a
// trusted constructor argument (validated at construction), so it is safe to name.
func (e toolPresence) failRequired() eval.Assessment {
	ev := diagnosticEvidence("exact/required_tool/absent", "required_tool_absent",
		"no tool call named "+e.tool)
	a := eval.Fail(e.desc, eval.Finding{
		Code:     codeRequiredToolAbsent,
		Severity: eval.SeverityHigh,
		Message:  "required tool call " + e.tool + " was not made",
		Evidence: []eval.EvidenceRef{{Evidence: ev.ID}},
	})
	a.Evidence = []eval.Evidence{ev}
	return a
}

// failForbidden builds the forbidden-tool-present failure, citing the offending
// call as a tool-operation evidence entry. The evidence records only safe
// metadata: the (trusted) tool name and the argument byte count. The raw
// arguments and the tool-use ID (which originate in the conversation) are never
// stored, and the offending call's message index is cited on the reference.
func (e toolPresence) failForbidden(block *content.ToolUseBlock, msgIndex int) eval.Assessment {
	idx := msgIndex
	ev := eval.Evidence{
		ID:   "exact/forbidden_tool/hit",
		Kind: eval.EvidenceToolOperation,
		ToolOperation: &eval.ToolOperationEvidence{
			ToolName:  eval.Name(e.tool),
			ArgsBytes: len(block.Input),
		},
	}
	a := eval.Fail(e.desc, eval.Finding{
		Code:     codeForbiddenToolPresent,
		Severity: eval.SeverityHigh,
		Message:  "forbidden tool call " + e.tool + " was made at message index " + strconv.Itoa(msgIndex),
		Evidence: []eval.EvidenceRef{{Evidence: ev.ID, MessageIndex: &idx}},
	})
	a.Evidence = []eval.Evidence{ev}
	return a
}

// findToolUse returns the first *content.ToolUseBlock named name, the index of
// the message it was found in, and whether one exists. It scans every message's
// blocks and recurses into the nested content of tool-result blocks. Only the
// block name is inspected; ToolUseBlock.Input (which may be malformed JSON) is
// never parsed.
func findToolUse(conv content.AgenticMessages, name string) (*content.ToolUseBlock, int, bool) {
	for idx, msg := range conv {
		blocks := messageBlocks(msg)
		if tu, ok := findToolUseInBlocks(blocks, name); ok {
			return tu, idx, true
		}
	}
	return nil, 0, false
}

// messageBlocks returns the top-level blocks of any conversation message.
func messageBlocks(msg content.Conversation) []content.Block {
	switch m := msg.(type) {
	case *content.UserMessage:
		return m.Blocks
	case *content.AIMessage:
		return m.Blocks
	case *content.SystemMessage:
		return m.Blocks
	case *content.ToolResultMessage:
		return m.Blocks
	default:
		return nil
	}
}

// findToolUseInBlocks searches blocks (recursing into nested tool-result content)
// for the first tool-use block named name.
func findToolUseInBlocks(blocks []content.Block, name string) (*content.ToolUseBlock, bool) {
	for _, blk := range blocks {
		switch b := blk.(type) {
		case *content.ToolUseBlock:
			if b.Name == name {
				return b, true
			}
		case *content.ToolResultBlock:
			if tu, ok := findToolUseInBlocks(b.Content, name); ok {
				return tu, true
			}
		}
	}
	return nil, false
}
