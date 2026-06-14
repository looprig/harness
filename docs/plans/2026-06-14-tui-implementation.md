# Nexus CLI TUI — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (or
> superpowers:subagent-driven-development) to implement this plan task-by-task.
> Every task is TDD: write the failing test, run it red, implement minimally, run
> it green, then commit. The authoritative spec is
> `docs/plans/2026-06-13-tui-design.md` — read the referenced section before each
> task. This plan gives the test-first scaffolding and the exact order.

**Goal:** A full-screen, streaming, multimodal Bubbletea chat TUI that drives a
locally-constructed agent, selected by name, reusing the loop session directly.

**Architecture:** A consumer-defined `tui.Agent` interface (Dependency Inversion);
`*personalassistant.Assistant` satisfies it (immutable, lock-free). `/clear` is
agent *lifecycle* — the TUI opens a fresh agent via an `OpenAgent` thunk and swaps
it on the single-threaded Elm loop (no mutex, no `Reset`). A generic
`internal/registry` maps name→constructor at the composition root (`cmd/cli`).

**Tech Stack:** Go 1.26, `bubbletea`/`bubbles`/`lipgloss`/`glamour` (Charm),
stdlib elsewhere. Module: `github.com/inventivepotter/urvi`.

## Conventions (every task)

- **Module root:** all import paths are `github.com/inventivepotter/urvi/...`.
- **Test command:** `CGO_ENABLED=0 go test -race ./<pkg>/...` — must pass.
- **Tests are table-driven** with `t.Parallel()`, covering happy/boundary/error/edge
  (CLAUDE.md). All package-level errors are concrete structs, `errors.As`-able.
- **No `_`-swallowed errors.** No `any`/`interface{}` except serialization seams.
- **Before each commit:** `CGO_ENABLED=0 go build -trimpath ./... && go vet ./...`.
  Run `make secure` at the end of each phase (vet + staticcheck + gosec + govulncheck).
- **Commit style:** conventional commits, e.g. `feat(tui): buildBlocks @path parsing`.
  End the commit body with the Co-Authored-By trailer.
- **Don't push or PR** unless asked.

---

## Phase 0 — Dependencies

### Task 0: Add Charm deps + amend CLAUDE.md

**Files:**
- Modify: `go.mod`, `go.sum` (via `go get`)
- Modify: `CLAUDE.md` (approved-deps list), `AGENTS.md` (mirror)

**Steps:**
1. `go get github.com/charmbracelet/bubbletea@latest github.com/charmbracelet/bubbles@latest github.com/charmbracelet/lipgloss@latest github.com/charmbracelet/glamour@latest`
2. `go mod tidy`
3. Add to CLAUDE.md's "Approved external packages" list (and mirror in AGENTS.md):
   - `github.com/charmbracelet/bubbletea` — TEA runtime for the CLI TUI
   - `github.com/charmbracelet/bubbles` — textarea + viewport widgets
   - `github.com/charmbracelet/lipgloss` — terminal styling/layout
   - `github.com/charmbracelet/glamour` — markdown → ANSI rendering
4. `CGO_ENABLED=0 go build ./...` — Expected: PASS (deps resolve).
5. Commit: `build(deps): add bubbletea/bubbles/lipgloss/glamour; amend approved deps`

---

## Phase 1 — Foundational core (no Charm imports)

### Task 1: `llm.Model.AcceptsImages` modality flag

Design: *Model capability constraint*. Single source of truth for modality.

**Files:**
- Modify: `internal/llm/llm.go` (add field to `ModelSpec`)
- Modify: `internal/llm/model.go` (add field to `Model`, carry in `Spec`)
- Test: `internal/llm/model_test.go`, `internal/llm/catalog_test.go`

**Step 1 — failing test** (`model_test.go`): add a table case asserting
`Model{AcceptsImages: true}.Spec("k","s").AcceptsImages == true`, and zero-value
`Model{}.Spec(...).AcceptsImages == false`. In `catalog_test.go` assert
`ChutesKimiK2().AcceptsImages == false`.

