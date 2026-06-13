# internal/content and internal/llm Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Implement `internal/content` (unified content vocabulary) and `internal/llm` (provider-neutral inference interface with lmstudio, phala, and chutes providers).

**Architecture:** `internal/content` is the shared type vocabulary imported by everything else. `internal/llm` defines the `LLM` interface and houses three providers under `openaiapi/` — all speak OpenAI wire format, differing only in transport and security. The shared codec (`openaiapi/encode.go`, `openaiapi/decode.go`) is called by each provider; provider-specific concerns (TEE attestation, E2E encryption) live entirely inside each provider's own files.

**Tech Stack:** Go 1.26, stdlib only (`net/http`, `encoding/json`, `crypto/mlkem`, `log/slog`). No external dependencies. Module path: `github.com/inventivepotter/urvi`.

**Reference source:** `/Users/ipotter/code/ciram/llm/` — port chutes and phala from there, adapting to the new `content.AgenticMessages` → OpenAI encoding layer.

---

## Phase 1 — `internal/content`

### Task 1: `internal/content/block.go`

**Files:**
- Create: `internal/content/block.go`
- Create: `internal/content/block_test.go`

**Step 1: Write the failing test**

```go
// internal/content/block_test.go
package content_test

import (
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

func TestBlock_TextBlock(t *testing.T) {
	b := &content.Block{
		Type: content.TypeText,
		Text: &content.TextBlock{Text: "hello"},
	}
	if b.Type != content.TypeText {
		t.Fatalf("expected TypeText, got %q", b.Type)
	}
	if b.Text.Text != "hello" {
		t.Fatalf("expected hello, got %q", b.Text.Text)
	}
}

func TestBlock_ImageBlock_URL(t *testing.T) {
	b := &content.Block{
		Type:  content.TypeImage,
		Image: &content.ImageBlock{MediaType: "image/png", Source: content.ImageSource{URL: "https://example.com/img.png"}},
	}
	if b.Image.Source.URL == "" {
		t.Fatal("expected URL to be set")
	}
	if b.Image.Source.Data != nil {
		t.Fatal("expected Data to be nil when URL is set")
	}
}

func TestBlock_ThinkingBlock_SignatureOnlyOnComplete(t *testing.T) {
	// Signature is empty during streaming — only set on complete block.
	partial := &content.ThinkingBlock{Thinking: "hmm"}
	if partial.Signature != "" {
		t.Fatal("partial ThinkingBlock should have empty Signature")
	}
	complete := &content.ThinkingBlock{Thinking: "hmm", Signature: "sig123"}
	if complete.Signature == "" {
		t.Fatal("complete ThinkingBlock should have non-empty Signature")
	}
}

func TestBlock_ToolUseBlock_InputIsRawJSON(t *testing.T) {
	raw := json.RawMessage(`{"key":"val"}`)
	b := &content.ToolUseBlock{ID: "tu_1", Name: "search", Input: raw}
	if string(b.Input) != `{"key":"val"}` {
		t.Fatalf("unexpected Input: %s", b.Input)
	}
}

func TestBlock_ToolResultBlock_NestedBlocks(t *testing.T) {
	r := &content.ToolResultBlock{
		ToolUseID: "tu_1",
		Content:   []*content.Block{{Type: content.TypeText, Text: &content.TextBlock{Text: "result"}}},
		IsError:   false,
	}
	if len(r.Content) != 1 {
		t.Fatalf("expected 1 nested block, got %d", len(r.Content))
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/content/... -run TestBlock -v
```
Expected: `cannot find package` or `undefined: content`

**Step 3: Write the implementation**

```go
// internal/content/block.go
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
	Signature string // end-of-stream attestation marker; empty until provider signals completion
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

**Step 4: Run test to verify it passes**

```bash
go test ./internal/content/... -run TestBlock -v
```
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/content/block.go internal/content/block_test.go
git commit -m "feat(content): add Block discriminated union and all block subtypes"
```

---

### Task 2: `internal/content/chunk.go`

**Files:**
- Create: `internal/content/chunk.go`
- Create: `internal/content/chunk_test.go`

**Step 1: Write the failing test**

```go
// internal/content/chunk_test.go
package content_test

import "testing"
import "github.com/inventivepotter/urvi/internal/content"

func TestChunk_TextChunk(t *testing.T) {
	c := &content.Chunk{
		Type: content.ChunkTypeText,
		Text: &content.TextChunk{Text: "hello"},
	}
	if c.Text.Text != "hello" {
		t.Fatalf("expected hello, got %q", c.Text.Text)
	}
	if c.Thinking != nil {
		t.Fatal("Thinking should be nil for text chunk")
	}
}

func TestChunk_ThinkingChunk(t *testing.T) {
	c := &content.Chunk{
		Type:     content.ChunkTypeThinking,
		Thinking: &content.ThinkingChunk{Thinking: "reasoning step"},
	}
	if c.Thinking.Thinking != "reasoning step" {
		t.Fatalf("unexpected thinking: %q", c.Thinking.Thinking)
	}
	if c.Text != nil {
		t.Fatal("Text should be nil for thinking chunk")
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/content/... -run TestChunk -v
```
Expected: `undefined: content.ChunkTypeText`

**Step 3: Write the implementation**

```go
// internal/content/chunk.go
package content

type ChunkType string

const (
	ChunkTypeText     ChunkType = "text_delta"
	ChunkTypeThinking ChunkType = "thinking_delta"
)

// Chunk is the tagged union for streaming content deltas.
// Separate from Block because complete blocks have fields only valid at end-of-stream
// (e.g. ThinkingBlock.Signature).
type Chunk struct {
	Type     ChunkType
	Text     *TextChunk
	Thinking *ThinkingChunk
}

type TextChunk     struct{ Text string }
type ThinkingChunk struct{ Thinking string }
```

**Step 4: Run test to verify it passes**

```bash
go test ./internal/content/... -run TestChunk -v
```
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/content/chunk.go internal/content/chunk_test.go
git commit -m "feat(content): add Chunk streaming delta types"
```

---

### Task 3: `internal/content/message.go`

**Files:**
- Create: `internal/content/message.go`
- Create: `internal/content/message_test.go`

**Step 1: Write the failing test**

```go
// internal/content/message_test.go
package content_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

func TestMessage_EmbeddedRoleAndBlocks(t *testing.T) {
	u := &content.UserMessage{
		Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []*content.Block{{Type: content.TypeText, Text: &content.TextBlock{Text: "hi"}}},
		},
	}
	if u.Role != content.RoleUser {
		t.Fatalf("expected RoleUser, got %q", u.Role)
	}
	if len(u.Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(u.Blocks))
	}
}

