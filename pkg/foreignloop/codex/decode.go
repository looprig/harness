package codex

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/foreignloop"
)

const (
	eventThreadStarted = "thread.started"
	eventTurnStarted   = "turn.started"
	eventItemCompleted = "item.completed"
	eventTurnCompleted = "turn.completed"
	eventTurnFailed    = "turn.failed"
	eventError         = "error"

	itemAgentMessage = "agent_message"
)

type eventLine struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     json.RawMessage `json:"item"`
	Error    json.RawMessage `json:"error"`
}

type completedItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type errorDetail struct {
	Message string `json:"message"`
	Detail  string `json:"detail"`
	Details string `json:"details"`
	Error   string `json:"error"`
}

func decodeLine(line []byte) ([]foreignloop.ForeignEvent, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var el eventLine
	if err := json.Unmarshal(line, &el); err != nil {
		return nil, &foreignloop.DecodeError{Cause: err}
	}
	switch el.Type {
	case eventThreadStarted:
		if el.ThreadID == "" {
			return nil, nil
		}
		return []foreignloop.ForeignEvent{{Kind: foreignloop.ForeignInit, SessionID: el.ThreadID}}, nil
	case eventTurnStarted:
		return nil, nil
	case eventItemCompleted:
		return decodeCompletedItem(el.Item)
	case eventTurnCompleted:
		return []foreignloop.ForeignEvent{{Kind: foreignloop.ForeignTerminalOK}}, nil
	case eventTurnFailed, eventError:
		return []foreignloop.ForeignEvent{{
			Kind:    foreignloop.ForeignTerminalError,
			ErrText: decodeErrorText(el.Error, el.Type),
		}}, nil
	default:
		return nil, nil
	}
}

func decodeCompletedItem(raw json.RawMessage) ([]foreignloop.ForeignEvent, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var item completedItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, &foreignloop.DecodeError{Cause: err}
	}
	if item.Type != itemAgentMessage || item.Text == "" {
		return nil, nil
	}
	msg := &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: item.Text}},
	}}
	return []foreignloop.ForeignEvent{{Kind: foreignloop.ForeignStepComplete, Message: msg}}, nil
}

func decodeErrorText(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s
	}
	var detail errorDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		return fallback
	}
	primary := firstNonEmpty(detail.Message, detail.Error)
	secondary := firstNonEmpty(detail.Detail, detail.Details)
	switch {
	case primary != "" && secondary != "" && primary != secondary:
		return primary + ": " + secondary
	case primary != "":
		return primary
	case secondary != "":
		return secondary
	default:
		return strings.TrimSpace(string(raw))
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
