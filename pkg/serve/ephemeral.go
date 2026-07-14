package serve

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// frameSchemaVersion is the wire-envelope schema version stamped into every SSE frame
// body under the "v" key (both classes). It starts at 1; bump it (never reuse a
// number) when the frame body shape changes incompatibly so a client can branch on
// the version of an old stream. It is deliberately independent of the durable event
// envelope's own "v" (event.MarshalEvent stamps that nested version).
const frameSchemaVersion = 1

// Ephemeral frame kinds — the discriminator a client switches on to decode Delta.
const (
	ephemeralKindTokenDelta        = "token_delta"
	ephemeralKindToolCallStarted   = "tool_call_started"
	ephemeralKindToolCallCompleted = "tool_call_completed"
	ephemeralKindInputQueued       = "input_queued"
	ephemeralKindCompactionStarted = "compaction_started"
)

// content.Chunk has NO wire codec (it is a sealed transport-only interface); these
// tagged live DTOs are the ONLY shape a chunk is ever serialized as. chunk_type is
// the discriminator; a client reconstructs the concrete chunk from it.
const (
	chunkTypeText     = "text"
	chunkTypeThinking = "thinking"
	chunkTypeToolUse  = "tool_use"
)

// enduringFrame is the wire body of an `event: enduring` SSE frame: a schema version
// and the durable event envelope (event.MarshalEvent's raw output) nested under
// "event". Wrapping (rather than emitting the bare envelope) lets the enduring body
// version independently of the durable codec and matches the ephemeral body's shape.
type enduringFrame struct {
	V     int             `json:"v"`     // 1
	Event json.RawMessage `json:"event"` // event.MarshalEvent(d.Event) raw JSON
}

// ephemeralFrame is the wire body of an `event: ephemeral` SSE frame. Ephemeral events
// are never sequenced or persisted; this is a SEPARATE transport encoder that never
// touches event.MarshalEvent (which fails closed on Ephemeral, an invariant preserved).
// Kind selects how Delta decodes; Header is the producing event's identity.
type ephemeralFrame struct {
	V      int             `json:"v"`               // 1
	Kind   string          `json:"kind"`            // token_delta|tool_call_started|tool_call_completed|input_queued|compaction_started
	Header event.Header    `json:"header,omitzero"` // producer identity (omitted when zero)
	Delta  json.RawMessage `json:"delta,omitempty"` // kind-specific payload (absent for input_queued)
}

// textChunkDTO is the tagged live wire form of a content.TextChunk.
type textChunkDTO struct {
	ChunkType string `json:"chunk_type"` // "text"
	Text      string `json:"text"`
}

// thinkingChunkDTO is the tagged live wire form of a content.ThinkingChunk.
type thinkingChunkDTO struct {
	ChunkType string `json:"chunk_type"` // "thinking"
	Thinking  string `json:"thinking"`
}

