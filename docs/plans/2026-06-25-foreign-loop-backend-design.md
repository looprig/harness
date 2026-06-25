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

**Out (deferred):**

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
pkg/session  ── defines ──>  Backend (interface)
                                  ^            ^
                                  | (Go structural typing — implementations
                                  |  do not import the interface; no cycle)
                    +-------------+            +-------------+
            pkg/loop  *loop.Loop                  pkg/foreignloop  *foreignloop.Loop
            (UNCHANGED native engine)             (NEW — spawns claude -p per turn)
                                                          |
                                                          v
                                                  ForeignAgent (interface)
                                                          |
                                                  adapters/claude  (now)
                                                  adapters/codex   (later)
                                                  adapters/opencode(later)
```

`Session` currently holds a concrete `*loop.Loop` and drives it purely through
its `Commands chan command.Command` and `Done` channel, publishing `event.Event`
to the hub. We extract the **narrow `Backend` interface** capturing exactly what
Session needs, and have Session depend on it. The composition root (the CLI
factory) picks native vs. foreign per session (and per subagent).

Per CLAUDE.md Open/Closed ("add a new type or wrap it; never modify a working
type"), `pkg/loop` is left **byte-for-byte unchanged**. Because Go interfaces are
methods and `Loop` exposes `Commands`/`Done` as struct *fields*, a ~10-line
`nativeBackend{ *loop.Loop }` wrapper in `pkg/session` adapts the field-shaped
handle to the method-shaped interface. The foreign loop, being new, implements
the `Backend` methods directly.

### The `Backend` interface

`Backend` is the minimal subset of the loop contract Session actually uses —
command submission, completion signalling, and the committed-state snapshot used
for restore verification. It deliberately excludes the native loop's internal
seams (gate registration, per-step commit/drain handshakes) that a foreign loop
has no analogue for.

```go
// Backend is a turn engine Session can drive. Both the native loop and a
// foreign-agent-backed loop satisfy it.
type Backend interface {
    // CommandSink is where Session submits UserInput, Interrupt, and Shutdown.
    CommandSink() chan<- command.Command
    // Done is closed when the backend actor has fully exited.
    Done() <-chan struct{}
    // Snapshot returns committed conversation state for restore verification
    // and dormant reads.
    Snapshot(ctx context.Context) (loop.Snapshot, error)
}
```

(Exact method set is finalized in the implementation plan; the principle is
"narrowest contract Session needs," per Interface Segregation.)

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

## Open questions for the implementation plan

- Final method set on `Backend` (does Session need more than
  Commands/Done/Snapshot — e.g. an id accessor for journaling?).
- Whether `Snapshot` for a foreign loop reconstructs from the transcript or
  returns a minimal "sid + last terminal" view.
- Exact `ForeignToolPolicy` shape and its mapping to Claude flags.
- Where the composition root chooses native vs. foreign (CLI flag, config, or
  per-agent registry entry).
