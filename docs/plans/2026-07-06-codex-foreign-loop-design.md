# Codex Foreign Loop Design

**Date:** 2026-07-06
**Status:** Draft

## Goal

Add Codex CLI as a second foreign-loop backend, alongside the existing Claude
Code adapter, so a looprig session can run Codex as either the primary loop or a
subagent while preserving looprig's session/event/journal model.

The integration target is the documented non-interactive CLI surface:

- `codex exec --json <prompt>` for the first turn.
- `codex exec resume <SESSION_ID> --json <prompt>` for follow-up turns.
- `--cd`, `--add-dir`, `--model`, `--profile`, `--sandbox`, and
  `--ask-for-approval` for the process boundary.
- JSONL events from stdout: `thread.started`, `turn.started`,
  `turn.completed`, `turn.failed`, `item.*`, and `error`.

This design intentionally treats Codex as a **foreign loop**, not as an
`inference.Client`. Codex owns its model calls, tool execution, approvals,
sandboxing, local transcript, and resume state. looprig observes and journals the
turn boundary and normalized event stream.

## Sources Checked

- Existing looprig foreign-loop design:
  `harness/docs/plans/2026-06-25-foreign-loop-backend-design.md`.
- Existing implementation: `harness/pkg/foreignloop`,
  `harness/pkg/foreignloop/claude`, `harness/pkg/session/foreign_*_test.go`.
- Installed CLI help from `codex --help`, `codex exec --help`,
  `codex exec resume --help`, and `codex --version`
  (`codex-cli-exec 0.142.5`).
- Official Codex manual fetched on 2026-07-06: CLI command reference
  (`https://developers.openai.com/codex/cli/reference`), non-interactive mode
  (`https://developers.openai.com/codex/noninteractive`), authentication and
  sessions (`https://developers.openai.com/codex/auth`), approvals/security
  (`https://developers.openai.com/codex/agent-approvals-security`), sandboxing
  (`https://developers.openai.com/codex/concepts/sandboxing`), and CLI features
  (`https://developers.openai.com/codex/cli/features`).

## Existing Context

The hard part already exists. `harness/pkg/foreignloop` is a generic actor that
satisfies `loop.Backend`, accepts normal session commands, drives a
`ForeignAgent` one turn at a time, normalizes foreign stream events into
looprig `event.Event`s, and snapshots committed state for restore.

The Claude adapter is intentionally small:

- `claude.NewSpec` resolves operator config into `foreignloop.Spec`.
- `Agent.Spawn` builds argv, starts a subprocess, decodes JSONL stdout, and
  returns a deterministic transcript path.
- The shared actor owns `TurnStarted`, streaming event publish, transcript-first
  commit, `TurnDone`/`TurnFailed`, process-group shutdown, sid locking, and
  restore seeding.

Codex should reuse that shape. It should not add another session type, bypass
the hub, or teach the native loop about Codex.

## Non-Goals

- Do not route Codex internal tool calls through looprig's `PermissionGate`.
  Codex has its own sandbox and approval policy; looprig observes.
- Do not model Codex as a single inference provider.
- Do not depend on undocumented transcript files for correctness in v1.
  Persist looprig's normalized committed events and use Codex's own session id
  only for resuming Codex.
- Do not add a long-lived Codex daemon in v1. One process per turn matches the
  existing foreign-loop lifecycle.
- Do not use `--dangerously-bypass-approvals-and-sandbox` by default.

## Design Choices

### 1. Add a Codex Adapter Package

Create `harness/pkg/foreignloop/codex`, mirroring
`harness/pkg/foreignloop/claude`.

Public surface:

```go
package codex

type SandboxMode uint8

const (
	SandboxReadOnly SandboxMode = iota
	SandboxWorkspaceWrite
	SandboxDangerFullAccess
)

type ApprovalPolicy uint8

const (
	ApprovalUntrusted ApprovalPolicy = iota
	ApprovalOnRequest
	ApprovalNever
)

type SpecConfig struct {
	ExecPath     string
	Model        string
	Profile      string
	Cwd          string
	AdditionalDirs []string
	Sandbox      SandboxMode
	Approval     ApprovalPolicy
	EnvAllow     []string
	Credential   map[string]string
	IgnoreUserConfig bool
	IgnoreRules      bool
	SkipGitRepoCheck bool
}

func NewSpec(parentEnv []string, cfg SpecConfig) (foreignloop.Spec, error)
```

`NewSpec` builds a `foreignloop.Spec` with `AdapterID` effectively `"codex"` at
the composition root. The env path follows Claude's rule: whitelist parent env,
append explicit credentials, never call `os.Environ()` inside the adapter.

