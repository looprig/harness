# internal/content and internal/llm Design

**Date:** 2026-06-13

---

## Goal

Define two foundational packages:

1. `internal/content` — the unified content vocabulary shared across the entire stack (user input, LLM output, streaming, conversation history).
2. `internal/llm` — the provider-neutral inference layer with three providers: Phala TEE, Chutes TEE, and LM Studio (local).

---

## `internal/content`

Single source of truth for all content types. Both the agent loop and the LLM layer import this package. Neither owns content types independently.

### Blocks (complete, non-streaming)

```go
// Block is a discriminated union: exactly one pointer field is non-nil; Type identifies which.
type Block struct {
    Type       BlockType
    Text       *TextBlock
    Image      *ImageBlock
    Audio      *AudioBlock
    Document   *DocumentBlock
    Thinking   *ThinkingBlock
    ToolUse    *ToolUseBlock
    ToolResult *ToolResultBlock
}

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

type TextBlock struct {
    Text string
}

// ImageSource is a sum type: set exactly one field.
type ImageSource struct {
    URL  string // non-empty → remote reference
    Data []byte // non-nil  → inline bytes (e.g. base64-decoded PNG)
}

type ImageBlock struct {
    MediaType string
    Source    ImageSource
}

type AudioBlock struct {
    MediaType string
    Data      []byte
}

type DocumentBlock struct {
    MediaType string
    Name      string
    Data      []byte
    Text      string // for plaintext documents
}

type ThinkingBlock struct {
    Thinking  string
    Signature string // end-of-stream attestation marker; empty until the provider signals completion
}

type ToolUseBlock struct {
    ID    string
    Name  string
    Input json.RawMessage
}

type ToolResultBlock struct {
    ToolUseID string
    Content   []*Block
    IsError   bool
}
```

### Chunks (streaming deltas)

Separate from blocks because completed blocks may have fields that are only valid at
end-of-stream (e.g. `ThinkingBlock.Signature`). Callers never confuse a delta with a
complete block.

```go
type Chunk struct {
    Type     ChunkType
    Text     *TextChunk
    Thinking *ThinkingChunk
}

type ChunkType string

const (
    ChunkTypeText     ChunkType = "text_delta"
    ChunkTypeThinking ChunkType = "thinking_delta"
)

type TextChunk     struct{ Text string }
type ThinkingChunk struct{ Thinking string }
```

### Messages

Typed messages are built on a bare `Message` struct that carries role and blocks.
The sealed `Conversation` interface lets `AgenticMessages` hold any message type
without `map[string]interface{}`.

```go
type Role string

const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleSystem    Role = "system"
    RoleTool      Role = "tool"
)

// Message is the bare base — role + blocks.
type Message struct {
    Role   Role
    Blocks []*Block
}

// Typed messages embed Message so the role is always present and accessible.
type UserMessage   struct{ Message }
type AIMessage     struct{ Message }
type SystemMessage struct{ Message }
type ToolMessage   struct {
    Message
    ToolUseID string
}

func (*UserMessage) isMessage()   {}
func (*AIMessage) isMessage()     {}
func (*SystemMessage) isMessage() {}
func (*ToolMessage) isMessage()   {}

// Conversation is the sealed interface over all message types.
type Conversation interface{ isMessage() }

// AgenticMessages is an ordered conversation thread.
type AgenticMessages []Conversation
```

### File layout

```
internal/content/
  block.go    — Block, BlockType, ImageSource, all *Block subtypes
  chunk.go    — Chunk, ChunkType, TextChunk, ThinkingChunk
  message.go  — Role, Message, UserMessage, AIMessage, SystemMessage, ToolMessage,
                 Conversation, AgenticMessages
```

---

## `internal/llm`

Provider-neutral inference interface plus three OpenAI-wire-compatible providers.
All providers live under `internal/llm/openaiapi/` because they all speak the OpenAI
chat completions wire format; only their transport and security models differ.

### Root types

