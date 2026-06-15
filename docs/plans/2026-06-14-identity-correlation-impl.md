# Identity & Correlation — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add the per-message identity/correlation model — command `Header{ID, CausationID}`, an extended event `EventEnvelope`, and a per-turn `TurnID` — so every message is traceable and the tools subsystem has the `CallID`/`Header` substrate it needs.

**Architecture:** Commands embed a `Header` and the sealed `Command` interface gains `CommandHeader()`. Domain events stay bare on the per-turn stream; correlation metadata (`TurnID`, `EventID`, `CausationID`, `CallID`) rides only in the sink-side `EventEnvelope`. The loop assigns `TurnID` per turn and stamps the envelope at emit; the session stamps `Header.ID` on each command it sends.

**Tech Stack:** Go, stdlib only (`crypto/rand` via `internal/uuid`). No new dependencies.

**Design reference:** `docs/plans/2026-06-14-identity-correlation-design.md` (the *identity doc*). Read it first.

**Conventions (CLAUDE.md):** table-driven tests, `go test -race ./...`, typed errors, `CGO_ENABLED=0 go build -trimpath`. Run `make secure` before the final commit of each phase.

---

## Task 1: `uuid.IsZero`

**Files:**
- Modify: `internal/uuid/uuid.go`
- Test: `internal/uuid/uuid_test.go`

**Step 1: Write the failing test** (add to the existing table-driven test file)

```go
func TestUUIDIsZero(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		u    UUID
		want bool
	}{
		{name: "zero value", u: UUID{}, want: true},
		{name: "non-zero", u: UUID{1}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.u.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}
```

**Step 2: Run, expect FAIL** — `go test ./internal/uuid/ -run TestUUIDIsZero` → `u.IsZero undefined`.

**Step 3: Implement**

```go
// IsZero reports whether u is the zero UUID (absent / root).
func (u UUID) IsZero() bool { return u == UUID{} }
```

**Step 4: Run, expect PASS** — `go test -race ./internal/uuid/`.

**Step 5: Commit** — `git add internal/uuid && git commit -m "feat(uuid): add IsZero"`

---

## Task 2: command `Header` + sealed-interface accessor

**Files:**
- Modify: `internal/agent/loop/command/command.go` (the `Command` interface)
- Create: `internal/agent/loop/command/header.go`
- Test: `internal/agent/loop/command/header_test.go`

**Step 1: Write the failing test**

```go
package command

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/uuid"
)

func TestHeaderPromotedOnCommands(t *testing.T) {
	t.Parallel()
	id, _ := uuid.New()
	// Every concrete command must expose CommandHeader() via the embedded Header.
	var cmds = []Command{
		StartTurn{Header: Header{ID: id}},
		Interrupt{Header: Header{ID: id}},
		Shutdown{Header: Header{ID: id}},
	}
	for _, c := range cmds {
		if c.CommandHeader().ID != id {
			t.Errorf("%T: CommandHeader().ID = %v, want %v", c, c.CommandHeader().ID, id)
		}
	}
}
```

**Step 2: Run, expect FAIL** — `go test ./internal/agent/loop/command/ -run TestHeaderPromoted` → `Header`/`CommandHeader` undefined; `unknown field Header`.

**Step 3: Implement**

`header.go`:
```go
package command

import "github.com/inventivepotter/urvi/internal/uuid"

// Header is the correlation/idempotency metadata embedded in every command.
type Header struct {
	ID          uuid.UUID // fresh per command instance (uuid.New at construction)
	CausationID uuid.UUID // message-ID of the cause; zero = root (user-initiated)
}

// CommandHeader is promoted onto every command that embeds Header.
func (h Header) CommandHeader() Header { return h }
```

In `command.go`, widen the interface:
```go
type Command interface {
	isCommand()
	CommandHeader() Header
}
```

Embed `Header` in each concrete command (`start_turn.go`, `interrupt.go`, `shutdown.go`) — add `Header` as the first embedded field of each struct. Leave existing `isCommand()` methods.

**Step 4: Run, expect PASS** — `go test -race ./internal/agent/loop/command/`. Then `CGO_ENABLED=0 go build ./...` — fix any constructor sites that now need `Header{}` (the session in Task 5; build errors there are expected until Task 5).

**Step 5: Commit** — `git add internal/agent/loop/command && git commit -m "feat(command): embed correlation Header in every command"`

---

## Task 3: extend `EventEnvelope`

**Files:**
- Modify: `internal/agent/loop/event/sink.go` (the `EventEnvelope` struct)
- Test: `internal/agent/loop/event/sink_test.go`

**Step 1: Write the failing test** — assert the new fields exist and a sink receives them.

