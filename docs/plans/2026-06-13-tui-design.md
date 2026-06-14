# Nexus CLI TUI — Design

Date: 2026-06-13 · Revised: 2026-06-14

> Revision 2026-06-14 refreshes references after the `content` sealed-interface,
> `loop/event` + `loop/command`, and `internal/agent/session` refactors landed
> (build is green), and resolves the review findings: agent lifecycle on `/clear`,
> image attachments gated on model modality, attachment-read hardening
> (`O_NOFOLLOW` + fd stat + `LimitReader`), interrupt-clears-queue, a `buildBlocks`
> fuzz target, generic registry error names, and `Init`/`View`/resize + bounded
> quit.
>
> Revision 2026-06-14 (b) supersedes the earlier in-place `Assistant.Reset` +
> `sync.Mutex` approach: `/clear` is **agent lifecycle**, not agent behavior, so
> `Assistant` stays immutable after construction (no lock) and the TUI replaces it
> via an `OpenAgent` thunk — build a fresh agent, swap the reference on the Elm
> loop, close the old one. `Reset` is dropped from the `Agent` interface.

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
| Agent integration | Extend `personalassistant.Assistant` additively (`StreamBlocks`, `Interrupt`, `AcceptsImages`); TUI depends on a narrow interface. `Assistant` is immutable after construction |
| External deps | `bubbletea`, `bubbles`, `lipgloss`, `glamour` (approved this session; CLAUDE.md to be amended) |
| Attachments | Inline `@path` tokens parsed from the input text |
| Interrupt UX | Esc key only; no `/interrupt`, `/cancel`, or other user-visible command |
| Entry point | `cmd/cli/main.go` (remove the empty `cmd/urvi` stub) |
| Registry | Generic `internal/registry`; concrete agents registered at the composition root |
| Display state | TUI-owned `DisplayMessage` history, independent of the loop's LLM context; no flag on `content.Block` |
| Content types | `content.Block`/`content.Chunk` sealed interfaces — **landed** (commit `bc81b29`); `event.Event` now in `internal/agent/loop/event`, `session` in `internal/agent/session`, and `session.Stream`/`Invoke` already take `[]content.Block` |
| Image attachments | Parsed/built but **model-gated**: sent only when the active agent advertises `AcceptsImages()`. The lone registered agent (Kimi K2) is text-only, so image `@path` tokens are rejected at the input boundary — see *Model capability constraint* |
| Interrupt + queue | Esc cancels the in-flight turn **and** clears the pending input queue (drops the not-yet-sent queued user messages) |
| Agent lifecycle (`/clear`) | Not a mutation of the live agent: the TUI opens a fresh agent via an `OpenAgent` thunk, swaps the reference on the (single-threaded) Elm loop, then closes the old one. No `sync.Mutex`, no `Reset` on the interface. `StatusResetting` blocks input during the swap |
| Auth | None |

---

## Architecture & layering

```
cmd/cli/main.go        — entrypoint: parse agent-name arg, build registry, open agent, run TUI (wiring only)
internal/registry      — generic name→constructor map; agent-agnostic, imports nothing domain-specific
tui/                   — Bubbletea TUI; owns ALL display state in the Elm model; owns the Agent interface
agents/personal-assistant — gains StreamBlocks + Interrupt + AcceptsImages (additive; immutable after construction)
```

Dependency direction (low → high), no cycles, layering preserved:

- `internal/registry` imports nothing from `agents/`, `tui/`, or `cmd/`. It is a
  generic container; its type parameter is supplied by the caller.
- `tui` imports `internal/content`, `internal/agent/loop/event`, `internal/llm`, and the
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

// Register binds name to a constructor. Returns *DuplicateNameError if name
// is already registered.
func (r *Registry[T]) Register(name string, f func(context.Context) (T, error)) error

// Open constructs the value bound to name. Returns *UnknownNameError if name
// was never registered.
func (r *Registry[T]) Open(ctx context.Context, name string) (T, error)

