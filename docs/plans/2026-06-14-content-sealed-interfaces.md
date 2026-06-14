# `content.Block` / `content.Chunk` as Sealed Interfaces — Design

Date: 2026-06-14

## Summary

Change `internal/content`'s `Block` and `Chunk` from tagged-union structs (a `Type`
discriminator plus nil-able payload pointers) to **sealed interfaces** whose concrete
payload type is the discriminator. Most illegal states — mismatched tag, two payloads, or
none — become unrepresentable and their nil-pointer panic class disappears (the one
residual typed-nil case is handled by construction; see *Nil handling*). `Block`/`Chunk`
then line up with `Conversation` and `loop.Event`, which are already sealed interfaces.

This is a foundational `internal/content` change. It is a prerequisite for the CLI TUI
work (`2026-06-13-tui-design.md`), which consumes these types, but it is not itself a
TUI concern and is specified independently here.

## Current state

`internal/content` (see `2026-06-13-content-llm-design.md`) models blocks as:

```go
type Block struct {
    Type       BlockType
    Text       *TextBlock
    Image      *ImageBlock
    // …5 more nil-able payload pointers
}
```

The invariant "exactly one payload is non-nil and `Type` agrees" is enforced only by a
doc comment, at several independent construction sites — the assistant's user-input
builder, provider decode, and the stream assembler — with more to come as persistence
lands. `Chunk` has the same shape.

## Motivation

- **Correctness.** Nothing stops a caller building an illegal `Block` (mismatched
  `Type`, two payloads, or none), and consumers can dereference the wrong payload. Live
  example: a streaming consumer doing `chunk.Text.Text` panics on a thinking delta
  (`chunk.Text == nil`). A sealed interface makes the concrete type the discriminator, so
  the mismatched-tag and wrong-payload states cannot be expressed and that panic class is
  gone. One nil case survives the move to an interface — a typed-nil `Block` such as
  `(*TextBlock)(nil)`, non-nil as an interface yet panicking on field access — which the
  *Nil handling* section closes by construction.
- **Narrower blast radius (not classic open/closed).** A new kind is a new type
  implementing the marker, not a new field widening a struct that every importer compiles
  against, and consumers that don't handle it degrade gracefully through their `default`
  arm. Consumers that must *act* on the new kind — a provider encoder, the persistence
  codec, a renderer — still opt in explicitly by adding a case. The win is that the
  discriminator stops being a fat struct shared by every package, not that new kinds need
  no downstream edits.
- **Consistency.** `Conversation` (`isMessage()`) and `loop.Event` (`isEvent()`) are
  already sealed interfaces; `Block`/`Chunk` were the outliers.
- **Not the wire format.** Providers never serialize `content` directly —
  `openaiapi/{encode,decode}.go` translate content↔OpenAI JSON — so the in-memory shape
  is free to optimise for correctness over `encoding/json` convenience.

## Design

```go
// Block — the concrete type is the discriminator. No Type field, no nil-able payload
// pointers. Only this package can add variants (unexported marker).
type Block interface{ isBlock() }

func (*TextBlock) isBlock()       {}
func (*ImageBlock) isBlock()      {}
func (*AudioBlock) isBlock()      {}
func (*DocumentBlock) isBlock()   {}
func (*ThinkingBlock) isBlock()   {}
func (*ToolUseBlock) isBlock()    {}
func (*ToolResultBlock) isBlock() {}

// Chunk — same pattern.
type Chunk interface{ isChunk() }

func (*TextChunk) isChunk()     {}
func (*ThinkingChunk) isChunk() {}
```

The concrete payload types (`TextBlock`, `ImageBlock`, …, `TextChunk`, `ThinkingChunk`)
keep their existing fields unchanged; only the `Block`/`Chunk` wrapper structs and their
`Type` fields are removed.

Collections become interface slices:

- `Message.Blocks` → `[]Block`
- `ToolResultBlock.Content` → `[]Block`
- `AgentSession.Invoke`/`Stream` input `[]*content.Block` → `[]content.Block` (the
  assistant's `Stream(ctx, text)` keeps its signature — it builds blocks internally via
  `userBlocks`, whose return type changes to `[]content.Block`)