func TestMessage_AIMessageBlocks(t *testing.T) {
	a := &content.AIMessage{
		Message: content.Message{
			Role: content.RoleAssistant,
			Blocks: []*content.Block{
				{Type: content.TypeText, Text: &content.TextBlock{Text: "response"}},
				{Type: content.TypeThinking, Thinking: &content.ThinkingBlock{Thinking: "thought", Signature: "sig"}},
			},
		},
	}
	if a.Role != content.RoleAssistant {
		t.Fatalf("expected RoleAssistant")
	}
	if len(a.Blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(a.Blocks))
	}
}

func TestMessage_ToolMessageHasToolUseID(t *testing.T) {
	tm := &content.ToolMessage{
		Message:   content.Message{Role: content.RoleTool},
		ToolUseID: "tu_abc",
	}
	if tm.ToolUseID != "tu_abc" {
		t.Fatalf("expected tu_abc, got %q", tm.ToolUseID)
	}
}

func TestAgenticMessages_HoldsAllTypes(t *testing.T) {
	msgs := content.AgenticMessages{
		&content.SystemMessage{Message: content.Message{Role: content.RoleSystem}},
		&content.UserMessage{Message: content.Message{Role: content.RoleUser}},
		&content.AIMessage{Message: content.Message{Role: content.RoleAssistant}},
		&content.ToolMessage{Message: content.Message{Role: content.RoleTool}, ToolUseID: "x"},
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
}

func TestConversation_SealedInterface(t *testing.T) {
	// Compile-time check: all typed messages satisfy Conversation.
	var _ content.Conversation = (*content.UserMessage)(nil)
	var _ content.Conversation = (*content.AIMessage)(nil)
	var _ content.Conversation = (*content.SystemMessage)(nil)
	var _ content.Conversation = (*content.ToolMessage)(nil)
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/content/... -run TestMessage -v
go test ./internal/content/... -run TestAgenticMessages -v
go test ./internal/content/... -run TestConversation -v
```
Expected: `undefined: content.UserMessage`

**Step 3: Write the implementation**

```go
// internal/content/message.go
package content

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Message is the bare base — role + blocks.
// Typed messages embed this so the role is always present and accessible.
type Message struct {
	Role   Role
	Blocks []*Block
}

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
// Only types in this package can implement it (unexported marker method).
type Conversation interface{ isMessage() }

// AgenticMessages is an ordered conversation thread.
type AgenticMessages []Conversation
```

**Step 4: Run all content tests**

```bash
go test ./internal/content/... -v
```
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/content/message.go internal/content/message_test.go
git commit -m "feat(content): add typed messages and AgenticMessages conversation thread"
```

---

## Phase 2 — `internal/llm` root

### Task 4: `internal/llm/errors.go`

**Files:**
- Create: `internal/llm/errors.go`
- Create: `internal/llm/errors_test.go`

**Step 1: Write the failing test**

```go
// internal/llm/errors_test.go
package llm_test

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
)

func TestNetworkError_Error(t *testing.T) {
	inner := errors.New("dial tcp: connection refused")
	e := &llm.NetworkError{Err: inner}
	if e.Error() == "" {
		t.Fatal("expected non-empty error string")
	}
	if !errors.Is(e, inner) {
		t.Fatal("expected errors.Is to unwrap to inner")
	}
}

func TestAPIError_Error(t *testing.T) {
	e := &llm.APIError{Status: 429, Message: "rate limited"}
	if e.Error() == "" {
		t.Fatal("expected non-empty error string")
	}
}

func TestValidationError_Error(t *testing.T) {
	e := &llm.ValidationError{Field: "ThinkingBudget", Reason: "requires Temperature=1.0"}
	if e.Error() == "" {
		t.Fatal("expected non-empty error string")
	}
}

func TestAttestationError_Error(t *testing.T) {
	e := &llm.AttestationError{Reason: "TDX quote signature mismatch"}
	if e.Error() == "" {
		t.Fatal("expected non-empty error string")
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/llm/... -run TestNetworkError -v
```
Expected: `cannot find package`

**Step 3: Write the implementation**

```go
// internal/llm/errors.go
package llm

import "fmt"

// NetworkError wraps a transport-level failure (DNS, TCP, TLS).
type NetworkError struct {
	Err error
}

func (e *NetworkError) Error() string { return "llm: network error: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }

// APIError is a non-2xx response from the provider.
type APIError struct {
	Status  int
	Message string
	Body    []byte // raw response body, may be nil
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: api error %d: %s", e.Status, e.Message)
}

// ValidationError is a self-contradictory request that must be rejected before
// sending to the provider (e.g. ThinkingBudget > 0 with Temperature != 1.0).
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("llm: validation error: %s: %s", e.Field, e.Reason)
}

// AttestationError is a TEE attestation failure. Fail-closed: the request is
// never sent to the provider when this error is returned.
type AttestationError struct {
	Reason string
	Err    error // may be nil
}

func (e *AttestationError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("llm: attestation error: %s: %v", e.Reason, e.Err)
	}
	return "llm: attestation error: " + e.Reason
}
func (e *AttestationError) Unwrap() error { return e.Err }
```

**Step 4: Run test to verify it passes**

```bash
go test ./internal/llm/... -run Test.*Error -v
```
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/llm/errors.go internal/llm/errors_test.go
git commit -m "feat(llm): add error types (NetworkError, APIError, ValidationError, AttestationError)"
```

---

### Task 5: `internal/llm/stream.go`

**Files:**
- Create: `internal/llm/stream.go`
- Create: `internal/llm/stream_test.go`

**Step 1: Write the failing test**

```go
// internal/llm/stream_test.go
package llm_test

import (
	"errors"
	"io"
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
)

func TestStreamReader_NextAndClose(t *testing.T) {
	items := []string{"a", "b", "c"}
	idx := 0
	r := llm.NewStreamReader(func() (string, error) {
		if idx >= len(items) {
			return "", io.EOF
		}
		v := items[idx]
		idx++
		return v, nil
	}, nil)

	for _, want := range items {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatalf("expected %q, got %q", want, got)
		}
	}

	_, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestStreamReader_Close(t *testing.T) {
	closed := false
	r := llm.NewStreamReader(func() (string, error) {
		return "", io.EOF
	}, func() error {
		closed = true
		return nil
	})
	if err := r.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	if !closed {
		t.Fatal("expected closer to be called")
	}
}

func TestStreamReader_ErrorPropagates(t *testing.T) {
	sentinel := errors.New("provider error")
	r := llm.NewStreamReader(func() (string, error) {
		return "", sentinel
	}, nil)
	_, err := r.Next()
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/llm/... -run TestStreamReader -v
```
Expected: `undefined: llm.NewStreamReader`

**Step 3: Write the implementation**

```go
// internal/llm/stream.go
package llm

// StreamReader is a pull-based iterator over streaming values of type T.
// Call Next to advance; it returns (zero, io.EOF) when the stream is exhausted.
// Always call Close when done — even after io.EOF — to release the underlying
// connection.
type StreamReader[T any] struct {
	next  func() (T, error)
	close func() error
}

// NewStreamReader constructs a StreamReader from a next function and an optional
// closer. If closer is nil, Close is a no-op. next must return (zero, io.EOF)
// when the stream is exhausted.
func NewStreamReader[T any](next func() (T, error), closer func() error) *StreamReader[T] {
	if closer == nil {
		closer = func() error { return nil }
	}
	return &StreamReader[T]{next: next, close: closer}
}

// Next returns the next value. Returns (zero, io.EOF) when exhausted.
func (r *StreamReader[T]) Next() (T, error) {
	return r.next()
}

// Close releases the underlying connection. Safe to call multiple times.
func (r *StreamReader[T]) Close() error {
	return r.close()
}
```

**Step 4: Run test to verify it passes**

```bash
go test ./internal/llm/... -run TestStreamReader -v
```
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/llm/stream.go internal/llm/stream_test.go
git commit -m "feat(llm): add generic StreamReader pull-based iterator"
```

---

### Task 6: `internal/llm/llm.go`

**Files:**
- Create: `internal/llm/llm.go`
- Create: `internal/llm/llm_test.go`

**Step 1: Write the failing test**

```go
// internal/llm/llm_test.go
package llm_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
)

func TestModelSpec_Validate_OK(t *testing.T) {
	temp := 1.0
	s := llm.ModelSpec{
		Model:          "gpt-4o",
		ThinkingBudget: 1000,
		Temperature:    &temp,
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("expected valid spec, got: %v", err)
	}
}

func TestModelSpec_Validate_ThinkingBudgetRequires1_0(t *testing.T) {
	temp := 0.7
	s := llm.ModelSpec{
		Model:          "claude-sonnet-4-6",
		ThinkingBudget: 1000,
		Temperature:    &temp,
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected validation error for ThinkingBudget + Temperature != 1.0")
	}
}

func TestModelSpec_Validate_ThinkingBudgetRequiresTemperatureSet(t *testing.T) {
	s := llm.ModelSpec{
		Model:          "claude-sonnet-4-6",
		ThinkingBudget: 1000,
		Temperature:    nil, // not set
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected validation error for ThinkingBudget with nil Temperature")
	}
}

func TestModelSpec_Validate_InvalidReasoningEffort(t *testing.T) {
	s := llm.ModelSpec{
		Model:           "o3",
		ReasoningEffort: "extreme",
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected validation error for unknown ReasoningEffort")
	}
}

func TestModelSpec_Validate_EmptyReasoningEffortOK(t *testing.T) {
	s := llm.ModelSpec{Model: "gpt-4o"}
	if err := s.Validate(); err != nil {
		t.Fatalf("empty ReasoningEffort should be valid: %v", err)
	}
}

func TestLLM_InterfaceCompiles(t *testing.T) {
	// Compile-time check only — no runtime assertion needed.
	var _ llm.LLM = (*fakeLLM)(nil)
}

// fakeLLM is a test double for llm.LLM.
type fakeLLM struct{}

func (f *fakeLLM) Invoke(ctx interface{ Done() <-chan struct{} }, req llm.Request) (*llm.Response, error) {
	return nil, nil
}
```

Note: the `fakeLLM` above won't compile directly due to the context type — use a real context in the final file. The key thing is the interface is satisfied.

**Step 3: Write the implementation**

```go
// internal/llm/llm.go
package llm

import (
	"context"
	"encoding/json"

	"github.com/inventivepotter/urvi/internal/content"
)

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
	System string

	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	Stop        []string

	// ThinkingBudget enables extended thinking (budget_tokens).
	// When >0, Temperature must be exactly 1.0; Validate enforces this.
	ThinkingBudget int

	ReasoningEffort ReasoningEffort
}

// Validate returns an error if the spec contains self-contradictory values.
func (s ModelSpec) Validate() error {
	if s.ThinkingBudget > 0 {
		if s.Temperature == nil {
			return &ValidationError{Field: "ThinkingBudget", Reason: "requires Temperature to be set to exactly 1.0"}
		}
		if *s.Temperature != 1.0 {
			return &ValidationError{Field: "ThinkingBudget", Reason: "requires Temperature == 1.0"}
		}
	}
	switch s.ReasoningEffort {
	case "", ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh:
		// valid
	default:
		return &ValidationError{Field: "ReasoningEffort", Reason: "must be low, medium, or high"}
	}
	return nil
}

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
	Model   string
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

**Step 2: Run test to verify it fails, then step 4 after implementation**

```bash
go test ./internal/llm/... -run TestModelSpec -v
```
Expected after implementation: all PASS

**Step 5: Run all llm tests**

```bash
go test ./internal/llm/... -v
```

**Step 6: Commit**

```bash
git add internal/llm/llm.go internal/llm/llm_test.go
git commit -m "feat(llm): add LLM interface, ModelSpec with Validate, Request, Response"
```

---

## Phase 3 — `internal/llm/openaiapi` shared codec

### Task 7: `openaiapi/types.go` — unexported wire structs

**Files:**
- Create: `internal/llm/openaiapi/types.go`

No tests for this file — it is only unexported wire structs used by encode/decode.

```go
// internal/llm/openaiapi/types.go
package openaiapi

import "encoding/json"

// chatRequest is the OpenAI chat completions wire request.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
	Stream      bool          `json:"stream,omitempty"`

	// o-series reasoning
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"` // string or []chatContentPart
	ToolCalls  []toolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

type toolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"` // always "function"
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"` // always "function"
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// chatResponse is the OpenAI chat completions wire response.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type chatChoice struct {
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// sseChunk is one streaming delta event.
type sseChunk struct {
	Choices []sseChoice `json:"choices"`
	Usage   *chatUsage  `json:"usage"`
}

type sseChoice struct {
	Delta sseMessageDelta `json:"delta"`
}

type sseMessageDelta struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content"` // DeepSeek / o-series
	ToolCalls        []toolCall `json:"tool_calls"`
}
```

**Commit:**

```bash
git add internal/llm/openaiapi/types.go
git commit -m "feat(llm/openaiapi): add unexported OpenAI wire structs"
```

---

### Task 8: `openaiapi/encode.go`

**Files:**
- Create: `internal/llm/openaiapi/encode.go`
- Create: `internal/llm/openaiapi/encode_test.go`

**Step 1: Write the failing test**

```go
// internal/llm/openaiapi/encode_test.go
package openaiapi_test

import (
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

func TestEncodeRequest_SystemFromModelSpec(t *testing.T) {
	req := llm.Request{
		Model: llm.ModelSpec{Model: "gpt-4o", System: "you are helpful"},
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []*content.Block{{Type: content.TypeText, Text: &content.TextBlock{Text: "hello"}}},
			}},
		},
	}
	body, err := openaiapi.EncodeRequest(req, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(wire["messages"], &msgs); err != nil {
		t.Fatalf("invalid messages: %v", err)
	}
	// First message must be system from ModelSpec.System
	var role string
	json.Unmarshal(msgs[0]["role"], &role)
	if role != "system" {
		t.Fatalf("expected first message role=system, got %q", role)
	}
}

func TestEncodeRequest_StreamFlag(t *testing.T) {
	req := llm.Request{Model: llm.ModelSpec{Model: "gpt-4o"}}
	body, err := openaiapi.EncodeRequest(req, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var wire map[string]json.RawMessage
	json.Unmarshal(body, &wire)
	var stream bool
	json.Unmarshal(wire["stream"], &stream)
	if !stream {
		t.Fatal("expected stream=true")
	}
}

func TestEncodeRequest_ImageBlock(t *testing.T) {
	req := llm.Request{
		Model: llm.ModelSpec{Model: "gpt-4o"},
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role: content.RoleUser,
				Blocks: []*content.Block{{
					Type:  content.TypeImage,
					Image: &content.ImageBlock{MediaType: "image/png", Source: content.ImageSource{URL: "https://example.com/img.png"}},
				}},
			}},
		},
	}
	body, err := openaiapi.EncodeRequest(req, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify the image_url part appears in messages
	if !json.Valid(body) {
		t.Fatal("invalid JSON")
	}
}

