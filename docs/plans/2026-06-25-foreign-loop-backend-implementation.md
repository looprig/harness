# Foreign Loop Backend Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a pluggable foreign-loop backend so a looprig session (primary loop or any subagent) can be backed by an external coding agent (Claude Code headless first), driven through the same `loop.Backend` contract, with the foreign session id as the durable handle.

**Architecture:** `pkg/loop` gains a small `Backend` interface + an `Engine` selector (additive only). A new `pkg/foreignloop` actor satisfies `Backend` by spawning `claude -p` one-shot per turn, decoding its `stream-json` + on-disk transcript into looprig `event.Event`s. `pkg/session.newLoop` (the single `loop.New` chokepoint) and the restore constructor branch on `cfg.Engine`, building native via `loop.New` or foreign via an injected `foreignloop.Builder`. The foreign session id rides as one `omitzero` field on the existing `LoopStarted` event. Foreign owns its tools (observe-only); looprig mirrors events.

**Tech Stack:** Go (stdlib `os/exec`, `bufio`, `encoding/json`, `context`), existing `pkg/{loop,session,event,content,uuid}`. No new external dependencies. All tests table-driven, run under `-race`.

**Design reference:** `docs/plans/2026-06-25-foreign-loop-backend-design.md`.

**Conventions for every task:** typed errors only (no `errors.New`/`fmt.Errorf` from package APIs); strict typing (no `any` past JSON boundaries); table-driven tests with happy/boundary/error/edge rows; `t.Parallel()`; build with `CGO_ENABLED=0 go build -trimpath`; run `gofmt`. Commit after each task. Run `make secure` before the final commit of each phase.

---

## Phase A — `pkg/loop`: additive `Engine` + `Backend` (no behavior change)

### Task A1: `Engine` type and `Config.Engine` field

**Files:**
- Modify: `pkg/loop/config.go` (add `Engine` type + constants + `Config.Engine` field)
- Test: `pkg/loop/config_engine_test.go` (create)

**Step 1 — Write the failing test.** `pkg/loop/config_engine_test.go`:

```go
package loop

import "testing"

func TestEngineZeroValueIsNative(t *testing.T) {
	t.Parallel()
	var c Config
	if c.Engine != EngineNative {
		t.Fatalf("zero Config.Engine = %v, want EngineNative", c.Engine)
	}
	if EngineNative != 0 {
		t.Fatalf("EngineNative = %d, want 0 (zero value must be native)", EngineNative)
	}
}
```

**Step 2 — Run, expect FAIL.** `go test ./pkg/loop/ -run TestEngineZeroValueIsNative` → FAIL: `undefined: EngineNative`.

**Step 3 — Implement.** In `pkg/loop/config.go`, above `type Config struct`:

```go
// Engine selects which backend constructs this loop. The zero value is native, so
// existing Config construction is unchanged.
type Engine uint8

const (
	EngineNative Engine = iota
	EngineForeignClaude
)
```

Add to `Config` struct (e.g. after `AgentName`):

```go
	// Engine selects the loop backend. Zero = EngineNative (the historical path).
	// EngineForeignClaude routes construction through the injected foreign Builder
	// at the session composition root; loop.New itself only ever builds native.
	Engine Engine
```

**Step 4 — Run, expect PASS.** `go test -race ./pkg/loop/ -run TestEngineZeroValueIsNative`.

**Step 5 — Commit.**
```bash
git add pkg/loop/config.go pkg/loop/config_engine_test.go
git commit -m "feat(loop): add Engine selector to Config (zero=native)"
```

### Task A2: `Backend` interface + accessor methods on `*loop.Loop`

**Files:**
- Create: `pkg/loop/backend.go`
- Test: `pkg/loop/backend_test.go`

**Step 1 — Write the failing test.** `pkg/loop/backend_test.go`:

```go
package loop

import "testing"

// compile-time: *Loop must satisfy Backend.
var _ Backend = (*Loop)(nil)

func TestLoopAccessorsExposeChannels(t *testing.T) {
	t.Parallel()
	l := &Loop{Commands: make(chan command.Command), Done: make(chan struct{})}
	if l.CommandSink() == nil {
		t.Fatal("CommandSink() returned nil")
	}
	if l.DoneChan() == nil {
		t.Fatal("DoneChan() returned nil")
	}
}
```

(Add the `command` import.)

**Step 2 — Run, expect FAIL.** `go test ./pkg/loop/ -run TestLoopAccessors` → FAIL: `Loop does not implement Backend` / undefined methods.

**Step 3 — Implement.** `pkg/loop/backend.go`:

```go
package loop

import (
	"context"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

// Backend is the narrow turn-engine contract Session drives. Both *loop.Loop and
// *foreignloop.Loop satisfy it. It is deliberately the minimal subset Session uses
// (command submission, completion signalling, committed-state snapshot) — it does
// NOT expose the native loop's internal gate/commit/drain seams, which a foreign
// loop has no analogue for. Snapshot's signature is exactly *Loop.Snapshot's.
type Backend interface {
	CommandSink() chan<- command.Command
	DoneChan() <-chan struct{}
	Snapshot(ctx context.Context) (content.AgenticMessages, event.TurnIndex, error)
}
```