**Step 2 — run red:** `go test -race ./internal/llm/...` → FAIL (unknown field).

**Step 3 — implement:**
- `ModelSpec` gains `AcceptsImages bool` (place near `Model string`/`System`).
- `Model` gains `AcceptsImages bool`.
- `Model.Spec` sets `AcceptsImages: m.AcceptsImages` in the returned `ModelSpec`.
- `ChutesKimiK2()` leaves it zero (`false`) — no change needed beyond the field.

**Step 4 — run green:** `go test -race ./internal/llm/...` → PASS.

**Step 5 — commit:** `feat(llm): add Model/ModelSpec AcceptsImages modality flag`.

---

### Task 2: `internal/registry` generic registry

Design: *internal/registry (generic)*.

**Files:**
- Create: `internal/registry/registry.go`, `internal/registry/errors.go`
- Test: `internal/registry/registry_test.go`, `internal/registry/errors_test.go`

**Step 1 — failing tests** (`registry_test.go`): table-driven, using
`Registry[string]` (a neutral test type so the package stays domain-free):
- Register+Open returns the constructed value (constructor returns `"hi"`).
- Register same name twice → `*DuplicateNameError{Name}` (assert via `errors.As`).
- Open unknown name → `*UnknownNameError{Name, Known}` with `Known` sorted.
- `Names()` returns registered names sorted.
- Open propagates a constructor error unchanged.

`errors_test.go`: assert `Error()` strings are non-empty and stable.

**Step 2 — run red:** `go test -race ./internal/registry/...` → FAIL (no package).

**Step 3 — implement:**
- `errors.go`: `type DuplicateNameError struct{ Name string }` and
  `type UnknownNameError struct{ Name string; Known []string }`, each with a
  value-receiver `Error() string`.
- `registry.go`: `type Registry[T any] struct{ m map[string]func(context.Context)(T,error) }`,
  `New[T any]() *Registry[T]` (init map), `Register`, `Open`, `Names()` (sorted via
  `slices.Sorted`/`sort.Strings`). `Open` returns the zero `T` + `*UnknownNameError`
  (populate `Known` from `Names()`).

**Step 4 — run green:** `go test -race ./internal/registry/...` → PASS.

**Step 5 — commit:** `feat(registry): generic name→constructor registry with typed errors`.

---

### Task 3: personal-assistant surface — `StreamBlocks`, `Interrupt`, `AcceptsImages`

Design: *Agent surface — personal-assistant changes (additive)*. Immutable; no mutex,
no `Reset`. Capture `acceptsImages` in `newWithClient`.

**Files:**
- Modify: `agents/personal-assistant/agent.go`
- Test: `agents/personal-assistant/agent_test.go` (extend, matching `fake_test.go` style)

**Step 1 — failing tests** (`agent_test.go`), fake-client based (see `fake_test.go`):
- `StreamBlocks` with a scripted `fakeLLM` (two text chunks) yields
  `TurnStarted`, `TokenDelta×2`, a terminal `TurnDone`, then `io.EOF` (read the
  `*llm.StreamReader[event.Event]` to completion).
- `Interrupt` on an in-flight turn returns `(true, nil)` (use `fakeLLM.hold` to keep
  the turn running, start a stream, then `Interrupt`).
- `AcceptsImages` returns the spec's value: build the assistant via `newWithClient`
  with a spec where `AcceptsImages` is true/false and assert the method echoes it.

**Step 2 — run red:** `go test -race ./agents/personal-assistant/...` → FAIL (methods
undefined / field missing).

**Step 3 — implement** (per design code block):
- Add `acceptsImages bool` to `Assistant`; set it in `newWithClient` from
  `spec.AcceptsImages`.