`Credential` should usually contain `CODEX_API_KEY` only for automation runs that
do not rely on an existing `CODEX_HOME` login. The adapter must not put secrets
on argv.

### 2. Generalize Foreign Session Binding

Claude can accept a caller-minted `--session-id <uuid>` on the first turn. The
current shared actor therefore mints `sid` at loop creation and stores it on
`LoopStarted.ForeignSID`.

Codex is different. The documented surface exposes:

- start: `codex exec --json <prompt>`
- resume: `codex exec resume <SESSION_ID> --json <prompt>`
- the generated session id appears in the JSONL stream as
  `thread.started.thread_id`

The adapter therefore needs **late-bound sid support**:

```go
type ForeignTurn struct {
	SystemPrompt string
	ForeignSID   string
	StartNew     bool
	Input        []content.Block
	Cwd          string
	Posture      PermissionPosture
}
```

should become either:

```go
type ForeignTurn struct {
	SystemPrompt string
	ForeignSID   string // empty allowed on StartNew for adapter-bound sid agents
	StartNew     bool
	Input        []content.Block
	Cwd          string
	Posture      PermissionPosture
}

type ForeignEvent struct {
	Kind      ForeignKind
	SessionID string // emitted by ForeignInit / ForeignSessionBound
	...
}
```

with a new durable event, or a new explicit binding callback. Prefer the event:

```go
type ForeignSessionBound struct {
	enduring
	loopScoped
	Header
	ForeignSID string `json:"foreign_sid"`
}
```

Rules:

- `LoopStarted.ForeignSID` stays populated for adapters that can prebind ids
  (Claude).
- `LoopStarted.ForeignSID` may be empty for late-bound adapters (Codex).
- On first `ForeignInit{SessionID: ...}` for a late-bound loop, the actor
  publishes `ForeignSessionBound{ForeignSID: sid}` exactly once and records it
  before any terminal.
- Restore reads the sid from `LoopStarted.ForeignSID` first, otherwise from the
  first `ForeignSessionBound` for the loop.
- A restored foreign loop still fails closed if no sid can be recovered.

This is the main core change. It avoids fabricating a Codex id and avoids
depending on `--last`, which would be unsafe under concurrent sessions.

### 3. Add `loop.EngineForeignCodex`

Extend the engine enum:

```go
const (
	EngineNative Engine = iota
	EngineForeignClaude
	EngineForeignCodex
)
```

The current session code treats every non-native engine as "foreign" and calls
the injected builder. That behavior can remain, but tests should pin that both
`EngineForeignClaude` and `EngineForeignCodex` fail closed when no builder is
wired.

The composition root decides which builder to install. For now,
`WithForeignBuilder` accepts one builder pair for the session, so a session can
run one foreign adapter family. If mixed Claude+Codex sessions become necessary,
replace it with a registry keyed by `loop.Engine`.

### 4. Codex Process Contract

The Codex adapter should build argv without a shell.

First turn:

```text
codex exec
  --json
  --cd <cwd>
  --model <model>              # optional
  --profile <profile>          # optional
  --sandbox <mode>
  --ask-for-approval <policy>
  --add-dir <dir> ...          # optional
  [--ignore-user-config]
  [--ignore-rules]
  [--skip-git-repo-check]
  <prompt>
```

Resume turn:

```text
codex exec resume
  --json
  --model <model>              # optional
  [same behavior flags supported by exec resume]
  <foreign_sid>
  <prompt>
```

The installed CLI help shows `codex exec resume` supports `--json`, `--model`,
`--dangerously-bypass-approvals-and-sandbox`, `--ignore-user-config`,
`--ignore-rules`, `--skip-git-repo-check`, `--ephemeral`,
`--output-schema`, and `--output-last-message`, but not all first-turn workspace
flags. The adapter must not assume parity: add an integration/contract spike to
confirm whether `--cd`, `--sandbox`, `--ask-for-approval`, and `--add-dir` are
accepted before or after `resume` in the installed CLI. If resume cannot accept
workspace/permission flags, set them through `-c` config overrides or require the
same profile/config to be active across resume.

Do not use `codex exec resume --last` in production code. It is ambiguous if
multiple Codex loops run concurrently.

### 5. Prompt and System Handling

Claude has a native system-prompt channel. Codex `exec` does not expose an
obvious `--system` flag in the checked help output.

For v1, the Codex adapter should prepend the looprig system prompt to the user
prompt in a stable envelope:

```text
<looprig-system>
...
</looprig-system>

<user-task>
...
</user-task>
```