Append the accessors to `*Loop` (in `backend.go` to keep `loop.go` untouched-by-feature):

```go
// CommandSink exposes the command send-side so callers can depend on Backend
// rather than the concrete *Loop. It returns the existing Commands field.
func (l *Loop) CommandSink() chan<- command.Command { return l.Commands }

// DoneChan exposes the completion channel for the Backend contract.
func (l *Loop) DoneChan() <-chan struct{} { return l.Done }
```

**Step 4 — Run, expect PASS.** `go test -race ./pkg/loop/`.

**Step 5 — Commit.**
```bash
git add pkg/loop/backend.go pkg/loop/backend_test.go
git commit -m "feat(loop): add Backend interface + Loop accessors (additive)"
```

---

## Phase B — `pkg/event`: `LoopStarted.ForeignSID` (additive omitzero field)

### Task B1: add `ForeignSID` to `LoopStarted` with round-trip + replay tests

**Files:**
- Modify: `pkg/event/event.go` (`LoopStarted` struct)
- Test: `pkg/event/loopstarted_foreignsid_test.go` (create); extend the existing table in `pkg/event/marshal_test.go:317` (`TestMarshalEventRoundTripEnduring`) to include a `LoopStarted` instance carrying `ForeignSID`.

**Step 1 — Write failing tests.** `pkg/event/loopstarted_foreignsid_test.go`:

```go
package event

import (
	"encoding/json"
	"testing"
)

func TestLoopStartedForeignSIDRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		foreign   string
		wantField bool // is "foreign_sid" present in JSON?
	}{
		{name: "native leaves empty (omitted)", foreign: "", wantField: false},
		{name: "foreign sid present", foreign: "11111111-1111-1111-1111-111111111111", wantField: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := LoopStarted{ForeignSID: tt.foreign}
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if got := json.Valid(b); !got {
				t.Fatal("invalid json")
			}
			if has := contains(b, "foreign_sid"); has != tt.wantField {
				t.Fatalf("foreign_sid present=%v, want %v (%s)", has, tt.wantField, b)
			}
			var out LoopStarted
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out.ForeignSID != tt.foreign {
				t.Fatalf("ForeignSID = %q, want %q", out.ForeignSID, tt.foreign)
			}
		})
	}
}

// replay compat: a legacy record with no foreign_sid key decodes to "".
func TestLoopStartedLegacyRecordDecodesEmpty(t *testing.T) {
	t.Parallel()
	var out LoopStarted
	if err := json.Unmarshal([]byte(`{"parent_tool_use_id":"x"}`), &out); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if out.ForeignSID != "" {
		t.Fatalf("legacy ForeignSID = %q, want empty", out.ForeignSID)
	}
}

func contains(b []byte, s string) bool { return string(b) != "" && bytesIndex(b, s) >= 0 }
func bytesIndex(b []byte, s string) int { return indexString(string(b), s) }
func indexString(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
```

> NOTE: `pkg/event` must **not** validate "foreign requires ForeignSID" — the event package cannot know a loop's engine. That invariant is enforced in `session.newLoop`/restore (Phase F). Keep these tests structural only.

**Step 2 — Run, expect FAIL.** `go test ./pkg/event/ -run ForeignSID` → FAIL: `unknown field ForeignSID`.

**Step 3 — Implement.** In `pkg/event/event.go`, add to `LoopStarted` (mirroring the existing `ParentToolUseID` doc + tag):

```go
	// ForeignSID is the foreign agent's session id this loop is bound to, for
	// foreign-engine loops only; empty for native loops. It is the durable handle
	// used to --resume the foreign session across turns and across restore. omitzero
	// so old journal records (and native loops) decode to "". Mirrors
	// ParentToolUseID: identity metadata carried on the loop's start event.
	ForeignSID string `json:"foreign_sid,omitzero"`
```

**Step 4 — Run, expect PASS.** `go test -race ./pkg/event/`. Then add a `LoopStarted{... ForeignSID: "..."}` row to `TestMarshalEventRoundTripEnduring` and re-run.

**Step 5 — Commit.**
```bash
git add pkg/event/event.go pkg/event/loopstarted_foreignsid_test.go pkg/event/marshal_test.go
git commit -m "feat(event): add omitzero LoopStarted.ForeignSID (structural only)"
```

---

## Phase C — `pkg/foreignloop`: pure types, decoders, mapper (no subprocess)

Everything in this phase is pure and unit-testable without spawning a process.

### Task C1: package types and typed errors

**Files:**
- Create: `pkg/foreignloop/foreignloop.go` (interfaces + value types)
- Create: `pkg/foreignloop/errors.go` (typed errors)
- Test: `pkg/foreignloop/foreignloop_test.go` (compile/zero-value assertions)