- `func (a *Assistant) StreamBlocks(ctx, blocks []content.Block) (*llm.StreamReader[event.Event], error)`
  → `return a.session.Stream(ctx, blocks)`.
- `func (a *Assistant) Interrupt(ctx) (bool, error)` → `return a.session.Interrupt(ctx)`.
- `func (a *Assistant) AcceptsImages() bool` → `return a.acceptsImages`.
- Leave `Send`/`Stream`/`Close`/`userBlocks` untouched. No mutex.

**Step 4 — run green:** `go test -race ./agents/personal-assistant/...` → PASS.

**Step 5 — commit:** `feat(personal-assistant): add StreamBlocks/Interrupt/AcceptsImages (immutable)`.

**End of Phase 1:** run `make secure` — must pass before moving on.

---

## Phase 2 — tui types, errors, and `buildBlocks` (no Charm imports)

> The `tui` package is created here. Keep Phase-2 files free of Charm imports so the
> domain logic (errors, block-building) is testable without a terminal.

### Task 4: tui core types — `Agent`, `OpenAgent`, `DisplayMessage`

Design: *Agent interface (consumer-defined)*, *DisplayMessage*.

**Files:**
- Create: `tui/agent.go` (interface + `OpenAgent`), `tui/message.go` (`DisplayRole`,
  `DisplayMessage`)
- Test: `tui/message_test.go`

**Step 1 — failing test** (`message_test.go`): assert the `DisplayRole` iota order
(`RoleUser=0 … RoleInterrupted`) and that a `DisplayMessage{Role: RoleInterrupted}`
has `Blocks == nil`. (Compile-asserting the types exist is the real point.)

**Step 2 — run red:** `go test -race ./tui/...` → FAIL (no package).

**Step 3 — implement:**
- `tui/agent.go`: the `Agent` interface (`StreamBlocks`, `Interrupt`, `Close`,
  `AcceptsImages`) and `type OpenAgent func(context.Context) (Agent, error)`.
  Import `internal/content`, `internal/agent/loop/event`, `internal/llm`.
- `tui/message.go`: `type DisplayRole uint8` with the five constants;
  `type DisplayMessage struct{ Role DisplayRole; Blocks []content.Block }`.

**Step 4 — run green:** PASS. **Step 5 — commit:** `feat(tui): Agent interface, OpenAgent, DisplayMessage`.

---

### Task 5: tui typed errors

Design: *Typed errors (tui package)*.

**Files:**
- Create: `tui/errors.go`
- Test: `tui/errors_test.go`

**Step 1 — failing test:** table over each error type; assert `Error()` non-empty and
that the struct fields round-trip (e.g. `(&AttachmentTooLargeError{Path:"p",Size:9,Max:5}).Error()`
mentions the path). Assert each is `errors.As`-able from a wrapped error.

**Step 2 — run red:** FAIL.

**Step 3 — implement** these concrete structs (value-receiver `Error()`):
`EmptyInputError`, `UnsupportedAttachmentError{Ext}`, `ImageUnsupportedError{Ext}`,
`DeniedAttachmentError{Path, Reason}`, `AttachmentTooLargeError{Path, Size, Max int64}`,
`AttachmentNotFoundError{Path string; Cause error}` (with `Unwrap() error`),
`AttachmentReadError{Path string; Cause error}` (with `Unwrap`).

**Step 4 — green. Step 5 — commit:** `feat(tui): typed attachment/input errors`.

---

### Task 6: `tui/blocks.go` — `buildBlocks` (@path attachments)

Design: *Block building — `@path` attachments* (read it fully). Order:
clean + denylist → **classify (ext + allowImages)** → open `O_NOFOLLOW` → fd-stat →
`LimitReader` read → build block.

**Files:**
- Create: `tui/blocks.go`
- Test: `tui/blocks_test.go`, `tui/blocks_fuzz_test.go`

