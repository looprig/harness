# `content.Block` / `content.Chunk` Sealed-Interface Migration — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Also: @superpowers:test-driven-development (Phase A is test-first), @superpowers:verification-before-completion (Phase C gates the single commit).

**Goal:** Convert `internal/content`'s `Block` and `Chunk` from tagged-union structs to sealed interfaces (concrete payload type = discriminator), add a JSON codec for the now-uninstantiable interface, and migrate every consumer.

**Architecture:** New design logic lives entirely in `internal/content` (interfaces + markers + `block_json.go` codec + per-message JSON). Everything downstream (`openaiapi`, `loop`, `session`, `personal-assistant`) is a **mechanical, compiler-driven** rewrite: `b.Type`/`switch b.Type` → type switch; `&content.Block{Type:…, X:…}` → `&content.X{…}`; `[]*content.Block` → `[]content.Block`. See the design/spec: `docs/plans/2026-06-14-content-sealed-interfaces.md`.

**Tech Stack:** Go, stdlib only (`encoding/json`, `testing` incl. `-fuzz`). No new dependencies. CLAUDE.md governs: typed errors, table-driven `-race` tests, fuzz target for the parser, `make secure` before commit.

---

## Pre-flight: this is ONE atomic commit

The design doc decided against compatibility shims. Changing the `Block`/`Chunk` type shape breaks all ~25 referencing files at once; the tree does **not** `go build` between tasks. Therefore:

- **Work top-down through the phases without committing.** Phase A (content) is self-consistent and its package tests can pass in isolation, but `go build ./...` stays red until Phase B finishes.
- **One functional commit at the end of Phase C**, after the full `-race` suite is green and `make secure` passes.
- TDD still applies *within* Phase A (write the codec tests first). For Phase B the compiler is the test harness — the work is done when `go build ./...` and the updated table-driven tests are green.
- The design doc itself is already modified in the working tree; commit it together with the code (it is the spec for this change).

**Branch:** currently `feat/cli-tui`. Consider an isolated worktree (@superpowers:using-git-worktrees) before starting — see Execution Handoff.

---

## Phase A — `internal/content` (the new design + codec)

### Task A1: Block + Chunk interfaces and markers

**Files:**
- Modify: `internal/content/block.go`
- Modify: `internal/content/chunk.go`

