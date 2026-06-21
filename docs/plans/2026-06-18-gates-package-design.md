# Gates Package — a reusable "machine asks, caller answers" primitive (Draft)

**Date:** 2026-06-18
**Status:** Draft / exploratory
**Related:** `docs/plans/loop-machine-design.md` (the turn-scoped permission/
user-input gates this generalizes; the stranded-message decision it would later host)

## 1. Motivation

The loop already has a gate mechanism, but it's hard-wired to one corner of the
design space: it lives in **turn** runtime state (`turn.pendingGates`, cleared at
turn end), serves exactly two kinds (**permission**, **user-input**), and its only
effect is to **resume a parked tool runner**. As the design grew we found other
"the machine needs a human/caller answer" needs that don't fit that corner — most
concretely the stranded-queued-message decision (resend / drop), which arises
*after* a turn has ended (so there's no turn to host a gate and no runner to
resume) and whose answer **initiates new work** rather than resuming parked work.

Rather than bolt on a bespoke mechanism per case, extract the common shape into a
small package any host can own. The machinery is already 90% general; what's
hard-coded is the *host* and the *effect*.

## 2. The two axes

A "machine asks, caller answers" interaction varies along two independent axes:

- **Host / lifetime** — who owns the pending-request registry and how long it
  survives: a **turn** (dies at turn end), a **step**, a **loop**, or a **session**
  (outlives turns).
- **Effect of the answer** — **resume** a parked computation, or **initiate** new
  work.

| Instance | Host | Effect | Status |
|---|---|---|---|
| Permission gate | turn | resume (the parked tool continues) | exists |
| User-input / AskUser | turn | resume | exists |
| Stranded-message resend/drop | session | initiate (a fresh `UserInput`) | future |
| "Pick a sub-plan / clarify before continuing" | step/turn | resume | possible |
| Session-level confirm (e.g. shutdown) | session | initiate / control | possible |

Today's gates are the `{turn, resume}` corner. The package should make the host
and effect choices, not assume them.

## 3. What is common (the package owns)

The invariant core, lifted verbatim from today's `gate.go`:

- **Register by id**, with a **kind** discriminator.
- **Install-before-emit**: registration completes before the prompt event is
  emitted, so a racing answer can never be dropped.
- **Route once, fail-safe**: an answer is delivered iff a gate is open for that id
  *and* its kind accepts the answer; any miss — unknown/stale id, kind mismatch, or
  a duplicate after the first delivery — is a silent no-op. Delivery removes the
  gate so it can't fire twice.
- **Drop one / clear all**: a requester can abandon its gate; the host clears all
  at teardown.
- **Single-goroutine ownership**: the registry is owned by one goroutine (the host
  actor), so it needs **no internal locking** — exactly how `pendingGates` is
  touched only by the loop actor today. The actor is the serialization point.

## 4. What is host-specific (the host owns)

- **Lifetime** — when to `Clear()`. A turn host clears at turn end; a session host
  keeps the registry across turns.
- **The prompt event** — what gets emitted to the caller (`PermissionRequested`,
  `UserInputRequested`, or a future session question). The package routes answers;
  it does not define prompts.
- **The effect** — what the reply *does*. For `resume`, a parked goroutine reads
  the reply and continues. For `initiate`, the host reads the reply and starts new
  work. The package only delivers the reply to a channel; the consumer differs.
- **The kinds** — each host declares the kinds it supports.

## 5. Proposed API (sketch)

```go
// Package gate provides a host-owned rendezvous registry: park a pending request
// keyed by an id and deliver exactly one routed reply, fail-safe. Host-agnostic
// (turn/step/loop/session) and effect-agnostic (the reply may resume parked work
// or initiate new work — the package only delivers it). A Registry is owned by a
// single goroutine; it does no locking.
package gate

// Kind discriminates request types so an answer for one kind cannot satisfy
// another. Hosts define their own kind values.
type Kind uint16

// Registry is generic over the reply type so a turn can use command.Command and a
// session can use its own answer type.
type Registry[Reply any] struct { /* pending map[uuid.UUID]entry[Reply] */ }

func New[Reply any]() *Registry[Reply]

// Register installs a pending gate under id+kind and returns the receive end of a
// buffered(1) reply channel. The caller emits its prompt only AFTER Register
// returns (install-before-emit). ok=false if id is already registered — ids must
// be fresh.
func (r *Registry[Reply]) Register(id uuid.UUID, kind Kind) (reply <-chan Reply, ok bool)

// Resolve delivers v to the gate at id iff one is open AND its kind == kind.
// Otherwise a fail-safe no-op (unknown/stale id, kind mismatch, duplicate).
// Delivers once and removes the gate.
func (r *Registry[Reply]) Resolve(id uuid.UUID, kind Kind, v Reply) (delivered bool)

func (r *Registry[Reply]) Drop(id uuid.UUID) // requester gave up
func (r *Registry[Reply]) Clear()            // host teardown (e.g. turn end)
```

