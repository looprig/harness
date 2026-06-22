# Subagent card rendering — design (v3, post-review, correlation corrected)

Date: 2026-06-22
Status: implementation-ready
Repos: `looprig` (primary), `swe` (spawner pass-through)

## Problem

When the orchestrator spawns a subagent, the TUI renders it badly: the spawning step shows a
generic working-word ("Toiling") instead of `Subagent(<agent>)`; each child step commits a
separate content-less `▸ <agent>: done` line; and those lines commit *before* the parent's
reasoning, so they float above it out of causal order.

## Goal

Render each `Subagent` tool call as a `●`-level card `Subagent(<agent>)  "<task>"` with the
subagent's tool calls + final result nested as `⎿` children, committed as one block. Detail
level: **tools + result only** (no subagent thinking/narration). Replace the `▸ done` path.

## Decision 1 — paint from ENDURING events only (review issue #1)

`DefaultEventFilter` (pkg/tui/agent.go:72) delivers **Ephemeral** events from the **primary
loop only**, **Enduring** from **all loops**. A subagent's `ToolCallStarted/Completed` and
`TokenDelta` are **Ephemeral** → never delivered; widening the filter would leak child token
firehoses. So every piece of the card derives from ENDURING child events, which are already
delivered (Enduring = all loops) and already replayed on cold restore. **No filter/hub change.**

| Card piece            | Source (all Enduring, all-loops delivered)                          |
|-----------------------|---------------------------------------------------------------------|
| agent name            | child `LoopStarted.AgentName`                                       |
| task message          | child's first `TurnStarted.Message` (truncated, newline-stripped)  |
| nested tool calls     | each child `StepDone.Messages` — `splitStepGroup` + a PURE `storedStepToolCard(use, results)` (see Decision 3a) |
| "N steps"             | count of child `StepDone` events                                    |
| terminal              | child `TurnDone` / `TurnFailed` / `TurnInterrupted`                |
| result summary        | the parent `Subagent` tool's `ToolResultMessage` text at the orchestrator's `StepDone` (the hand-back), truncated |

## Decision 2 — correlation via the durable provider tool-use ID (review issue #2; no identity changes)

The runner mints a `ToolExecutionID` UUID per call (`resolved.callID`) AND holds the provider's
`content.ToolUseBlock.ID` string (`resolved.block.ID`). Two different namespaces:

- `ToolExecutionID` (UUID) rides on `ToolCallStarted` (Ephemeral) AND on `PermissionRequested`
  (Enduring — so it IS persisted *when a call gates*). But it is NOT a **universal** durable
  link: a call that never prompts persists no `ToolExecutionID`, and nothing durably maps
  `ToolExecutionID → ToolUseBlock.ID`. So execution ids cannot correlate every card on restore.
  Keep `ToolExecutionID` for **gate routing only**.
- `ToolUseBlock.ID` (provider string) is what the **durable** `StepDone.Messages` carries for
  EVERY call (AIMessage tool-use blocks + `ToolResultMessage.ToolUseID`). This is the universal
  id that both survives restore AND appears at the parent `StepDone`.

So correlate on the provider tool-use ID, threaded as a **dedicated relationship field** — NOT
by extending `identity.Coordinates`, `identity.Cause`, `event.Header`, or `loop.Provenance`:

1. **Runner** (pkg/loop/runner.go `runOne`): inject the call's provider id into the per-call
   ctx next to the existing `withCallID`, exposed by a new exported
   `loop.ToolUseIDFrom(ctx) (string, bool)` (writer `withToolUseID` stays internal).
2. **Subagent tool** (pkg/tools/subagent.go): read `loop.ToolUseIDFrom(ctx)` and forward the
   string through the `Spawner.Spawn(... , parentToolUseID string)` signature.
