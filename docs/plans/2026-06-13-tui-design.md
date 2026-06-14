# Nexus CLI TUI — Design

Date: 2026-06-13

## Scope

A terminal UI (Bubbletea) that lets a human chat with a locally-constructed
agent. It is a full-screen, streaming, multimodal chat loop. It reuses the
**loop session directly** — there is no `client/` middleman layer, no auth, and
no gateway. The agent is selected by name passed as a command-line argument.

This adapts the old `docs/old/4.md` (TUI) and the agent-selection idea from
`docs/old/5.md`, dropping the `client.Client` abstraction, the auth flows, and
the gateway catalog HTTP.

### Decisions locked in

| Decision | Choice |
|---|---|
| Feature scope | Full multimodal port: streaming, slash commands, file/image attach, input queue |
| Agent integration | Extend `personalassistant.Assistant` additively (`StreamBlocks`, `Interrupt`, `Reset`); TUI depends on a narrow interface |
| External deps | `bubbletea`, `bubbles`, `lipgloss`, `glamour` (approved this session; CLAUDE.md to be amended) |
| Attachments | Inline `@path` tokens parsed from the input text |
| Interrupt UX | Esc key only; no `/interrupt`, `/cancel`, or other user-visible command |
| Entry point | `cmd/cli/main.go` (remove the empty `cmd/urvi` stub) |
| Registry | Generic `internal/registry`; concrete agents registered at the composition root |
| Display state | TUI-owned `DisplayMessage` history, independent of the loop's LLM context; no flag on `content.Block` |
| Content types | `content.Block`/`content.Chunk` are sealed interfaces (prerequisite) — see `2026-06-14-content-sealed-interfaces.md` |
| Auth | None |

---

## Architecture & layering

```
cmd/cli/main.go        — entrypoint: parse agent-name arg, build registry, open agent, run TUI (wiring only)
internal/registry      — generic name→constructor map; agent-agnostic, imports nothing domain-specific
tui/                   — Bubbletea TUI; owns ALL display state in the Elm model; owns the Agent interface
agents/personal-assistant — gains StreamBlocks + Interrupt + Reset (additive)
```

Dependency direction (low → high), no cycles, layering preserved:

- `internal/registry` imports nothing from `agents/`, `tui/`, or `cmd/`. It is a
  generic container; its type parameter is supplied by the caller.
- `tui` imports `internal/content`, `internal/agent/loop`, `internal/llm`, and the
  charm libraries. It **defines** the `Agent` interface it consumes (Dependency
  Inversion); it does not import `agents/` or `internal/registry`.
- `agents/personal-assistant` imports `internal/*` only — it is oblivious to `tui`
  and `registry`.
- `cmd/cli` is the composition root: it imports `internal/registry`, `tui`, and
  `agents/personal-assistant`, and wires them together.

---

## internal/registry (generic)

A reusable, agent-agnostic name→constructor map. Generics keep it free of any
domain import, so a low-level `internal/` package never depends on the
high-level `agents/` layer — concrete agents are registered at the composition
root.

```go
package registry

type Registry[T any] struct {
    m map[string]func(context.Context) (T, error)
}

func New[T any]() *Registry[T]

// Register binds name to a constructor. Returns *DuplicateAgentError if name
// is already registered.
func (r *Registry[T]) Register(name string, f func(context.Context) (T, error)) error

// Open constructs the agent bound to name. Returns *UnknownAgentError if name
// was never registered.
func (r *Registry[T]) Open(ctx context.Context, name string) (T, error)

// Names returns the registered names, sorted, for the help/usage message.
func (r *Registry[T]) Names() []string
```

Typed errors (CLAUDE.md: all package APIs return typed errors):

- `DuplicateAgentError{Name string}`
- `UnknownAgentError{Name string; Known []string}` — `Known` lets the caller print
  the available names.

`Names()` is sorted so usage output and the (future) picker are deterministic.

---

## Agent surface — personal-assistant changes (additive)

Purely additive to the existing, committed wrapper. The text-only `Send`,
`Stream`, and `Close` are untouched (open/closed). `Assistant` keeps the
constructed `llm.LLM` client and `llm.ModelSpec` so it can create a fresh
`session.AgentSession` for `/clear` without re-reading environment variables or
rebuilding provider transport.