`BlockType` and its constants **remain** as the wire/persistence discriminator for the
codec below — no longer a field on the in-memory `Block`. `ChunkType` is **removed**:
chunks are ephemeral streaming deltas, assembled into blocks and never serialized, so once
`Chunk` is an interface its constants have no consumer (the stream assembler builds
`&content.TextChunk{…}` directly; `turn.go` type-switches the result). Consumers dispatch
with a type switch and handle a `default` for kinds they do not render:

```go
switch b := blk.(type) {
case *content.TextBlock:  // render text
case *content.ImageBlock: // render image
default:                  // e.g. *ToolUseBlock in a text TUI — skip or placeholder
}
```

### Nil handling

The interface reintroduces one nil hazard the struct did not have: a *typed-nil* `Block`
(`var b content.Block = (*TextBlock)(nil)`) is non-nil at the interface level, matches its
`case *content.TextBlock` arm, then panics on field access. A plain nil interface
(`var b content.Block`) is the other shape — it matches no arm and falls to `default`.
Both are kept out of every `[]Block` by construction, not by checks scattered across
consumers:

- **Construction is always non-nil.** Every site builds blocks as `&content.TextBlock{…}`
  composite literals (provider decode, the loop assembler, and the assistant input builder
  already do); the address of a literal is never nil, so a typed-nil `Block` is reachable
  only by writing one deliberately.
- **The codec never yields nil.** `UnmarshalBlock` allocates the concrete type for a known
  tag and returns it; an unknown or empty tag is a typed `UnknownBlockTypeError`, never a
  nil `Block`.
- **Consumers stay total.** The canonical type switch always carries `default`, with a
  `case nil` added where a slice may be sparse. A variable is dereferenced only inside its
  matching arm, so even a stray typed nil degrades to a skipped block, not a panic.

So the precise claim is "a class of nil-pointer bugs disappears" — the mismatched-tag and
wrong-payload dereferences — held by the discipline above, not "nil is now impossible."

## Serialization codec

A sealed interface cannot round-trip through `encoding/json` alone: the decoder can't pick
a concrete type for an interface value, and a struct field typed `[]Block` marshals to a
tagless array the decoder can't reverse. `internal/content` therefore owns a small codec
keyed on `BlockType`, at the two granularities that actually occur — a single `Block`, and
the `[]Block` slice that both `Message.Blocks` and `ToolResultBlock.Content` hold:

```go
// Single block. Marshal writes {"type": <BlockType>, …payload}; Unmarshal reads the tag,
// allocates the concrete type, and decodes into it.
func MarshalBlock(Block) ([]byte, error)
func UnmarshalBlock([]byte) (Block, error)

// Slice — the unit Message.Blocks and ToolResultBlock.Content are stored as.
func MarshalBlocks([]Block) ([]byte, error)   // JSON array of tagged blocks
func UnmarshalBlocks([]byte) ([]Block, error)
```

`ToolResultBlock` itself carries a `[]Block`, so it implements `json.Marshaler`/
`json.Unmarshaler` over `Marshal/UnmarshalBlocks`; that makes the recursion concrete and
is the only payload type needing custom JSON (every other leaf has concrete fields and uses
default struct marshaling). To flatten the tag into the object, `MarshalBlock` marshals the
payload to a `json.RawMessage` and writes `{"type":…}` merged with its fields — it must
**not** wrap the payload in an outer struct, since a payload that implements `json.Marshaler`
(`ToolResultBlock`) would promote its method and shadow the `type` key. `UnmarshalBlock`
peeks the tag, allocates the concrete type, and decodes the same bytes into it;
`UnmarshalBlocks` is the single recursion point for nested content.