// Names returns the registered names, sorted, for the help/usage message.
func (r *Registry[T]) Names() []string
```

Typed errors (CLAUDE.md: all package APIs return typed errors). Named for the
generic key, not for "agent", since the package imports nothing domain-specific:

- `DuplicateNameError{Name string}`
- `UnknownNameError{Name string; Known []string}` — `Known` lets the caller print
  the available names.

`Names()` is sorted so usage output and the (future) picker are deterministic.

On `T any`: this is a generic **type-parameter constraint** (the unconstrained
"any type" idiom), not a dynamically-typed `any`/`interface{}` value flowing
through business logic — so it is outside the scope of CLAUDE.md's no-`any` rule,
which targets the latter. It also matches the precedent already in the tree
(`llm.StreamReader[T any]`), and the instantiation at the composition root
(`registry.Registry[tui.Agent]`) is fully concrete.

---

## Agent surface — personal-assistant changes (additive)

Additive and **immutable**. `Assistant` keeps its existing shape — `{session,
cancel}` set once in `newWithClient` (`agents/personal-assistant/agent.go:39`) and
never reassigned — so all methods stay lock-free. The only additions are the two
new multimodal methods and a single captured `acceptsImages bool` (no `client`/
`spec` retained, no `sync.Mutex`). There is **no `Reset`**: `/clear` is an agent
*lifecycle* operation, owned by the TUI, not a mutation of the live agent (see
*Agent lifecycle on `/clear`*).

```go
type Assistant struct {
    session       *session.AgentSession
    cancel        context.CancelFunc
    acceptsImages bool // snapshot of spec.AcceptsImages; set once, immutable
}

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

// AcceptsImages reports whether the underlying model accepts image blocks, so
// the TUI can reject image @path tokens at the input boundary instead of letting
// the provider fail the turn. Immutable; lock-free.
func (a *Assistant) AcceptsImages() bool { return a.acceptsImages }
```

`newWithClient` captures `acceptsImages: spec.AcceptsImages` alongside the existing
fields; nothing else changes, and `Send`/`Stream`/`Close` are untouched.
`*Assistant` then satisfies `tui.Agent` structurally (`StreamBlocks`, `Interrupt`,
`Close`, `AcceptsImages`) and never imports `tui`. (`AcceptsImages bool` is a small
additive field on `llm.Model`, zero value `false`, carried onto `llm.ModelSpec` by
`Spec` — see *Model capability constraint*.)

---

## tui package

### Agent interface (consumer-defined)

```go
// Agent is the narrow surface the TUI drives. *personalassistant.Assistant
// satisfies it; the TUI never imports any agent package.
type Agent interface {
    StreamBlocks(ctx context.Context, blocks []content.Block) (*llm.StreamReader[event.Event], error)
    Interrupt(ctx context.Context) (bool, error)
    Close(ctx context.Context) error
    // AcceptsImages reports whether the model accepts image blocks, so
    // buildBlocks can reject image @path tokens at the boundary (fail fast)
    // rather than letting the provider error mid-turn. A single bool keeps the
    // interface segregated; richer capabilities would add further methods.
    AcceptsImages() bool
}