// toolUseChunkDTO is the tagged live wire form of a content.ToolUseChunk.
type toolUseChunkDTO struct {
	ChunkType string `json:"chunk_type"` // "tool_use"
	Index     int    `json:"index"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	InputJSON string `json:"input_json"`
}

// toolCallStartedDelta is the Delta payload for a ToolCallStarted ephemeral frame:
// only the event's public fields, never internal types.
type toolCallStartedDelta struct {
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	ToolName        string    `json:"tool_name,omitempty"`
	Summary         string    `json:"summary,omitempty"`
}

// toolCallCompletedDelta is the Delta payload for a ToolCallCompleted ephemeral frame.
type toolCallCompletedDelta struct {
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	IsError         bool      `json:"is_error,omitzero"`
	ResultPreview   string    `json:"result_preview,omitempty"`
}

// compactionStartedDelta is the public progress payload for a running compaction.
type compactionStartedDelta struct {
	AttemptID event.CompactAttemptID `json:"attempt_id"`
	Reason    event.CompactionReason `json:"reason"`
	Basis     event.ContextBasis     `json:"basis"`
}

// encodeDelivery renders one fan-in delivery as a complete SSE frame (through the
// trailing blank line). ok==false means SKIP the delivery entirely — an Enduring
// event the marshaler rejects (a foreign type outside the sealed union) or an
// unrecognized Ephemeral event (an unknown future kind, or a TokenDelta whose chunk
// variant this transport does not know). The caller writes nothing on a skip.
func encodeDelivery(d event.Delivery) ([]byte, bool) {
	switch d.Event.Class() {
	case event.Enduring:
		return encodeEnduringFrame(d)
	case event.Ephemeral:
		return encodeEphemeralFrame(d.Event)
	default:
		slog.Debug("serve: events skip event of unknown class", "type", fmt.Sprintf("%T", d.Event))
		return nil, false
	}
}

// encodeEnduringFrame builds the `event: enduring` frame:
//
//	event: enduring\nid: <JournalSeq>\ndata: {"v":1,"event":<envelope>}\n\n
//
// The id: line always stamps d.JournalSeq (0 for a would-be zero-seq append — emitted
// as "id: 0" for a stable, always-present field rather than a conditional line). An
// Enduring event MarshalEvent rejects is skipped (as in Phase 1); Ephemeral events
// never reach here (class-gated by encodeDelivery), so the invariant holds.
func encodeEnduringFrame(d event.Delivery) ([]byte, bool) {
	raw, err := event.MarshalEvent(d.Event)
	if err != nil {
		slog.Debug("serve: events skip unmarshalable enduring event", "err", err)
		return nil, false
	}
	body, err := json.Marshal(enduringFrame{V: frameSchemaVersion, Event: raw})
	if err != nil {
		slog.Debug("serve: events encode enduring frame", "err", err)
		return nil, false
	}
	return fmt.Appendf(nil, "event: enduring\nid: %d\ndata: %s\n\n", d.JournalSeq, body), true
}

// encodeEphemeralFrame builds the `event: ephemeral` frame (NO id: line — Ephemeral is
// never sequenced): event: ephemeral\ndata: {ephemeralFrame}\n\n. An unrecognized
// concrete ephemeral event (or an unknown chunk variant) is SKIPPED with a debug log,
// never emitted as lossy ad-hoc JSON.
func encodeEphemeralFrame(ev event.Event) ([]byte, bool) {
	frame, ok := buildEphemeralFrame(ev)
	if !ok {
		slog.Debug("serve: events skip unrecognized ephemeral event", "type", fmt.Sprintf("%T", ev))
		return nil, false
	}
	body, err := json.Marshal(frame)
	if err != nil {
		slog.Debug("serve: events encode ephemeral frame", "err", err)
		return nil, false
	}
	return fmt.Appendf(nil, "event: ephemeral\ndata: %s\n\n", body), true
}

// buildEphemeralFrame maps a concrete Ephemeral event to its wire frame via a type
// switch over the sealed Ephemeral set (TokenDelta, ToolCallStarted, ToolCallCompleted,
// InputQueued, CompactionStarted). ok==false for any unrecognized event or a TokenDelta this transport
// cannot represent, so the caller skips rather than emit a partial frame.
func buildEphemeralFrame(ev event.Event) (ephemeralFrame, bool) {
	switch e := ev.(type) {
	case event.TokenDelta:
		delta, ok := encodeChunkDelta(e.Chunk)
		if !ok {
			return ephemeralFrame{}, false
		}
		return ephemeralFrame{V: frameSchemaVersion, Kind: ephemeralKindTokenDelta, Header: e.EventHeader(), Delta: delta}, true
	case event.ToolCallStarted:
		delta, ok := marshalDelta(toolCallStartedDelta{ToolExecutionID: e.ToolExecutionID, ToolName: e.ToolName, Summary: e.Summary})
		if !ok {
			return ephemeralFrame{}, false
		}
		return ephemeralFrame{V: frameSchemaVersion, Kind: ephemeralKindToolCallStarted, Header: e.EventHeader(), Delta: delta}, true
	case event.ToolCallCompleted:
		delta, ok := marshalDelta(toolCallCompletedDelta{ToolExecutionID: e.ToolExecutionID, IsError: e.IsError, ResultPreview: e.ResultPreview})
		if !ok {
			return ephemeralFrame{}, false
		}
		return ephemeralFrame{V: frameSchemaVersion, Kind: ephemeralKindToolCallCompleted, Header: e.EventHeader(), Delta: delta}, true
	case event.InputQueued:
		// InputQueued carries no public payload beyond its Header, so it has no Delta.
		return ephemeralFrame{V: frameSchemaVersion, Kind: ephemeralKindInputQueued, Header: e.EventHeader()}, true
	case event.CompactionStarted:
		delta, ok := marshalDelta(compactionStartedDelta{AttemptID: e.AttemptID, Reason: e.Reason, Basis: e.Basis})
		if !ok {
			return ephemeralFrame{}, false
		}
		return ephemeralFrame{V: frameSchemaVersion, Kind: ephemeralKindCompactionStarted, Header: e.EventHeader(), Delta: delta}, true
	default:
		return ephemeralFrame{}, false
	}
}

// encodeChunkDelta maps a content.Chunk (the sealed, codec-less transport interface)
// to its tagged live DTO bytes. A nil chunk, a typed-nil pointer variant (e.g.
// (*content.TextChunk)(nil)), or an unknown variant yields ok==false so the enclosing
// TokenDelta frame is skipped — content.Chunk itself is NEVER serialized, and a
// typed-nil deref never panics deep in the request path (fail closed).
func encodeChunkDelta(chunk content.Chunk) (json.RawMessage, bool) {
	switch c := chunk.(type) {
	case *content.TextChunk:
		if c == nil {
			return nil, false
		}
		return marshalDelta(textChunkDTO{ChunkType: chunkTypeText, Text: c.Text})
	case *content.ThinkingChunk:
		if c == nil {
			return nil, false
		}
		return marshalDelta(thinkingChunkDTO{ChunkType: chunkTypeThinking, Thinking: c.Thinking})
	case *content.ToolUseChunk:
		if c == nil {
			return nil, false
		}
		return marshalDelta(toolUseChunkDTO{ChunkType: chunkTypeToolUse, Index: c.Index, ID: c.ID, Name: c.Name, InputJSON: c.InputJSON})
	default:
		return nil, false
	}
}

// marshalDelta encodes a typed delta DTO to raw JSON at the single serialization
// boundary. The DTOs are flat structs of scalars, so a marshal failure is not
// expected; ok==false on the impossible error keeps every caller fail-closed (skip)
// rather than emit a malformed frame.
func marshalDelta(v any) (json.RawMessage, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return b, true
}
