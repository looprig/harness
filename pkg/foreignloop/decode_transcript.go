package foreignloop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"github.com/looprig/core/content"
)

// transcriptRecord is the typed envelope for one on-disk transcript line. Only
// the fields that gate allowlisting (Type, IsSidechain) and carry payload
// (Message) are decoded; every other transcript field is ignored by design.
type transcriptRecord struct {
	Type        string          `json:"type"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

// transcriptMessage is the inner message of a transcript record. Content is left
// raw because a user record's content may be a bare string (a plain prompt) or
// an array of blocks (tool_result), which we narrow per record type.
type transcriptMessage struct {
	Content json.RawMessage `json:"content"`
}

// decodeTranscriptTail reads a claude on-disk transcript and returns its
// committed thread, grouped per assistant round (each group is one step's
// messages). It is version-tolerant and soft-degrading: an allowlist accepts
// only `assistant` and `user` records, `isSidechain` records are skipped, and
// any per-line parse failure is logged and skipped — never fatal. Only a
// missing/unopenable file is returned as a *TranscriptUnavailableError.
//
// ASSUMPTION: a transcript message shares the stream-json content-block shapes
// (text/thinking/tool_use), and a user record's content is either a bare string
// or a tool_result array — pinned later by an integration test.
//
// sinceTurn is the requested lower bound for the tail. v1 reads and returns the
// full thread regardless (a simple, correct superset); honoring it as a true
// lower bound is a later refinement.
func decodeTranscriptTail(path string, sinceTurn int) ([]content.AgenticMessages, error) {
	_ = sinceTurn // documented above: full thread returned for v1.
	clean := filepath.Clean(path)
	f, err := os.Open(clean) // #nosec G304 — caller supplies a deterministic <sid>.jsonl path under a known root.
	if err != nil {
		return nil, &TranscriptUnavailableError{Path: clean, Cause: err}
	}
	defer func() { _ = f.Close() }()
	return foldTranscript(f), nil
}

// foldTranscript scans transcript lines and folds them into per-step groups.
func foldTranscript(f *os.File) []content.AgenticMessages {
	var out []content.AgenticMessages
	var cur content.AgenticMessages
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		msgs, boundary, err := decodeTranscriptLine(sc.Bytes())
		if err != nil {
			log.Printf("foreignloop: transcript line skipped: %v", err)
			continue
		}
		if boundary && len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
		cur = append(cur, msgs...)
	}
	if err := sc.Err(); err != nil {
		log.Printf("foreignloop: transcript scan stopped: %v", err)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// decodeTranscriptLine maps one transcript line to zero or more messages. The
// bool reports whether the line is an assistant record (a step boundary). A
// blank line, an unknown/unlisted type, or an isSidechain record yields nothing;
// only a malformed line returns a *DecodeError (which the caller logs & skips).
func decodeTranscriptLine(line []byte) ([]content.Conversation, bool, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, false, nil
	}
	var rec transcriptRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, false, &DecodeError{Cause: err}
	}
	if rec.IsSidechain {
		return nil, false, nil
	}
	switch rec.Type {
	case typeAssistant:
		msgs, err := decodeTranscriptAssistant(rec.Message)
		return msgs, true, err
	case typeUser:
		msgs, err := decodeTranscriptUser(rec.Message)
		return msgs, false, err
	default:
		return nil, false, nil
	}
}

func decodeTranscriptAssistant(raw json.RawMessage) ([]content.Conversation, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m transcriptMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &DecodeError{Cause: err}
	}
	var sbs []streamBlock
	if len(m.Content) > 0 {
		if err := json.Unmarshal(m.Content, &sbs); err != nil {
			return nil, &DecodeError{Cause: err}
		}
	}
	ai := &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: assistantBlocks(sbs)}}
	return []content.Conversation{ai}, nil
}

func decodeTranscriptUser(raw json.RawMessage) ([]content.Conversation, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m transcriptMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &DecodeError{Cause: err}
	}
	// A user record's content is either a bare prompt string or a block array.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		um := &content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: s}},
		}}
		return []content.Conversation{um}, nil
	}
	var sbs []streamBlock
	if err := json.Unmarshal(m.Content, &sbs); err != nil {
		return nil, &DecodeError{Cause: err}
	}
	return toolResultMessages(sbs), nil
}

// toolResultMessages builds one ToolResultMessage per tool_result block.
func toolResultMessages(sbs []streamBlock) []content.Conversation {
	var out []content.Conversation
	for _, b := range sbs {
		if b.Type != blockToolResult {
			continue
		}
		out = append(out, &content.ToolResultMessage{
			Message: content.Message{
				Role:   content.RoleTool,
				Blocks: []content.Block{&content.TextBlock{Text: renderToolResultText(b.Content)}},
			},
			ToolUseID: b.ToolUseID,
			IsError:   b.IsError,
		})
	}
	return out
}
