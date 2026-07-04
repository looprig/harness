package foreignloop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/looprig/core/content"
)

// maxLineBytes is the scanner's per-line ceiling. claude stream-json lines can be
// long (a full assistant message with tool inputs), so we raise the default 64KiB.
const maxLineBytes = 1 << 20

// maxResultPreviewRunes caps a tool_result preview so a large tool output cannot
// bloat a normalized event.
const maxResultPreviewRunes = 512

// Wire-tag constants for the claude stream-json contract. No magic strings.
const (
	typeSystem      = "system"
	typeStreamEvent = "stream_event"
	typeAssistant   = "assistant"
	typeUser        = "user"
	typeResult      = "result"

	subtypeInit        = "init"
	subtypeSuccess     = "success"
	subtypeErrorPrefix = "error"

	blockText       = "text"
	blockThinking   = "thinking"
	blockToolUse    = "tool_use"
	blockToolResult = "tool_result"

	eventContentBlockDelta = "content_block_delta"
	deltaText              = "text_delta"
	deltaThinking          = "thinking_delta"
)

// streamLine is the typed envelope every JSONL line decodes into first. Nothing
// past this boundary is a map[string]any; we narrow per Type.
type streamLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"`
	Event     json.RawMessage `json:"event"`
	Result    string          `json:"result"`
}

// streamMessage is an assistant/user message's typed content envelope.
type streamMessage struct {
	Role    string        `json:"role"`
	Content []streamBlock `json:"content"`
}

// streamBlock covers every content-block shape we understand (text, thinking,
// tool_use, tool_result). Only the fields valid for its Type are populated.
type streamBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result payload: string or array
	IsError   bool            `json:"is_error"`
}

// streamEvent is the inner Anthropic SSE event carried by a stream_event line.
type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
}

// decodeStream reads claude stream-json JSONL from r, emits normalized
// ForeignEvents on the returned channel, and closes the channel at EOF. The
// returned func returns the first *DecodeError encountered (or nil) and MUST be
// called only after the channel is fully drained — the closing of the channel
// establishes the happens-before edge that makes the error read race-free.
func decodeStream(r io.Reader) (<-chan ForeignEvent, func() error) {
	ch := make(chan ForeignEvent)
	var firstErr error
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
		for sc.Scan() {
			evs, err := decodeStreamLine(sc.Bytes())
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, ev := range evs {
				ch <- ev
			}
		}
		if err := sc.Err(); err != nil && firstErr == nil {
			firstErr = &DecodeError{Cause: err}
		}
	}()
	return ch, func() error { return firstErr }
}

// decodeStreamLine maps one JSONL line to zero or more ForeignEvents. A blank
// line yields nothing; an unknown type is skipped; only a malformed line returns
// a *DecodeError. One line may yield several events (tool_use blocks + a step).
func decodeStreamLine(line []byte) ([]ForeignEvent, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var sl streamLine
	if err := json.Unmarshal(line, &sl); err != nil {
		return nil, &DecodeError{Cause: err}
	}
	switch sl.Type {
	case typeSystem:
		return decodeSystem(sl), nil
	case typeStreamEvent:
		return decodeStreamEvent(sl)
	case typeAssistant:
		return decodeAssistant(sl)
	case typeUser:
		return decodeUser(sl)
	case typeResult:
		return decodeResult(sl), nil
	default:
		return nil, nil
	}
}

func decodeSystem(sl streamLine) []ForeignEvent {
	if sl.Subtype != subtypeInit {
		return nil
	}
	return []ForeignEvent{{Kind: ForeignInit, SessionID: sl.SessionID}}
}

func decodeStreamEvent(sl streamLine) ([]ForeignEvent, error) {
	if len(sl.Event) == 0 {
		return nil, nil
	}
	var ev streamEvent
	if err := json.Unmarshal(sl.Event, &ev); err != nil {
		return nil, &DecodeError{Cause: err}
	}
	if ev.Type != eventContentBlockDelta {
		return nil, nil
	}
	switch ev.Delta.Type {
	case deltaText:
		return []ForeignEvent{{Kind: ForeignTextDelta, Text: ev.Delta.Text}}, nil
	case deltaThinking:
		return []ForeignEvent{{Kind: ForeignThinkingDelta, Text: ev.Delta.Thinking}}, nil
	default:
		return nil, nil
	}
}