```go
// StreamBlocks delivers a multimodal user message and returns the session's
// event stream: TurnStarted, TokenDelta×N, one terminal event, then EOF.
// Callers must read to EOF or call sr.Close().
func (a *Assistant) StreamBlocks(ctx context.Context, blocks []content.Block) (*llm.StreamReader[event.Event], error) {
    return a.session.Stream(ctx, blocks) // reuse the loop session directly
}

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
func (a *Assistant) Interrupt(ctx context.Context) (bool, error) {
    return a.session.Interrupt(ctx)
}

// Reset replaces the current idle session with a fresh session using the same
// client and model spec. The TUI calls this only while StatusIdle. Construction
// of the replacement happens before the old session is shut down; if construction
// fails, the existing session remains active.
func (a *Assistant) Reset(ctx context.Context) error {
    // new session first, then close/swap old; implementation owns locking if needed
}
```

`*Assistant` then satisfies the `tui.Agent` interface structurally
(`StreamBlocks`, `Interrupt`, `Reset`, `Close`). It never imports `tui`.

---

## tui package

### Agent interface (consumer-defined)

```go
// Agent is the narrow surface the TUI drives. *personalassistant.Assistant
// satisfies it; the TUI never imports any agent package.
type Agent interface {
    StreamBlocks(ctx context.Context, blocks []content.Block) (*llm.StreamReader[event.Event], error)
    Interrupt(ctx context.Context) (bool, error)
    Reset(ctx context.Context) error
    Close(ctx context.Context) error
}
```

### Display state vs. loop context state

The loop's documented contract **rolls back** turns from its LLM-context thread:

- `TurnInterrupted` → the cancelled turn's user message is rolled back.
- `TurnFailed` → the failed user message is rolled back; the thread keeps only
  completed user/assistant pairs.

So the loop's context legitimately differs from what the human saw. The TUI
therefore keeps its **own** display history, independent of the loop:

- **Separation of concerns (SRP).** The loop owns the truth of *what the model
  sees*. The TUI owns the truth of *what the human saw*. Neither mirrors the other.
- **No presentation flag on `content.Block`.** A `boolean`/`InContext` field on the
  shared `content.Block` would leak a TUI concern into a domain type used by
  providers, session, and llm — and would couple the TUI to the loop's rollback
  internals. Rejected.
- The interrupted **partial** reply has no loop/content equivalent at all
  (`TurnInterrupted` carries no `Message`) — it is inherently display-only, which a
  TUI-local `DisplayMessage` models naturally.

Consequence to note: after an interrupt the model will not remember the
interrupted exchange (the loop dropped it) even though the TUI still shows it. If
"retain interrupted partials in context" is ever wanted, that is a **loop-level**
change, out of scope here.

### DisplayMessage

```go
type DisplayRole uint8
const (
    RoleUser DisplayRole = iota
    RoleAssistant
    RoleSystem
    RoleError
    RoleInterrupted // tombstone — Blocks is nil
)

type DisplayMessage struct {
    Role   DisplayRole
    Blocks []content.Block
}
```

One uniform `[]content.Block` field for every role — the renderer iterates blocks
and type-switches on each block's concrete type, no special-cased string fields.
Per-role source:

| Role | Blocks |
|---|---|
| `RoleUser` | the blocks the user sent (text + `@path` image/file blocks), verbatim |
| `RoleAssistant` | `TurnDone.Message.Blocks` verbatim; or the flushed interrupt partial wrapped in one `TextBlock` |
| `RoleSystem` | one `TextBlock` (e.g. "session ready") |
| `RoleError` | one `TextBlock` (the `TurnFailed.Err` text or an attachment error) |
| `RoleInterrupted` | `nil` — renders `└─ interrupted` |

`RoleSystem`, `RoleError`, `RoleInterrupted` are TUI-only concepts with no
`content`/`loop` analog.

### Screen (the Elm model)

`Screen` owns everything the old `client.Client` did — but as Elm state, with no
separate goroutine and no notify callback.

```go
type Screen struct {
    agent  Agent
    appCtx context.Context // long-lived; cancelled on quit

    messages []DisplayMessage              // display history
    stream   string                        // live token accumulator (current turn)
    status   Status                        // StatusIdle | StatusRunning | StatusInterrupting
    queue    [][]content.Block             // inputs submitted while Running, FIFO
    reader   *llm.StreamReader[event.Event] // active turn's stream; nil when idle

    history       components.ChatHistory
    input         components.InputBox
    slashComplete *components.SlashComplete // nil = hidden
    width, height int
    ready         bool
}

func New(ctx context.Context, agent Agent) Screen
```