func TestEncodeRequest_ToolUseInAIMessage(t *testing.T) {
	req := llm.Request{
		Model: llm.ModelSpec{Model: "gpt-4o"},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []*content.Block{{
					Type:    content.TypeToolUse,
					ToolUse: &content.ToolUseBlock{ID: "tc_1", Name: "search", Input: json.RawMessage(`{"q":"go"}`)},
				}},
			}},
		},
	}
	body, err := openaiapi.EncodeRequest(req, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !json.Valid(body) {
		t.Fatal("invalid JSON")
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/llm/openaiapi/... -run TestEncodeRequest -v
```
Expected: `undefined: openaiapi.EncodeRequest`

**Step 3: Write the implementation**

```go
// internal/llm/openaiapi/encode.go
package openaiapi

import (
	"encoding/json"
	"fmt"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// EncodeRequest converts a provider-neutral llm.Request to an OpenAI
// chat completions JSON body. stream=true adds "stream":true to the body.
// ModelSpec.System is prepended as a system message if non-empty.
func EncodeRequest(req llm.Request, stream bool) ([]byte, error) {
	wire := chatRequest{
		Model:           req.Model.Model,
		Temperature:     req.Model.Temperature,
		TopP:            req.Model.TopP,
		MaxTokens:       req.Model.MaxTokens,
		Stop:            req.Model.Stop,
		Stream:          stream,
		ReasoningEffort: string(req.Model.ReasoningEffort),
	}

	// System prompt from ModelSpec — prepend before conversation messages.
	if req.Model.System != "" {
		wire.Messages = append(wire.Messages, chatMessage{
			Role:    "system",
			Content: req.Model.System,
		})
	}

	for _, conv := range req.Messages {
		msg, err := encodeConversation(conv)
		if err != nil {
			return nil, err
		}
		wire.Messages = append(wire.Messages, msg...)
	}

	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}

	return json.Marshal(wire)
}

func encodeConversation(conv content.Conversation) ([]chatMessage, error) {
	switch m := conv.(type) {
	case *content.SystemMessage:
		return []chatMessage{{Role: "system", Content: textContent(m.Blocks)}}, nil
	case *content.UserMessage:
		parts, err := encodeContentParts(m.Blocks)
		if err != nil {
			return nil, err
		}
		return []chatMessage{{Role: "user", Content: parts}}, nil
	case *content.AIMessage:
		return encodeAIMessage(m)
	case *content.ToolMessage:
		return []chatMessage{{
			Role:       "tool",
			Content:    textContent(m.Blocks),
			ToolCallID: m.ToolUseID,
		}}, nil
	default:
		return nil, fmt.Errorf("openaiapi encode: unknown message type %T", conv)
	}
}

func encodeAIMessage(m *content.AIMessage) ([]chatMessage, error) {
	msg := chatMessage{Role: "assistant"}
	var textParts []string
	var toolCalls []toolCall

	for _, b := range m.Blocks {
		switch b.Type {
		case content.TypeText:
			if b.Text != nil {
				textParts = append(textParts, b.Text.Text)
			}
		case content.TypeToolUse:
			if b.ToolUse != nil {
				toolCalls = append(toolCalls, toolCall{
					ID:   b.ToolUse.ID,
					Type: "function",
					Function: toolCallFunction{
						Name:      b.ToolUse.Name,
						Arguments: b.ToolUse.Input,
					},
				})
			}
		}
		// ThinkingBlock is not encoded in OpenAI wire format.
	}

	if len(textParts) > 0 {
		combined := ""
		for _, p := range textParts {
			combined += p
		}
		msg.Content = combined
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return []chatMessage{msg}, nil
}

func encodeContentParts(blocks []*content.Block) (interface{}, error) {
	// If all blocks are plain text, use a single string (backwards compat).
	allText := true
	for _, b := range blocks {
		if b.Type != content.TypeText {
			allText = false
			break
		}
	}
	if allText {
		return textContent(blocks), nil
	}

	var parts []chatContentPart
	for _, b := range blocks {
		switch b.Type {
		case content.TypeText:
			if b.Text != nil {
				parts = append(parts, chatContentPart{Type: "text", Text: b.Text.Text})
			}
		case content.TypeImage:
			if b.Image != nil {
				url := b.Image.Source.URL
				if url == "" && b.Image.Source.Data != nil {
					url = "data:" + b.Image.MediaType + ";base64," + encodeBase64(b.Image.Source.Data)
				}
				parts = append(parts, chatContentPart{
					Type:     "image_url",
					ImageURL: &imageURLPart{URL: url},
				})
			}
		}
	}
	return parts, nil
}

func textContent(blocks []*content.Block) string {
	out := ""
	for _, b := range blocks {
		if b.Type == content.TypeText && b.Text != nil {
			out += b.Text.Text
		}
	}
	return out
}

func encodeBase64(data []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	_ = table
	// Use stdlib encoding/base64 via import.
	import_encoding_base64_StdEncoding_EncodeToString := func(b []byte) string {
		// placeholder — real impl uses encoding/base64
		return ""
		_ = b
	}
	return import_encoding_base64_StdEncoding_EncodeToString(data)
}
```

> **Note to implementer:** Replace the `encodeBase64` placeholder with `import "encoding/base64"` and `base64.StdEncoding.EncodeToString(data)`. The placeholder above is to make the intent clear; it won't compile as-is.

**Step 4: Run test to verify it passes**

```bash
go test ./internal/llm/openaiapi/... -run TestEncodeRequest -v
```

**Step 5: Commit**

```bash
git add internal/llm/openaiapi/encode.go internal/llm/openaiapi/encode_test.go
git commit -m "feat(llm/openaiapi): add shared OpenAI request encoder"
```

---

### Task 9: `openaiapi/decode.go`

**Files:**
- Create: `internal/llm/openaiapi/decode.go`
- Create: `internal/llm/openaiapi/decode_test.go`

**Step 1: Write the failing test**

```go
// internal/llm/openaiapi/decode_test.go
package openaiapi_test

import (
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

func TestDecodeResponse_TextContent(t *testing.T) {
	raw := []byte(`{
		"id": "chatcmpl-1",
		"model": "gpt-4o",
		"choices": [{"message": {"role": "assistant", "content": "Hello!"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5}
	}`)
	resp, err := openaiapi.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %q", resp.Model)
	}
	if resp.Message == nil {
		t.Fatal("expected non-nil message")
	}
	if len(resp.Message.Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(resp.Message.Blocks))
	}
	if resp.Message.Blocks[0].Type != content.TypeText {
		t.Fatalf("expected TypeText block")
	}
	if resp.Message.Blocks[0].Text.Text != "Hello!" {
		t.Fatalf("expected Hello!, got %q", resp.Message.Blocks[0].Text.Text)
	}
	if resp.Usage.InputTokens != 10 {
		t.Fatalf("expected 10 input tokens, got %d", resp.Usage.InputTokens)
	}
}

func TestDecodeResponse_ToolCallContent(t *testing.T) {
	raw := []byte(`{
		"id": "chatcmpl-2",
		"model": "gpt-4o",
		"choices": [{"message": {
			"role": "assistant",
			"content": null,
			"tool_calls": [{"id":"tc_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"go\"}"}}]
		}, "finish_reason": "tool_calls"}]
	}`)
	resp, err := openaiapi.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Message.Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(resp.Message.Blocks))
	}
	b := resp.Message.Blocks[0]
	if b.Type != content.TypeToolUse {
		t.Fatalf("expected TypeToolUse, got %q", b.Type)
	}
	if b.ToolUse.Name != "search" {
		t.Fatalf("expected search, got %q", b.ToolUse.Name)
	}
}

