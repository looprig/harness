# Per-Loop Compaction Input Queue Design

## Goal

While one loop is compacting its conversation, keep every new message targeting
that loop in its existing actor-owned FIFO until the compaction reaches a durable
terminal. Other loops continue accepting input normally.

## Existing behavior

`loopState.inbox` already owns pending `UserInput` and `SubagentResult` values.
Interactive input may fold into a running turn, managed delegate follow-ups carry
`NoFold` and start distinct turns, and subagent hand-backs bypass rejection but use
the same inbox.

The bug is that input is considered busy only when `loopState.status` is running or
waiting for execution admission. Idle compaction deliberately leaves that status
idle because no turn goroutine exists. Input can therefore start a turn while the
compaction hustle is still using the prior conversation snapshot. Safe-boundary
drains and post-turn chaining also do not treat an active compaction as an input
barrier.

`loopRunning` must continue to mean that an installed turn goroutine owns
`turnID`, `causationID`, and `cancelTurn`. Compaction is orthogonal: it can run at
an idle boundary or pause an existing turn. It must not be represented as a false
running status.

## Design

The existing `compactionControl.pending` slot is the canonical actor-owned signal
that compaction blocks input. A small `blocksInput` query exposes that fact without
adding mutable state.

All message admission paths treat `blocksInput` like a busy execution state and
append to `loopState.inbox`. The existing queue limit and special never-reject
contract for `SubagentResult` remain unchanged. Queued input publishes only the
existing ephemeral `InputQueued` event.

No path may remove input from the inbox while compaction blocks input:

- execution admission cannot start a queued turn;
- a tool-continuation drain cannot fold queued input;
- a completed turn cannot chain into the next queued turn.

Once a compaction commit or rejection has been durably finalized and the canonical
pending slot has been cleared, an idle loop asks for execution admission for the
FIFO head. A running loop resumes its existing turn, whose later drain or terminal
uses the normal inbox behavior. Pre-start rejection and abort paths perform the
same wake-up after clearing the slot.

The session's command intent log remains audit-only and preserves its existing
pre-dispatch behavior. The compaction barrier protects the conversation state and
context basis: no `TurnStarted` or `TurnFoldedInto` caused by blocked input can be
durably appended before the compaction terminal.

## Failure and lifecycle behavior

Interrupt and shutdown retain their existing priority over compaction. Shutdown
returns queued entries through `InputCancelled`; it never invents a turn. A hard
compaction finalization failure faults the session and does not resume input.

Because each loop actor owns its own inbox and compaction controller, compacting a
subagent blocks only that subagent. The root and sibling loops are independent.

## Verification

Regression coverage will prove idle manual compaction queues all input variants,
safe-boundary drains do not fold during compaction, durable terminal ordering
precedes queued turn start, rejection resumes the queue, sibling loops remain
independent, and shutdown resolves rather than starts queued input.
