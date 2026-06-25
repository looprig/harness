# Foreign Loop Backend Design

**Date:** 2026-06-25
**Status:** Draft

## Motivation

Today every agent turn runs on our own engine: `pkg/loop` is an actor that owns
the turn lifecycle, drives the `llm.LLM` provider interface, executes tools,
enforces permission gates, and emits a typed `event.Event` stream. That is the
right design when *we* own inference and tooling.

But mature coding agents — Claude Code, Codex, OpenCode — already ship their own
complete agentic loops in headless mode: their own tools, model calls, permission
models, and durable per-session memory. We want to **onboard those agents as an
alternative loop backend**: hand a foreign agent a user request plus our system
prompt, let it run its own loop to completion, mirror what it does back into our
event stream and journal, track it by *its* session id, and send follow-ups to
the same session.

The key realization is that the integration seam is **not** the single-inference
`llm.LLM` interface. A foreign agent runs a whole loop, not one completion. The
seam is the **turn/session level** — the same level `pkg/loop` occupies. So the
foreign loop is a sibling of `pkg/loop`, not a sibling of an `llm` provider.

## Scope

**In:**

- A `Backend` interface (defined in `pkg/session`, the consumer) that both the
  native `*loop.Loop` and a new `*foreignloop.Loop` satisfy.
- A new `pkg/foreignloop` package: a loop actor with the native loop's handle
  shape, whose runner spawns a foreign agent subprocess per turn instead of
  calling an LLM and running tools.
- A generalizing `ForeignAgent` adapter interface, with **one** implementation
  now: a Claude Code adapter (`claude -p` headless).
- Two JSONL decoders — a live `stream-json` decoder and a version-tolerant
  on-disk transcript decoder — both normalizing to a small `ForeignEvent` union,
  and a mapper from `ForeignEvent` to `event.Event`.
- Durable continuity via the foreign session id, recorded in our journal.
- The `pkg/session` seam that makes a foreign agent spawnable as **either the
  primary loop or a subagent**: the `Engine` switch in `newLoop` and
  `loopHandle.backend`.

**Out (deferred):**

- The swarm-catalog wiring that maps a named agent to `cfg.Engine` (lands in the
  swarm package; this plan delivers the seam it plugs into).

- Codex and OpenCode adapters (the interface is designed for them; they are
  later work).
- Routing foreign tool calls back through our `PermissionGate` (we observe, we do
  not gate — see Trust Boundary).
- A long-lived foreign process fed turns over stdin (we use one-shot-per-turn).
- Using a foreign agent as an inference-only provider (that would be a foreign
  *provider*, not a foreign *loop*).

## Decisions

These six decisions were settled during design and frame everything below.

1. **Pluggable backend.** The native `pkg/loop` stays as one implementation; the
   foreign loop is a sibling satisfying the same session/turn contract. Session,
   hub, journal, TUI, and the `event.Event` taxonomy are unchanged.
2. **Foreign owns tools; we observe.** The foreign agent runs its own tools. We
   constrain only via its own CLI flags (`--permission-mode`, `--allowedTools`,
   `--add-dir`) and *observe* the resulting tool events to mirror them into our
   stream. Our `PermissionGate` is not in the foreign loop.
3. **Native per-agent system prompt.** Each adapter delivers our system prompt
   through the agent's real system-prompt channel (Claude:
   `--append-system-prompt`), not by wrapping it into a user message.
4. **Claude Code first, interface designed to generalize.** Build the Claude
   adapter end-to-end against a `ForeignAgent` interface; Codex/OpenCode follow.
5. **Foreign session id is the durable handle.** We journal the foreign session
   id plus the adapted event stream. The foreign agent owns the real
   conversation; follow-up = resume by id; restore = reload the id and resume.
6. **One-shot subprocess per turn.** Each turn spawns a fresh `claude -p --resume
   <sid>`, streams to completion, and exits. Maps 1:1 onto our single-flight
   turn model. Interrupt = signal/kill the child.

## Architecture

```text
            ┌──────────────── import direction (acyclic, verified) ────────────────┐
            │                                                                       │
        pkg/session ──► pkg/loop ◄── pkg/foreignloop                                │
            │  (branch)    │  (defines Backend, Engine)   │                         │
            │              ▲                               │                        │
            │              └──────── implements ───────────┘                        │
            │                                                                        │
            └─ session.newLoop: switch cfg.Engine { native → loop.New; foreign → foreignloop.New }
                                 both return loop.Backend; stored in loopHandle.backend

        pkg/foreignloop.Loop ──► ForeignAgent (interface)
                                     ├─ adapters/claude   (now)
                                     ├─ adapters/codex    (later)
                                     └─ adapters/opencode (later)
```