This is less clean than Claude's `--append-system-prompt`, but it is explicit,
testable, and does not require new Codex CLI behavior. The spec should note that
if a supported Codex system-prompt override becomes available, the adapter should
move to it and keep the envelope only for older versions.

Input block support remains text-only in v1, matching the current Claude adapter.
Images can be added later through Codex `-i/--image` once looprig's block model
and foreign adapter config agree on local file materialization.

### 6. JSONL Decoder and Event Mapping

Codex `--json` stdout is the primary contract. Implement a typed decoder with
allowlisted event shapes. Unknown event or item types are ignored, not fatal.
Malformed JSON returns a typed `DecodeError` and the actor logs/continues where
safe.

Minimum event mapping:

| Codex JSONL | `foreignloop.ForeignEvent` | looprig event |
|---|---|---|
| `thread.started.thread_id` | `ForeignInit{SessionID}` | `ForeignSessionBound` if late-bound |
| `turn.started` | no event | actor already emitted `TurnStarted` |
| `item.started` command execution | `ForeignToolUse` | `ToolCallStarted` |
| `item.completed` command execution | `ForeignToolResult` | `ToolCallCompleted` |
| `item.completed` agent message | `ForeignStepComplete{Message}` | committed `StepDone` |
| `item.completed` reasoning | optional thinking event | `TokenDelta` or skip |
| `item.completed` file change | `ForeignToolUse/Result` or skip | observed tool event if useful |
| `turn.completed` | `ForeignTerminalOK` | `TurnDone` |
| `turn.failed` or `error` | `ForeignTerminalError` | `TurnFailed` |

The exact item schemas must be pinned by contract fixtures generated from the
installed CLI. The official manual documents the event families, not every item
payload field. The decoder must therefore use typed envelopes plus small
per-item structs for the fields it needs.

For final response text, prefer the last completed `agent_message` item. If that
is absent, use `--output-last-message <tempfile>` as a fallback and synthesize a
single assistant message from the file content. This fallback should be covered
by a test.

### 7. Commit Model

Unlike Claude, Codex v1 should not depend on an on-disk transcript decoder for
committed history. Use the JSONL stream as both live and committed source:

- Actor publishes `TurnStarted` before spawn, same as today.
- Decoder accumulates completed assistant messages from `item.completed`.
- On `turn.completed`, actor emits one `StepDone` containing the assistant
  message(s), then `TurnDone`.
- On `turn.failed` or process failure, emit `TurnFailed`. Commit any completed
  assistant message only if it appeared before the failure and the JSONL contract
  marks it completed.

This keeps restore consistent because looprig reconstructs from its own
`StepDone.Messages`, not from Codex transcript files.

Codex's own transcript remains important for resume, but not for looprig replay.

### 8. Restore

Restore uses the same `foreignloop.NewRestored` path after the sid recovery
change.

Recovery order for a foreign root loop:

1. Find root `LoopStarted`.
2. If `LoopStarted.ForeignSID` is non-empty, use it.
3. Otherwise scan root-loop enduring events for the first
   `ForeignSessionBound.ForeignSID`.
4. If still empty, fail closed with `RestoreForeignSIDMissing`.

For restored Codex loops:

- `hasSpawned = true`.
- Next turn uses `codex exec resume <sid> --json`.
- Folded looprig messages are retained for `Snapshot`, TUI replay, and restore
  verification, but are not replayed into Codex. Codex resumes from its own
  session store.

### 9. Config Fingerprint

The composition root must inject foreign-specific fingerprint fields:

- `WorkspaceRoot`: Codex `--cd` root.
- `AgentAdapter`: `"codex"`.
- `PermissionPosture`: encode sandbox and approval policy, for example
  `sandbox=workspace-write;approval=never;profile=ci`.

The loop-derived fingerprint already includes:

- `ModelID`: `cfg.Model.Name`.
- `SystemPromptRev`: digest of `cfg.System`.
- `ToolPolicyRev`: currently irrelevant for foreign observe-only loops, but
  harmless.

Additional fingerprint recommendations:

- Include profile name because it can change model, tools, MCP, sandbox, hooks,
  and instructions.
- Include `IgnoreUserConfig` and `IgnoreRules` because they materially change
  behavior.
- Do not include `ExecPath`; binary paths drift across machines.
- Do not include non-secret env allowlist values. Log them for diagnostics if
  needed.
- Do not include credentials.

If the existing `PermissionPosture` string becomes too overloaded, add a
structured foreign posture field later. For v1, a stable string is enough.

### 10. Security and Trust Boundary