**Step 1 — Write the failing test.**

```go
package foreignloop

import "testing"

func TestPostureZeroAndForeignTurnShape(t *testing.T) {
	t.Parallel()
	var p PermissionPosture
	if p != PostureDefault {
		t.Fatalf("zero PermissionPosture = %v, want PostureDefault", p)
	}
	tr := ForeignTurn{StartNew: true, ForeignSID: "sid"}
	if !tr.StartNew || tr.ForeignSID != "sid" {
		t.Fatal("ForeignTurn fields not wired")
	}
}
```

**Step 2 — Run, expect FAIL** (`undefined: PermissionPosture`).

**Step 3 — Implement.** `pkg/foreignloop/foreignloop.go` — transcribe the design's interface block:

```go
package foreignloop

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/core/uuid"
)

// PermissionPosture is the typed, non-interactive permission mode passed to the
// foreign agent (no raw strings cross the boundary).
type PermissionPosture uint8

const (
	PostureDefault PermissionPosture = iota
	PostureAcceptEdits
)

// EventPublisher is the foreign loop's narrow consumer of the session event
// fan-in. Defined here (exported) because the native loop's equivalent is
// unexported; *session.Session satisfies it via PublishEvent.
type EventPublisher interface {
	PublishEvent(context.Context, event.Event) error
}

// ForeignAgent hides everything agent-specific (argv, system-prompt channel,
// stream framing, transcript layout). One implementation now: adapters/claude.
type ForeignAgent interface {
	Spawn(ctx context.Context, t ForeignTurn) (ForeignStream, error)
}

// ForeignTurn is one turn's input to an agent. The sid is ALWAYS set (minted at
// loop creation); StartNew selects --session-id (first turn) vs --resume.
type ForeignTurn struct {
	SystemPrompt string
	ForeignSID   string
	StartNew     bool
	Input        []content.Block
	Cwd          string
	Posture      PermissionPosture
}

// ForeignStream is the live decoded stream plus the deterministic transcript path.
type ForeignStream interface {
	Events() <-chan ForeignEvent
	TranscriptPath() string
	Close() error
}

// ForeignKind is the normalized event-union discriminant.
type ForeignKind uint8

const (
	ForeignInit ForeignKind = iota // carries the confirmed session id
	ForeignTextDelta
	ForeignThinkingDelta
	ForeignToolUse        // ToolUseID (string), ToolName
	ForeignToolResult     // ToolUseID (string), IsError, ResultPreview
	ForeignStepComplete   // an assistant round finished
	ForeignTerminalOK     // result success
	ForeignTerminalError  // result error / max-turns
)

// ForeignEvent is the small normalized union both decoders emit and the mapper
// consumes. Only the fields relevant to Kind are set.
type ForeignEvent struct {
	Kind          ForeignKind
	SessionID     string             // ForeignInit
	Text          string             // text/thinking delta
	ToolUseID     string             // tool_use / tool_result
	ToolName      string             // tool_use
	IsError       bool               // tool_result / terminal
	ResultPreview string             // tool_result
	Message       *content.AIMessage // ForeignStepComplete / ForeignTerminalOK (authoritative)
	ErrText       string             // ForeignTerminalError
}

// Builder is the composition-root seam Session uses to construct a foreign loop.
// EventPublisher is foreignloop.EventPublisher; returns the Backend and the minted
// ForeignSID (stamped onto LoopStarted by the caller).
type Builder func(loopCtx context.Context, sessionID, loopID uuid.UUID,
	parent loop.Provenance, pub EventPublisher, cfg loop.Config,
	idGen func() (uuid.UUID, error), fac *event.Factory) (loop.Backend, string, error)

// Spec is the per-agent foreign wiring resolved at the composition root. It is NOT
// on loop.Config (that would invert the package dependency).
type Spec struct {
	Agent    ForeignAgent
	ExecPath string
	Cwd      string
	Posture  PermissionPosture
	Env      []string // whitelisted child environment (NOT os.Environ())
}
```

`pkg/foreignloop/errors.go` — typed errors:

```go
package foreignloop

import "fmt"

type SpawnError struct{ Cause error }
func (e *SpawnError) Error() string { return "foreignloop: spawn: " + e.Cause.Error() }
func (e *SpawnError) Unwrap() error { return e.Cause }

type DecodeError struct{ Cause error }
func (e *DecodeError) Error() string { return "foreignloop: decode: " + e.Cause.Error() }
func (e *DecodeError) Unwrap() error { return e.Cause }

type ForeignExitError struct{ Code int }
func (e *ForeignExitError) Error() string { return fmt.Sprintf("foreignloop: agent exited %d", e.Code) }

type TranscriptUnavailableError struct{ Path string; Cause error }
func (e *TranscriptUnavailableError) Error() string { return "foreignloop: transcript unavailable: " + e.Path }
func (e *TranscriptUnavailableError) Unwrap() error { return e.Cause }

type ForeignSessionBusyError struct{ SID, Cwd string; PID int }
func (e *ForeignSessionBusyError) Error() string {
	return fmt.Sprintf("foreignloop: session %s busy (pid %d holds %s lock)", e.SID, e.PID, e.Cwd)
}

type ConfigError struct{ Field, Reason string }
func (e *ConfigError) Error() string { return "foreignloop: config: " + e.Field + ": " + e.Reason }
```