func TestDecodeResponse_ReasoningContent(t *testing.T) {
	raw := []byte(`{
		"id": "chatcmpl-3",
		"model": "o3",
		"choices": [{"message": {
			"role": "assistant",
			"content": "answer",
			"reasoning_content": "step by step"
		}, "finish_reason": "stop"}]
	}`)
	resp, err := openaiapi.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should have ThinkingBlock + TextBlock
	hasThinking, hasText := false, false
	for _, b := range resp.Message.Blocks {
		if b.Type == content.TypeThinking {
			hasThinking = true
		}
		if b.Type == content.TypeText {
			hasText = true
		}
	}
	if !hasThinking {
		t.Fatal("expected ThinkingBlock from reasoning_content")
	}
	if !hasText {
		t.Fatal("expected TextBlock from content")
	}
}

func TestDecodeResponse_EmptyChoices(t *testing.T) {
	raw := []byte(`{"id":"x","model":"m","choices":[]}`)
	_, err := openaiapi.DecodeResponse(raw)
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
}

// Compile-time check: DecodeResponse returns *llm.Response.
var _ func([]byte) (*llm.Response, error) = openaiapi.DecodeResponse
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/llm/openaiapi/... -run TestDecodeResponse -v
```

**Step 3: Write the implementation**

```go
// internal/llm/openaiapi/decode.go
package openaiapi