Codex foreign loops are observe-only from looprig's perspective. Codex enforces
its own sandbox and approvals for model-generated shell commands. looprig does
not inspect or approve each internal Codex action before it happens.

Safe defaults for adapter config:

- `SandboxWorkspaceWrite`
- `ApprovalNever` only for non-interactive automation where no prompt can be
  answered; otherwise `ApprovalOnRequest`.
- no `danger-full-access` default.
- no `--dangerously-bypass-approvals-and-sandbox` default.
- no inherited full environment.
- explicit `--cd <cwd>` rooted at a dedicated worktree for concurrent agents.

This means a Codex-backed loop is trusted to the extent of its configured Codex
sandbox. It is safer than a raw unsandboxed subprocess, but it is not looprig's
native permission gate.

### 11. Concurrency and Locks

Keep the existing `(sid,cwd)` lock discipline, with one adjustment:

- Before first-turn binding, lock on `(loopID,cwd)` or another temporary key.
- Once `thread.started` yields `sid`, transition the lock record to `(sid,cwd)`
  for the remainder of the process.
- Resume turns lock `(sid,cwd)` before spawning.

This prevents two looprig processes from resuming the same Codex session in the
same workspace at once. Dedicated worktrees are still the recommended policy for
parallel foreign subagents.

### 12. Error Handling

Add Codex-specific typed errors under `foreignloop/codex` where the failure is
adapter-specific:

- `SpecConfigError`
- `SpawnConfigError`
- `ArgsError`
- `JSONEventError` or wrap existing `foreignloop.DecodeError`

Reuse shared errors where possible:

- `foreignloop.SpawnError`
- `foreignloop.ForeignExitError`
- `foreignloop.ForeignResultError`
- `foreignloop.ForeignSessionBusyError`
- `foreignloop.SnapshotError`

Process stderr should be drained to avoid deadlock. It may be captured into a
bounded buffer for diagnostics, but must not be emitted into durable events if it
can contain secrets.

### 13. Testing

Unit tests:

- `codex.NewSpec` validates required fields and env whitelisting.
- argv builder for first turn and resume turn, including optional flags.
- sandbox/approval enum string mapping.
- JSONL decoder fixtures:
  - `thread.started`
  - agent message item
  - command execution started/completed
  - file change item
  - turn completed with usage
  - turn failed
  - error
  - unknown item type
  - malformed JSON
- mapper correlation for command execution ids.
- final-message fallback from `--output-last-message`.

Session/actor tests with fake Codex `ForeignAgent`:

- first turn late-binds sid and publishes `ForeignSessionBound`.
- `LoopStarted.ForeignSID` remains empty for late-bound first turn.
- `RunSubagent` returns final Codex text.
- restore recovers sid from `ForeignSessionBound`.
- restore fails closed if no sid was ever bound.
- quota/depth behavior remains unchanged.
- concurrent resume of same `(sid,cwd)` fails with `ForeignSessionBusyError`.

Integration/contract tests, gated behind an environment variable:

- run `codex exec --json --sandbox read-only --ask-for-approval never "..."`.
- assert `thread.started` contains a stable session id field.
- assert `codex exec resume <sid> --json "..."` continues the same session.
- assert argv accepted by resume path for `--cd`/sandbox/approval, or document
  the required config override fallback.
- assert command/file-change item shapes for a harmless read-only command.

### 14. Implementation Order

1. Add `ForeignSessionBound` event and restore discovery support.
2. Relax/gate foreign actor sid minting so a `ForeignAgent` can be prebound
   (Claude) or late-bound (Codex).
3. Add `EngineForeignCodex` and fail-closed tests.
4. Add `foreignloop/codex` pure config, env, argv, and decoder tests.
5. Add Codex `Agent.Spawn`.
6. Add fake-agent session tests for primary/subagent/restore.
7. Add opt-in real CLI contract tests.
8. Wire the composition root/swarm catalog entry.

## Open Questions

- Does `codex exec resume` accept `--cd`, `--sandbox`, `--ask-for-approval`, and
  `--add-dir` in the installed CLI when flags are placed before or after
  `resume`? If not, which `-c` overrides are required?
- Are `item.completed` command/file-change schemas stable enough to map to
  `ToolCallStarted`/`ToolCallCompleted`, or should v1 only commit final assistant
  messages and terminals?
- Is there a supported Codex system-prompt override? If yes, use it instead of
  the prompt envelope.
- Should mixed foreign sessions (Claude and Codex in one looprig session) be in
  scope? If yes, replace the single `WithForeignBuilder` with an engine-keyed
  registry.