`Session` currently holds a concrete `*loop.Loop` (`loopHandle.loop`) and drives
it purely through its `Commands chan command.Command` and `Done` channel,
publishing `event.Event` to the hub. We introduce a narrow **`loop.Backend`
interface** that both engines satisfy, change `loopHandle.loop *loop.Loop` to
`loopHandle.backend loop.Backend`, and select the engine at the single
construction chokepoint.

**Where the interface lives — `pkg/loop`, not `pkg/session`.** The import graph is
one-way `session → loop`, and `pkg/loop` is a leaf (imports nothing above it).
`pkg/foreignloop` must import `pkg/loop` for `loop.Config`, `loop.Provenance`, and
`loop.Snapshot`. So `loop` is the only package both engines can depend on — it
owns `Backend`. (If `Backend` lived in `session`, `loop.New` could not return it
without a `session → loop → session` cycle.)

**Where the branch lives — `session.newLoop`, not `loop.New`.** There is exactly
one `loop.New` call site in the tree (`pkg/session/session.go:531`, inside
`newLoop`), and `newLoop` is the sole core behind the primary loop
(`newSession → NewLoop`), model-driven subagents (`NewLoop`), and swarm subagents
(`RunSubagent`). Branching there makes every loop native-or-foreign in one place.
The branch cannot live in `loop.New`: that would force `loop → foreignloop` while
`foreignloop → loop`, a compile-breaking cycle, and would put a composition
decision inside a low-level engine (violating "wire at the composition root").

**No separate `BackendFactory` type.** The `switch cfg.Engine` in `newLoop` *is*
the factory (YAGNI). A named factory interface is extracted only if/when the swarm
needs to inject custom builders (e.g. test doubles).

### The `loop.Backend` interface

`Backend` is the minimal subset of the loop contract Session actually uses —
command submission, completion signalling, and the committed-state snapshot used
for restore verification. It deliberately excludes the native loop's internal
seams (gate registration, per-step commit/drain handshakes) that a foreign loop
has no analogue for.

```go
// In pkg/loop. Both *loop.Loop and *foreignloop.Loop satisfy it.
type Backend interface {
    CommandSink() chan<- command.Command        // Session submits UserInput/Interrupt/Shutdown
    DoneChan() <-chan struct{}                   // closed when the actor has fully exited
    Snapshot(ctx context.Context) (Snapshot, error) // committed state for restore verification
}

// Engine selects which backend newLoop builds. Zero value = native.
type Engine uint8
const (
    EngineNative Engine = iota
    EngineForeignClaude
    // EngineForeignCodex, EngineForeignOpenCode — later
)
```

`Engine` is a new field on `loop.Config` (zero value `EngineNative`, so existing
construction is unchanged). `*loop.Loop` already has `Snapshot`; it gains two
**additive** accessor methods — `CommandSink()` returning its existing `Commands`
field and `DoneChan()` returning `Done` — so it satisfies `Backend` despite those
being struct fields. No behavior changes; nothing else in `pkg/loop` moves.
`loop.New` keeps its body and either returns `*loop.Loop` (which `newLoop` assigns
to a `loop.Backend`) or, for explicitness, returns `loop.Backend`.

## The foreign loop actor (`pkg/foreignloop`)

A `*foreignloop.Loop` is an actor with the native loop's handle shape
(`Commands`, `Done`) and the same command/event discipline, so Session cannot
tell it apart from a native loop:

- **`UserInput`** → run one foreign turn: spawn/resume the subprocess, decode its
  stream, publish live events, then commit the authoritative result from the
  transcript, then record the foreign session id.
- **`Interrupt`** → SIGINT the child, then kill after a short grace; publish
  `TurnInterrupted`.
- **`Shutdown`** → finish or abort the in-flight turn, then close `Done`.
- It has **no permission gates** (tools run inside the foreign agent), so it never
  emits `PermissionRequested` and never receives `ApproveToolCall`/`DenyToolCall`.

### The `ForeignAgent` adapter interface

This is the generalizing seam beneath the actor. It hides everything
agent-specific (argv construction, system-prompt channel, stream framing, on-disk
transcript layout):