`Status`:

```go
type Status uint8
const (
    StatusIdle Status = iota   // no turn; Enter sends immediately
    StatusRunning              // turn in flight; Enter queues
    StatusInterrupting         // Interrupt issued; awaiting TurnInterrupted
)
```

### Streaming — tea.Cmd recursion (no drain goroutine)

Each `event.Event` becomes a `tea.Msg`; the model accumulates. This replaces the
old `Listen` goroutine + `notify` callback.

Internal messages:

```go
type eventMsg struct{ ev event.Event }
type streamEOFMsg struct{}
type streamErrMsg struct{ err error }
type interruptResultMsg struct {
    cancelled bool
    err       error
}
type resetResultMsg struct{ err error }
```

A single command pulls one event:

```go
func readNext(r *llm.StreamReader[event.Event]) tea.Cmd {
    return func() tea.Msg {
        ev, err := r.Next()
        switch {
        case errors.Is(err, io.EOF): return streamEOFMsg{}
        case err != nil:             return streamErrMsg{err}
        default:                     return eventMsg{ev}
        }
    }
}
```

Interrupt is also a `tea.Cmd` so `Screen.Update` never blocks on the session's
interrupt ack. This is internal Bubbletea plumbing, not a user-visible command:
the only user gesture for interrupt is Esc.

```go
func interruptTurn(ctx context.Context, agent Agent) tea.Cmd {
    return func() tea.Msg {
        interruptCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
        defer cancel()
        cancelled, err := agent.Interrupt(interruptCtx)
        return interruptResultMsg{cancelled: cancelled, err: err}
    }
}
```

Reset is also a `tea.Cmd` because it shuts down and replaces the underlying
session. The TUI only starts this command while idle.

```go
func resetAgent(ctx context.Context, agent Agent) tea.Cmd {
    return func() tea.Msg {
        resetCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
        defer cancel()
        return resetResultMsg{err: agent.Reset(resetCtx)}
    }
}
```

Flow:

- **Submit (Idle):** build blocks from input, append `RoleUser` message,
  `reader = agent.StreamBlocks(appCtx, blocks)`, `status = Running`, return
  `readNext(reader)`.
- **`eventMsg`:**
  - `TurnStarted` → no-op (already Running); return `readNext`.
  - `TokenDelta` → type-switch on `ev.Chunk` (a sealed `content.Chunk`): a
    `*content.TextChunk` appends its `Text` to `stream`; any other variant (e.g.
    `*content.ThinkingChunk`) is skipped — thinking rendering is out of scope. Return
    `readNext`. (Because `Chunk` is an interface, there is no nil-deref risk from
    reading a text field on a thinking delta.)
  - `TurnDone` → append `RoleAssistant` from `Message.Blocks`; clear `stream`;
    return `readNext` (to consume the trailing EOF).
  - `TurnFailed` → append `RoleError` from `Err`; clear `stream`; return `readNext`.
  - `TurnInterrupted` → if `stream != ""` flush it as a `RoleAssistant` partial;
    append `RoleInterrupted` tombstone; clear `stream`; return `readNext`.
- **`interruptResultMsg`:** if `err != nil`, append `RoleError` and set `status =
  Running` because the turn may still be active; otherwise stay `Interrupting` and
  wait for the loop's `TurnInterrupted` terminal event.
- **`resetResultMsg`:** if `err != nil`, append `RoleError`; otherwise clear
  display history, render cache, stream, and queue. The replacement session is
  now active behind the same `Agent` value.
- **`streamEOFMsg`:** `reader.Close()`; `reader = nil`; `status = Idle`. **Pop the
  queue:** if non-empty, dequeue the next blocks, start a new turn (`status =
  Running`), return `readNext(newReader)`.
- **`streamErrMsg`:** append `RoleError`; `reader.Close()`; `reader = nil`;
  `status = Idle`; pop the queue.

The turn context is `appCtx` (background, cancelled only on quit). Interruption is
done via `agent.Interrupt`, **not** by cancelling the stream context — the loop
then emits `TurnInterrupted` followed by EOF, which the read loop already handles.

### Input queue while Running

Submitting while `status != Idle`:

- append the `RoleUser` `DisplayMessage` immediately (the user sees their message),
- append its blocks to `queue`,
- the renderer marks the last `len(queue)` user messages as "(queued)".

`StatusInterrupting` submissions are no-ops: the input is left intact and nothing is
queued until the interrupt resolves.

