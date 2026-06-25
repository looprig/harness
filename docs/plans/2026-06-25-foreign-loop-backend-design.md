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

- A `Backend` interface (defined in `pkg/loop`, the leaf both engines import) that both the
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
   hub, journal, and TUI are unchanged; the `event.Event` taxonomy gains exactly
   **one additive** type — `ForeignSessionStarted` — and nothing else changes.
2. **Foreign owns tools; we observe.** The foreign agent runs its own tools. We
   set only a non-interactive `--permission-mode` (so a headless agent never hangs)
   and `--add-dir <cwd>`, and *observe* the resulting tool events to mirror them into our
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

**Migration is wider than `loopHandle` — every `*loop.Loop` reference in Session
becomes `loop.Backend`.** Audited sites in `pkg/session/session.go`:
`loopHandle.loop` (252), `deliverSubagentResult` (389), `loopFor` return type
(623), `interruptLoop` (787), the `interruptTarget`/`shutdownTarget` structs
(983/1083), `resolveGate` return type (1258), and `routeGate` parameter (1283),
plus `submitToLoop` (862). All of these touch the loop only through `.Commands`
(send), `.Done` (receive), or `.Snapshot` — verified by reading `resolveGate`
(it just looks the loop up and builds a `GateRoute`) and `routeGate` (it only
sends `cmd` to `.Commands` and selects on `.Done`). No native-only gate method is
called, so the 3-method `Backend` below covers every path. Restore's
`restore_constructor.go:329` call to `loop.NewRestored` is a separate site (see
Restore).

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
    CommandSink() chan<- command.Command  // Session submits UserInput/Interrupt/Shutdown
    DoneChan() <-chan struct{}            // closed when the actor has fully exited
    // Snapshot returns committed state for restore verification + dormant reads.
    // EXACT signature matches the existing *loop.Loop.Snapshot (restored.go:69) —
    // no new Snapshot type is introduced.
    Snapshot(ctx context.Context) (content.AgenticMessages, event.TurnIndex, error)
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
construction is unchanged). `*loop.Loop` already has `Snapshot` with this exact
signature; it gains two **additive** accessor methods — `CommandSink()` returning
its existing `Commands` field and `DoneChan()` returning `Done` — so it satisfies
`Backend` despite those being struct fields. No behavior changes; nothing else in
`pkg/loop` moves. `loop.New` keeps its body and either returns `*loop.Loop` (which
`newLoop` assigns to a `loop.Backend`) or, for explicitness, returns
`loop.Backend`. The foreign actor accepts the same `command.Command` set but
**no-ops commands it cannot honor** (gate answers, `SubagentResult`): they can
never legitimately arrive (a foreign loop emits no `PermissionRequested` and
spawns no looprig subagents), and a `default:` case logs-and-drops them so the
actor never deadlocks — honoring the "accept commands" contract (Liskov).

## The foreign loop actor (`pkg/foreignloop`)

A `*foreignloop.Loop` is an actor satisfying `loop.Backend` (its `CommandSink()`
and `DoneChan()` expose the same command/`Done` discipline) so Session cannot tell
it apart from a native loop.

**Creation / sid journaling — Session publishes, not the actor.** Today
`session.newLoop` (not the loop actor) builds and publishes `LoopStarted`
(`session.go:575`). We keep that ownership: `foreignloop.New` **mints the per-loop
sid** and returns it as metadata; `newLoop`, after registration, publishes
`LoopStarted` and then `ForeignSessionStarted{LoopID, AgentName, ForeignSID, Cwd}`
in that order. The actor never publishes lifecycle events itself.

Per command:

- **`UserInput`** → (1) commit the user message into the turn and publish
  `TurnStarted{Message: user}` **before** any spawn — exactly as the native loop
  does (`turn.go:46`), so a spawn failure still leaves the user request journaled,
  followed by `TurnFailed`. (2) Spawn/resume the subprocess, decode its stream,
  publish live events. (3) Commit the authoritative result (assistant messages via
  `StepDone`) from the transcript. The sid is already journaled — not re-recorded.
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
    // Spawn runs one foreign turn and returns a live stream plus the path to the
    // agent's durable transcript. The sid is ALWAYS present (minted at loop
    // creation); StartNew distinguishes the first turn (--session-id <sid>) from
    // a resume (--resume <sid>).
    Spawn(ctx context.Context, t ForeignTurn) (ForeignStream, error)
}

type ForeignTurn struct {
    SystemPrompt string            // delivered via the agent's native channel
    ForeignSID   string            // always set (minted at creation)
    StartNew     bool              // true => --session-id <sid> (first turn); false => --resume <sid>
    Input        []content.Block   // the user turn (content.Block is a sealed interface; not a pointer)
    Cwd          string            // working directory (also keys the transcript path)
    Posture      PermissionPosture // typed; the single non-interactive --permission-mode value (v1)
    // NOTE: tool-capability restriction (--tools / --allowedTools) is parked — no
    // policy field in v1 (observe-only boundary). Add one when a real need arises.
}

// PermissionPosture is the typed, validated non-interactive permission mode (no
// raw strings crossing the boundary). Exactly one value is used in v1; the set is
// pinned by the integration test against the installed CLI.
type PermissionPosture uint8
const (
    PostureDefault PermissionPosture = iota // "default"
    PostureAcceptEdits                       // "acceptEdits"
    // PostureDontAsk … — added only if the pinned posture changes
)

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
| On-disk transcript `~/.claude/projects/<cwd→->/<sid>.jsonl` | Authoritative, durable record | Committed `StepDone`/`TurnDone` (exact content; usage deferred — see mapping), restore/replay of full scrollback, subagent sidechains |

### Foreign session id: minted and journaled at loop creation (resolved)

A foreign loop corresponds to exactly **one** Claude session, so its `sid` is a
**per-loop** value, not per-turn. We **mint** it with `pkg/uuid` at foreign-loop
construction and pass it via `--session-id <uuid>` on the first turn (and
`--resume <uuid>` thereafter). Because we own both the `cwd` and the `sid`, the
transcript path is deterministic and known before any process starts.