```go
type ForeignAgent interface {
    // Spawn starts (ForeignSID == "") or resumes one foreign turn and returns a
    // live stream plus the path to the agent's durable transcript for this session.
    Spawn(ctx context.Context, t ForeignTurn) (ForeignStream, error)
}

type ForeignTurn struct {
    SystemPrompt string              // delivered via the agent's native channel
    ForeignSID   string              // "" => new session (we mint the id); else resume
    Input        []*content.Block    // the user turn
    ToolPolicy   ForeignToolPolicy   // allowedTools, permission-mode, add-dir
    Cwd          string              // working directory (also keys the transcript path)
}

type ForeignStream interface {
    Events() <-chan ForeignEvent     // normalized, decoded live events
    TranscriptPath() string          // deterministic on-disk transcript for committed reads
    Close() error
}
```

`ForeignEvent` is a small normalized union (init, text/thinking delta, tool-use,
tool-result, step-complete, terminal{ok|error}) that both decoders emit and the
mapper consumes — neither the actor nor the mapper depends on Claude-specific
shapes.

## Swarm / subagent integration

Because every loop — primary, model-driven subagent, and swarm subagent — funnels
through the single `newLoop` chokepoint, a foreign-backed agent becomes spawnable
as a subagent for **free**, with no change to the model-facing contract:

```text
model: Subagent{agent:"claude-coder", message:"…"}        (UNCHANGED)
  -> Spawner.Spawn(parent, "claude-coder", msg)            (UNCHANGED — narrow iface)
  -> swarm resolves the name in its registry catalog        (entry now carries Engine)
  -> Session.RunSubagent(parent, cfg, blocks)              (UNCHANGED signature)
  -> newLoop -> switch cfg.Engine { native | foreign }      (the one new branch)
```

The `Subagent` tool depends only on the narrow `Spawner` interface and a typed
`identity.AgentName`; it never sees the engine. The swarm's agent definition
(which already yields a `loop.Config`) just sets `cfg.Engine` and the foreign
bits (tool policy, cwd, model alias). Everything Session expresses in terms of
commands + events keeps working with a foreign backend in the `loopHandle`:
provenance/lineage, depth & quota caps, Interrupt/Shutdown fan-out, event
publishing, agent-card rendering, and `RunSubagent`'s drain-to-final-text (the
foreign loop's `TurnDone` result text *is* the returned string).

Two semantic boundaries this creates, stated deliberately:

- **A foreign subagent is a leaf in looprig's loop tree.** It runs its own loop
  with its own tools and does not carry looprig's `Subagent` tool, so it cannot
  spawn looprig subagents. If it fans out internally (Claude Code's own
  Task/sidechains — `isSidechain` in the transcript), that fan-out is
  observed-only and sits *outside* looprig's depth/quota accounting. Our caps
  bound our tree; the foreign agent governs its own.
- **Concurrent foreign subagents need cwd isolation.** Each is its own subprocess
  with its own minted sid, so concurrency is safe — but two coding agents editing
  the same `cwd` will clobber each other. A foreign subagent should therefore get
  a per-subagent working dir (a git worktree, which the swarm already uses), as a
  policy knob on the agent definition.

The swarm-catalog wiring itself (mapping a catalog entry to `cfg.Engine`) lands in
the swarm package (currently the `swe-swarm` branch); this design and its plan
cover the `pkg/loop` + `pkg/session` + `pkg/foreignloop` seam that makes it
possible.

## Two-source continuity model

Claude Code exposes two complementary representations of a turn, and we use both,
mirroring our own loop's split between ephemeral live events and enduring
committed events:

| Source | Role | Drives |
|---|---|---|
| Live `stream-json` stdout (`--output-format stream-json --include-partial-messages`) | Real-time, documented contract | `TurnStarted`, `TokenDelta`, observed `ToolCallStarted`/`ToolCallCompleted` |
| On-disk transcript `~/.claude/projects/<cwd→->/<sid>.jsonl` | Authoritative, durable record | Committed `StepDone`/`TurnDone` (exact content + usage), restore/replay of full scrollback, subagent sidechains |

We **mint** the foreign session id with `--session-id <uuid>` (we already have
`pkg/uuid`). Because we own both the `cwd` and the `sid`, the transcript path is
deterministic and known *before* the process starts — so the durable handle is
journaled up front, with no window where a crash loses it and no need to scrape
the id out of stdout.

The transcript records map almost 1:1 onto our model: assistant
`message.content[]` block types are exactly `text` / `thinking` / `tool_use`
(matching `pkg/content`), `toolUseResult` carries structured tool output,
`usage`/`model`/`stop_reason` sit on each message, and `isSidechain` marks
subagent runs.

**Robustness rule:** the transcript schema is internal and version-drifts across
Claude Code releases. So `stream-json` is the *primary* contract; the transcript
decoder is a *version-tolerant, validate-at-boundary* enhancement that allowlists
the record types it understands and **degrades soft** — on any parse failure it
falls back to a `stream-json`-derived terminal plus the foreign-sid handle, never
crashing and never trusting an unrecognized format.