**Step 1 — failing tests** (`blocks_test.go`), build fixtures under `t.TempDir()`:
| case | input | expect |
|---|---|---|
| text only | `"hello world"` | one `*content.TextBlock{Text:"hello world"}` |
| empty | `"   "` | `*EmptyInputError` |
| single image, allowed | `"@a.png"`, allowImages=true | one `*content.ImageBlock{MediaType: image/png}` |
| image, disallowed | `"@a.png"`, allowImages=false | `*ImageUnsupportedError{Ext:".png"}` |
| plaintext | `"@a.txt"` | `*content.TextBlock{Text:"[a.txt]\n<contents>"}` |
| `.svg`→plaintext | `"@a.svg"` | `*content.TextBlock` (not image) |
| unknown ext | `"@a.xyz"` | `*UnsupportedAttachmentError{Ext:".xyz"}` (no file opened) |
| missing file | `"@nope.txt"` | `*AttachmentNotFoundError` |
| directory | `"@<tmpdir>"` (a dir) | non-regular rejected (e.g. `*DeniedAttachmentError` or read error — assert error, not nil) |
| symlink | `"@link.txt"`→file | rejected (open w/ `O_NOFOLLOW` fails) |
| denied basename | `"@.env"` | `*DeniedAttachmentError` |
| denied segment | `"@.ssh/x.txt"` | `*DeniedAttachmentError` |
| denied ext | `"@k.pem"` | `*DeniedAttachmentError` |
| too big | a 6 MB `.txt` | `*AttachmentTooLargeError` |
| prompt + attach order | `"see @a.txt now"` | `[TextBlock("see now"), TextBlock("[a.txt]…")]` |

Helper: a `writeFile(t, dir, name, sizeOrContent)`; for symlink use `os.Symlink`.

**Step 2 — run red:** `go test -race ./tui/...` → FAIL (no `buildBlocks`).

**Step 3 — implement** `func buildBlocks(input string, allowImages bool) ([]content.Block, error)`
per design. Key details:
- tokenize on `strings.Fields`; `@`-prefixed tokens of len>1 are attachments; rejoin
  the rest as the leading prompt; leading `*content.TextBlock` omitted when empty
  with attachments present.
- denylist: lower-case the cleaned path's segments/basename/ext; sets per design
  (segments `.ssh .aws .gcloud .gnupg .kube`; basenames `.env .env.* .npmrc .netrc
  .pypirc .dockercfg id_rsa id_dsa id_ecdsa id_ed25519`; exts `.env .pem .key .p12
  .pfx .jks .keystore`). Use `path.Match`/manual suffix for `.env.*`.
- classify: image exts `{.png,.jpg,.jpeg,.gif,.webp}` → if `!allowImages` →
  `*ImageUnsupportedError`; plaintext exts (design list incl `.svg`); else
  `*UnsupportedAttachmentError`.
- open: `os.OpenFile(clean, os.O_RDONLY|syscall.O_NOFOLLOW, 0)`; map errors
  (`os.IsNotExist`→NotFound; `ELOOP`/syscall→Denied). `defer f.Close()`.
- fd-stat: `fi, _ := f.Stat()`; reject `!fi.Mode().IsRegular()`; reject
  `fi.Size() > maxBytes` (const `maxBytes int64 = 5 << 20`).
- read: `io.ReadAll(io.LimitReader(f, maxBytes+1))`; reject if `len > maxBytes`.
- build `*content.ImageBlock{MediaType: mediaTypeByExt(ext), Source: content.ImageSource{Data: b}}`
  or `*content.TextBlock{Text: "[" + filepath.Base(clean) + "]\n" + string(b)}`.

**Step 4 — run green:** PASS. Add `blocks_fuzz_test.go`:

**Step 5 — fuzz target** `FuzzBuildBlocks`: seed with `"hi"`, `"@a.txt"`, `"@"`,
`"@../x"`, `"x\x00y"`, a long token. In the fuzz body, **rewrite every `@path`
token to `@<tmpDir>/<base>`** (base = `filepath.Base(filepath.Clean(token[1:]))`;
drop the token if base escapes, i.e. is `.`/`..`/empty) before calling `buildBlocks`,
so no host path is opened. Assert: never panics; on error the result is one of the
typed errors (type-switch, fail on an unknown error type). Run a short smoke:
`go test -run x -fuzz FuzzBuildBlocks -fuzztime 20s ./tui` → no crashers.

**Step 6 — commit:** `feat(tui): buildBlocks @path parsing with classify-before-open hardening`.

**End of Phase 2:** `make secure` (note: gosec G304 file-open will flag `buildBlocks`;
add a justified `// #nosec G304 -- user-selected local path, validated by denylist +
O_NOFOLLOW + fd stat` annotation on the `OpenFile` line and re-run).

---

## Phase 3 — tui presentation components (Charm)

### Task 7: `tui/styles/styles.go`

Design: *Components* (styles bullet). Exported lipgloss styles + `Dot`/`DotWidth` +
a glamour `MdStyle` config. Minimal test: `TestStyles` asserts `DotWidth ==
lipgloss.Width(Dot)` and that a renderer builds from `MdStyle` without error.
Commit: `feat(tui): lipgloss/glamour styles`.

### Task 8: `tui/components/statusline.go`

Pure function `RenderStatusLine(status Status) string`. **Note:** `Status` lives in
the `tui` package (Task 13). To avoid an import cycle, define `Status` and its
constants in `tui` and have `components` take it as a parameter — OR keep
`RenderStatusLine` in package `tui`. **Decision:** put status-line rendering in
package `tui` (`tui/statusline.go`) since it depends on `tui.Status`; the other
components stay in `tui/components`. Test table: Idle→"", Running→"thinking…",
Interrupting→"interrupting…", Resetting→"clearing…". (Do Task 8 after Task 13 defines
`Status`, or define `Status` first.) Commit: `feat(tui): status line rendering`.

### Task 9: `tui/components/slashcomplete.go`

Design: *Slash commands*, *Components* (slashcomplete bullet). `SlashCmd{Name,Desc}`,
`var slashCmds`, `SlashComplete` with a filtered list + wrapping cursor;
`NewSlashComplete(prefix) *SlashComplete` returns `nil` when no command matches.
Methods: `Selected() SlashCmd`, `Up()`, `Down()` (wrap), `View() string`.
Test table: prefix `/c`→only `/clear`; `/`→both; `/zz`→nil; cursor wraps past ends.
Commit: `feat(tui): slash-command completion`.

### Task 10: `tui/components/render.go`

Design: *Components* (render bullet) + *DisplayMessage* table. Pure helpers:
- `renderMD(md string) string` (glamour via styles, dot prefix, cache keyed by input
  is optional here; the index cache lives in ChatHistory).
- `wrapText`/`wordWrap(s string, width int) string`.
- `renderMessages(msgs []DisplayMessage, stream string, queued map[int]bool, width int) string`:
  dispatch on `DisplayRole` and each block's concrete type; `*content.TextBlock`→md,
  `*content.ImageBlock`→`[image: <media type>, <n> bytes]` placeholder; `RoleInterrupted`
  (nil Blocks)→`└─ interrupted`; append the live `stream` as a trailing assistant
  entry; mark rows whose index ∈ `queued` with a "(queued)" suffix.
Tests: each role renders; queued marker present; image placeholder; wrap boundaries
(empty, exact width, long word). Commit: `feat(tui): message + markdown rendering`.

### Task 11: `tui/components/input.go`

`InputBox` wrapping `textarea.Model`: fixed 3-line height, `CharLimit(0)`, line
numbers off, no placeholder newlines. Methods `Value() string`, `Reset()`,
`SetValue(string)`, `Resize(w int)`, `Update(tea.Msg)`, `View() string`, `Focus()`.
Test: construct, `SetValue`/`Value` round-trip, `Reset` clears. Commit:
`feat(tui): input box component`.