Message-level JSON follows from the slice codec: `Message` implements `MarshalJSON`/
`UnmarshalJSON` over `{role, blocks}`, which `UserMessage`/`AIMessage`/`SystemMessage`
inherit unchanged. `ToolMessage` adds a `ToolUseID`, so it must **not** inherit the promoted
method — that would silently drop the field — and instead gets its own pair over
`{role, blocks, tool_use_id}`, delegating the block half to the slice codec. Reconstructing
a whole `AgenticMessages` thread additionally needs a per-element tag for the sealed
`Conversation` type; that tag and the on-disk *file* format are the persistence layer's
concern — out of scope here (see below). This package guarantees that a `Block`, a
`[]Block`, and a single typed `Message` round-trip.

The discriminator lives in this one place. Per CLAUDE.md the codec gets a **round-trip
fuzz target** (`marshal→unmarshal→equal`) exercising a nested `ToolResultBlock`. A restored
history file is an **untrusted boundary**: the unmarshal path validates the tag (unknown →
typed `UnknownBlockTypeError`), caps element count and total size, and fails secure.

The OpenAI codec (`openaiapi/encode.go`, `decode.go`, `stream.go`) is unchanged in shape
— it already builds/consumes concrete content; its `switch`-on-`.Type` sites become type
switches.

### Faster JSON (sonic) — not adopted

Deferred. History is read back lazily/windowed, so JSON throughput is not the bottleneck;
sonic also honours custom marshalers, so it would call this codec rather than JIT-compile
`Block` and could not accelerate it without a rewrite against sonic's API; and it is a new
dependency requiring approval under CLAUDE.md. Revisit only behind a measured bottleneck
on a trusted bulk path, keeping stdlib on the untrusted restore path.

## Migration surface

Cross-cutting and **atomic**: turning `Block`/`Chunk` from struct to interface breaks every
`*content.Block`/`content.Chunk` site at once — over a dozen files across `content`, the
OpenAI codec and providers, the loop, `session`, and the assistant, plus their tests. The
tree does **not** compile between the steps below; it compiles only once the whole sequence
lands, so this is **one commit, not six**. The numbered list is the edit order *within* that
commit, and the compiler is the worklist — each removed `.Type`/payload field surfaces the
next site to fix:

1. `internal/content`: replace `Block`/`Chunk` structs with interfaces + markers; keep
   `BlockType` for the codec but drop the now-unused `ChunkType` constants (chunks are never
   serialized); change `Message.Blocks` and `ToolResultBlock.Content` to `[]Block`; add
   `block_json.go` (block + slice codec), the `ToolResultBlock`/`Message`/`ToolMessage` JSON
   methods, and the round-trip fuzz test.
2. `internal/llm/openaiapi/{encode,decode,stream}.go`: `switch`-on-`.Type` → type
   switches; construct concrete `*content.X` values.
3. `internal/llm`, `internal/session`: `Stream`/`Invoke` signatures `[]*content.Block` →
   `[]content.Block`.
4. `internal/agent/loop`: `TokenDelta.Chunk` consumers type-switch (text vs thinking).
5. `agents/personal-assistant`: `userBlocks` builds `&content.TextBlock{…}`.
6. Update the table-driven tests across the above to the concrete-type form.

Every old `.Type`/payload access becomes a type switch or an `&content.X{…}` literal; when
`go build ./...` is clean the migration is complete.

If review requires smaller commits, the only way to keep each one compiling is temporary
compatibility shims — retain the old `Block` struct with adapters to and from the interface,
then delete it in a final commit. Given the change is internal, ~a dozen files, and has no
persisted histories to migrate, the single atomic commit is simpler and is the recommended
path; shims add a throwaway second representation and extra review surface for no durable
benefit.

## Testing

- Round-trip fuzz for the block codec (`MarshalBlock`/`MarshalBlocks`), including nested
  `ToolResultBlock`; plus a `Message`/`ToolMessage` round-trip asserting `ToolUseID` survives.
- Table-driven unit tests per variant, plus unknown-tag and oversized-input rejection.
- Existing content/llm/loop/agent tests updated to the concrete-type form; run `-race`.

## Out of scope

- History persistence *format* (append-only JSONL + offset index, windowed read-back) —
  a separate session/runtime concern; this doc fixes only the in-memory type and its codec.
- New block/chunk variants (audio/video chunks, etc.).
- Migrating already-persisted histories (none exist).