import (
	"encoding/json"
	"fmt"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// DecodeResponse parses an OpenAI chat completions JSON response body into
// a provider-neutral *llm.Response.
func DecodeResponse(body []byte) (*llm.Response, error) {
	var wire chatResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, &llm.NetworkError{Err: fmt.Errorf("decode response: %w", err)}
	}
	if len(wire.Choices) == 0 {
		return nil, &llm.APIError{Status: 0, Message: "response contains no choices", Body: body}
	}

	msg, err := decodeMessage(wire.Choices[0].Message)
	if err != nil {
		return nil, err
	}

	resp := &llm.Response{
		Message: msg,
		Model:   wire.Model,
	}
	if wire.Usage != nil {
		resp.Usage = &llm.Usage{
			InputTokens:  wire.Usage.PromptTokens,
			OutputTokens: wire.Usage.CompletionTokens,
		}
	}
	return resp, nil
}

func decodeMessage(m chatMessage) (*content.AIMessage, error) {
	var blocks []*content.Block

	// reasoning_content → ThinkingBlock (before text, consistent with Anthropic ordering)
	if m.ReasoningContent != "" {
		blocks = append(blocks, &content.Block{
			Type:     content.TypeThinking,
			Thinking: &content.ThinkingBlock{Thinking: m.ReasoningContent},
		})
	}

	// content string → TextBlock
	if s, ok := m.Content.(string); ok && s != "" {
		blocks = append(blocks, &content.Block{
			Type: content.TypeText,
			Text: &content.TextBlock{Text: s},
		})
	}

	// tool_calls → ToolUseBlock
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, &content.Block{
			Type: content.TypeToolUse,
			ToolUse: &content.ToolUseBlock{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: tc.Function.Arguments,
			},
		})
	}

	return &content.AIMessage{
		Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: blocks,
		},
	}, nil
}
```

**Step 4: Run test to verify it passes**

```bash
go test ./internal/llm/openaiapi/... -run TestDecodeResponse -v
```

**Step 5: Commit**

```bash
git add internal/llm/openaiapi/decode.go internal/llm/openaiapi/decode_test.go
git commit -m "feat(llm/openaiapi): add shared OpenAI response decoder"
```

---

### Task 10: `openaiapi/sse.go` — SSE line reader

**Files:**
- Create: `internal/llm/openaiapi/sse.go`
- Create: `internal/llm/openaiapi/sse_test.go`

**Step 1: Write the failing test**

```go
// internal/llm/openaiapi/sse_test.go
package openaiapi_test

import (
	"io"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

func TestSSEReader_YieldsDataLines(t *testing.T) {
	body := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n" +
		"data: [DONE]\n\n",
	)
	r := openaiapi.NewSSEReader(body)

	line1, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if line1 != `{"choices":[{"delta":{"content":"hi"}}]}` {
		t.Fatalf("unexpected line1: %q", line1)
	}

	line2, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if line2 != `{"choices":[{"delta":{"content":" there"}}]}` {
		t.Fatalf("unexpected line2: %q", line2)
	}

	_, err = r.Next()
	if err != io.EOF {
		t.Fatalf("expected io.EOF after [DONE], got %v", err)
	}
}