3. **swe spawner** (swarms/swe/spawner.go): forward it to `Session.RunSubagent(... , parentToolUseID string)`.
4. **Session**: `RunSubagent` passes it through a **private loop-creation path** (e.g.
   `newLoop(parent, cfg, parentToolUseID)`, with public `NewLoop` calling it with `""`) so the
   child's `event.LoopStarted` is stamped with the new field — the **only struct change**:

   ```go
   // event.LoopStarted — the durable loop-creation event; ParentToolUseID is the
   // provider tool-use id of the Subagent call that spawned this loop ("" for the
   // primary loop and any non-tool/direct spawn). Relationship payload, not identity.
   ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
   ```

`LoopStarted.Cause` already carries the parent loop/turn/step coordinates (no change needed).

## Decision 3 — detached accumulator + reconciliation (review issue #3)

The reducer keeps a detached accumulator (child `StepDone` precedes parent `StepDone`; restore
rebuilds from scratch). **Key = `{parentLoopID, parentTurnID, parentStepID, ParentToolUseID}`**,
all read from the child `LoopStarted` (`.Cause.*` + `.ParentToolUseID`):

```
transcriptModel gains:
  loopParent  map[childLoopID]spawnKey        // from child LoopStarted (Cause + ParentToolUseID)
  subagentAccum map[spawnKey]*subagentAccum   // built purely from Enduring child events
spawnKey:   parentLoopID, parentTurnID, parentStepID uuid.UUID; toolUseID string
subagentAccum: agent, task string; children []ToolCallView; steps int;
               status running|done|failed|interrupted; nested int
```

**Reconciliation (identical live and restore):** at the **orchestrator's `StepDone`**,
`splitStepGroup` yields the AIMessage; for each `*content.ToolUseBlock` named `Subagent`,
build `spawnKey{thisLoopID, thisTurnID, thisStepID, block.ID}` and attach the matching
accumulator's `children/steps/status/summary` to that card. Because live and cold-restore
`FoldDisplay` consume the same Enduring sequence, restored cards equal committed cards
(headline property — a test asserts `EqualTranscript`).

## Decision 3a — child cards must use a PURE stored-card builder