### Keys

| Key | Condition | Action |
|---|---|---|
| Enter | slashComplete visible | run `Selected()`; reset input; hide panel |
| Enter | input empty | no-op |
| Enter | starts with `/` | match slash command; `/help`→append help; `/clear` while Idle→return `resetAgent(appCtx, agent)`; `/clear` while Running/Interrupting→no-op and keep input intact; no match → treat as plain text submit |
| Enter | otherwise | build blocks from input; submit (or queue); reset input |
| Esc | Running | `status = Interrupting`; return `interruptTurn(appCtx, agent)` |
| Ctrl+C | any | `agent.Close(appCtx)`; `tea.Quit` |
| Tab | slashComplete visible | fill input with `Selected().Name`; hide panel |
| ↑ / ↓ | slashComplete visible | move selection (wraps) |
| printable | input starts with `/`, no space | rebuild slashComplete from prefix |

### Components (value types, ported from old doc 4)

- `components/history.go` — `ChatHistory`: `viewport.Model` + markdown render cache
  (`map[int]string`, keyed by history index, invalidated on resize). `Refresh`
  re-renders from `Screen` state and auto-scrolls to bottom only if already at the
  bottom. `Clear`, `Resize`.
- `components/input.go` — `InputBox`: `textarea.Model` wrapper, fixed 3-line height,
  `CharLimit(0)`, line numbers off. `Value`, `Reset`, `SetValue`, `Resize`.
- `components/statusline.go` — stateless `RenderStatusLine(Status) string`:
  Idle→"", Running→"thinking…", Interrupting→"interrupting…".
- `components/slashcomplete.go` — `SlashComplete`: filtered command list + wrapping
  cursor. `NewSlashComplete(prefix)` returns nil when no match (nil = hidden).
- `components/render.go` — `renderMD` (glamour via `styles.MdStyle`, dot prefix,
  cached), `renderMessages` (dispatch on `DisplayRole` + each block's concrete type, append live
  `stream` entry, mark queued users), `wordWrap`/`wrapText`.
- `styles/styles.go` — exported lipgloss styles + `Dot`/`DotWidth` + glamour
  `MdStyle` config.

### Slash commands

```go
type SlashCmd struct {
    Name string
    Desc string
}
var slashCmds = []SlashCmd{
    {"/clear", "clear the conversation"},
    {"/help",  "list commands"},
}
```

The action for each is handled in `Screen.Update` (they touch TUI-local state:
session reset + history clear, help append) — there is no shared client to run
them against.
There is deliberately no slash command for interrupt/cancel; Esc is the complete
interrupt UX. There is deliberately no `/quit`; exit is Ctrl+C.

Slash parsing is a TUI concern only. Commands that affect model/session state are
translated into typed agent operations; the loop never parses `/...` strings.
`/clear` is accepted only while `StatusIdle`. It creates a fresh underlying agent
session via `Agent.Reset`, then clears display-only state. This is stronger than
clearing the TUI transcript: the next prompt starts with an empty loop context. If
the user types `/clear` while a turn is running or interrupting, the command is a
no-op and the input stays intact.

### Block building — `@path` attachments

`tui/blocks.go`:

```go
func buildBlocks(input string) ([]content.Block, error)
```

1. Split `input` on whitespace. Tokens of the form `@<path>` (len > 1) are
   attachments; the remaining words rejoin into the leading prompt text.
2. Leading block: one `TextBlock` with the prompt text (omitted if empty but
   attachments exist).
3. For each `@path`, in order of appearance:
   - `filepath.Clean(path)`.
   - Reject denied attachment paths before opening the file.
   - `os.Lstat`; symlinks are rejected; the path must be a **regular file**.
   - Enforce a size cap (**5 MB**) — refuse larger (`*AttachmentTooLargeError`).
   - Image ext (`.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.svg`) → read bytes →
     `&content.ImageBlock{MediaType: <by ext>, Source: content.ImageSource{Data:
     bytes}}` (media type mapped to `content.MediaTypeImage*`).
   - Plaintext ext (`.txt .md .go .py .js .ts .json .yaml .yml .toml .sh
     .csv .html .xml .rs .java .c .cpp .h`) → read →
     `&content.TextBlock{Text: "[" + base + "]\n" + string(data)}`.
   - Otherwise → `*UnsupportedAttachmentError{Ext}`.
4. Empty input with no attachments → `*EmptyInputError`.