**Cross-goroutine registration.** When the requester runs in a different goroutine
than the host actor (today's turn runner vs the loop actor), registration crosses
goroutines and must preserve install-before-emit. The requester creates the
buffered(1) reply channel, hands the send end to the host via a registration
channel, and blocks on an ack; the host calls the registry's internal install and
closes the ack; the requester then emits its prompt and reads the reply. This is
exactly the existing `gateRegistration{callID, reply, kind, ack}` protocol —
promoted into the package as `gate.Registration[Reply]`.

## 6. Migration: today's turn-gates become an instance

`internal/agent/loop/gate.go` collapses onto the package with **no behaviour
change**:

- `Registry[command.Command]` owned by the loop actor for the active turn.
- `Kind` values `KindPermission`, `KindUserInput` (the current `gateKind`).
- `routeControl` → `Resolve(route.ToolCallID, kind, cmd)`.
- `clearGates` → `Clear()` at turn end.
- The runner's `askPermission`/`RequestUserInput` → `Registration` + emit + read.

This is the safe first step: extract, keep `{turn, resume}` semantics identical,
lock the existing tests.

## 7. Future: the stranded-message decision as `{session, initiate}`

Hosted by the **session** (survives turn end), `Registry[Answer]` where `Answer`
is resend/drop. The loop returns the stranded message to the session
(`InputCancelled{Message}`); the session `Register`s a gate, emits a session
question, and on `Resolve` either submits a fresh `command.UserInput` (initiate) or
drops it. Note this couples to quiescence: a session-hosted `initiate` gate that
holds a pending message is itself an "in-flight thing that will start a turn" —
i.e. it behaves like the `expectTurn`/`{wake,…}` token, so the two concepts should
be unified rather than duplicated (see Open questions).

## 8. Security / fail-secure (CLAUDE.md)

- **Fail-secure routing**: an answer that doesn't match an open gate of the right
  kind is dropped, never guessed. Deny-by-default on ambiguity.
- **Kind-checked**: an answer for one kind can't satisfy a gate of another (no
  cross-wiring a permission grant onto a user-input gate).
- **Idempotent**: `Resolve`/`Drop`/`Clear` are no-ops on absent ids; a duplicate
  answer after delivery is dropped (the gate is gone).
- **Install-before-emit**: the prompt is emitted only after the gate is installed,
  so a fast answer can't race ahead of registration and be lost.
- **Typed**: `Kind` and the reply type are concrete; no `any` on the hot path
  beyond the `Reply` type parameter, which a host fixes to a concrete type.
- **No external deps** — stdlib + the internal `uuid` package only.

## 9. Concurrency model

- One `Registry` is owned by one goroutine (the host actor). All map operations run
  on that goroutine — no mutex, matching today's `pendingGates`.
- Reply channels are **buffered(1)** with the requester (or host, for `initiate`)
  as the sole reader, so `Resolve` never blocks the actor.
- Cross-goroutine registration uses the channel+ack handshake; everything else is
  in-actor.

## 10. Open questions

- **Generics vs concrete reply.** `Registry[Reply any]` is clean but the loop's
  reply is `command.Command`; confirm the generic instantiation doesn't force
  awkward imports (the package must not import `loop`/`command` — the host supplies
  the type).
- **Kind representation.** Fixed `uint16` constants per host, or an open
  registration? Fixed is simplest and fail-secure; revisit only if hosts need
  dynamic kinds.
- **Prompt-event shape.** Should the package also offer a generic prompt/answer
  *event* shape, or leave prompts entirely to the host? Leaning host-owned (prompts
  carry host-specific payloads and redaction rules).
- **`initiate` × quiescence.** A session-hosted `initiate` gate holding a pending
  turn-trigger is the same idea as the `expectTurn` token. Unify: an open
  `initiate` gate *is* an `active` entry (`{gate, id}`), so `SessionIdle` already
  waits for it. This is the most promising unification and should be designed
  together with the quiescence model.
- **Route correlation.** The gate id is `command.Route.ToolCallID` today; for
  non-tool hosts (session questions) define what the id is and how the answer
  command carries it.

## 11. Phasing

- **P1** — extract `internal/agent/gate` and migrate the turn permission/user-input
  gates onto it with identical semantics (`{turn, resume}`); no behaviour change,
  existing tests green.
- **P2** — add a session-hosted registry and the `initiate` effect; unify
  `initiate` gates with the quiescence `active` set; host the stranded-message
  resend/drop decision on it.
- **P3** — open it to step/loop hosts as concrete needs appear.

## 12. Testing (when built)

- Table-driven, `-race`. Register→Resolve delivers once; second Resolve is a no-op.
- Kind mismatch and unknown/stale id are fail-safe no-ops.
- `Clear` drops all; a late `Resolve` after `Clear` is a no-op.
- Install-before-emit: a `Resolve` issued concurrently with `Register` is never
  lost once `Register` has returned.
- Migration: the loop's permission and user-input flows behave identically on the
  package (port the existing gate tests).