The sid is journaled **once, up front**, via a new typed enduring event
`ForeignSessionStarted{LoopID, AgentName, ForeignSID, Cwd}`, emitted by the
foreign actor immediately after its `LoopStarted` — before the first spawn. This
is the single source of truth for the handle: there is no window where a crash
loses it, and nothing is scraped from stdout. (The per-turn data flow below does
**not** re-journal the sid; turn end only commits the turn's *messages*.) Restore
recovers the sid by folding this event during replay (see Restore).

The transcript records map almost 1:1 onto our model: assistant
`message.content[]` block types are exactly `text` / `thinking` / `tool_use`
(matching `pkg/content`), `toolUseResult` carries structured tool output,
`usage`/`model`/`stop_reason` sit on each message, and `isSidechain` marks
subagent runs.

**Robustness rule:** the transcript schema is internal and version-drifts across
Claude Code releases. So `stream-json` is the *primary* contract; the transcript
decoder is a *version-tolerant, validate-at-boundary* enhancement that allowlists
the record types it understands and **degrades soft** — on any parse failure it
falls back to the `stream-json`-accumulated assistant message, never crashing and
never trusting an unrecognized format.

Because restore rebuilds committed history **only from `StepDone.Messages`**
(`restore.go:251`; terminals add nothing to `msgs`), the fallback must emit a
synthetic `StepDone` carrying the stream-accumulated assistant message **before**
the `TurnDone` — otherwise that turn's assistant reply would vanish from a
restored `Snapshot`. This makes the soft-degrade path produce the same committed
history shape as the transcript path.

## Data flow — one turn

```text
Session.StartTurn(input)
  -> foreignloop actor receives UserInput
  -> commit user message + publish TurnStarted{Message: user}   <-- BEFORE spawn
  -> ForeignAgent.Spawn (StartNew => --session-id <uuid>; else --resume <uuid>):
       claude -p --session-id <uuid> \
         --output-format stream-json --include-partial-messages --verbose \
         --append-system-prompt <sys> \
         --permission-mode <non-interactive-posture> --add-dir <cwd> \
         --model <model>
       (spawn failure here -> TurnFailed; the user request is already journaled)
  -> decode stdout JSONL -> ForeignEvent -> event.Event (publish live)
  -> process exits
  -> read transcript tail -> emit committed StepDone (assistant msgs) then TurnDone
```

(The sid is already journaled at loop creation; this path does not re-journal it.
`--verbose` is passed defensively — `-p`+`stream-json` historically requires it;
the integration test pins the requirement against the installed CLI version.
Tool-capability flags are intentionally absent — see Security.)

Event mapping:

| Foreign | looprig event |
|---|---|
| *(UserInput accepted, before spawn)* | `TurnStarted{Message: user input}` |
| `system`/`init` (session_id, tools, model) | *(no event — confirm minted sid matches; log on mismatch)* |
| partial assistant text/thinking delta | `TokenDelta` |
| assistant `tool_use` block | `ToolCallStarted` (observed) |
| user `tool_result` / `toolUseResult` | `ToolCallCompleted` (observed) |
| assistant round complete | `StepDone` (committed from transcript) |
| `result` `{success}` | `TurnDone` (Message only) |
| `result` `{error, error_max_turns, …}` | `TurnFailed` (typed `Err`) |
| child killed via Interrupt | `TurnInterrupted` |

**ID minting and correlation (resolved).** looprig events carry `uuid.UUID`
correlation ids (`TurnID`, `StepID`, `ToolExecutionID`); Claude's ids are opaque
strings (`toolu_…`). The foreign actor mints its own uuids via the loop's `idGen`
(the same seam the native loop uses) and keeps a **per-turn map
`foreignToolUseID(string) → ToolExecutionID(uuid)`** so a `tool_use` and its later
`tool_result` map to the same `ToolCallStarted`/`ToolCallCompleted` pair. The map
is turn-scoped and discarded at turn end.

**Usage is deferred (resolved).** `event.TurnDone` has no usage field and
`content.AIMessage` is `struct{ Message }`, so Claude's per-message `usage` has
nowhere to land without a taxonomy change. v1 does **not** surface usage through
events (it remains visible in the transcript); adding a usage-bearing event is a
separate, later change.

## Restore

Restore today is a second hardcoded native construction site:
`restore_constructor.go:329` calls `loop.NewRestored(... RestoredState{Msgs,
TurnIndex})`, seeding the primary loop with its folded committed history. A
foreign-backed session needs a parallel path, so restore branches on `cfg.Engine`
exactly as `newLoop` does:

```text
restore: switch cfg.Engine {
  native  -> loop.NewRestored(... RestoredState{Msgs, TurnIndex})              (unchanged)
  foreign -> foreignloop.NewRestored(... RestoredForeign{ForeignSID, TurnIndex, Msgs})
}
```

- **SID recovery.** The fold over replayed events (which already produces
  `folded.Msgs`/`folded.TurnIndex`) additionally extracts the latest
  `ForeignSessionStarted.ForeignSID` for the loop. The sid + `TurnIndex` let the
  loop resume Claude's session by sid on the next turn; Claude owns the
  conversation, so we do **not** replay messages into the agent. The folded `Msgs`
  are passed into `RestoredForeign` and **retained** so the foreign loop's
  `Snapshot` returns the same `(Msgs, TurnIndex)` as a native loop (restore-verify
  + TUI scrollback) — they are never re-sent to the agent.
- **Crash with an open foreign turn.** The half-finished turn committed no
  terminal, so restore brings the loop up **idle** at the recovered `TurnIndex`
  and replays the journaled partial events for scrollback only. But a crashed
  *parent* does not guarantee a dead *child*: a headless `claude` can outlive its
  parent and keep editing `cwd` and appending to the transcript — and starting a
  second process on the **same sid + cwd** would corrupt both. This is a safety
  requirement, not an optimization:
  - **Process-group containment.** The adapter starts each child in its **own
    process group** (`Setpgid`) and, on `Interrupt`/`Shutdown` or turn-ctx
    deadline, signals the **whole group** (not just the leader), so no descendant
    survives a normal stop.
  - **Liveness lock + fail-closed restore.** Each foreign loop holds a
    per-`(sid,cwd)` lockfile recording the live child PID. On restore, if a live
    process still holds that lock, restore is **fail-closed** for that loop: it
    refuses the next `UserInput` (returns a typed `ForeignSessionBusyError`) until
    the prior child is confirmed gone or reaped, rather than racing a second
    `claude` onto the same session. Never assume the child is dead.
- **Restore verification.** `checkFingerprint`/`checkAgentName` are unchanged; the
  foreign loop's `Snapshot` returns its retained `Msgs`/`TurnIndex` so the same
  verification path applies.

## Error handling

Every distinct failure mode is a typed error (`SpawnError`, `DecodeError`,
`ForeignExitError{Code}`, `TranscriptUnavailableError`, `ForeignSessionBusyError`),
inspectable via `errors.As`. Non-zero subprocess exit or a malformed terminal
record → `TurnFailed`. Transcript missing or unparseable → log a
security/observability event and soft-degrade to a stream-derived `StepDone` (the
accumulated assistant message) followed by `TurnDone` (so restored history is
intact — see the Robustness rule). Interrupt signals the child's **process group**
(SIGINT then kill-after-grace). The turn ctx carries a deadline; an unresponsive
group is killed when it elapses. A restore that finds the prior child still alive
on the loop's `(sid,cwd)` lock fails closed with `ForeignSessionBusyError`.

## Security

The trust boundary is **observe-only**: looprig does not gate the foreign agent's
tools; it mirrors what the agent did. Two distinct axes follow from that, and only
one is in v1 scope.

- **Approval posture (in scope, minimal).** A headless `-p` agent has no human to
  answer a permission prompt, so it must not *hang*. v1 sets exactly one
  non-interactive `--permission-mode` so the agent proceeds autonomously, and
  passes `--add-dir <cwd>` to scope filesystem context. It never passes
  `--dangerously-skip-permissions`.
- **Capability restriction (parked).** *Which* tools Claude may use — the
  `--tools` capability set and `--allowedTools`/`--disallowedTools` lists — is
  **deliberately deferred**. v1 runs the agent with its default capabilities under
  the observe-only boundary. A future read-only / restricted mode becomes a new
  attribute on the foreign loop's config when a concrete need arises; this design
  does not pre-build it (YAGNI). (Note the axes are independent in the CLI:
  `--tools` = capability set, `--allowedTools` = run-without-prompting list,
  `--permission-mode` = posture.)
- **No shell string:** spawn via `exec.CommandContext` with an **argv list**, not
  `sh -c`. System prompt, model, and dirs are separate args.
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
  unit tests; assert the command/event contract (UserInput→`TurnStarted`(user)
  emitted **before** Spawn→events→terminal, Interrupt→TurnInterrupted,
  Shutdown→Done), a **spawn failure still yields `TurnStarted` then `TurnFailed`**,
  the soft-degrade path emits `StepDone`→`TurnDone`, and that an unexpected
  gate/`SubagentResult` command is dropped without deadlock. `newLoop` (not the
  actor) is asserted to publish `LoopStarted`→`ForeignSessionStarted` in order.
- **Restore tests** for the foreign branch: fold a journal containing
  `ForeignSessionStarted` → recover the sid, pass folded `Msgs` into
  `RestoredForeign`, come up idle at the right `TurnIndex`, `Snapshot` returns the
  retained `(Msgs, TurnIndex)`, and resume by `--resume` next turn (fake agent).
  Include crash-with-open-turn (no terminal → idle restore) and the
  **fail-closed** case: a live `(sid,cwd)` lock → next `UserInput` returns
  `ForeignSessionBusyError`.
- **Session migration tests:** a fake `loop.Backend` substituted at the `newLoop`
  branch exercises interrupt/shutdown/gate-route/subagent-result paths against the
  interface (guards the `*loop.Loop`→`loop.Backend` refactor).
- **Integration tests** (`//go:build integration`) that actually spawn
  `claude -p` against a recorded prompt and assert sid continuity across a
  follow-up `--resume`, and pin the `--verbose`/`--permission-mode` requirements.
- All tests run under `-race`.

## Resolved seam decisions

- **Interface placement:** `Backend` + `Engine` live in `pkg/loop` (the leaf both
  engines import); not in `pkg/session`.
- **Selection point:** a `switch cfg.Engine` in `session.newLoop` AND a matching
  branch in `restore_constructor.go` (the two construction sites). No
  `BackendFactory` type; no `nativeBackend` wrapper.
- **`pkg/loop` change is additive only:** `Backend`/`Engine` declarations, an
  `Engine` field on `Config`, and `CommandSink()`/`DoneChan()` accessor methods on
  `*loop.Loop`.
- **Migration:** every `*loop.Loop` in `pkg/session` (audited site list above)
  becomes `loop.Backend`; `Backend` is 3 methods (no gate method needed).
- **`Backend.Snapshot` signature:** the existing tuple
  `(content.AgenticMessages, event.TurnIndex, error)` — no new type.
- **Foreign sid:** per-loop, minted by `foreignloop.New`, returned as metadata;
  `session.newLoop` publishes `LoopStarted` then the additive typed
  `ForeignSessionStarted` event (the actor publishes no lifecycle events).
  Recovered on restore by folding it.
- **`TurnStarted` timing:** emitted with the user message on `UserInput`
  acceptance, **before** spawn — so a spawn failure journals the request then
  `TurnFailed`. `system/init` maps to no event (sid confirmation only).
- **Soft-degrade:** transcript loss emits a synthetic `StepDone` (assistant msg)
  then `TurnDone`, because restore rebuilds `Msgs` only from `StepDone`.
- **Restore state:** `RestoredForeign{ForeignSID, TurnIndex, Msgs}` — folded `Msgs`
  retained for `Snapshot`/scrollback, never re-sent to the agent.
- **Adapter mode:** sid is always present; `ForeignTurn.StartNew` selects
  `--session-id` (first turn) vs `--resume` (later turns).
- **Process safety:** child in its own process group (group-signalled on
  stop/deadline) + a per-`(sid,cwd)` liveness lock; restore is **fail-closed**
  (`ForeignSessionBusyError`) if the prior child is still alive.
- **IDs:** foreign loop mints `uuid` correlation ids via `idGen` and keeps a
  per-turn `foreignToolUseID→ToolExecutionID` map.
- **`ForeignTurn.Input`:** `[]content.Block` (sealed interface, not pointer).
- **Posture:** typed `PermissionPosture` enum (no raw permission-mode strings).
- **Tool capability restriction:** parked (observe-only; v1 sets only a
  non-interactive permission posture).
- **Usage:** deferred (no event field exists in v1).

## Open questions for the implementation plan

- Whether `loop.New` changes its return type to `loop.Backend` or keeps
  `*loop.Loop` (assigned to the interface at the `newLoop` call site).
- The exact non-interactive `--permission-mode` value (`default` vs `acceptEdits`
  vs `dontAsk`) — pinned by the integration test against the installed CLI.
- Whether `--verbose` is still required alongside `-p`+`stream-json` in 2.1.191
  (confirmed in the integration test).