Denied attachments are never read, even if their extension would otherwise be
supported. The deny check is based on the cleaned path's lower-cased path
segments, basename, and extension:

- Denied path segments: `.ssh`, `.aws`, `.gcloud`, `.gnupg`, `.kube`.
- Denied basenames/patterns: `.env`, `.env.*`, `.npmrc`, `.netrc`, `.pypirc`,
  `.dockercfg`, `id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`.
- Denied extensions: `.env`, `.pem`, `.key`, `.p12`, `.pfx`, `.jks`,
  `.keystore`.

**Security:** this is a local single-user tool acting with the user's own
privileges, so there is no path root to confine to; validation is
`filepath.Clean` + denylist + symlink rejection + regular-file check + size cap.
Attachment errors are surfaced as a `RoleError` line and the message is **not**
sent — the input is left intact so the user can fix the path.

### Typed errors (tui package)

- `EmptyInputError`
- `UnsupportedAttachmentError{Ext string}`
- `DeniedAttachmentError{Path string; Reason string}`
- `AttachmentTooLargeError{Path string; Size, Max int64}`
- `AttachmentNotFoundError{Path string}` (or wrap the `os` error with context)
- `AttachmentReadError{Path string; Cause error}`

All are concrete structs (`errors.As`-able), per CLAUDE.md.

---

## cmd/cli/main.go

Wiring only.

```
1. ctx, stop := signal.NotifyContext(Background, SIGINT, SIGTERM); defer stop()
2. name := first non-flag arg, default "personal-assistant"
3. reg := registry.New[tui.Agent]()
   reg.Register("personal-assistant", func(c) (tui.Agent, error) { return personalassistant.New(c) })
4. agent, err := reg.Open(ctx, name)   // *UnknownAgentError → print Names(), exit non-zero
5. screen := tui.New(ctx, agent)
6. prog := tea.NewProgram(screen, tea.WithAltScreen())
7. prog.Run()
8. agent.Close(ctx) on exit (also wired to Ctrl+C inside the TUI)
```

`cmd/cli` imports `internal/registry`, `tui`, `agents/personal-assistant`, stdlib,
and bubbletea. No other `internal/*` imports.

The empty `cmd/urvi/main.go` stub is removed.

---

## Dependencies (CLAUDE.md amendment)

Approved this session; CLAUDE.md's approved-deps list to be amended:

- `github.com/charmbracelet/bubbletea` — TEA runtime
- `github.com/charmbracelet/bubbles` — textarea + viewport widgets
- `github.com/charmbracelet/lipgloss` — styling/layout
- `github.com/charmbracelet/glamour` — markdown → ANSI rendering

---

## Testing

Table-driven, run with `-race`.

- **blocks_test.go** — `buildBlocks`: happy text-only, single image, single
  plaintext, multiple mixed, unknown ext, missing file, directory (non-regular),
  symlink, denied basename, denied path segment, denied extension, too-big, no
  tokens / empty, prompt-then-attachments ordering.
- **render_test.go** — `renderMD`, `renderMessages` (each role + queued marker +
  live stream), `wordWrap`/`wrapText` boundaries.
- **slashcomplete_test.go** — prefix filtering, nil-on-no-match, cursor wrap.
- **model_test.go** — `Screen.Update` transitions via a **fake `Agent`** returning a
  scripted `StreamReader`: submit-idle→Running, queue-while-Running, TokenDelta
  accumulation, TurnDone/TurnFailed/TurnInterrupted handling, EOF→pop queue,
  Esc→Interrupting, `/clear` idle→reset+clear, `/clear` running→no-op+keep input,
  Ctrl+C→quit. Drive `Update` directly with synthetic msgs (no `teatest`
  dependency).
- **registry_test.go** — Register/Open happy path, duplicate name error, unknown
  name error, `Names()` sorted.
- **personal-assistant** — extend existing tests: `StreamBlocks`, `Interrupt`, and
  `Reset` delegate to / replace the session (fake-client based, matching the
  existing test style).

---

## Out of scope

- Auth, gateway, catalog HTTP.
- Session resume / persistence / `/sessions` picker.
- Multi-agent picker UI (registry supports many; only one agent registered today).
- Retaining interrupted partials in the loop's LLM context (loop-level change).
- Tool-call and thinking-block rendering (events exist; rendering deferred).
- Clipboard image paste (terminal-dependent); attachments are `@path` only.
- Config file.