## Data flow — one turn

```text
Session.StartTurn(input)
  -> foreignloop actor receives UserInput
  -> mint sid (first turn) or reuse journaled sid
  -> ForeignAgent.Spawn:
       claude -p --resume <sid> \
         --output-format stream-json --include-partial-messages \
         --append-system-prompt <sys> \
         --permission-mode <mode> --allowedTools <...> --add-dir <cwd> \
         --model <model>
  -> decode stdout JSONL -> ForeignEvent -> event.Event (publish live)
  -> process exits
  -> read transcript tail -> emit committed StepDone/TurnDone (authoritative)
  -> record foreign_sid in journal
```

Event mapping:

| Foreign | looprig event |
|---|---|
| `system`/`init` (session_id, tools, model) | `TurnStarted` (+ confirm minted sid) |
| partial assistant text/thinking delta | `TokenDelta` |
| assistant `tool_use` block | `ToolCallStarted` (observed) |
| user `tool_result` / `toolUseResult` | `ToolCallCompleted` (observed) |
| assistant round complete | `StepDone` (committed from transcript) |
| `result` `{success, usage}` | `TurnDone` (+ usage) |
| `result` `{error, error_max_turns, …}` | `TurnFailed` |
| child killed via Interrupt | `TurnInterrupted` |

## Error handling

Every distinct failure mode is a typed error (`SpawnError`, `DecodeError`,
`ForeignExitError{Code}`, `TranscriptUnavailableError`), inspectable via
`errors.As`. Non-zero subprocess exit or a malformed terminal record →
`TurnFailed`. Transcript missing or unparseable → log a security/observability
event and fall back to the stream-derived `TurnDone` (soft-degrade). Interrupt is
SIGINT followed by kill-after-grace. The turn ctx carries a deadline; an
unresponsive child is killed when it elapses.

## Security

- **Least privilege:** constrain the foreign agent via `--permission-mode` /
  `--allowedTools` / `--add-dir`. Never pass `--dangerously-skip-permissions`.
- **No shell string:** spawn via `exec.CommandContext` with an **argv list**, not
  `sh -c`. The system prompt, model, and tool lists are separate args.
- **Path safety:** `cwd` and `sid` are validated and `filepath.Clean`'d, and the
  derived transcript path is verified to stay within the expected
  `~/.claude/projects` root before opening (defeats traversal via a crafted sid).
- **Secrets:** any foreign API key/credential is passed through the child's
  environment, never logged and never placed on the command line (argv is visible
  in `ps`).
- **Validate at boundary:** every JSONL line (live and transcript) is validated;
  unknown record types are ignored, not trusted.

## Testing

- **Table-driven decoder tests** over canned `stream-json` and transcript
  fixtures: happy path, boundary (empty turn, zero tools), error
  (`result` error subtypes, truncated JSONL), edge (unknown record type,
  sidechain records, partial-message reassembly).
- **Fuzz target** on both JSONL decoders (external, untrusted input).
- **Actor tests** for `foreignloop` use a fake `ForeignAgent` — no subprocess in
  unit tests; assert the command/event contract (UserInput→events→terminal,
  Interrupt→TurnInterrupted, Shutdown→Done).
- **Integration tests** (`//go:build integration`) that actually spawn
  `claude -p` against a recorded prompt and assert sid continuity across a
  follow-up `--resume`.
- All tests run under `-race`.

## Resolved seam decisions

- **Interface placement:** `Backend` + `Engine` live in `pkg/loop` (the leaf both
  engines import); not in `pkg/session`.
- **Selection point:** a `switch cfg.Engine` in `session.newLoop` (the one
  `loop.New` call site, shared by primary + all subagents). No `BackendFactory`
  type; no `nativeBackend` wrapper.
- **`pkg/loop` change is additive only:** `Backend`/`Engine` declarations, an
  `Engine` field on `Config`, and `CommandSink()`/`DoneChan()` accessor methods on
  `*loop.Loop`.
- **`loopHandle`:** `loop *loop.Loop` becomes `backend loop.Backend`.

## Open questions for the implementation plan

- Whether `Snapshot` for a foreign loop reconstructs from the transcript or
  returns a minimal "sid + last terminal" view.
- Exact `ForeignToolPolicy` shape and its mapping to Claude flags.
- Whether `loop.New` changes its return type to `loop.Backend` or keeps
  `*loop.Loop` (assigned to the interface at the `newLoop` call site).
- Journaling the foreign sid: new typed event/journal field vs. piggybacking on an
  existing record.