The existing `stepToolCard(use, results, idx)` consults `m.live.Calls[idx]` and reuses the
live card when the name matches — correct for the PRIMARY loop's own step, but WRONG for a
child: in a mixed parent batch (e.g. parent `Bash` at index 0, child's first tool also `Bash`),
the child's index-0 card would steal the parent's live `Bash` card. Extract the pure fallback
into `storedStepToolCard(use, results)` — built ONLY from the stored tool-use block + the paired
`ToolResultMessage` (correlated by `use.ID`), with no `m.live.Calls` access — and use it
EXCLUSIVELY for child accumulation and restore. `stepToolCard` keeps the live-preference path
for the primary loop and delegates its own fallback to `storedStepToolCard` (DRY). A regression
test (Decision 9, test 13) proves the child uses its own durable result, never a parent live card.

## Decision 4 — terminal semantics (review issue #4)

From the **child terminal event**, never `ToolCallCompleted.IsError` (Subagent returns failures
as text):

- child `TurnDone` → `⎿ done · N steps — "<result summary>"`
- child `TurnFailed` → `⎿ failed · N steps — "<error text>"`
- child `TurnInterrupted` → `⎿ interrupted · N steps`
- **spawn failure before any child `LoopStarted`** (unknown agent / spawn error → tool returns
  an `"error: …"` text result, no child loop): no accumulator; the parent card renders its
  error tool-result text as the body, no children, no `done` child.
- **Parent result body SUPPRESSED for Subagent cards**: the hand-back text shows ONLY in the
  `done — <summary>` child, never also as the card's normal result preview (no doubling).

## Decision 5 — rendering / commit topology

A `Subagent` call is **always promoted to a `●`-level card** `Subagent(<agent>)  "<task>"`
with its steps as `⎿` children, regardless of step shape:

- **Single Subagent, empty-text step:** one `●` card + `⎿` children.
- **All-Subagent step (≥2, no narration):** each its own `●` card; "Multiple actions" umbrella
  suppressed when every tool-use in the step is `Subagent`.
- **Mixed step:** narration is the `●` assistant bullet; ordinary tools are `⎿` cards; each
  `Subagent` call is still its OWN `●` card (children at one `⎿` level — never `⎿ ⎿`).

## Decision 6 — nested spawns (depth ≥ 2)

Depth-2 cards are not rendered. The depth-1 card shows a single collapsed counter
`⎿ +N nested subagent steps`, incremented per deeper `StepDone`. The deeper loop is attributed
to the right depth-1 card by **walking `LoopStarted.Cause.LoopID` ancestry** up to the loop
whose parent is the primary loop and which has a non-empty `ParentToolUseID` — NOT by the spawn
id. Updates live, freezes at commit. (YAGNI.)

## Decision 7 — child permission / AskUser stay visible + routable

Child `PermissionRequested`/`UserInputRequested` are Enduring → delivered → handled by the
existing interaction model, routed by child `loopID` (Approve/Deny/ProvideAnswer take loopID).
The nested card is display-only; gates are unaffected. A test feeds a child
`PermissionRequested` and asserts it still enqueues for the child loop.

## Decision 8 — async / background (out of scope v1; documented hook)

`Spawn` is synchronous today. Hook for later: a one-line **pending** card at spawn, then a
**standalone completion block** on hand-back (header + `⎿` children + result; NO "finished"
prefix). **Status:** while an async subagent is outstanding the session is non-idle (hub
`ExpectTurn` wake token) and the status line reads **"waiting"** (not "idle") until it hands
back. v1 (synchronous): the orchestrator step is in flight, so status is already waiting —
no change.

## Definitions

- **"N steps"** = child `StepDone` count.
- **Truncation** (task + summary): single line (newlines→spaces), trim, cap ~80 cols + ellipsis;
  full text remains in the durable journal.

## Testing (explicit)

1. Paints from **Enduring only**: LoopStarted + child TurnStarted + child StepDone(s) + child
   TurnDone + orchestrator StepDone → full card; adding child `ToolCall*`/`TokenDelta` changes
   nothing.
2. **ID-namespace regression**: a test where `ToolExecutionID` and `ToolUseBlock.ID` deliberately
   DIFFER — both live and enduring-only restore attach the SAME child card (proves the link uses
   the provider id, not the execution id).
3. **Restore equivalence**: `FoldDisplay` over the Enduring backlog `EqualTranscript` the live
   transcript.
4. **Concurrent**: two subagents, different provider tool-use ids → no cross-contamination.
5. **Failure before child loop** → parent card shows error text, no nesting.
6. **Interruption** → `interrupted` child.
7. **Mixed batch** → topology (ordinary `⎿`, Subagent `●`).
8. **Empty `ParentToolUseID`** (non-tool spawn) → fallback `▸ agent: done` line.
9. **Child gate routable** → child `PermissionRequested` enqueues for the child loop.
10. **Result not doubled** → Subagent card body suppressed; summary only in the `done` child.
11. **Depth-2 ancestry** → a depth-2 StepDone increments the correct depth-1 card via
    `Cause.LoopID` walk.
13. **Mixed-batch same-index isolation** → parent batch is `Bash` (idx 0) then `Subagent`
    (idx 1); the child's FIRST tool is also `Bash`. Assert the nested child card carries the
    CHILD's durable result (from the child `StepDone`), never the parent's live `Bash` card —
    proving `storedStepToolCard` (not `stepToolCard`) is used for child reconstruction.
12. Plumbing: `loop.ToolUseIDFrom` (present/absent); `Subagent` forwards it;
    `event.LoopStarted.ParentToolUseID` round-trips; `RunSubagent` stamps it.

## Out of scope (separate bugs)

- Red markdown symbols (`/ → - +`) — glamour `DarkStyleConfig` override.
- Big vertical gap after a turn responds — surface row budget / live-tail capacity.