### Task 12: `tui/components/history.go`

`ChatHistory` wrapping `viewport.Model` + render cache `map[int]string` keyed by
history index. `Refresh(msgs, stream, queued, width)` re-renders (using `renderMessages`,
caching per-index markdown) and auto-scrolls to bottom only if already at bottom;
`Clear()` empties messages + cache; `Resize(w,h int)` (invalidates cache);
`Update(tea.Msg)`; `View() string`. Test: construct; `Clear` empties; `Resize`
invalidates cache; bottom-scroll behavior with a stub. Commit:
`feat(tui): scrolling chat history with render cache`.

**End of Phase 3:** `make secure`.

---

## Phase 4 — Screen (the Elm model)

> Drive `Screen.Update` directly with synthetic msgs in tests (no `teatest`). Build a
> **fake `Agent`** and a **fake `OpenAgent`**; for streams return
> `llm.NewStreamReader(next, nil)` where `next` yields scripted `event.Event` values
> then `io.EOF`.

### Task 13: Screen scaffolding — struct, `Status`, msgs, `New`, `Init`

Design: *Screen (the Elm model)*, *Status*, *Streaming — internal messages*,
*Init, View, and window resize*.

**Files:** Create `tui/screen.go`, `tui/status.go`, `tui/messages.go`.
Test: `tui/screen_test.go` (start it here; grows through Task 17).

**Step 1 — failing test:** `New(ctx, fakeAgent, fakeOpen)` returns a `Screen` with
`status == StatusIdle`, `agent` set, `openAgent` set; `Init()` returns a non-nil
`tea.Cmd` (batched blink + a `systemReadyMsg`-emitting cmd). Assert `Status` constant
order.

**Step 2 — red. Step 3 — implement** the `Screen` struct (design fields incl.
`openAgent OpenAgent`), `Status` + 4 constants (`tui/status.go`), the internal msg
types (`tui/messages.go`: `eventMsg`, `streamEOFMsg`, `streamErrMsg`,
`interruptResultMsg`, `reopenResultMsg{agent Agent; err error}`, plus a
`systemReadyMsg`), `New`, and `Init` (returns `tea.Batch(textarea.Blink, func()
tea.Msg{return systemReadyMsg{}})`). Add `func (s Screen) Agent() Agent { return
s.agent }`. **Step 4 — green. Step 5 — commit:** `feat(tui): Screen scaffolding, Status, messages`.

(Do Task 8 `RenderStatusLine` now if not already — it needs `Status`.)

### Task 14: stream/lifecycle commands + `startTurn`

Design: *Streaming — tea.Cmd recursion*, the `startTurn` helper, *Flow*.

**Files:** `tui/commands.go` (cmds), extend `tui/screen.go` (`startTurn`, `appendError`).
Test: `tui/commands_test.go`.

**Step 1 — failing tests:**
- `readNext` over a scripted reader yields `eventMsg` for each event then
  `streamEOFMsg` at `io.EOF` (and `streamErrMsg` on a non-EOF error).
- `startTurn(blocks)` on a fake agent whose `StreamBlocks` succeeds sets
  `status=Running`, `reader!=nil`, returns `(cmd,true)`.
- `startTurn` on a fake whose `StreamBlocks` returns `*command.TurnBusyError`
  appends a `RoleError`, leaves `status=Idle`, `reader==nil`, returns `(nil,false)`.

**Step 2 — red. Step 3 — implement** `readNext`, `interruptTurn` (2 s timeout),
`reopenAgent` (5 s, calls `OpenAgent`), `closeAgent` (5 s `Background`), and the
`startTurn`/`appendError` methods per design. **Step 4 — green. Step 5 — commit:**
`feat(tui): stream/lifecycle commands and startTurn`.

### Task 15: `Update` — keys