func TestSSEReader_IgnoresNonDataLines(t *testing.T) {
	body := strings.NewReader(": keep-alive\n\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	r := openaiapi.NewSSEReader(body)
	line, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if line != `{"choices":[]}` {
		t.Fatalf("unexpected line: %q", line)
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/llm/openaiapi/... -run TestSSEReader -v
```

**Step 3: Write the implementation**

```go
// internal/llm/openaiapi/sse.go
package openaiapi

import (
	"bufio"
	"io"
	"strings"
)

// SSEReader reads OpenAI-style Server-Sent Events from an HTTP response body.
// Each call to Next returns the JSON payload from one "data: <json>" line.
// Returns io.EOF after "data: [DONE]" or end of stream.
type SSEReader struct {
	scanner *bufio.Scanner
}

// NewSSEReader constructs an SSEReader from an HTTP response body.
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{scanner: bufio.NewScanner(r)}
}

// Next returns the next SSE data payload, stripping the "data: " prefix.
// Returns io.EOF when the stream ends (either [DONE] or connection close).
func (s *SSEReader) Next() (string, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return "", io.EOF
		}
		return payload, nil
	}
	if err := s.scanner.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}
```

**Step 4: Run test to verify it passes**

```bash
go test ./internal/llm/openaiapi/... -run TestSSEReader -v
```

**Step 5: Commit**

```bash
git add internal/llm/openaiapi/sse.go internal/llm/openaiapi/sse_test.go
git commit -m "feat(llm/openaiapi): add SSE line reader"
```

---

### Task 11: `openaiapi/stream.go` — stream assembler

**Files:**
- Create: `internal/llm/openaiapi/stream.go`
- Create: `internal/llm/openaiapi/stream_test.go`

**Step 1: Write the failing test**

```go
// internal/llm/openaiapi/stream_test.go
package openaiapi_test