func decodeAssistant(sl streamLine) ([]ForeignEvent, error) {
	msg, err := decodeMessage(sl.Message)
	if err != nil {
		return nil, err
	}
	out := toolUseEvents(msg.Content)
	ai := &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: assistantBlocks(msg.Content),
	}}
	return append(out, ForeignEvent{Kind: ForeignStepComplete, Message: ai}), nil
}

// assistantBlocks builds the in-memory content blocks of an assistant message
// (text / thinking / tool_use). Shared by the stream and transcript decoders.
func assistantBlocks(sbs []streamBlock) []content.Block {
	var blocks []content.Block
	for _, b := range sbs {
		switch b.Type {
		case blockText:
			blocks = append(blocks, &content.TextBlock{Text: b.Text})
		case blockThinking:
			blocks = append(blocks, &content.ThinkingBlock{Thinking: b.Thinking, Signature: b.Signature})
		case blockToolUse:
			blocks = append(blocks, &content.ToolUseBlock{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return blocks
}

// toolUseEvents emits one ForeignToolUse per tool_use block, in order.
func toolUseEvents(sbs []streamBlock) []ForeignEvent {
	var out []ForeignEvent
	for _, b := range sbs {
		if b.Type == blockToolUse {
			out = append(out, ForeignEvent{Kind: ForeignToolUse, ToolUseID: b.ID, ToolName: b.Name})
		}
	}
	return out
}

func decodeUser(sl streamLine) ([]ForeignEvent, error) {
	msg, err := decodeMessage(sl.Message)
	if err != nil {
		return nil, err
	}
	var out []ForeignEvent
	for _, b := range msg.Content {
		if b.Type != blockToolResult {
			continue
		}
		out = append(out, ForeignEvent{
			Kind:          ForeignToolResult,
			ToolUseID:     b.ToolUseID,
			IsError:       b.IsError,
			ResultPreview: renderToolResultPreview(b.Content),
		})
	}
	return out, nil
}

// decodeResult maps a terminal result line. ASSUMPTION: claude reports terminal
// outcomes via subtype "success" or an "error"-prefixed subtype (e.g.
// "error_max_turns"); the human-readable text rides in the top-level `result`
// field. Confirmed against the design doc; pinned later by an integration test.
func decodeResult(sl streamLine) []ForeignEvent {
	switch {
	case sl.Subtype == subtypeSuccess:
		return []ForeignEvent{{Kind: ForeignTerminalOK, Message: resultMessage(sl.Result)}}
	case strings.HasPrefix(sl.Subtype, subtypeErrorPrefix):
		txt := sl.Result
		if txt == "" {
			txt = sl.Subtype
		}
		return []ForeignEvent{{Kind: ForeignTerminalError, ErrText: txt}}
	default:
		return nil
	}
}

func decodeMessage(raw json.RawMessage) (streamMessage, error) {
	var m streamMessage
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, &DecodeError{Cause: err}
	}
	return m, nil
}

// resultMessage wraps a terminal result string as an authoritative AIMessage.
// nil when empty — the on-disk transcript is the authoritative record later.
func resultMessage(text string) *content.AIMessage {
	if text == "" {
		return nil
	}
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// renderToolResultPreview is the capped rendering used in live stream events.
func renderToolResultPreview(raw json.RawMessage) string {
	return capPreview(renderToolResultText(raw))
}

// renderToolResultText renders a tool_result `content` (a JSON string or an array
// of text parts) to a plain string, uncapped. ASSUMPTION: claude tool_result
// content is either a bare string or an array of {type,text} parts.
func renderToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

func capPreview(s string) string {
	r := []rune(s)
	if len(r) <= maxResultPreviewRunes {
		return s
	}
	return string(r[:maxResultPreviewRunes])
}