**Step 4 — Run, expect PASS.** `go test -race ./pkg/foreignloop/`.

**Step 5 — Commit.**
```bash
git add pkg/foreignloop/foreignloop.go pkg/foreignloop/errors.go pkg/foreignloop/foreignloop_test.go
git commit -m "feat(foreignloop): package types, normalized event union, typed errors"
```

### Task C2: `stream-json` decoder

**Files:**
- Create: `pkg/foreignloop/decode_stream.go`
- Create: `pkg/foreignloop/testdata/stream/*.jsonl` (fixtures)
- Test: `pkg/foreignloop/decode_stream_test.go`

**What to build:** `decodeStream(r io.Reader) (<-chan ForeignEvent, func() error)` — reads JSONL lines with `bufio.Scanner` (raise the buffer cap, lines can be long), decodes each `{"type":...}` object, maps to `ForeignEvent`, sends on the channel, closes on EOF. Unknown `type` values are **skipped** (validate-at-boundary). A malformed line yields a `*DecodeError` via the returned error func; the channel still closes.

Map (per design §Data flow): `system/init`→`ForeignInit{SessionID}`; partial assistant text/thinking→`ForeignTextDelta`/`ForeignThinkingDelta`; assistant `tool_use` block→`ForeignToolUse`; user `tool_result`→`ForeignToolResult`; assistant message complete→`ForeignStepComplete{Message}`; `result{subtype:success}`→`ForeignTerminalOK{Message}`; `result{subtype:error*}`→`ForeignTerminalError{ErrText}`.

**Step 1 — Write failing table test** with fixtures covering: happy multi-event stream; boundary (empty stream → just close); error (truncated/garbage line → `*DecodeError`, channel closes); edge (unknown `type` ignored; partial-message reassembly of `--include-partial-messages` deltas). Assert the produced `[]ForeignEvent` sequence per fixture.

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement** `decodeStream`. Use a typed intermediate struct for the line envelope (`type streamLine struct { Type string \`json:"type"\`; Subtype string; SessionID string \`json:"session_id"\`; Message json.RawMessage; ... }`) then narrow per `Type`. Never decode into `map[string]any` past this boundary.

**Step 4 — Run, expect PASS.** `go test -race ./pkg/foreignloop/ -run Stream`.

**Step 5 — Commit** `feat(foreignloop): stream-json decoder with fixtures`.

### Task C3: transcript decoder (version-tolerant, soft-degrade)

**Files:**
- Create: `pkg/foreignloop/decode_transcript.go`
- Create: `pkg/foreignloop/testdata/transcript/*.jsonl`
- Test: `pkg/foreignloop/decode_transcript_test.go`

**What to build:** `decodeTranscriptTail(path string, sinceTurn int) ([]content.AgenticMessages, error)` — opens the `<sid>.jsonl`, reads records, **allowlists** known `type`s (`assistant`, `user`) and ignores the rest (`mode`, `permission-mode`, `file-history-snapshot`, `attachment`, `last-prompt`, `queue-operation`, `ai-title`, …). Decodes `message.content[]` blocks (`text`/`thinking`/`tool_use`) into `content` blocks and `toolUseResult` into tool-result messages; skips `isSidechain:true` records (observed-only). **Soft-degrade:** any unrecognized/parse error on a line is logged + skipped, never fatal; a missing file returns `*TranscriptUnavailableError`.

Fixtures: a real-shaped transcript (use the schema confirmed in the design — `text`/`thinking`/`tool_use` blocks, `toolUseResult`, `isSidechain`), an unknown-type record (ignored), a truncated line (skipped), a sidechain record (skipped).

**Steps:** failing table test → FAIL → implement → PASS → commit `feat(foreignloop): version-tolerant transcript decoder (soft-degrade)`.

### Task C4: fuzz targets on both decoders

**Files:** `pkg/foreignloop/decode_fuzz_test.go`