```go
func TestEventEnvelopeFields(t *testing.T) {
	t.Parallel()
	sid, _ := uuid.New()
	tid, _ := uuid.New()
	eid, _ := uuid.New()
	cid, _ := uuid.New()
	callid, _ := uuid.New()
	env := EventEnvelope{
		SessionID: sid, TurnID: tid, TurnIndex: 1,
		EventID: eid, CausationID: cid, CallID: callid,
		Event: TurnStarted{TurnIndex: 1},
	}
	if env.TurnID != tid || env.EventID != eid || env.CausationID != cid || env.CallID != callid {
		t.Fatal("envelope correlation fields not preserved")
	}
}
```

**Step 2: Run, expect FAIL** — unknown fields `TurnID`/`EventID`/`CausationID`/`CallID`.

**Step 3: Implement** — add the four `uuid.UUID` fields to `EventEnvelope` (see identity doc "Events stay bare…"):
```go
type EventEnvelope struct {
	SessionID   uuid.UUID
	TurnID      uuid.UUID // NEW
	TurnIndex   TurnIndex
	EventID     uuid.UUID // NEW
	CausationID uuid.UUID // NEW
	CallID      uuid.UUID // NEW — zero unless the event pertains to a tool call
	Event       Event
}
```

**Step 4: Run, expect PASS** — `go test -race ./internal/agent/loop/event/`.

**Step 5: Commit** — `git add internal/agent/loop/event && git commit -m "feat(event): add TurnID/EventID/CausationID/CallID to EventEnvelope"`

---

## Task 4: assign `TurnID` and stamp the envelope in the loop

**Files:**
- Modify: `internal/agent/loop/loop.go` (`loopState` gains `turnID`; set on turn start, clear on end)
- Modify: `internal/agent/loop/turn.go` or wherever the loop builds `EventEnvelope` for sinks (the emit/publish path)
- Test: `internal/agent/loop/loop_test.go` (a fake `EventSink` capturing envelopes)

**Step 1: Write the failing test** — drive a turn through a recording sink; assert every turn event's envelope shares one non-zero `TurnID`, each has a unique non-zero `EventID`, and `CausationID == StartTurn.ID`.

```go
// Use a recordingSink implementing event.EventSink that appends envelopes under a mutex.
// Run one StartTurn with a fake llm.LLM that streams "hi" then completes.
// Assert: all captured envelopes have the same TurnID (non-zero); EventIDs are all
// distinct and non-zero; CausationID equals the StartTurn's Header.ID.
```

**Step 2: Run, expect FAIL** — `TurnID`/`EventID` zero (loop not stamping yet).

**Step 3: Implement**
- `loopState` gains `turnID uuid.UUID`. When the actor accepts a `StartTurn`, set `state.turnID, _ = uuid.New()`; clear to `uuid.UUID{}` when the turn ends.
- In the sink-publish path, build `EventEnvelope` with `SessionID`, `TurnID: state.turnID`, `TurnIndex`, a fresh `EventID, _ := uuid.New()`, `CausationID:` the active `StartTurn.CommandHeader().ID`, and `CallID:` zero (tools set it later). The bare event on `StartTurn.Events` is unchanged.

**Step 4: Run, expect PASS** — `go test -race ./internal/agent/loop/`.

**Step 5: Commit** — `git add internal/agent/loop && git commit -m "feat(loop): assign TurnID and stamp envelope correlation fields"`

---

## Task 5: session stamps `Header.ID` on commands

**Files:**
- Modify: `internal/agent/session/agent.go` (every place it constructs `command.StartTurn`/`Interrupt`/`Shutdown`)
- Test: `internal/agent/session/agent_test.go`

**Step 1: Write the failing test** — capture the command the session sends (via a fake loop or by inspecting) and assert `CommandHeader().ID` is non-zero.

(If the session sends on an unexported channel, add the assertion at the seam used by existing session tests — e.g. a fake that records the received command.)

**Step 2: Run, expect FAIL** — `Header.ID` is zero.

**Step 3: Implement** — at each command construction, set `Header: command.Header{ID: mustNewUUID()}` where `mustNewUUID` calls `uuid.New()` and maps its error to the existing `SessionError` path (do not ignore the error — CLAUDE.md). `StartTurn` from a user prompt has `CausationID` zero (root).

**Step 4: Run, expect PASS** — `go test -race ./internal/agent/session/` and `CGO_ENABLED=0 go build ./...` (build is now fully green again).

**Step 5: Commit** — `git add internal/agent/session && git commit -m "feat(session): stamp command Header.ID on send"`

---

## Phase close

- Run `go test -race ./...` (all green) and `make secure`.
- The `command.Command` interface now carries `CommandHeader()`; `EventEnvelope` carries full correlation; the loop assigns `TurnID`. The tools plan builds on this (its `CallID`/control commands embed `Header`, and `PermissionRequested`/`UserInputRequested` set the envelope `CallID`).

## Out of scope (per design)

- A dedup/idempotency cache (IDs are substrate only).
- Surfacing `EventID` on bare stream events (envelope-only).
- `Redactable.SinkProjection` (defined in the **tools** plan, where the sensitive events live).