```go
// LLM is the provider-neutral inference interface.
type LLM interface {
    Invoke(ctx context.Context, req Request) (*Response, error)
    Stream(ctx context.Context, req Request) (*StreamReader[content.Chunk], error)
}

// ReasoningEffort selects o-series inference intensity. Zero value = disabled.
// Silently ignored by providers that do not support it.
type ReasoningEffort string

const (
    ReasoningEffortLow    ReasoningEffort = "low"
    ReasoningEffortMedium ReasoningEffort = "medium"
    ReasoningEffortHigh   ReasoningEffort = "high"
)

// ModelSpec identifies a model and its sampling configuration.
// Call Validate before encoding to catch self-contradictory combinations.
type ModelSpec struct {
    Model  string
    System string // system prompt

    Temperature    *float64
    TopP           *float64
    MaxTokens      *int
    Stop           []string

    // ThinkingBudget enables extended thinking (budget_tokens).
    // When >0, Temperature must be exactly 1.0; Validate enforces this.
    ThinkingBudget int

    ReasoningEffort ReasoningEffort
}

// Validate returns an error if the spec contains self-contradictory values.
// Every provider must call this at the top of Invoke and Stream before encoding.
func (s ModelSpec) Validate() error { ... }

// Request is the provider-neutral inference request.
type Request struct {
    Model    ModelSpec
    Messages content.AgenticMessages
    Tools    []Tool
}

// Response is the complete provider-neutral response.
type Response struct {
    Message *content.AIMessage
    Usage   *Usage
    Model   string // echoed model name
}

// Tool is a callable function definition exposed to the model.
type Tool struct {
    Name        string
    Description string
    Schema      json.RawMessage
}

// Usage reports token consumption for the request.
type Usage struct {
    InputTokens  int
    OutputTokens int
}
```

### Package layout

```
internal/llm/
  llm.go        — LLM interface, Request, Response, ModelSpec, ReasoningEffort, Tool, Usage
  stream.go     — StreamReader[T any]
  errors.go     — NetworkError, APIError, ValidationError, AttestationError

  openaiapi/
    types.go    — unexported OpenAI wire structs (chatCompletionRequest, etc.)
    encode.go   — content.AgenticMessages → OpenAI JSON (shared by all providers)
    decode.go   — OpenAI JSON → content.AIMessage (shared by all providers)
    sse.go      — SSE line reader (shared)
    stream.go   — stream assembler: SSE events → content.Chunk (shared)

    chutes/     — Chutes TEE: E2E encrypted, NVIDIA GPU attestation
      client.go
      encode.go  — injectResponsePK, ML-KEM sealing on top of openaiapi encode
      decode.go  — decryptResponse, error body unwrapping
      attest.go  — TDX quote verification + NVIDIA NRAS GPU evidence
      discover.go — instance + nonce discovery
      stream.go  — pump goroutine, per-frame AEAD decryption
      errors.go  — AttestationError, ReasonCode enum

    phala/      — Phala TEE: RA-TLS attested transport
      client.go
      encode.go
      decode.go
      attest.go
      discover.go
      stream.go
      errors.go

    lmstudio/   — LM Studio: plain HTTP, OpenAI-compatible, no auth required
      client.go
      encode.go  — thin wrapper; sets local defaults (no Authorization header)
      decode.go
```

### Provider relationship

All three providers implement `llm.LLM`. The `openaiapi/` parent package owns the
shared codec (encode/decode/SSE/stream); each provider subpackage handles its own
transport and security layer, calling into the shared codec as needed.

```
llm.LLM
  ├── openaiapi/lmstudio  — plain http.Client → openaiapi encode/decode
  ├── openaiapi/phala     — attested transport → openaiapi encode/decode
  └── openaiapi/chutes    — ML-KEM sealed transport → chutes encode/decode → openaiapi decode
```

### Import layering

```
internal/content          (no imports from internal/)
internal/llm              → internal/content
internal/llm/openaiapi    → internal/llm, internal/content
internal/llm/openaiapi/*  → internal/llm/openaiapi, internal/llm, internal/content
```

No provider subpackage may import another provider subpackage.

---

## What is explicitly out of scope

- Anthropic native API provider (Anthropic SDK) — separate provider if needed later
- Fallback/retry across providers — caller's responsibility via `llm.LLM` wrapping
- Prompt caching (`CacheControl`) — deferred; add a typed field to `ModelSpec` when a provider requires it natively
- Audio/video streaming chunks — `AudioChunk` added when a provider supports it
- `ToolMessage` encoding for providers that fold tool results into user turns (Anthropic) — handled inside each provider's `encode.go`