import (
	"io"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

func TestNewStream_YieldsTextChunks(t *testing.T) {
	body := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	sr := openaiapi.NewStream(io.NopCloser(body))
	defer sr.Close()

	c1, err := sr.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c1.Type != content.ChunkTypeText || c1.Text.Text != "Hello" {
		t.Fatalf("unexpected chunk: %+v", c1)
	}

	c2, err := sr.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c2.Text.Text != " world" {
		t.Fatalf("expected ' world', got %q", c2.Text.Text)
	}

	_, err = sr.Next()
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestNewStream_YieldsThinkingChunks(t *testing.T) {
	body := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hmm\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	sr := openaiapi.NewStream(io.NopCloser(body))
	defer sr.Close()

	c, err := sr.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Type != content.ChunkTypeThinking {
		t.Fatalf("expected ChunkTypeThinking, got %q", c.Type)
	}
	if c.Thinking.Thinking != "hmm" {
		t.Fatalf("expected hmm, got %q", c.Thinking.Thinking)
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/llm/openaiapi/... -run TestNewStream -v
```

**Step 3: Write the implementation**

```go
// internal/llm/openaiapi/stream.go
package openaiapi

import (
	"encoding/json"
	"io"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// NewStream constructs a StreamReader[content.Chunk] from an HTTP response body
// containing OpenAI SSE events. The caller must Close the reader when done.
func NewStream(body io.ReadCloser) *llm.StreamReader[content.Chunk] {
	sse := NewSSEReader(body)
	return llm.NewStreamReader(func() (content.Chunk, error) {
		for {
			line, err := sse.Next()
			if err != nil {
				return content.Chunk{}, err
			}
			var ev sseChunk
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue // skip malformed lines
			}
			if len(ev.Choices) == 0 {
				continue
			}
			delta := ev.Choices[0].Delta

			if delta.ReasoningContent != "" {
				return content.Chunk{
					Type:     content.ChunkTypeThinking,
					Thinking: &content.ThinkingChunk{Thinking: delta.ReasoningContent},
				}, nil
			}
			if delta.Content != "" {
				return content.Chunk{
					Type: content.ChunkTypeText,
					Text: &content.TextChunk{Text: delta.Content},
				}, nil
			}
			// Empty delta (role-only or finish): keep reading.
		}
	}, body.Close)
}
```

**Step 4: Run test to verify it passes**

```bash
go test ./internal/llm/openaiapi/... -run TestNewStream -v
```

**Step 5: Run all openaiapi tests**

```bash
go test ./internal/llm/openaiapi/... -v
```

**Step 6: Commit**

```bash
git add internal/llm/openaiapi/stream.go internal/llm/openaiapi/stream_test.go
git commit -m "feat(llm/openaiapi): add stream assembler yielding content.Chunk from SSE"
```

---

## Phase 4 — `lmstudio` provider

### Task 12: `openaiapi/lmstudio` provider

**Files:**
- Create: `internal/llm/openaiapi/lmstudio/client.go`
- Create: `internal/llm/openaiapi/lmstudio/encode.go`
- Create: `internal/llm/openaiapi/lmstudio/decode.go`
- Create: `internal/llm/openaiapi/lmstudio/client_test.go`

LM Studio is OpenAI-compatible with no auth. Default base URL: `http://localhost:1234/v1`.

**Step 1: Write the failing test**

```go
// internal/llm/openaiapi/lmstudio/client_test.go
package lmstudio_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi/lmstudio"
)

func TestClient_Invoke(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("lmstudio should not send Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-1",
			"model": "local-model",
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 1},
		})
	}))
	defer srv.Close()

	c := lmstudio.New(srv.URL, lmstudio.WithHTTPClient(srv.Client()))
	resp, err := c.Invoke(context.Background(), llm.Request{
		Model: llm.ModelSpec{Model: "local-model"},
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []*content.Block{{Type: content.TypeText, Text: &content.TextBlock{Text: "hello"}}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Message == nil {
		t.Fatal("expected non-nil message")
	}
	if len(resp.Message.Blocks) == 0 {
		t.Fatal("expected at least one block")
	}
}

func TestClient_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := lmstudio.New(srv.URL, lmstudio.WithHTTPClient(srv.Client()))
	sr, err := c.Stream(context.Background(), llm.Request{
		Model: llm.ModelSpec{Model: "local-model"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer sr.Close()

	chunk, err := sr.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk.Type != content.ChunkTypeText || chunk.Text.Text != "hi" {
		t.Fatalf("unexpected chunk: %+v", chunk)
	}
}

// Compile-time: lmstudio.Client must satisfy llm.LLM.
var _ llm.LLM = (*lmstudio.Client)(nil)
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/llm/openaiapi/lmstudio/... -v
```
Expected: `cannot find package`

**Step 3: Write `encode.go`**

```go
// internal/llm/openaiapi/lmstudio/encode.go
package lmstudio

import (
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

// encodeRequest delegates to the shared openaiapi encoder.
// LM Studio has no lmstudio-specific encoding requirements.
func encodeRequest(req llm.Request, stream bool) ([]byte, error) {
	return openaiapi.EncodeRequest(req, stream)
}
```

**Write `decode.go`**

```go
// internal/llm/openaiapi/lmstudio/decode.go
package lmstudio

import (
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

// decodeResponse delegates to the shared openaiapi decoder.
func decodeResponse(body []byte) (*llm.Response, error) {
	return openaiapi.DecodeResponse(body)
}
```

**Write `client.go`**

```go
// internal/llm/openaiapi/lmstudio/client.go
package lmstudio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

const defaultBaseURL = "http://localhost:1234/v1"

// Client is a plain HTTP OpenAI-compatible client for LM Studio.
// It sends no Authorization header — LM Studio does not require auth.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// New constructs a Client. baseURL defaults to http://localhost:1234/v1 if empty.
func New(baseURL string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Invoke sends a non-streaming request and returns the complete response.
func (c *Client) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	body, err := encodeRequest(req, false)
	if err != nil {
		return nil, err
	}
	respBody, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}
	return decodeResponse(respBody)
}

// Stream sends a streaming request and returns a StreamReader[content.Chunk].
func (c *Client) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	body, err := encodeRequest(req, true)
	if err != nil {
		return nil, err
	}
	httpResp, err := c.doStream(ctx, body)
	if err != nil {
		return nil, err
	}
	return openaiapi.NewStream(httpResp.Body), nil
}

func (c *Client) do(ctx context.Context, body []byte) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return nil, &llm.APIError{Status: httpResp.StatusCode, Message: string(respBody), Body: respBody}
	}
	return respBody, nil
}

func (c *Client) doStream(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &llm.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		defer httpResp.Body.Close()
		b, _ := io.ReadAll(httpResp.Body)
		return nil, &llm.APIError{Status: httpResp.StatusCode, Message: fmt.Sprintf("lmstudio stream: %s", b), Body: b}
	}
	return httpResp, nil
}
```

**Step 4: Run test to verify it passes**

```bash
go test ./internal/llm/openaiapi/lmstudio/... -v
```

**Step 5: Commit**

```bash
git add internal/llm/openaiapi/lmstudio/
git commit -m "feat(llm/lmstudio): add plain HTTP OpenAI-compatible LM Studio provider"
```

---

## Phase 5 — `phala` provider

Port from `/Users/ipotter/code/ciram/llm/phala/`. Adapt to the new `llm.LLM` interface and `content.AgenticMessages` encoding.

### Task 13: `phala/errors.go` and `phala/discover.go`

Port directly from:
- `/Users/ipotter/code/ciram/llm/phala/errors.go` → `internal/llm/openaiapi/phala/errors.go`
- `/Users/ipotter/code/ciram/llm/phala/discover.go` → `internal/llm/openaiapi/phala/discover.go`

**Adaptations required:**
- Change `github.com/ipotter/toto/llm` imports to `github.com/inventivepotter/urvi/internal/llm`
- Keep `AttestationError` — it matches our `llm.AttestationError` interface. Use that instead of a local one.

**Step 1: Copy and adapt files**

```bash
cp /Users/ipotter/code/ciram/llm/phala/errors.go internal/llm/openaiapi/phala/errors.go
cp /Users/ipotter/code/ciram/llm/phala/discover.go internal/llm/openaiapi/phala/discover.go
```

Update package declarations to `package phala` and fix import paths. Replace any `llm.NetworkError` / `llm.APIError` usages to use `github.com/inventivepotter/urvi/internal/llm`.

**Step 2: Run build check**

```bash
go build ./internal/llm/openaiapi/phala/...
```

**Step 3: Commit**

```bash
git add internal/llm/openaiapi/phala/errors.go internal/llm/openaiapi/phala/discover.go
git commit -m "feat(llm/phala): port errors and instance discovery from ciram"
```

---

### Task 14: `phala/attest.go` and `phala/sign.go`

Port from:
- `/Users/ipotter/code/ciram/llm/phala/attest.go`
- `/Users/ipotter/code/ciram/llm/phala/sign.go`

```bash
cp /Users/ipotter/code/ciram/llm/phala/attest.go internal/llm/openaiapi/phala/attest.go
cp /Users/ipotter/code/ciram/llm/phala/sign.go internal/llm/openaiapi/phala/sign.go
```

Fix import paths. Run `go build ./internal/llm/openaiapi/phala/...` after each file.

**Commit:**

```bash
git add internal/llm/openaiapi/phala/attest.go internal/llm/openaiapi/phala/sign.go
git commit -m "feat(llm/phala): port RA-TLS attestation and request signing from ciram"
```

---

### Task 15: `phala/encode.go` and `phala/decode.go`

Phala-specific encoding wraps the shared openaiapi encoder and adds any Phala request headers or signing. Decoding wraps the shared decoder.

**Files:**
- Create: `internal/llm/openaiapi/phala/encode.go`
- Create: `internal/llm/openaiapi/phala/decode.go`

Port from:
- `/Users/ipotter/code/ciram/llm/phala/` (check for any encode/decode specific files)

If none exist (Phala uses the same OpenAI wire format without modification), these are thin wrappers:

```go
// internal/llm/openaiapi/phala/encode.go
package phala

import (
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

func encodeRequest(req llm.Request, stream bool) ([]byte, error) {
	return openaiapi.EncodeRequest(req, stream)
}
```

```go
// internal/llm/openaiapi/phala/decode.go
package phala

import (
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

func decodeResponse(body []byte) (*llm.Response, error) {
	return openaiapi.DecodeResponse(body)
}
```

**Commit:**

```bash
git add internal/llm/openaiapi/phala/encode.go internal/llm/openaiapi/phala/decode.go
git commit -m "feat(llm/phala): add encode/decode wrappers"
```

---

### Task 16: `phala/stream.go`

Port from `/Users/ipotter/code/ciram/llm/phala/stream.go`. Adapt to return `*llm.StreamReader[content.Chunk]` using `openaiapi.NewStream`.

```bash
cp /Users/ipotter/code/ciram/llm/phala/stream.go internal/llm/openaiapi/phala/stream.go
```

Key adaptation: wherever the ciram code returns `*openai.Stream`, replace with `openaiapi.NewStream(httpResp.Body)` which returns `*llm.StreamReader[content.Chunk]`.

**Commit:**

```bash
git add internal/llm/openaiapi/phala/stream.go
git commit -m "feat(llm/phala): port streaming adapted to StreamReader[content.Chunk]"
```

---

### Task 17: `phala/client.go`

**Files:**
- Create: `internal/llm/openaiapi/phala/client.go`
- Create: `internal/llm/openaiapi/phala/client_test.go`

Port from `/Users/ipotter/code/ciram/llm/phala/client.go`. The client must:
1. Satisfy `llm.LLM` (compile-time assertion)
2. Call `req.Model.Validate()` at the top of `Invoke` and `Stream`
3. Use `encodeRequest` / `decodeResponse` from this package
4. Use `openaiapi.NewStream` for streaming

**Step 1: Write the failing test**

```go
// internal/llm/openaiapi/phala/client_test.go
package phala_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi/phala"
)

// Compile-time: phala.Client must satisfy llm.LLM.
var _ llm.LLM = (*phala.Client)(nil)

func TestClient_ValidateCalledOnInvoke(t *testing.T) {
	c := phala.New("https://phala.example.com", "apikey")
	temp := 0.7
	_, err := c.Invoke(nil, llm.Request{
		Model: llm.ModelSpec{
			Model:          "model",
			ThinkingBudget: 1000,
			Temperature:    &temp, // wrong — should be 1.0
		},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}
```

**Step 2–4:** Port `client.go` from ciram, add compile-time assertion `var _ llm.LLM = (*Client)(nil)`, run tests.

**Step 5: Commit**

```bash
git add internal/llm/openaiapi/phala/client.go internal/llm/openaiapi/phala/client_test.go
git commit -m "feat(llm/phala): add Phala TEE LLM client satisfying llm.LLM"
```

---

## Phase 6 — `chutes` provider

Port from `/Users/ipotter/code/ciram/llm/chutes/`. Same pattern as phala.

### Task 18: `chutes/errors.go` and `chutes/discover.go`

```bash
cp /Users/ipotter/code/ciram/llm/chutes/errors.go internal/llm/openaiapi/chutes/errors.go
cp /Users/ipotter/code/ciram/llm/chutes/discover.go internal/llm/openaiapi/chutes/discover.go
```

Fix import paths. Commit.

```bash
git commit -m "feat(llm/chutes): port errors and instance discovery from ciram"
```

---

### Task 19: `chutes/attest.go`

```bash
cp /Users/ipotter/code/ciram/llm/chutes/attest.go internal/llm/openaiapi/chutes/attest.go
```

Also port `tee/` types if referenced:

```bash
cp -r /Users/ipotter/code/ciram/llm/tee internal/llm/tee
```

Fix import paths. Commit.

```bash
git commit -m "feat(llm/chutes): port TDX + NVIDIA NRAS attestation from ciram"
```

---

### Task 20: `chutes/encode.go`

Port from `/Users/ipotter/code/ciram/llm/chutes/`. The chutes encoder:
1. Calls `openaiapi.EncodeRequest` for the OpenAI JSON body
2. Injects `e2e_response_pk` (the ephemeral ML-KEM encapsulation key) into the JSON
3. Returns the plaintext bytes to be sealed by `e2e.Seal`

```bash
cp /Users/ipotter/code/ciram/llm/chutes/client.go internal/llm/openaiapi/chutes/encode.go
# Extract only the injectResponsePK and encoding helpers; client logic goes in client.go
```

Create `internal/llm/openaiapi/chutes/encode.go` with:
- `encodeRequest(req llm.Request, stream bool) ([]byte, error)` — calls openaiapi.EncodeRequest then injects the response PK
- `injectResponsePK(plaintext, ek []byte) ([]byte, error)` — ported directly from ciram

**Commit:**

```bash
git commit -m "feat(llm/chutes): add chutes-specific request encoder with e2e_response_pk injection"
```

---

### Task 21: `chutes/decode.go`

Port from ciram. The chutes decoder:
1. Decapsulates the ML-KEM response ciphertext
2. AEAD-opens the plaintext
3. Calls `openaiapi.DecodeResponse` on the plaintext

```go
// internal/llm/openaiapi/chutes/decode.go
package chutes
// decryptAndDecode opens the sealed response and delegates to openaiapi.DecodeResponse.
// Port decodeResponse + tryDecryptErrorBody + tryDecryptJSONWrappedDetail +
// tryDecryptRawEnvelope + synthesizeOpaqueDetail + dumpUndecryptableBody from ciram.
```

**Commit:**

```bash
git commit -m "feat(llm/chutes): add e2e response decryption and error body unwrapping"
```

---

### Task 22: `chutes/stream.go`

Port `/Users/ipotter/code/ciram/llm/chutes/stream.go`. Key adaptation: return `*llm.StreamReader[content.Chunk]` by wrapping the decrypted SSE body with `openaiapi.NewStream`.

The pump goroutine decrypts each `e2e` frame and writes plaintext OpenAI SSE bytes to a pipe. `openaiapi.NewStream` reads from the pipe reader end.

```bash
cp /Users/ipotter/code/ciram/llm/chutes/stream.go internal/llm/openaiapi/chutes/stream.go
```

Adapt `ChatStream` to return `(*llm.StreamReader[content.Chunk], error)` instead of `(*openai.Stream, error)`.
Replace the final `openai.NewStream(rc)` call with `openaiapi.NewStream(rc)`.

Also port:
```bash
cp /Users/ipotter/code/ciram/llm/chutes/ssereader.go internal/llm/openaiapi/chutes/ssereader.go
```

**Commit:**

```bash
git commit -m "feat(llm/chutes): port E2E encrypted streaming adapted to StreamReader[content.Chunk]"
```

---

### Task 23: `chutes/client.go` with tests

**Files:**
- Create: `internal/llm/openaiapi/chutes/client.go`
- Create: `internal/llm/openaiapi/chutes/client_test.go`

Port from `/Users/ipotter/code/ciram/llm/chutes/client.go`. Adaptations:
1. `Invoke` and `Stream` replace `Chat` and `ChatStream` as the `llm.LLM` method names
2. Both call `req.Model.Validate()` before encoding
3. `Stream` returns `(*llm.StreamReader[content.Chunk], error)`
4. Add compile-time assertion: `var _ llm.LLM = (*Client)(nil)`

**Step 1: Write the failing test**

```go
// internal/llm/openaiapi/chutes/client_test.go
package chutes_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi/chutes"
)

// Compile-time: chutes.Client must satisfy llm.LLM.
var _ llm.LLM = (*chutes.Client)(nil)

func TestClient_ValidateCalledOnInvoke(t *testing.T) {
	c := chutes.New("https://api.chutes.ai", "apikey")
	temp := 0.7
	_, err := c.Invoke(nil, llm.Request{
		Model: llm.ModelSpec{
			Model:          "model",
			ThinkingBudget: 1000,
			Temperature:    &temp,
		},
	})
	if err == nil {
		t.Fatal("expected validation error before any network call")
	}
}
```

Port integration tests from `/Users/ipotter/code/ciram/llm/chutes/client_test.go` into a separate `client_integration_test.go` with a build tag `//go:build integration`.

**Step 5: Commit**

```bash
git add internal/llm/openaiapi/chutes/
git commit -m "feat(llm/chutes): add Chutes TEE LLM client satisfying llm.LLM"
```

---

## Final verification

```bash
go build ./...
go test ./internal/content/... ./internal/llm/... -v
go vet ./...
```

Expected: all packages build, all tests pass.

```bash
git add -A
git commit -m "chore: verify full build and test suite for content and llm packages"
```

---

## What is explicitly out of scope

- Anthropic native provider
- Fallback/retry wrapper over `llm.LLM`
- Prompt caching (`CacheControl`)
- Integration tests for phala/chutes (require live TEE environments)
- `AudioChunk` streaming type