```go
func FuzzDecodeStreamLine(f *testing.F) {
	f.Add([]byte(`{"type":"system","subtype":"init","session_id":"x"}`))
	f.Add([]byte(`{"type":"result","subtype":"success"}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		// must never panic; either yields events or a *DecodeError.
		_ = decodeStreamLine(line) // refactor a single-line helper out of decodeStream for fuzzing
	})
}
```

Add a sibling `FuzzDecodeTranscriptLine`. **Run:** `go test -run x -fuzz=FuzzDecodeStreamLine ./pkg/foreignloop -fuzztime=30s` then commit `test(foreignloop): fuzz both JSONL decoders`.

### Task C5: event mapper + id minting / correlation

**Files:**
- Create: `pkg/foreignloop/mapper.go`
- Test: `pkg/foreignloop/mapper_test.go`

**What to build:** a `mapper` struct holding the turn's `TurnID/StepID` (minted via the injected `idGen`) and a `map[string]uuid.UUID` (`foreignToolUseID → ToolExecutionID`). Method `toEvents(fe ForeignEvent) []event.Event` produces:
- `ForeignTextDelta`/`ThinkingDelta` → `event.TokenDelta{TurnIndex, Chunk}` (build a `content.Chunk` — read `pkg/content` for the text-chunk constructor).
- `ForeignToolUse` → mint a `ToolExecutionID`, store in the map, emit `event.ToolCallStarted{ToolExecutionID, ToolName}`.
- `ForeignToolResult` → look up the map, emit `event.ToolCallCompleted{ToolExecutionID, IsError, ResultPreview}`.
- `ForeignStepComplete` → `event.StepDone{Messages: AgenticMessages{Message}}`.
- `ForeignTerminalOK` → `event.TurnDone{TurnIndex, Message}`. `ForeignTerminalError` → `event.TurnFailed{TurnIndex, Err: &ForeignExitError{...}}`.
- `ForeignInit` → no event (assert sid match; the actor logs a mismatch).

> `TurnStarted` is emitted by the actor on command acceptance, NOT by the mapper (see D2). Usage is dropped (no event field).

**Steps:** failing table test (one row per `ForeignKind`, plus tool start/result correlation through the map) → FAIL → implement → PASS → commit `feat(foreignloop): ForeignEvent→event.Event mapper with id correlation`.

---

## Phase D — `pkg/foreignloop`: the actor (fake `ForeignAgent`, no subprocess)

Create a `fakeAgent` test helper implementing `ForeignAgent` that returns a scripted `ForeignStream` (a channel the test feeds) and a stub transcript path, plus a `fakePublisher` capturing published `event.Event`s. Mirror the fake patterns in `pkg/loop/fake_test.go`.

### Task D1: `New`, `Loop` satisfies `loop.Backend`, `Shutdown`

**Files:** `pkg/foreignloop/loop.go`, `pkg/foreignloop/loop_test.go`, `pkg/foreignloop/fake_test.go`

**What to build:** `func New(loopCtx, sessionID, loopID, parent, pub EventPublisher, cfg loop.Config, spec Spec, idGen func()(uuid.UUID,error), fac *event.Factory) (*Loop, string, error)` — validates (`cfg.Model.System` required → `*ConfigError`; `spec.Agent != nil`), mints the foreign sid (`idGen`), starts the actor goroutine, returns `(*Loop, sid, nil)`. `*Loop` exposes `Commands chan command.Command` + `Done chan struct{}` and the `Backend` methods (`CommandSink`/`DoneChan`/`Snapshot`). `Shutdown` (via command) cancels any in-flight turn (kill process group — stubbed by fake here) and closes `Done`.

Provide a thin package-level `Build` adapter matching `foreignloop.Builder` that wraps `New` (drops the spec resolution onto a closure the composition root supplies — see Phase F).

**Test:** `var _ loop.Backend = (*Loop)(nil)`; `New` with empty `Model.System` → `*ConfigError`; Shutdown closes `Done`; the returned sid is non-empty and stable.

**Commit:** `feat(foreignloop): actor constructor + Backend + Shutdown (fake agent)`.

### Task D2: `UserInput` happy path — `TurnStarted` before spawn, `StepDone`, `TurnDone`

**What to build:** on `command.UserInput`, the actor: (1) builds `*content.UserMessage` from the input blocks (`content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}}`, mirroring `turn.go:367`) and publishes `event.TurnStarted{TurnIndex, Message: user}` **before** calling `spec.Agent.Spawn`; (2) drains `ForeignStream.Events()` through the `mapper`, publishing each event; (3) at stream end reads the transcript tail to emit authoritative `StepDone`(s) then `TurnDone`. Enduring events are stamped via `fac.Stamp` at the publish chokepoint, fail-secure (mirror `loop.go:443`).

**Test (table):** assert published sequence `TurnStarted → … → StepDone → TurnDone`, and that `TurnStarted` carries the user message and is published *before* the fake's `Spawn` is invoked (fake records call order).

**Commit:** `feat(foreignloop): UserInput turn — TurnStarted before spawn, commit StepDone/TurnDone`.

### Task D3: spawn failure → `TurnFailed`; transcript-loss soft-degrade → `StepDone`+`TurnDone`

**What to build:** if `Spawn` returns error → publish `event.TurnFailed{Err: &SpawnError{...}}` (the `TurnStarted` already published the request). If the transcript read fails (`*TranscriptUnavailableError`) → emit a **synthetic** `StepDone` from the stream-accumulated assistant message, then `TurnDone` (so restore's `StepDone`-only fold keeps the assistant message).

**Test:** fake `Spawn` error → `TurnStarted` then `TurnFailed`; fake stream with assistant text + missing transcript → `StepDone`(synthetic) then `TurnDone`.

**Commit:** `feat(foreignloop): spawn-failure TurnFailed + transcript soft-degrade StepDone`.

### Task D4: `Interrupt` (process-group) → `TurnInterrupted`; no-op gate/`SubagentResult`

**What to build:** `command.Interrupt` cancels the turn ctx (the adapter signals the child's process group — fake records the cancel) and publishes `event.TurnInterrupted{TurnIndex}`; ack `true`. A `default:` case in the command loop logs-and-drops any command it cannot honor (gate answers, `SubagentResult`) without blocking.

**Test:** Interrupt during a (fake, blocked) turn → `TurnInterrupted` + ack true; sending an `ApproveToolCall`/`SubagentResult` does not deadlock and publishes nothing.

**Commit:** `feat(foreignloop): Interrupt→TurnInterrupted; drop un-honorable commands`.

### Task D5: per-spawn `(sid,cwd)` lock guard + `NewRestored`

**Files:** `pkg/foreignloop/lock.go`, `pkg/foreignloop/restored.go`, tests.

**What to build:**
- `lock.go`: acquire a per-`(sid,cwd)` lockfile recording the child PID before every spawn; if a live process holds it (PID alive), return `*ForeignSessionBusyError`. Use `os.OpenFile(..., O_CREATE|O_EXCL, ...)` + a liveness check (`syscall.Kill(pid, 0)`); stale locks (dead pid) are reclaimed. Released on turn end.
- `restored.go`: `func NewRestored(... seed RestoredForeign) (*Loop, error)` where `RestoredForeign{ForeignSID, TurnIndex, Msgs content.AgenticMessages}` comes up **idle** at `TurnIndex`, retains `Msgs` for `Snapshot`. `Snapshot` returns `(Msgs, TurnIndex, nil)`.

**Test:** a held live lock → next `UserInput` turn → `TurnFailed` wrapping `*ForeignSessionBusyError`; stale (dead-pid) lock is reclaimed and the turn proceeds; `NewRestored` `Snapshot` returns the seeded `(Msgs, TurnIndex)`.

**Commit:** `feat(foreignloop): per-spawn liveness lock + NewRestored`. Run `make secure` to close Phase D.

---

## Phase E — `adapters/claude`: real subprocess (pure parts unit-tested, spawn integration-tested)

### Task E1: argv builder (pure)

**Files:** `pkg/foreignloop/claude/args.go`, `pkg/foreignloop/claude/args_test.go`

**What to build:** `func buildArgs(t foreignloop.ForeignTurn, model string) []string` returning the argv (NOT a shell string): `-p`, `--output-format stream-json`, `--include-partial-messages`, `--verbose`, `--append-system-prompt <sys>`, `--model <model>`, `--permission-mode <posture>`, `--add-dir <cwd>`, and **either** `--session-id <sid>` (when `StartNew`) **or** `--resume <sid>`. `posture` maps from `PermissionPosture` via a typed switch (no raw strings leak in).

**Test (table):** StartNew vs resume produce the right session flag; posture enum → correct string; system prompt/model/cwd appear as **separate args** (assert slice membership, never concatenation).

**Commit:** `feat(foreignloop/claude): argv builder (table-tested)`.

### Task E2: transcript path derivation + path safety

**Files:** `pkg/foreignloop/claude/transcript.go`, `..._test.go`

**What to build:** `func transcriptPath(home, cwd, sid string) (string, error)` → `<home>/.claude/projects/<encoded-cwd>/<sid>.jsonl` where `encoded-cwd` replaces `/` (and other separators) per Claude's scheme (confirmed: `/Users/x/y` → `-Users-x-y`). `filepath.Clean` cwd; reject a `sid` that is not a plain uuid (no separators/`..`); verify the final path stays within `<home>/.claude/projects` (defeat traversal).

**Test (table):** known cwd+sid → expected path; `sid` containing `../` or `/` → typed error; cwd with trailing slash normalizes.

**Commit:** `feat(foreignloop/claude): transcript path derivation + traversal guard`.

### Task E3: env whitelist builder

**Files:** `pkg/foreignloop/claude/env.go`, `..._test.go`

**What to build:** `func whitelistEnv(parent []string, allow []string, extra map[string]string) []string` — returns only `allow`-listed keys from `parent` (PATH, HOME, TERM, …) plus `extra` (the one credential). Never returns `os.Environ()` wholesale.

**Test:** a parent env with a stray `SECRET_TOKEN` is excluded; allow-listed keys pass; `extra` credential is appended.

**Commit:** `feat(foreignloop/claude): env whitelist for child process`.

### Task E4: `claude` adapter `Spawn` + process group + integration test

**Files:** `pkg/foreignloop/claude/claude.go`, `pkg/foreignloop/claude/claude_integration_test.go` (`//go:build integration`)