Design: *Keys* table + *Slash commands* (shared dispatch) + *Input queue*.

**Files:** `tui/update.go` (Update + a `handleKey` + a shared `dispatchSlash`).
Test: extend `tui/screen_test.go`.

**Step 1 — failing tests** (drive `Update` with `tea.KeyMsg`):
- Enter on plain text while Idle → appends `RoleUser`, returns a non-nil cmd,
  `status=Running` (fake stream).
- Enter while Running → queues: appends `RoleUser`, `len(queue)==1`, input reset.
- Enter on bad `@path` (Idle) → `RoleError`, **input intact**, `status=Idle`, no turn.
- Enter `/help` → appends a `RoleSystem` help row.
- Enter `/clear` while Idle → `status=Resetting`, returns reopen cmd.
- Enter `/clear` while Running → no-op, input intact.
- Esc while Running → `status=Interrupting`, queue cleared + queued rows removed,
  returns a cmd.
- Ctrl+C → returns a `tea.Sequence` (quit). (Assert it returns a non-nil cmd; deeper
  assertion optional.)
- Slash-complete visible + Enter on a no-op (`/clear` while Running) → input + panel
  intact; on a runnable one → reset + hide.

**Step 2 — red. Step 3 — implement** `Update` key handling per the Keys table,
routing typed-`/` Enter and slash-complete Enter through one `dispatchSlash` that
returns `(cmd, ran bool)`; only reset/hide on `ran`. Submit path calls `buildBlocks`
then `startTurn` (Idle) or queues (Running) per *Flow*/*Input queue*; gate images on
`agent.AcceptsImages()`. Esc clears queue + removes `queue[i].DisplayIndex` rows +
invalidates history cache. **Step 4 — green. Step 5 — commit:** `feat(tui): key handling + slash dispatch`.

### Task 16: `Update` — event/stream msgs

Design: *Flow* (eventMsg / streamEOFMsg / streamErrMsg).

**Step 1 — failing tests** (drive `Update` with synthetic `eventMsg{...}` etc.):
- `eventMsg{TokenDelta{Chunk:*TextChunk}}` appends to `stream`; a `ThinkingChunk` is
  skipped.
- `eventMsg{TurnDone{Message}}` appends `RoleAssistant` from `Message.Blocks`; nil
  `Message` → empty assistant turn (no panic).
- `eventMsg{TurnFailed{Err}}` appends `RoleError`.
- `eventMsg{TurnInterrupted}` with non-empty `stream` flushes a partial
  `RoleAssistant` then appends a `RoleInterrupted` tombstone.
- `streamEOFMsg`: `status=Idle`; with a queued item it **peeks**, starts it, removes
  the head on success; on a start failure (fake busy) the head stays queued,
  `status=Idle`, no `readNext(nil)`.
- `streamErrMsg` behaves like EOF + a `RoleError`.

**Step 2 — red. Step 3 — implement** the event/stream arms per *Flow* (peek/remove
queue semantics). **Step 4 — green. Step 5 — commit:** `feat(tui): event + stream message handling`.

### Task 17: `Update` — interrupt/reopen msgs + View + resize

Design: *Flow* (interruptResultMsg, reopenResultMsg), *Init, View, and window resize*.

**Step 1 — failing tests:**
- `interruptResultMsg{err!=nil}` → `RoleError` + `status=Running`.
- `interruptResultMsg{cancelled:true,err:nil}` → stays `Interrupting`.
- `reopenResultMsg{err!=nil}` → keeps old agent, `RoleError`, `status=Idle`.
- `reopenResultMsg{agent:new,err:nil}` → `m.agent==new`, history/stream/queue cleared,
  `status=Idle`, returns a `closeAgent` cmd for the old one.
- `tea.WindowSizeMsg` sets width/height, `ready=true`, resizes components.
- `View()` returns "" until `ready`, then a non-empty composite.
- `systemReadyMsg` appends the `RoleSystem` "session ready" row.

**Step 2 — red. Step 3 — implement** these arms, `View()` (vertical join: history,
status line, slash panel if non-nil, input; height math), and `WindowSizeMsg`.
**Step 4 — green. Step 5 — commit:** `feat(tui): interrupt/reopen handling, View, resize`.

**End of Phase 4:** `go test -race ./tui/...` and `make secure`.

---

## Phase 5 — Composition root

### Task 18: `cmd/cli/main.go` + remove `cmd/urvi`

Design: *cmd/cli/main.go*.

**Files:** Create `cmd/cli/main.go`; delete `cmd/urvi/main.go` (and the dir).
Test: `cmd/cli/main_test.go` (light — see below).

**Step 1 — failing test:** factor the wireable bits into testable helpers, e.g.
`func agentName(args []string) string` (default `"personal-assistant"`) and
`func buildRegistry() *registry.Registry[tui.Agent]`. Test `agentName` table
(no args→default; first non-flag arg wins) and that `buildRegistry().Names()`
contains `"personal-assistant"`. Don't start the TUI in tests.

**Step 2 — red. Step 3 — implement** `main()` per design steps: `signal.NotifyContext`,
parse name, build registry registering `"personal-assistant"`→`personalassistant.New`,
`open := func(c)(tui.Agent,error){return reg.Open(c,name)}`, `agent,err := open(ctx)`
(on `*UnknownNameError` print `reg.Names()` to stderr + exit non-zero),
`screen := tui.New(ctx, agent, open)`, `prog := tea.NewProgram(screen, tea.WithAltScreen())`,
`go func(){<-ctx.Done(); prog.Quit()}()`, `final, runErr := prog.Run()`, bounded
backstop close of `final.(tui.Screen).Agent()` (fallback to `agent`), then report
`runErr` + exit non-zero. Remove `cmd/urvi`.

**Step 4 — green** + `CGO_ENABLED=0 go build -trimpath ./...`. **Step 5 — commit:**
`feat(cli): cmd/cli composition root; remove cmd/urvi stub`.

---

## Phase 6 — Finalize

### Task 19: Full verification

1. `CGO_ENABLED=0 go build -trimpath ./...` → PASS.
2. `CGO_ENABLED=0 go test -race ./...` → PASS.
3. `go test -run x -fuzz FuzzBuildBlocks -fuzztime 30s ./tui` → no crashers.
4. `make secure` → PASS (resolve any new gosec findings with justified `#nosec` or a
   fix).
5. Manual smoke (optional, needs a key): `LLM_API_KEY=… go run ./cmd/cli` — type a
   message, stream, Esc to interrupt, `/clear`, Ctrl+C to quit.
6. Update the design doc's status note if desired; commit any final docs.

Commit (if anything changed): `chore(tui): finalize — full build/test/secure green`.

---

## Notes / gotchas for the implementer

- **Import-cycle care:** `RenderStatusLine` depends on `tui.Status`, so it lives in
  package `tui`, not `tui/components`. `tui/components` must not import `tui`. The
  components take primitives/`content.Block`/widths, not `Screen`.
- **`event` vs `command`:** events are in `internal/agent/loop/event` (value types
  `event.TurnStarted` etc.); `*command.TurnBusyError` is in
  `internal/agent/loop/command`. The session signatures already take `[]content.Block`.
- **StreamReader fakes:** `llm.NewStreamReader(next, nil)` — `next` returns
  `(event.Event, error)`, `io.EOF` to end.
- **gosec G304:** `buildBlocks` opens a user-named path; annotate with a justified
  `#nosec G304` (validated by denylist + `O_NOFOLLOW` + fd-stat) — do not disable the
  linter globally.
- **Bubbletea value model:** `Screen` is a value type; `Update` has a pointer-or-value
  receiver that returns the (modified) model. `New` returns `Screen` by value;
  `prog.Run()` returns the final model for the backstop close.