**Step 1 — Rewrite `block.go`.** Remove the `Block` struct and its `Type` field; add the sealed interface + markers. Keep `BlockType` and its constants (the codec's wire discriminator). Change `ToolResultBlock.Content` to `[]Block`. Concrete payload structs are otherwise unchanged.

```go
// Package content defines the unified content vocabulary shared across all
// internal packages. Block is a sealed interface; the concrete payload type is
// the discriminator. Only this package can add variants (unexported marker).
package content

import "encoding/json"

type BlockType string

const (
	TypeText       BlockType = "text"
	TypeImage      BlockType = "image"
	TypeAudio      BlockType = "audio"
	TypeDocument   BlockType = "document"
	TypeThinking   BlockType = "thinking"
	TypeToolUse    BlockType = "tool_use"
	TypeToolResult BlockType = "tool_result"
)

// Block is the sealed interface over all content block payloads. The concrete
// type is the discriminator; there is no Type field and no nil-able payload
// pointers. BlockType is retained only as the wire tag for the JSON codec
// (block_json.go), not as a field on any in-memory value.
type Block interface{ isBlock() }

func (*TextBlock) isBlock()       {}
func (*ImageBlock) isBlock()      {}
func (*AudioBlock) isBlock()      {}
func (*DocumentBlock) isBlock()   {}
func (*ThinkingBlock) isBlock()   {}
func (*ToolUseBlock) isBlock()    {}
func (*ToolResultBlock) isBlock() {}

type TextBlock struct {
	Text string
}

// ImageSource is a sum type for the origin of image data.
// Set exactly one of URL (remote) or Data (inline bytes).
type ImageSource struct {
	URL  string
	Data []byte
}

type ImageBlock struct {
	MediaType MediaType
	Source    ImageSource
}

type AudioBlock struct {
	MediaType MediaType
	Data      []byte
}

// DocumentBlock carries document data. Either Data (binary) or Text (extracted
// text) may be populated depending on how the document was provided.
type DocumentBlock struct {
	MediaType MediaType
	Name      string
	Data      []byte
	Text      string
}

// ThinkingBlock carries model reasoning text.
// Signature is empty during streaming and non-empty only on a complete block.
type ThinkingBlock struct {
	Thinking  string
	Signature string
}

type ToolUseBlock struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResultBlock nests its own []Block, so it implements json.Marshaler /
// json.Unmarshaler in block_json.go (the only payload type that needs custom
// JSON). Do not add a Type field.
type ToolResultBlock struct {
	ToolUseID string
	Content   []Block
	IsError   bool
}
```

**Step 2 — Rewrite `chunk.go`.** Sealed interface + markers. **Drop `ChunkType` and its constants** — chunks are ephemeral streaming deltas, never serialized, and have no consumer once `Chunk` is an interface.

```go
package content

// Chunk is the sealed interface over streaming content deltas. Separate from
// Block because complete blocks have fields only valid at end-of-stream (e.g.
// ThinkingBlock.Signature). Chunks are never serialized, so there is no codec
// and no ChunkType wire tag.
type Chunk interface{ isChunk() }

func (*TextChunk) isChunk()     {}
func (*ThinkingChunk) isChunk() {}

type TextChunk struct{ Text string }

type ThinkingChunk struct{ Thinking string }
```

**Step 3 — Verify it compiles in isolation (it will, but `block_test.go`/`chunk_test.go` won't yet — that's Task A6).**

Run: `go vet ./internal/content/` — expect failures only from the old test files referencing removed fields. Source files compile.

---

### Task A2: Message.Blocks → []Block (struct only; JSON in A4)

**Files:**
- Modify: `internal/content/message.go:18` (`Blocks []*Block` → `Blocks []Block`)

Change only the field type. Leave the typed-message structs and `isMessage()` markers as-is. JSON methods are added in Task A4.

---

### Task A3: Block codec — TDD (the heart of the change)

**Files:**
- Create: `internal/content/block_json.go`
- Create: `internal/content/errors.go` (typed codec errors)
- Test: `internal/content/block_json_test.go`

**Step 1 — Write the failing round-trip test first.** @superpowers:test-driven-development

```go
package content

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBlockCodecRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   Block
	}{
		{"text", &TextBlock{Text: "hello"}},
		{"thinking", &ThinkingBlock{Thinking: "reasoning", Signature: "sig"}},
		{"image url", &ImageBlock{MediaType: MediaTypeImagePNG, Source: ImageSource{URL: "https://x/y.png"}}},
		{"image data", &ImageBlock{MediaType: MediaTypeImagePNG, Source: ImageSource{Data: []byte{1, 2, 3}}}},
		{"tool use", &ToolUseBlock{ID: "t1", Name: "search", Input: json.RawMessage(`{"q":"go"}`)}},
		{"empty tool result", &ToolResultBlock{ToolUseID: "t1"}},
		{"nested tool result", &ToolResultBlock{
			ToolUseID: "t1",
			Content:   []Block{&TextBlock{Text: "out"}, &ImageBlock{MediaType: MediaTypeImagePNG, Source: ImageSource{Data: []byte{9}}}},
			IsError:   true,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalBlock(tt.in)
			if err != nil {
				t.Fatalf("MarshalBlock: %v", err)
			}
			got, err := UnmarshalBlock(data)
			if err != nil {
				t.Fatalf("UnmarshalBlock: %v", err)
			}
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("round-trip mismatch:\n got %#v\nwant %#v", got, tt.in)
			}
		})
	}
}

func TestUnmarshalBlockUnknownTag(t *testing.T) {
	_, err := UnmarshalBlock([]byte(`{"type":"video","src":"x"}`))
	var ue *UnknownBlockTypeError
	if !errorsAs(err, &ue) { // use errors.As; helper omitted for brevity
		t.Fatalf("want *UnknownBlockTypeError, got %v", err)
	}
}
```

(Use real `errors.As`; the snippet abbreviates.) Add table cases for: oversized input rejection (`UnmarshalBlocks` over the element cap), malformed JSON, and a `nil`/foreign `Block` to `MarshalBlock` → typed error.

**Step 2 — Run, expect compile failure / FAIL.** `go test ./internal/content/ -run TestBlockCodec` → undefined `MarshalBlock`.

**Step 3 — Implement `errors.go`:**

```go
package content

import "fmt"

// UnknownBlockTypeError is returned by the codec when a serialized block carries
// a tag with no concrete type — including the empty tag. The restore path is an
// untrusted boundary; callers fail secure on this error.
type UnknownBlockTypeError struct{ Type BlockType }

func (e *UnknownBlockTypeError) Error() string {
	return fmt.Sprintf("content: unknown block type %q", string(e.Type))
}

// BlockEncodeError wraps a failure to marshal a concrete block payload.
type BlockEncodeError struct {
	Type  BlockType
	Cause error
}

func (e *BlockEncodeError) Error() string {
	return fmt.Sprintf("content: encode block %q: %v", string(e.Type), e.Cause)
}
func (e *BlockEncodeError) Unwrap() error { return e.Cause }

// BlockDecodeError wraps a failure to unmarshal serialized block bytes.
type BlockDecodeError struct{ Cause error }

func (e *BlockDecodeError) Error() string { return "content: decode block: " + e.Cause.Error() }
func (e *BlockDecodeError) Unwrap() error { return e.Cause }

// BlockLimitError is returned when serialized input exceeds a codec safety cap.
type BlockLimitError struct {
	Limit string // which cap: "block_bytes" | "slice_count"
	Got   int
	Max   int
}

func (e *BlockLimitError) Error() string {
	return fmt.Sprintf("content: block input exceeds %s cap (%d > %d)", e.Limit, e.Got, e.Max)
}
```

**Step 4 — Implement `block_json.go`** (flat `{type, …payload}` via a key-merge — never an embedding wrapper, which would let `ToolResultBlock`'s promoted `MarshalJSON` shadow the `type` key):

```go
package content

import "encoding/json"

// Codec safety caps for the untrusted restore boundary. Tune to real history
// sizes; these are conservative starting values.
const (
	maxBlockBytes     = 8 << 20 // 8 MiB per serialized block
	maxBlocksPerSlice = 10_000  // elements in one []Block
)

// blockTag returns the wire discriminator for a concrete Block. A nil or foreign
// value yields UnknownBlockTypeError (typed-nil/nil-interface fail closed here).
func blockTag(b Block) (BlockType, error) {
	switch b.(type) {
	case *TextBlock:
		return TypeText, nil
	case *ImageBlock:
		return TypeImage, nil
	case *AudioBlock:
		return TypeAudio, nil
	case *DocumentBlock:
		return TypeDocument, nil
	case *ThinkingBlock:
		return TypeThinking, nil
	case *ToolUseBlock:
		return TypeToolUse, nil
	case *ToolResultBlock:
		return TypeToolResult, nil
	default:
		return "", &UnknownBlockTypeError{}
	}
}

// MarshalBlock writes {"type": <tag>, …payload}. The payload is marshaled first
// (so ToolResultBlock's custom MarshalJSON runs), then the tag is merged in as a
// sibling key. Key order is not significant.
func MarshalBlock(b Block) ([]byte, error) {
	tag, err := blockTag(b)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(b)
	if err != nil {
		return nil, &BlockEncodeError{Type: tag, Cause: err}
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, &BlockEncodeError{Type: tag, Cause: err}
	}
	tagJSON, _ := json.Marshal(tag) // BlockType is a string; cannot fail
	fields["type"] = tagJSON
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &BlockEncodeError{Type: tag, Cause: err}
	}
	return out, nil
}

// UnmarshalBlock reads the tag, allocates the concrete type, and decodes the same
// bytes into it (the extra "type" key is ignored by the struct decode).
func UnmarshalBlock(data []byte) (Block, error) {
	if len(data) > maxBlockBytes {
		return nil, &BlockLimitError{Limit: "block_bytes", Got: len(data), Max: maxBlockBytes}
	}
	var probe struct {
		Type BlockType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &BlockDecodeError{Cause: err}
	}
	switch probe.Type {
	case TypeText:
		return decodeInto[TextBlock](data)
	case TypeImage:
		return decodeInto[ImageBlock](data)
	case TypeAudio:
		return decodeInto[AudioBlock](data)
	case TypeDocument:
		return decodeInto[DocumentBlock](data)
	case TypeThinking:
		return decodeInto[ThinkingBlock](data)
	case TypeToolUse:
		return decodeInto[ToolUseBlock](data)
	case TypeToolResult:
		return decodeInto[ToolResultBlock](data)
	default:
		return nil, &UnknownBlockTypeError{Type: probe.Type}
	}
}

// decodeInto unmarshals data into a freshly allocated *T and returns it as Block.
func decodeInto[T any](data []byte) (Block, error) {
	v := new(T)
	if err := json.Unmarshal(data, v); err != nil {
		return nil, &BlockDecodeError{Cause: err}
	}
	// new(T) is non-nil; any(*T) satisfies Block for the seven payload types.
	return any(v).(Block), nil
}

// MarshalBlocks encodes a []Block as a JSON array of tagged blocks.
func MarshalBlocks(bs []Block) ([]byte, error) {
	raws := make([]json.RawMessage, len(bs))
	for i, b := range bs {
		r, err := MarshalBlock(b)
		if err != nil {
			return nil, err
		}
		raws[i] = r
	}
	return json.Marshal(raws)
}

// UnmarshalBlocks decodes a JSON array of tagged blocks. It is the single
// recursion point for nested content (ToolResultBlock.Content) and enforces the
// element-count cap.
func UnmarshalBlocks(data []byte) ([]Block, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, &BlockDecodeError{Cause: err}
	}
	if len(raws) > maxBlocksPerSlice {
		return nil, &BlockLimitError{Limit: "slice_count", Got: len(raws), Max: maxBlocksPerSlice}
	}
	blocks := make([]Block, 0, len(raws))
	for _, r := range raws {
		b, err := UnmarshalBlock(r)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// toolResultJSON is the wire form of ToolResultBlock; Content goes through the
// slice codec so nested blocks stay tagged.
type toolResultJSON struct {
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (t *ToolResultBlock) MarshalJSON() ([]byte, error) {
	var content json.RawMessage
	if len(t.Content) > 0 {
		c, err := MarshalBlocks(t.Content)
		if err != nil {
			return nil, err
		}
		content = c
	}
	return json.Marshal(toolResultJSON{ToolUseID: t.ToolUseID, Content: content, IsError: t.IsError})
}

func (t *ToolResultBlock) UnmarshalJSON(data []byte) error {
	var j toolResultJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	t.ToolUseID = j.ToolUseID
	t.IsError = j.IsError
	if len(j.Content) > 0 {
		blocks, err := UnmarshalBlocks(j.Content)
		if err != nil {
			return err
		}
		t.Content = blocks
	}
	return nil
}
```

> Note on `decodeInto`'s `any(v).(Block)`: each of the seven `*T` satisfies `Block`, so the assertion never fails at runtime; the switch only dispatches the seven known tags. If preferred, replace generics with seven explicit `var v X; json.Unmarshal(...); return &v, nil` arms — identical behavior, no generic.

> Alternative if the flat-merge ever proves fragile: a nested `{"type":…, "data":{…}}` envelope removes the merge entirely. We default to flat per the design doc; the envelope is the documented fallback.

**Step 5 — Run, expect PASS.** `go test -race ./internal/content/ -run 'TestBlockCodec|TestUnmarshalBlock'`

---

### Task A4: Message / ToolMessage JSON

**Files:**
- Modify: `internal/content/message.go` (add JSON methods)
- Test: `internal/content/message_json_test.go`

**Step 1 — Failing test first** — assert `ToolUseID` survives (the bug the design flagged):

```go
func TestToolMessageJSONPreservesToolUseID(t *testing.T) {
	in := &ToolMessage{
		Message:   Message{Role: RoleTool, Blocks: []Block{&TextBlock{Text: "ok"}}},
		ToolUseID: "tu_42",
	}
	data, err := json.Marshal(in)
	if err != nil { t.Fatal(err) }
	var got ToolMessage
	if err := json.Unmarshal(data, &got); err != nil { t.Fatal(err) }
	if got.ToolUseID != "tu_42" { t.Fatalf("ToolUseID dropped: %q", got.ToolUseID) }
	if !reflect.DeepEqual(got.Blocks, in.Blocks) { t.Fatalf("blocks mismatch") }
}
```

Add table cases for `Message`/`UserMessage`/`AIMessage`/`SystemMessage` round-trips.

**Step 2 — Implement.** `Message` gets value-receiver `MarshalJSON` + pointer-receiver `UnmarshalJSON` (User/AI/System inherit via embedding). `ToolMessage` gets its **own** pair (shadows the promoted method so `ToolUseID` is not dropped):

```go
type messageJSON struct {
	Role   Role            `json:"role"`
	Blocks json.RawMessage `json:"blocks,omitempty"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	var blocks json.RawMessage
	if len(m.Blocks) > 0 {
		b, err := MarshalBlocks(m.Blocks)
		if err != nil { return nil, err }
		blocks = b
	}
	return json.Marshal(messageJSON{Role: m.Role, Blocks: blocks})
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var j messageJSON
	if err := json.Unmarshal(data, &j); err != nil { return err }
	m.Role = j.Role
	if len(j.Blocks) > 0 {
		blocks, err := UnmarshalBlocks(j.Blocks)
		if err != nil { return err }
		m.Blocks = blocks
	}
	return nil
}

type toolMessageJSON struct {
	Role      Role            `json:"role"`
	Blocks    json.RawMessage `json:"blocks,omitempty"`
	ToolUseID string          `json:"tool_use_id"`
}

func (m ToolMessage) MarshalJSON() ([]byte, error) {
	var blocks json.RawMessage
	if len(m.Blocks) > 0 {
		b, err := MarshalBlocks(m.Blocks)
		if err != nil { return nil, err }
		blocks = b
	}
	return json.Marshal(toolMessageJSON{Role: m.Role, Blocks: blocks, ToolUseID: m.ToolUseID})
}

func (m *ToolMessage) UnmarshalJSON(data []byte) error {
	var j toolMessageJSON
	if err := json.Unmarshal(data, &j); err != nil { return err }
	m.Role = j.Role
	m.ToolUseID = j.ToolUseID
	if len(j.Blocks) > 0 {
		blocks, err := UnmarshalBlocks(j.Blocks)
		if err != nil { return err }
		m.Blocks = blocks
	}
	return nil
}
```

Add `import "encoding/json"` to `message.go`.

**Step 3 — Run, expect PASS.** `go test -race ./internal/content/ -run 'Message'`

---

### Task A5: Fuzz target (CLAUDE.md: parser of external input)

**Files:**
- Test: `internal/content/block_json_fuzz_test.go`

```go
func FuzzUnmarshalBlock(f *testing.F) {
	f.Add([]byte(`{"type":"text","text":"hi"}`))
	f.Add([]byte(`{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"x"}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := UnmarshalBlock(data)
		if err != nil {
			return // rejecting malformed input is correct; must not panic
		}
		// Re-marshal must succeed and re-unmarshal to an equal value (round-trip stable).
		out, err := MarshalBlock(b)
		if err != nil { t.Fatalf("re-marshal: %v", err) }
		b2, err := UnmarshalBlock(out)
		if err != nil { t.Fatalf("re-unmarshal: %v", err) }
		if !reflect.DeepEqual(b, b2) { t.Fatalf("not stable:\n%#v\n%#v", b, b2) }
	})
}
```

Run: `go test ./internal/content/ -run x -fuzz=FuzzUnmarshalBlock -fuzztime=30s` → no crashers.

---

### Task A6: Rewrite existing content tests to the concrete-type form

**Files:**
- Modify: `internal/content/block_test.go` (672 lines), `chunk_test.go` (136), `message_test.go` (284)

Transformation:
- Delete tests asserting union invariants that are now **unrepresentable** (two payloads set, `Type` disagreeing with payload, zero payloads) — the type system enforces these; tests for them no longer compile and add no value.
- Replace `&content.Block{Type: TypeText, Text: &TextBlock{…}}` literals with `&content.TextBlock{…}`.
- Replace any `b.Type ==` assertions with type switches / `errors.As` / `reflect.TypeOf`.
- Keep and adapt construction, equality, and slice-shape tests; fold codec coverage into the A3/A4 tests rather than duplicating.

Run: `go test -race ./internal/content/` → all green. **Phase A is internally complete; the rest of the repo is still red.**

---

## Phase B — consumers (mechanical, compiler-driven)

Uniform rules everywhere:
1. `[]*content.Block` → `[]content.Block`
2. `&content.Block{Type: content.TypeX, X: &content.XBlock{…}}` → `&content.XBlock{…}`
3. `switch b.Type { case content.TypeX: … b.X.Field }` → `switch b := b.(type) { case *content.XBlock: … b.Field }`
4. `content.Chunk{Type: content.ChunkTypeX, X: &content.XChunk{…}}` → `&content.XChunk{…}`
5. `switch chunk.Type { case content.ChunkTypeText: … }` → `switch c := chunk.(type) { case *content.TextChunk: … }`
6. Type switches that don't render every kind get a `default:` (skip).

Let the compiler drive: `go build ./...` after each task surfaces the next site.

### Task B1: `internal/llm/openaiapi` codec

**Files:** Modify `encode.go`, `decode.go`, `stream.go`. Test: `encode_test.go`, `decode_test.go`, `stream_test.go`.

Representative before→after (`encode.go:textContent`, lines 103-111):

```go
// before
func textContent(blocks []*content.Block) string {
	var out string
	for _, b := range blocks {
		if b.Type == content.TypeText && b.Text != nil {
			out += b.Text.Text
		}
	}
	return out
}
// after
func textContent(blocks []content.Block) string {
	var out string
	for _, b := range blocks {
		if t, ok := b.(*content.TextBlock); ok {
			out += t.Text
		}
	}
	return out
}
```

Apply the same to `encodeContentParts` (`encode.go:115`, switch on `*TextBlock`/`*ImageBlock`), `encodeAIMessage` (`encode.go:161`, switch `*TextBlock`/`*ToolUseBlock`/`*ThinkingBlock`), `imageURL` (unchanged — already takes `*content.ImageBlock`). `decode.go:buildBlocks` → return `[]content.Block`, append `&content.ThinkingBlock{…}` / `&content.TextBlock{…}` / `&content.ToolUseBlock{…}`. `stream.go:NewStream` → return `&content.ThinkingChunk{…}` / `&content.TextChunk{…}`; the `content.Chunk{}` zero-value error returns become `nil` (interface zero value): `return nil, err`.

Run: `go test -race ./internal/llm/openaiapi/`

### Task B2: `internal/agent/loop`

**Files:** Modify `turn.go`, `command.go`. (`event.go` needs **no change** — `TokenDelta.Chunk content.Chunk` is now an interface field, same name.) Test: `turn_test.go`, `loop_test.go`, `fake_test.go`.

- `command.go:23` `Input []*content.Block` → `[]content.Block`.
- `turn.go:22` `input []*content.Block` → `[]content.Block`; the chunk dispatch (`turn.go:58-67`) → `switch c := chunk.(type) { case *content.TextChunk: textBuf.WriteString(c.Text); case *content.ThinkingChunk: thinkBuf.WriteString(c.Thinking) }`; assistant block construction (`turn.go:70-82`) → `&content.ThinkingBlock{…}` / `&content.TextBlock{…}`, slice typed `[]content.Block`.

Run: `go test -race ./internal/agent/loop/`

### Task B3: `internal/session`

**Files:** Modify `agent.go` (`Invoke` `agent.go:82`, `Stream` `agent.go:123`: `input []*content.Block` → `[]content.Block`). Test: `agent_test.go` (the `textChunk` helper `agent_test.go:24` → `&content.TextChunk{…}`; stub returns `content.Chunk` interface).

Run: `go test -race ./internal/session/`

### Task B4: `agents/personal-assistant`

**Files:** Modify `agent.go:133` `userBlocks` → return `[]content.Block`, build `[]content.Block{&content.TextBlock{Text: text}}`. Test: `agent_test.go`, `fake_test.go`.

Run: `go test -race ./agents/personal-assistant/`

### Task B5: providers — verify no-op

**Files:** `internal/llm/llm.go`, `openaiapi/{phala,chutes,lmstudio}/*.go`, `llm_test.go`, provider `client_test.go`.

These reference `content.Chunk`/`content.Block` only as generic type args (`StreamReader[content.Chunk]`) or passthrough — an interface satisfies the type parameter, so **source should compile unchanged**. Only `chutes/client_test.go` and `lmstudio/client_test.go` construct literals and need the Phase B rules applied.

Run: `go build ./... ` → expect clean (first time the whole tree compiles).

---

## Phase C — verify, then the single commit

@superpowers:verification-before-completion — run each, confirm output, before claiming done:

**Step 1:** `CGO_ENABLED=0 go build -trimpath ./...` → clean.
**Step 2:** `go test -race ./...` → all green.
**Step 3:** `go test ./internal/content/ -run x -fuzz=FuzzUnmarshalBlock -fuzztime=30s` → no crashers.
**Step 4:** `make secure` (vet + staticcheck + gosec + govulncheck) → clean.
**Step 5:** `rg -n 'content\.Block\{|ChunkType|\.Type ==|\[\]\*content\.Block' --type go` → only intended hits (e.g. `BlockType` const block); no leftover union usage.

**Step 6 — Commit (design doc + code together):**

```bash
git add internal/content agents internal/agent internal/llm internal/session docs/plans/2026-06-14-content-sealed-interfaces.md docs/plans/2026-06-14-content-sealed-interfaces-impl.md
git commit -m "refactor(content): Block/Chunk as sealed interfaces + JSON codec

Replace tagged-union Block/Chunk structs with sealed interfaces (concrete
payload = discriminator); add block_json.go codec (MarshalBlock/Blocks,
UnmarshalBlock/Blocks, ToolResultBlock + Message/ToolMessage JSON) with a
round-trip fuzz target; migrate openaiapi/loop/session/personal-assistant
consumers to type switches. Drop unused ChunkType.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Risk notes for the executor

- **Atomic commit:** do not `git commit` mid-phase; the tree won't build until B5.
- **Typed-nil discipline:** every block construction stays `&content.X{…}` (non-nil); never assign a nil `*X` into a `Block`. Consumers keep a `default` arm.
- **`decodeInto` generic** is optional sugar — fall back to explicit per-type arms if staticcheck/readability prefers.
- **Size caps** (`maxBlockBytes`, `maxBlocksPerSlice`) are placeholders — confirm against expected history sizes before merge.
- **`omitempty`** on wire structs keeps serialized output compact and makes empty `ToolResultBlock`/`Message` round-trip cleanly; verify the round-trip tests cover the empty cases (they do: "empty tool result").