**What to build:** `type Agent struct { ExecPath, Home string; Env []string }` implementing `foreignloop.ForeignAgent`. `Spawn` builds argv (E1), sets `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` (own process group), `cmd.Env = whitelistEnv(...)`, `cmd.Dir = t.Cwd`, pipes stdout into `decodeStream` (C2), returns a `ForeignStream` whose `TranscriptPath()` is from E2 and whose `Close()` signals the **process group** (`syscall.Kill(-pgid, SIGINT)` then SIGKILL after grace). Spawn errors → `*SpawnError`; non-zero exit surfaced as `*ForeignExitError`.

**Integration test (`//go:build integration`, skip if `claude` not on PATH):** spawn a real `claude -p --session-id <uuid> …` with a trivial prompt; assert a terminal event arrives and the transcript file exists at the derived path; then a second `Spawn` with `StartNew:false --resume <uuid>` continues the same session (assert continuity). **This test also pins** the `--verbose` requirement and the chosen `--permission-mode` value (per the design's "Implementation must pin").

**Run:** `go test -race ./pkg/foreignloop/claude/` (unit) and `go test -tags integration -race ./pkg/foreignloop/claude/` (integration).

**Commit:** `feat(foreignloop/claude): Spawn with process group + resume integration test`. Run `make secure`.

---

## Phase F — `pkg/session`: wire the backend in (migration + branch + restore + fingerprint)

### Task F1: migrate `loopHandle.loop` → `loop.Backend` across Session

**Files:** Modify `pkg/session/session.go`; tests stay green.

**Step 1 — Lean on existing tests.** Run `go test -race ./pkg/session/` first to capture green baseline.

**Step 2 — Refactor (mechanical, no behavior change).** Change `loopHandle.loop *loop.Loop` → `backend loop.Backend`. Update every audited site to use the interface methods: `.loop.Commands` → `.backend.CommandSink()`, `.loop.Done` → `.backend.DoneChan()`, `.loop.Snapshot` unchanged. Sites (from the design audit): `loopHandle` (252), `deliverSubagentResult` (389), `loopFor` return type (623), `interruptLoop` (787), `interruptTarget`/`shutdownTarget` structs (983/1083), `resolveGate` return type (1258), `routeGate` param (1283), `submitToLoop` (862), and the `loop.New` assignment (531). `loopFor`/`resolveGate` now return `loop.Backend`.

**Step 3 — Run, expect PASS** (`go test -race ./pkg/session/`). No new test needed — the existing suite guards the refactor. If any test referenced `*loop.Loop` concretely, update it to the interface.

**Step 4 — Commit** `refactor(session): drive loops through loop.Backend (no behavior change)`.

### Task F2: `WithForeignBuilder` option + `newLoop` Engine switch + `ForeignSID` stamping

**Files:** Modify `pkg/session/command_journal.go` (option), `pkg/session/session.go` (`foreignBuild` field, `newLoop` switch, `LoopStarted.ForeignSID` stamp); test `pkg/session/foreign_newloop_test.go`.

**Step 1 — Write failing test.** Construct a Session with `WithForeignBuilder(fakeBuilder)` where the fake returns a fake `loop.Backend` + a fixed sid. Spawn a loop with `cfg.Engine = loop.EngineForeignClaude`; assert: the published `LoopStarted` carries `ForeignSID == fixedSID`; a foreign cfg with **no** builder wired → typed error (fail-closed); a native cfg still routes to `loop.New` and leaves `ForeignSID` empty.

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement:**
- `WithForeignBuilder(b foreignloop.Builder) Option` setting `s.foreignBuild`.
- In `newLoop`, replace the bare `loop.New(...)` with:
  ```go
  var b loop.Backend
  var foreignSID string
  switch cfg.Engine {
  case loop.EngineNative:
      b, err = loop.New(loopCtx, s.SessionID, loopID, parent, s, cfg)
  default:
      if s.foreignBuild == nil {
          return uuid.UUID{}, &SessionError{Kind: SessionForeignBuilderMissing}
      }
      b, foreignSID, err = s.foreignBuild(loopCtx, s.SessionID, loopID, parent, s, cfg, uuid.New, s.eventFactory)
  }
  ```
- Stamp `ev := event.LoopStarted{Header: startedHeader, ParentToolUseID: parentToolUseID, ForeignSID: foreignSID}` and store `&loopHandle{backend: b, ...}`.
- Add `SessionForeignBuilderMissing` to the `SessionErrorKind` set.

**Step 4 — Run, expect PASS.**

**Step 5 — Commit** `feat(session): WithForeignBuilder + newLoop Engine switch + ForeignSID stamp`.

### Task F3: restore branch on `Engine` + SID recovery + fail-closed

**Files:** Modify `pkg/session/restore_constructor.go` (the `loop.NewRestored` site ~329) and the fold in `pkg/session/restore.go` (read `LoopStarted.ForeignSID`); test `pkg/session/foreign_restore_test.go`.

**Step 1 — Write failing test.** Build a journal whose root `LoopStarted` carries `ForeignSID`. Restore with `WithForeignBuilder(fakeBuilder)` + a foreign `cfg.Engine`; assert the loop comes up idle at the recovered `TurnIndex`, `Snapshot` returns the folded `Msgs`, and the fake builder received the recovered sid. Add a row: foreign `LoopStarted` with **empty** `ForeignSID` → restore fails closed (typed error).

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement:** in the fold, surface `LoopStarted.ForeignSID` (e.g. on `foldResult`); branch the restore construction:
```go
switch cfg.Engine {
case loop.EngineNative:
    l, err = loop.NewRestored(loopCtx, sessionID, primaryLoopID, s, cfg, loop.RestoredState{Msgs: folded.Msgs, TurnIndex: folded.TurnIndex})
default:
    if folded.ForeignSID == "" { return nil, &RestoreError{Kind: RestoreForeignSIDMissing} }
    if s.foreignBuildRestored == nil { return nil, &RestoreError{Kind: RestoreForeignBuilderMissing} }
    l, err = s.foreignBuildRestored(... RestoredForeign{ForeignSID: folded.ForeignSID, TurnIndex: folded.TurnIndex, Msgs: folded.Msgs})
}
```
(Reuse the same `WithForeignBuilder`-set builder; expose a restored-builder variant in `foreignloop` or pass a `restored bool`.) Add the `RestoreErrorKind`s.

**Step 4 — Run, expect PASS.**

**Step 5 — Commit** `feat(session): restore foreign loops by LoopStarted.ForeignSID (fail-closed on empty)`.

### Task F4: config fingerprint — foreign behavior inputs

**Files:** Modify `pkg/session/config_fingerprint.go` (+ `command_journal.go` if extending `ConfigFingerprintFields`); test `pkg/session/foreign_fingerprint_test.go`.

**What to build:** fold the foreign `Spec.Cwd` into the existing `WorkspaceRoot` fingerprint field, and add adapter-identity + posture to `ConfigFingerprintFields` (new fields) so a mid-session change of cwd/adapter/posture is detected as `ConfigMismatchError` on restore. **Do not** fingerprint `ExecPath`/version or `Spec.Env` (permitted to drift — log only).

**Test (table):** same foreign spec → identical fingerprint; changed cwd/posture/adapter → different fingerprint; changed exec path → **same** fingerprint.

**Commit** `feat(session): fingerprint foreign cwd/adapter/posture (exec/env permitted to drift)`.

### Task F5: end-to-end — foreign loop as primary AND as subagent

**Files:** `pkg/session/foreign_e2e_test.go`

**What to build:** with a fake `ForeignAgent` (scripted stream) wired through `WithForeignBuilder`:
1. **Primary foreign:** `New(ctx, cfg{Engine: EngineForeignClaude, Model.System: "..."}, WithForeignBuilder(...))`, `Submit` a turn, drain events, assert `TurnStarted→…→TurnDone` and the journaled `LoopStarted.ForeignSID`.
2. **Foreign subagent:** a native primary whose `RunSubagent` is called with a foreign `cfg`; assert the subagent runs to a final string (drain-to-final-text), provenance/lineage recorded, and looprig depth/quota caps still apply (the foreign subagent counts as one leaf loop).

**Run:** `go test -race ./pkg/session/`. **Commit** `test(session): e2e foreign loop as primary and as subagent`. Run `make secure`.

---

## Phase G — composition root wiring (CLI) — minimal, behind a flag

### Task G1: wire a foreign builder at the CLI composition root

**Files:** the CLI factory (find with `grep -rn "session.New(" cmd/ pkg/cli`); add a builder that resolves a `foreignloop.Spec` (claude adapter, cwd/worktree, posture, whitelisted env from a curated allowlist) and passes `WithForeignBuilder`. Gate behind an explicit config/flag so default behavior is unchanged (native).

**Steps:** wire it; manual smoke via the `/run` skill or a CLI invocation with the foreign engine selected on one agent; confirm a real `claude`-backed turn streams into the TUI/journal. **Commit** `feat(cli): optional foreign-loop backend wiring (claude), default off`.

> The swarm-catalog mapping (agent name → `cfg.Engine` + `Spec`) lands in the `swe-swarm` branch; this plan delivers the seam it plugs into.

---

## Done criteria

- `go test -race ./...` green; `go test -tags integration -race ./pkg/foreignloop/...` green where `claude` is installed.
- `make secure` clean.
- A foreign-backed session (primary and subagent) runs a turn end-to-end, survives restart via `--resume`, and fails closed when a stale child holds the `(sid,cwd)` lock.
- `pkg/loop` diff is additive only (Engine, Backend, two accessors, one `LoopStarted` field); no native behavior changed.