// OpenAgent constructs a fresh Agent. The composition root binds it to
// registry.Open(name); the TUI calls it on /clear to replace the current agent.
// There is deliberately no Reset on Agent — a session actor is shut down and
// replaced, not mutated in place.
type OpenAgent func(context.Context) (Agent, error)
```

### Agent lifecycle on `/clear`

`/clear` means *a new session altogether* — lifecycle ownership, not normal agent
behavior — so it lives in the TUI, not inside `Assistant`. `Screen` holds both the
current `agent Agent` and the `openAgent OpenAgent` thunk. On `/clear` while Idle
(as a bounded `tea.Cmd` that only *builds* the new agent):

1. `newAgent, err := openAgent(resetCtx)` — construct a fresh agent.
2. on `err` → keep the old agent, surface a `RoleError`, `status = Idle`.
3. on success → swap `m.agent = newAgent`.
4. close the **old** agent via a bounded best-effort `closeAgent` cmd.
5. clear display history, render cache, `stream`, and `queue`.

Steps 2–5 run in `reopenResultMsg` on the Update loop. Because `m.agent` is read
and written only on that single goroutine — and each in-flight `tea.Cmd` captured
its agent value at dispatch — the swap needs no lock, which is exactly why
`Assistant` can stay immutable. `StatusResetting` blocks `Submit`/queue during the
open so no turn starts on the agent about to be replaced.

The default `openAgent` (`registry.Open(name)`) re-runs `personalassistant.New`,
which re-reads `LLM_API_KEY` and rebuilds provider transport. For `/clear` that is
acceptable — it even picks up a rotated key. If it ever becomes a cost, bind
`openAgent` to a composition-root factory that captures the already-built
client/spec; **do not** move reset mutation back into `Assistant`.

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

### Future persistence seam

Session save/restore is intentionally out of scope for this TUI slice, but this
design leaves the seam in the right layer:

- The **loop/session** owns model-visible conversation state. Future persistence
  must add a session-level snapshot/hydrate API for the loop's
  `content.AgenticMessages` plus session metadata; the TUI must not reconstruct
  model context from display rows.
- The **TUI** owns display transcript state. Future restore can load
  `[]DisplayMessage` separately so the human sees errors, interrupted partials,
  tombstones, and other UI-only rows exactly as they appeared, without making
  those rows model-visible.
- `OpenAgent` is a fresh-session thunk today. A later persistence design can
  generalize the composition-root factory to `OpenAgent(ctx, OpenOptions{...})`
  or introduce a sibling restore factory, without adding mutation to `Assistant`
  or making the TUI import concrete agent/session packages.
- Attachment persistence must choose deliberately between storing the expanded
  content actually sent to the model and storing only original paths. Accurate
  restore requires expanded content; storing paths alone is not replay-safe.

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
    agent     Agent
    openAgent OpenAgent       // builds a replacement agent on /clear
    appCtx    context.Context // long-lived; cancelled on quit

    messages []DisplayMessage              // display history
    stream   string                        // live token accumulator (current turn)
    status   Status                        // Idle | Running | Interrupting | Resetting
    queue    []queuedInput                 // inputs submitted while Running, FIFO
    reader   *llm.StreamReader[event.Event] // active turn's stream; nil when idle

    history       components.ChatHistory
    input         components.InputBox
    slashComplete *components.SlashComplete // nil = hidden
    width, height int
    ready         bool
}

// queuedInput is a submission made while a turn was Running. DisplayIndex is the
// index in `messages` of the RoleUser row shown for it, so the renderer can mark
// exactly that row "(queued)" and an interrupt can remove exactly those rows.
// This is robust against other rows appended mid-turn (a /help RoleSystem row, an
// attachment-error RoleError row), which a "last len(queue) rows" heuristic would
// mis-delete.
type queuedInput struct {
    Blocks       []content.Block
    DisplayIndex int
}

func New(ctx context.Context, agent Agent, openAgent OpenAgent) Screen
```

`Status`:

```go
type Status uint8
const (
    StatusIdle Status = iota   // no turn; Enter sends immediately
    StatusRunning              // turn in flight; Enter queues
    StatusInterrupting         // Interrupt issued; awaiting TurnInterrupted
    StatusResetting            // /clear reopen in flight; Enter and queueing are blocked
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
type reopenResultMsg struct {
    agent Agent // the freshly opened replacement; nil on err
    err   error
}
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

Reopen is also a `tea.Cmd`: `/clear` (while Idle) sets `status = Resetting`
**before** dispatching it, so no `Submit` can start a turn on the agent about to be
replaced. The cmd only *builds* the new agent; the swap and the old agent's
shutdown happen on the Update loop in `reopenResultMsg`, so no two goroutines ever
touch `m.agent`.

```go
func reopenAgent(ctx context.Context, open OpenAgent) tea.Cmd {
    return func() tea.Msg {
        resetCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
        defer cancel()
        a, err := open(resetCtx)
        return reopenResultMsg{agent: a, err: err}
    }
}
```

Close-on-quit is bounded so a hung session cannot wedge the exit. `appCtx` is
cancelled as the program tears down (and in raw mode Ctrl+C arrives as a
`tea.KeyMsg`, so `signal.NotifyContext` may never fire), so `closeAgent` derives
its own timeout from `context.Background()`, not `appCtx`:

```go
func closeAgent(agent Agent) tea.Cmd {
    return func() tea.Msg {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = agent.Close(ctx) // best-effort; Close is idempotent
        return nil
    }
}
```

Starting a turn is shared by the initial submit and queued advance, because
`agent.StreamBlocks` can fail **before** a reader exists (`*command.TurnBusyError`,
`SessionLoopExited`, or `SessionContextDone` from the session). It must never set
`Running` without a reader, nor call `readNext(nil)`:

```go
func (m *Screen) startTurn(blocks []content.Block) (tea.Cmd, bool) {
    r, err := m.agent.StreamBlocks(m.appCtx, blocks)
    if err != nil {
        m.appendError(err)                  // RoleError
        m.status, m.reader = StatusIdle, nil
        return nil, false                   // never readNext(nil); stays Idle
    }
    m.reader, m.status = r, StatusRunning
    return readNext(r), true
}
```

Flow:

- **Submit (Idle only):** `blocks, err := buildBlocks(input, agent.AcceptsImages())`.
  On `err` (bad/denied/oversized/unsupported `@path`): append `RoleError`, **keep
  the input intact**, start no turn, return nil. Otherwise `cmd, ok :=
  startTurn(blocks)`: on `ok` append the `RoleUser` message and reset the input; on
  `!ok` the start failed — `startTurn` already appended the `RoleError` and stayed
  `Idle`, so **keep the input intact** (no `RoleUser` row, input not reset) for the
  user to retry. Return `cmd`.
- **`eventMsg`** (type-switch on `event.*`):
  - `TurnStarted` → no-op (already Running); return `readNext`.
  - `TokenDelta` → type-switch on `ev.Chunk` (a sealed `content.Chunk`): a
    `*content.TextChunk` appends its `Text` to `stream`; any other variant (e.g.
    `*content.ThinkingChunk`) is skipped — thinking rendering is out of scope. Return
    `readNext`. (Because `Chunk` is an interface, there is no nil-deref risk from
    reading a text field on a thinking delta.)
  - `TurnDone` → append `RoleAssistant` from `Message.Blocks`, guarding a nil
    `Message` (treat as an empty assistant turn rather than dereferencing the
    pointer); clear `stream`; return `readNext` (to consume the trailing EOF).
  - `TurnFailed` → append `RoleError` from `Err`; clear `stream`; return `readNext`.
  - `TurnInterrupted` → if `stream != ""` flush it as a `RoleAssistant` partial;
    append `RoleInterrupted` tombstone; clear `stream`; return `readNext`. (The
    queue was already cleared by the Esc handler that began the interrupt.)
- **`interruptResultMsg`:** if `err != nil`, append `RoleError` and set `status =
  Running` because the turn may still be active. On success stay `Interrupting`:
  with `cancelled == true` the loop's `TurnInterrupted` terminal event is coming;
  with `cancelled == false` (the turn finished between Esc and the ack, so no
  `TurnInterrupted` arrives) the in-flight stream's pending EOF (`streamEOFMsg`)
  is what returns the model to `Idle`.
- **`reopenResultMsg`** (model is `StatusResetting`): if `err != nil`, **keep the
  old agent**, append `RoleError`, set `status = Idle`. On success: swap
  `m.agent = msg.agent`, return `closeAgent(oldAgent)` to shut the old one down
  (bounded, best-effort), clear display history, render cache, `stream`, and
  `queue`, set `status = Idle`.
- **`streamEOFMsg`:** `reader.Close()` (error ignored — idempotent closer, nothing
  actionable at the UI); `reader = nil`; `status = Idle`. **Peek the queue:** if
  non-empty, read `q := queue[0]` without removing it, then `cmd, ok :=
  startTurn(q.Blocks)`. If `ok`, remove the head from `queue`; its `RoleUser` row
  already exists from queue time and now renders unmarked as the active prompt. If
  start fails, `startTurn` shows a `RoleError`, stays `Idle`, and leaves the head
  queued so the transcript still marks it as unsent; do not auto-retry in a loop
  and do not call `readNext(nil)`. Return `cmd`.
- **`streamErrMsg`:** append `RoleError`; `reader.Close()` (error ignored);
  `reader = nil`; `status = Idle`; peek/start the queue exactly as
  `streamEOFMsg` does. Reaching this case is defensive: `session.Stream`'s reader
  only ever yields a value or `io.EOF`, so in practice `readNext` produces
  `streamEOFMsg`, not `streamErrMsg`.

The turn context is `appCtx` (background, cancelled only on quit). Interruption is
done via `agent.Interrupt`, **not** by cancelling the stream context — the loop
then emits `TurnInterrupted` followed by EOF, which the read loop already handles.

### Input queue while Running

Submitting while `status == Running` builds blocks the same way; a `buildBlocks`
error appends a `RoleError`, keeps the input intact, and queues nothing. On
success:

- append the `RoleUser` `DisplayMessage` immediately (the user sees their message),
- push `queuedInput{Blocks: blocks, DisplayIndex: len(messages)-1}` onto `queue`,
- the renderer marks each `queue[i].DisplayIndex` row "(queued)".

Tracking the display index per queued input — rather than assuming the queued rows
are the trailing `len(queue)` entries — is deliberate: other rows **can** be
appended mid-turn (a `/help` `RoleSystem` row, or an attachment/build `RoleError`
when a later submission's `@path` is bad), so the queued `RoleUser` rows are
neither necessarily contiguous nor last.

`StatusInterrupting` and `StatusResetting` submissions are no-ops: the input is
left intact and nothing is queued until the turn resolves.

**Interrupt clears the queue.** Pressing Esc means *stop*, so the Esc handler — in
addition to cancelling the in-flight turn — drops the pending queue and removes
**exactly** the queued-but-unsent rows: collect `{q.DisplayIndex}` into a set,
rebuild `messages` skipping those indices, set `queue = nil`, and invalidate the
render cache (remaining indices shift down). They were never sent to the model, so
leaving them in the transcript would be misleading. (Without this, the
post-interrupt `streamEOFMsg` would pop the queue and send them anyway — the
surprising "I hit Esc but my queued messages ran" behavior this avoids.)

On **queue advance** (`streamEOFMsg` starts the next turn), no rows are removed.
The head leaves `queue` only after `startTurn` succeeds, so its `RoleUser` row —
no longer in `queue` — renders unmarked as the active prompt. If `startTurn`
fails, the head stays queued and remains marked. Because advancing never deletes a
row, the `DisplayIndex` values of the remaining queued inputs stay valid until the
next interrupt clears the whole queue at once.

### Keys

| Key | Condition | Action |
|---|---|---|
| Enter | slashComplete visible | fill input with `Selected().Name`, then route it through the **same** slash dispatch as the row below; reset input + hide panel **only if the command actually ran** (e.g. `/clear` while Running/Interrupting/Resetting is a no-op → keep input and panel intact) |
| Enter | input empty | no-op |
| Enter | starts with `/` | match slash command; `/help`→append help; `/clear` while Idle→`status = Resetting`, return `reopenAgent(appCtx, openAgent)`; `/clear` while Running/Interrupting/Resetting→no-op, keep input intact; no match → treat as plain text submit |
| Enter | otherwise | build blocks (gating images on `agent.AcceptsImages()`); submit while Idle, queue while Running, no-op while Interrupting/Resetting; reset input on success |
| Esc | Running | `status = Interrupting`; clear the queue and remove exactly the `queue[i].DisplayIndex` rows from `messages` (then invalidate the render cache); return `interruptTurn(appCtx, agent)` |
| Ctrl+C | any | `return tea.Sequence(closeAgent(agent), tea.Quit)` — `closeAgent` closes under a bounded (5 s) `Background` context so a hung session can't wedge quit |
| Tab | slashComplete visible | fill input with `Selected().Name`; hide panel |
| ↑ / ↓ | slashComplete visible | move selection (wraps) |
| printable / Backspace | edits the input | if input now starts with `/` and has no space, (re)build slashComplete from the prefix; otherwise hide the panel (set to nil) |

### Init, View, and window resize

The model implements the full `tea.Model` trio; the flow above details `Update`,
these are the other two plus resize:

- **`Init()`** returns `textarea.Blink` (cursor blink) batched with a command that
  appends the initial `RoleSystem` "session ready" line. No turn is started at init.
- **`tea.WindowSizeMsg`** sets `width`/`height`, flips `ready = true` on first
  receipt, resizes `history` and `input`, and invalidates the render cache (a width
  change reflows markdown).
- **`View()`** renders an empty string until `ready` (avoids a 0×0 first frame),
  then vertically joins: the `history` viewport, the status line
  (`RenderStatusLine(status)`), the slash-complete panel when non-nil, and the
  `input` box. History height = `height` minus the measured heights of the other
  rows.

### Components (value types, ported from old doc 4)

- `components/history.go` — `ChatHistory`: `viewport.Model` + markdown render cache
  (`map[int]string`, keyed by history index, invalidated on resize). `Refresh`
  re-renders from `Screen` state and auto-scrolls to bottom only if already at the
  bottom. `Clear`, `Resize`.
- `components/input.go` — `InputBox`: `textarea.Model` wrapper, fixed 3-line height,
  `CharLimit(0)`, line numbers off. `Value`, `Reset`, `SetValue`, `Resize`.
- `components/statusline.go` — stateless `RenderStatusLine(Status) string`:
  Idle→"", Running→"thinking…", Interrupting→"interrupting…", Resetting→"clearing…".
- `components/slashcomplete.go` — `SlashComplete`: filtered command list + wrapping
  cursor. `NewSlashComplete(prefix)` returns nil when no match (nil = hidden).
- `components/render.go` — `renderMD` (glamour via `styles.MdStyle`, dot prefix,
  cached), `renderMessages` (dispatch on `DisplayRole` + each block's concrete type, append live
  `stream` entry, mark rows whose index is in `queue` as "(queued)"; an `ImageBlock`
  renders a `[image: <media type>, <n> bytes]` placeholder — terminals can't show
  pixels), `wordWrap`/`wrapText`.
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
agent reopen + history clear, help append) — there is no shared client to run
them against. A typed Enter starting with `/` and an Enter on a visible
slash-complete selection route through the **same** dispatch, which decides
run-vs-no-op; only an actual run resets the input and hides the panel, so a no-op
(e.g. `/clear` while busy) leaves both intact.
There is deliberately no slash command for interrupt/cancel; Esc is the complete
interrupt UX. There is deliberately no `/quit`; exit is Ctrl+C.

Slash parsing is a TUI concern only. Commands that affect model/session state are
translated into typed agent operations; the loop never parses `/...` strings.
`/clear` is accepted only while `StatusIdle`. It opens a fresh agent via the
`OpenAgent` thunk, swaps it in, closes the old one, then clears display-only state.
This is stronger than clearing the TUI transcript: the next prompt starts with an
empty loop context. If the user types `/clear` while a turn is running,
interrupting, or resetting, the command is a no-op and the input stays intact.

### Block building — `@path` attachments

`tui/blocks.go`:

```go
// allowImages comes from agent.AcceptsImages(); when false an image @path is
// rejected at the boundary instead of being sent to a text-only model.
func buildBlocks(input string, allowImages bool) ([]content.Block, error)
```

1. Split `input` on whitespace. Tokens of the form `@<path>` (len > 1) are
   attachments; the remaining words rejoin into the leading prompt text.
2. Leading block: one `TextBlock` with the prompt text (omitted if empty but
   attachments exist).
3. For each `@path`, in order of appearance, **classify before touching the file**
   so an unsupported or text-only-model attachment is never opened or read:
   - `filepath.Clean(path)`, then run the denylist check (still **before any
     syscall**) → `*DeniedAttachmentError`.
   - **Classify by extension + `allowImages` and return the rejection now**, before
     any I/O:
     - image ext (`.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`) with `!allowImages`
       → `*ImageUnsupportedError{Ext}`;
     - an extension in neither the image nor the plaintext set →
       `*UnsupportedAttachmentError{Ext}`.
     (`.svg` classifies as **plaintext**, not image — providers reject
     `image/svg+xml` and SVG is XML.)
   - Only for an accepted classification, open + stat + read:
     - **`os.OpenFile(clean, os.O_RDONLY|syscall.O_NOFOLLOW, 0)`** so a symlinked
       final component fails to open rather than being followed — closes the
       `Lstat`→open TOCTOU window. Missing file → `*AttachmentNotFoundError`;
       `ELOOP`/symlink → `*DeniedAttachmentError`. `defer f.Close()`.
     - `f.Stat()` the **open descriptor** (not the path): reject non-regular files
       (`mode.IsRegular()` — also rejects directories and devices) and
       `Size() > 5 MB` up front (`*AttachmentTooLargeError`).
     - Read through a bounded reader — `io.ReadAll(io.LimitReader(f, maxBytes+1))` —
       and reject if the result exceeds `maxBytes`, so a file that grows after the
       stat can't be slurped unbounded into memory.
     - Build the block: image ext → `&content.ImageBlock{MediaType: <by ext>,
       Source: content.ImageSource{Data: bytes}}` (mapped to
       `content.MediaTypeImage*`); plaintext ext (`.txt .md .go .py .js .ts .json
       .yaml .yml .toml .sh .csv .html .xml .rs .java .c .cpp .h .svg`) →
       `&content.TextBlock{Text: "[" + base + "]\n" + string(data)}`.
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
privileges, so there is no path root to confine to. Validation is
`filepath.Clean` + denylist (pre-syscall) + **classify-before-open** + `O_NOFOLLOW`
open + fd-based regular-file check + stat-time size cap + `LimitReader`-bounded
read. Classifying the extension/modality before any I/O means a denied,
unsupported, or text-only-model attachment is rejected without ever opening a file.
Checking the **open descriptor** rather than the path removes the classic
`Lstat`→open time-of-check/time-of-use gap; the bounded read removes the "stat says
small, read is huge" gap. Attachment errors are surfaced as a `RoleError` line and
the message is **not** sent — the input is left intact so the user can fix the path.

### Model capability constraint (image attachments)

The design parses and builds image blocks, but the only registered agent —
`personal-assistant` — runs `llm.ChutesKimiK2()`
(`internal/llm/catalog.go`), a **text-only** model, and `llm.Model`
(`internal/llm/model.go`) carries no modality metadata today. Sending an
`ImageBlock` to a text-only provider surfaces as an `event.TurnFailed` *after* the
turn starts — the wrong place to discover an input mistake (CLAUDE.md: *validate
at every boundary*).

Fix, in two additive parts:

1. **`llm.Model` gains `AcceptsImages bool`** (zero value `false`; Kimi K2 leaves
   it false). `Model.Spec` carries it onto `llm.ModelSpec`. This is the single
   source of truth for modality.
2. **The agent exposes it**: `Assistant.AcceptsImages()` returns the bool it
   captured from `spec.AcceptsImages` at construction; the consumer-defined
   `tui.Agent` declares `AcceptsImages() bool`. `buildBlocks`
   takes `allowImages` and returns `*ImageUnsupportedError` for an image `@path`
   when the model is text-only — rejected at the boundary, never sent.

Until a vision-capable agent is registered, image attachments are therefore
*defined but inert*: the parse/validate path exists and is tested, but every image
`@path` is rejected with a clear `RoleError`. Registering a vision model (set
`AcceptsImages: true` on its `Model`) is the only thing needed to light them up —
no TUI change.

### Typed errors (tui package)

- `EmptyInputError`
- `UnsupportedAttachmentError{Ext string}`
- `ImageUnsupportedError{Ext string}` — image `@path` while the active model is
  text-only (`AcceptsImages() == false`)
- `DeniedAttachmentError{Path string; Reason string}`
- `AttachmentTooLargeError{Path string; Size, Max int64}`
- `AttachmentNotFoundError{Path string; Cause error}` — `Cause` carries the
  underlying `os` error (`errors.As`/`Unwrap`-able), keeping the type explicit
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
4. open := func(c context.Context) (tui.Agent, error) { return reg.Open(c, name) }  // the OpenAgent thunk
5. agent, err := open(ctx)   // *UnknownNameError → print Names(), exit non-zero
6. screen := tui.New(ctx, agent, open)
7. prog := tea.NewProgram(screen, tea.WithAltScreen())
7b. go func() { <-ctx.Done(); prog.Quit() }()  // SIGINT/SIGTERM (non-keyboard) → clean TUI teardown; no-op if already quit. `defer stop()` later cancels ctx, so this goroutine never leaks past exit.
8. final, runErr := prog.Run()   // capture the error — do not discard with `_`
9. backstop bounded Close of the **current** agent (which `/clear` may have replaced),
   even on a Run error: prefer the live agent off the final model, else fall back to
   the initial one:
   `toClose := agent; if s, ok := final.(tui.Screen); ok { toClose = s.Agent() }`
   `closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second); defer cancel(); _ = toClose.Close(closeCtx)`
   — Ctrl+C inside the TUI already closes the current agent via `closeAgent`; Close is idempotent, so the double call is safe. `tui.Screen.Agent()` is a tiny accessor returning the live agent; closing the initial `agent` would miss one swapped in by `/clear`, and the fallback covers the case where `Run` errored before producing a usable model.
10. if `runErr != nil`: report it (stderr) and exit non-zero — **after** step 9, so the agent is always released.
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

- **blocks_test.go** — `buildBlocks`: happy text-only, single image
  (`allowImages=true`), image rejected when `allowImages=false`
  (`*ImageUnsupportedError`), single plaintext, `.svg`→plaintext branch, multiple
  mixed, unknown ext, missing file, directory (non-regular), symlink (rejected via
  `O_NOFOLLOW`), denied basename, denied path segment, denied extension, too-big
  (stat-time), grows-after-stat (`LimitReader` bound), no tokens / empty,
  prompt-then-attachments ordering. Files built under `t.TempDir()`.
- **blocks_fuzz_test.go** — `FuzzBuildBlocks` (CLAUDE.md mandates a fuzz target for
  parsers of external input): seed with prompts, `@path` tokens, and adversarial
  paths (`@`, `@../`, embedded NUL, very long tokens); assert it never panics and
  only ever returns one of the typed errors above. **The fuzzer must never open a
  host path:** every fuzzed `@path` token is rewritten to its basename joined under
  a per-run `t.TempDir()` before `buildBlocks` is called (a leading-`@` token whose
  cleaned basename would escape the dir is dropped), so the only files the I/O path
  can touch are temp fixtures — fuzzing exercises tokenize + denylist + ext/modality
  classification + error typing, not arbitrary filesystem reads. (The classify-
  before-open ordering already means most adversarial inputs return before any
  `open`; the rewrite covers the accepted-extension tokens that don't.)
- **render_test.go** — `renderMD`, `renderMessages` (each role + queued marker +
  live stream), `wordWrap`/`wrapText` boundaries.
- **slashcomplete_test.go** — prefix filtering, nil-on-no-match, cursor wrap.
- **model_test.go** — `Screen.Update` transitions via a **fake `Agent`** returning a
  scripted `StreamReader`: submit-idle→Running, queue-while-Running, TokenDelta
  accumulation, TurnDone (incl. nil `Message`) / TurnFailed / TurnInterrupted,
  EOF→advance queue (peek head, remove only after start success, no row removed),
  Esc→Interrupting **+ queue cleared +
  only the queued rows dropped even with an interleaved `/help`/error row present**
  (the regression `len(queue)`-truncation would mis-delete),
  `interruptResultMsg{cancelled:false}`, `/clear` idle→Resetting→reopen **success**
  swaps `m.agent` + closes old + clears + Idle, reopen **failure** keeps the old
  agent + `RoleError` + Idle, `/clear` running→no-op+keep input,
  submit-while-Resetting is a no-op, Ctrl+C→bounded close + quit. **`StreamBlocks`
  immediate error** (fake returns `*command.TurnBusyError`): on submit →
  `RoleError` + stays `Idle` + input intact + no `RoleUser` row + no
  `readNext(nil)`; on queued advance → `RoleError` + stays `Idle` + head still
  queued/marked + whole queue preserved + no auto-retry loop. **Slash-complete
  Enter** on a no-op command (`/clear` while Running) keeps input and panel; on a
  runnable one resets+hides. **`buildBlocks` error on submit** (bad `@path`) →
  `RoleError` + input intact + no turn started. Uses a fake `Agent` and a fake
  `OpenAgent` thunk; drive `Update` directly with synthetic msgs (no `teatest`
  dependency).
- **registry_test.go** — Register/Open happy path, `*DuplicateNameError`,
  `*UnknownNameError`, `Names()` sorted.
- **personal-assistant** — extend existing tests: `StreamBlocks`, `Interrupt`, and
  `AcceptsImages` (fake-client based, matching the existing test style). No `Reset`
  to test — the wrapper is immutable; lifecycle is the TUI's via `OpenAgent`.
- **internal/llm** — extend `model_test.go`/`catalog_test.go` for the new
  `AcceptsImages` field: `Model.Spec` carries it onto `ModelSpec`, the zero value
  is `false`, and `ChutesKimiK2()` reports `false` (happy path + boundary, per
  CLAUDE.md's table requirement).

---

## Out of scope

- Auth, gateway, catalog HTTP.
- Session resume / persistence / `/sessions` picker.
- Multi-agent picker UI (registry supports many; only one agent registered today).
- Retaining interrupted partials in the loop's LLM context (loop-level change).
- Tool-call and thinking-block rendering (events exist; rendering deferred).
- Clipboard image paste (terminal-dependent); attachments are `@path` only.
- `@path` tokens containing whitespace (e.g. macOS `Screen Shot ….png`): tokens
  split on whitespace, so spaced paths can't be expressed. Quoting/escape support
  (`@"…"`) is a follow-up; v1 is unquoted `@path` only — note this in `/help`.
- Config file.
