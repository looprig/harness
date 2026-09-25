# harness v0.41.0: message principal, message metadata and the Message Presenter: implementation plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

> **Unverified code (cross-plan review, 2026-09-25).** The code blocks in this plan were
> written against harness v0.40.2 source but were **not compiled or run** while planning.
> Known soft spots the executor must resolve against the real tree, not guess:
> Task 6's fuzz target name (`FuzzUnmarshalEvent` may not exist); Task 13's
> `SessionInvalidInput` kind and `submitToLoopAttributed` (described, not written);
> Task 14's store-level golden test body (`// ... steps 1–3 above ...` is an outline);
> Task 15's probe expectations (a measurement, not a known result). Each Red step must be
> observed failing for the stated reason before any Green step.

**Goal:** Carry the optional verified `principal` on all five admitted runtime command
kinds and optional `metadata` on `create`/`input`. Let a rig install one
`present.Presenter` that may prepend and append text blocks to a user message. It runs
once, when the message enters the session, and the rendering is journaled so replay,
restore and compaction reproduce what the model saw. Principal and metadata reach the
journal for audit. The public projection keeps the principal and strips the metadata.
Then release harness v0.41.0, which also ships impl-04 (unbounded execution and opaque
tool input).

**Architecture:** A new leaf package `pkg/present` holds the presenter contract and
validates frames. It does not import `rig`, `sessionruntime`, `command` or `event`.
`runtimecommand.Admitted` gains `Principal`/`Metadata`. The session presents at its
dispatch boundary: `prepareAdmittedInput` for Host-admitted input, `dispatchUserInput`
for in-process user input. It stores the frame on `command.UserInput.Presented`, which
rides the load-bearing intent record. The loop composes `Prefix ++ Blocks ++ Suffix`
into the committed `UserMessage`. `TurnStarted`, `TurnFoldedInto` and `InputCancelled`
record a `MessageInput{Principal, Metadata, Prefix, Suffix}`, so a reader can recover
the user's exact blocks. `TurnInterrupted` and `GateResolved` gain `Principal`. Machine
input bypasses the presenter. A presenter failure refuses an attempt-bearing command
with a `refused` disposition and writes no intent record. Every new member is
`omitzero`/`omitempty`, and event `v` stays 1, so a record without them is byte-identical
to v0.40.2.

**Tech Stack:** Go 1.26.8 (`GOTOOLCHAIN=go1.26.8 GOWORK=off`). Only `core/content`,
`core/uuid` and `core/sessionwire/v1` from core v0.12.0, sessionstore v0.14.0 and
inference v0.14.0. Stdlib only. No new external dependency.

**Binding inputs:**
- Design: `docs/plans/2026-09-25-message-principal-metadata-presenter-design.md`
  (principal on all five kinds, metadata on create/input, prepend/append only, one
  journaled presentation, `refused` on failure, public redaction strips metadata and
  keeps principal).
- Master plan: `docs/plans/2026-09-25-impl-00-master-plan.md` (this is row 05; it
  depends on 01, 02 and 03 being published and 04 being merged).
- Workspace rules: `/Users/ipotter/code/looprig/AGENTS.md`.

Every path below is relative to the harness repository root
`/Users/ipotter/code/looprig/harness` unless it is absolute. Line numbers are verified
at tag `v0.40.2` (`9bbba937`). impl-04 lands on `main` first, so re-locate each anchor
with the quoted `grep` before you edit.

---

## Verified facts, and five corrections to the design

The executor must read this section before Task 1. Each item was checked against
v0.40.2 source. Where the design's premise is wrong, this plan implements the design's
intent and marks the deviation.

1. **Pre-v0.41.0 decoders do NOT fail closed on the new members. Rollback is lossy,
   not refused.** The design (§2 rows 2–3, §3 "Old readers") says harness ≤ v0.40.2
   "fails `DisallowUnknownFields` (`record_json.go:57,132`)". Those two lines are
   `UnmarshalLeaseFence` and `UnmarshalGatePreparedRecord`. The records this feature
   touches decode permissively:
   - `command.UnmarshalCommand` → `decodeUserInput` / `decodePlain[Interrupt]` use a
     bare `json.Unmarshal` (`pkg/command/marshal.go:358-388`).
   - `TurnStarted`, `TurnFoldedInto`, `InputCancelled` and `TurnInterrupted` use
     `decodePlain` (`pkg/event/marshal.go:688-700`, `:810-818`).
   - `decodeGateResolved` uses a bare `json.Unmarshal` (`pkg/event/marshal.go:1052-1056`).
     The `GatePrepared.Resume` doc (`pkg/event/gate.go:24-27`) already relies on this.

   So a v0.40.2 runtime opening a v0.41.0 journal **decodes it, drops
   `input`/`principal`/`metadata`/`presented`, and restores.** Committed turns are
   unaffected: `TurnStarted.Message` already holds the assembled frame, and
   `foldLoop` appends it verbatim. There are two real losses:
   - (a) The old process neither sees nor re-emits attribution.
   - (b) An **owed** presented input (disposition `applied`, no caused event;
     `internal/sessionruntime/admitted_input_replay.go:48-98`) is re-offered by the old
     runtime **without its frame**, so the model sees different text.

   Making old readers fail closed would need a conditional event `v: 2`. That is
   **rejected**: the published event envelope schema pins `"v": {"const": 1}`
   (`pkg/serve/testdata/schema/event_envelope.schema.json`, mirrored in wui
   `packages/protocol/src/schema.ts`), so every validating consumer would break. This
   plan keeps `v` = 1 (the design's own ruling) and makes the **new** decoders strict
   on the new members (Task 6), so the *next* addition fails closed. Task 15 measures
   the actual v0.40.2 behaviour and records it. The one-way release note (Task 16)
   says "lossy, never roll back" instead of "fails closed". **REQUIRES OWNER
   ACKNOWLEDGEMENT of that wording before Task 18.**
2. **A disposition carries no reason.** `runtimecommand.CommandDisposition`
   (`pkg/runtimecommand/disposition.go:121-129`) has no `Reason` field, and the durable
   frame is SessionStore's envelope. The design's `Disposition{Kind: refused, Reason:
   "presenter"}` is therefore a durable `refused` with no reason. The reason travels
   on the returned `*present.Error` and on a `slog.WarnContext` line. That line holds
   the error kind only, never frame or user text.
3. **The presenter-failure `refused` arm writes the application prefix first.** The
   design says the failure happens "before `appendAdmittedIntent`, so a failure leaves
   nothing durable naming the input". This plan keeps that: no intent record is
   written. For an **attempt-bearing** command it then appends the bodiless
   application prefix, then `refused`. That is the existing "effect failed after the
   prefix → refused" arm (`runtime_command.go:447`). Two reasons:
   - The prefix makes a redelivery of the same attempt a `Duplicate`, so the presenter
     is not re-run and no second disposition is attempted.
   - The released evidence reader accepts a disposition with or without a prefix
     (sessionstore `disposition_evidence.go:97-160`), so settlement is unchanged.

   A legacy (no-attempt) command returns a zero `Disposition` plus the error and
   writes nothing, so it may be re-offered.
4. **`pkg/session` has no concrete `Session`.** `session.Session`
   (`pkg/session/session.go:17-28`) is an interface. Adding `SubmitInput` to it would
   break every implementer. Following the `LeaseEpochReporter` precedent
   (`pkg/session/session.go:408-412`), `SubmitInput` is a **segregated capability**,
   `session.InputSubmitter`, discovered by assertion. `*sessionruntime.Session`
   implements it.
5. **Where the principal of each non-message kind lands.**
   - `interrupt`: it lands on every per-loop `command.Interrupt` intent record
     (`internal/sessionruntime/interrupt.go:225-228`). The fan-out writes one per live
     loop even when idle. It also lands on `TurnInterrupted` when a running turn is
     cancelled.
   - `gate_response`: it lands on `GateResolved`.
   - `restore`: harness writes no record of its own for a restore, only the bodiless
     prefix and disposition (`runtime_command.go:487-493`). Its principal is validated
     and then journaled **only** in SessionStore's disposition descriptor (impl-02).
     Adding a harness record for it would put a runtime-control record in the slot the
     settlement correlation reads as the effect (`runtime_command.go:752-756`), so
     none is added.
6. **The design's anchor is off.** The design cites `buildAndAuditUserInput
   (runtime_command.go:785-793)`. Those lines are `prepareAdmittedInput`'s two
   branches. The attempt branch (`:789-795`) builds its `UserInput` literally and
   calls `appendAdmittedIntent`. It does not call `buildAndAuditUserInput`
   (`internal/sessionruntime/session.go:2605`). Presentation therefore goes at the
   **top of the shared tail of `prepareAdmittedInput`**, before the branch.
7. **Known limit (recorded, not fixed).** Consider a redelivery after a crash
   *between* the intent append and the prefix append. It re-runs the presenter, and
   `appendAdmittedIntent` treats the intent collision as success
   (`runtime_command.go:809-812`). So the in-memory rendering is sent while the
   durable intent keeps the first one. With a deterministic presenter (design §5)
   they are equal. Otherwise `TurnStarted` records what the model actually saw, and a
   later restore uses `TurnStarted`, so the model context stays self-consistent. The
   README states the determinism obligation.

---

## Task 0: Workspace and baseline

**Files:** none committed.

**Step 1: Check the preconditions.** core v0.12.0, sessionstore v0.14.0 and inference
v0.14.0 must be published, or their local `main` must have the plan-01/02/03 APIs. The
impl-04 commits must be on harness `main`.

```bash
cd /Users/ipotter/code/looprig/harness
git checkout main && git pull --ff-only
git log --oneline v0.40.2..main          # expect the impl-04 commits
git ls-remote --tags git@github.com:looprig/core.git v0.12.0
git ls-remote --tags git@github.com:looprig/sessionstore.git v0.14.0
git ls-remote --tags git@github.com:looprig/inference.git v0.14.0
grep -n "type Principal struct\|type MessageMetadata\|HostLinkCapabilityAttributionPrincipal" \
  /Users/ipotter/code/looprig/core/sessionwire/v1/*.go
```

Expected: the impl-04 commits are listed, and the core grep shows `Principal`,
`MessageMetadata` and the capability constant. If a tag is not yet on the remote,
continue with Step 2 against local checkouts. Task 18 still needs the tags.

**Step 2: Uncommitted go.work (only while a dependency is unpublished).** Never add a
`replace` directive.

```bash
cd /Users/ipotter/code/looprig/harness
test -f go.work && echo "go.work exists; inspect it, do not overwrite" || \
cat > go.work <<'EOF'
go 1.26.8

use (
	.
	../core
	../sessionstore
	../inference
)
EOF
git status --short go.work   # must show "?? go.work"; never stage it
```

**Step 3: Baseline.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./... 2>&1 | tail -5
```

Expected: `ok` for every package. Record any pre-existing failure in the task log
before changing code.

---

## Task 1: Freeze pre-feature goldens with the real v0.40.2 codec

The byte-identity and rollback claims need bytes written by the **released** codec,
not by the code under change. Build a probe module that pins harness v0.40.2. Task 15
reuses it.

**Files:**
- Create: `internal/compat/testdata/v0402probe/go.mod`
- Create: `internal/compat/testdata/v0402probe/main.go`
- Create (generated): `internal/compat/testdata/pre_v0410/*.json`

The `testdata` directory keeps the probe out of `go list ./...`, `make fmt` and
`make check`.

**Step 1: go.mod.**

```
module v0402probe

go 1.26.8

require github.com/looprig/harness v0.40.2
```

Then run:

```bash
cd internal/compat/testdata/v0402probe && GOWORK=off GOTOOLCHAIN=go1.26.8 go mod tidy
```

This resolves core v0.11.0, inference v0.13.0 and the rest exactly as v0.40.2 pins
them. Commit `go.sum` with it.

**Step 2: main.go.** There are two modes. `gen DIR` writes one canonical encoding per
record kind the feature touches. `decode FILE...` decodes each file with the v0.40.2
codec, re-encodes it, and prints what survived.

```go
// Command v0402probe runs the RELEASED harness v0.40.2 codecs. It freezes the
// pre-feature goldens (gen) and measures how v0.40.2 reads v0.41.0 records
// (decode). It is a separate module pinned to v0.40.2 on purpose: the code under
// change cannot testify about the release it replaces.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

func id(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

var (
	session = id("11111111-1111-4111-8111-111111111111")
	loopID  = id("22222222-2222-4222-8222-222222222222")
	turnID  = id("33333333-3333-4333-8333-333333333333")
	cmdID   = id("44444444-4444-4444-8444-444444444444")
	evID    = id("55555555-5555-4555-8555-555555555555")
	gateID  = id("66666666-6666-4666-8666-666666666666")
)

func header() event.Header {
	return event.Header{
		EventID:     evID,
		Coordinates: identity.Coordinates{SessionID: session, LoopID: loopID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: cmdID, Agency: identity.AgencyUser},
	}
}

func user(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: text}}}}
}

func gen(dir string) error {
	events := map[string]event.Event{
		"turn_started.json":     event.TurnStarted{Header: header(), TurnIndex: 1, Message: user("hello")},
		"turn_folded_into.json": event.TurnFoldedInto{Header: header(), TurnIndex: 1, Message: user("more")},
		"input_cancelled.json":  event.InputCancelled{Header: header(), TurnIndex: 1, Reason: event.CancelTurnInterrupted, Message: user("late")},
		"turn_interrupted.json": event.TurnInterrupted{Header: header(), TurnIndex: 1},
		"gate_resolved.json": event.GateResolved{
			Header: header(), GateID: gate.ID(gateID), Resolver: gate.ResolverSession,
			Reason: gate.CloseAnswered, Action: "approve",
		},
	}
	for name, ev := range events {
		b, err := event.MarshalEvent(ev)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o600); err != nil {
			return err
		}
	}
	commands := map[string]command.Command{
		"user_input.json": command.UserInput{
			Header: command.Header{CommandID: cmdID, Agency: identity.AgencyUser},
			Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
		},
		"interrupt.json": command.Interrupt{Header: command.Header{CommandID: cmdID, Agency: identity.AgencyUser}},
	}
	for name, cmd := range commands {
		b, err := command.MarshalCommand(cmd)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func decode(files []string) error {
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			return err
		}
		raw = bytes.TrimSpace(raw)
		var out []byte
		if strings.Contains(filepath.Base(f), "user_input") || strings.Contains(filepath.Base(f), "interrupt.json") {
			cmd, derr := command.UnmarshalCommand(raw)
			if derr != nil {
				fmt.Printf("%s\tREFUSED\t%v\n", filepath.Base(f), derr)
				continue
			}
			out, err = command.MarshalCommand(cmd)
		} else {
			ev, derr := event.UnmarshalEvent(raw)
			if derr != nil {
				fmt.Printf("%s\tREFUSED\t%v\n", filepath.Base(f), derr)
				continue
			}
			out, err = event.MarshalEvent(ev)
		}
		if err != nil {
			return err
		}
		verdict := "IDENTICAL"
		if !bytes.Equal(out, raw) {
			verdict = "LOSSY"
		}
		fmt.Printf("%s\t%s\t%s\n", filepath.Base(f), verdict, out)
	}
	return nil
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: v0402probe gen DIR | decode FILE...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = gen(os.Args[2])
	case "decode":
		err = decode(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

If a symbol is spelled differently at v0.40.2, adjust the probe to v0.40.2's API: the
probe must compile against the release, not against `main`. Check the spellings with
`go doc github.com/looprig/harness@v0.40.2/pkg/gate ResolverSession`.

**Step 3: Generate and self-check.**

```bash
cd /Users/ipotter/code/looprig/harness/internal/compat/testdata
mkdir -p pre_v0410
(cd v0402probe && GOWORK=off GOTOOLCHAIN=go1.26.8 go run . gen ../pre_v0410)
(cd v0402probe && GOWORK=off GOTOOLCHAIN=go1.26.8 go run . decode ../pre_v0410/*.json)
```

Expected: seven lines, each `IDENTICAL`.

**Step 4: Makefile target.** Add this to `Makefile`. It is not wired into `check`
because it resolves a released module:

```make
# compat runs the released v0.40.2 codecs over the frozen goldens and the v0.41.0
# records (Task 15). It needs the module cache or network; it is a release gate.
compat:
	cd internal/compat/testdata/v0402probe && GOWORK=off go run . decode ../pre_v0410/*.json ../v0410/*.json
```

Also add `compat` to `.PHONY`.

**Step 5: Commit.**

```bash
git add internal/compat/testdata Makefile
git commit -m "test(compat): freeze v0.40.2 codec goldens and a release-pinned probe"
```

---

## Task 2: `pkg/present`: contract, frame validation and a safe runner

**Files:**
- Create: `pkg/present/present.go`
- Create: `pkg/present/present_test.go`
- Create: `pkg/present/README.md` (one paragraph, which the package README convention in
  `pkg/*/README.md` requires)

**Step 1: Write the failing test.**

```go
package present_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/present"
)

func text(s string) content.Block { return &content.TextBlock{Text: s} }

func TestFrameValidate(t *testing.T) {
	t.Parallel()
	many := make([]content.Block, present.MaxFrameBlocks+1)
	for i := range many {
		many[i] = text("x")
	}
	tests := []struct {
		name    string
		frame   present.Frame
		wantErr bool
	}{
		{name: "empty frame is valid", frame: present.Frame{}},
		{name: "prefix only", frame: present.Frame{Prefix: []content.Block{text("[from: A]")}}},
		{name: "prefix and suffix at block cap", frame: present.Frame{Prefix: many[:4], Suffix: many[:4]}},
		{name: "over block cap", frame: present.Frame{Prefix: many}, wantErr: true},
		{name: "text at byte cap", frame: present.Frame{Prefix: []content.Block{text(strings.Repeat("a", present.MaxFrameTextBytes))}}},
		{name: "text over byte cap", frame: present.Frame{Prefix: []content.Block{text(strings.Repeat("a", present.MaxFrameTextBytes)), text("b")}}, wantErr: true},
		{name: "nil block", frame: present.Frame{Suffix: []content.Block{nil}}, wantErr: true},
		{name: "typed nil text block", frame: present.Frame{Prefix: []content.Block{(*content.TextBlock)(nil)}}, wantErr: true},
		{name: "non-text block", frame: present.Frame{Prefix: []content.Block{&content.ThinkingBlock{Thinking: "x"}}}, wantErr: true},
		{name: "empty text block", frame: present.Frame{Prefix: []content.Block{text("")}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.frame.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				var pe *present.Error
				if !errors.As(err, &pe) || pe.Kind != present.ErrorFrameInvalid {
					t.Fatalf("error = %#v, want *present.Error{Kind: frame_invalid}", err)
				}
			}
		})
	}
}

type presenterFunc func(context.Context, present.Input) (present.Frame, error)

func (f presenterFunc) Present(ctx context.Context, in present.Input) (present.Frame, error) {
	return f(ctx, in)
}

func TestRunClonesAndClassifies(t *testing.T) {
	t.Parallel()
	user := []content.Block{text("Add milk")}
	var seen present.Input
	frame, err := present.Run(context.Background(), presenterFunc(func(_ context.Context, in present.Input) (present.Frame, error) {
		in.Blocks[0].(*content.TextBlock).Text = "MUTATED" // must not reach the caller
		seen = in
		return present.Frame{Prefix: []content.Block{text("[from: Alex]")}}, nil
	}), present.Input{Blocks: user})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if user[0].(*content.TextBlock).Text != "Add milk" {
		t.Fatal("presenter mutated the caller's blocks")
	}
	if seen.Principal != nil {
		t.Fatal("nil principal must reach the presenter as nil")
	}
	if got := frame.Prefix[0].(*content.TextBlock).Text; got != "[from: Alex]" {
		t.Fatalf("prefix = %q", got)
	}

	_, err = present.Run(context.Background(), presenterFunc(func(context.Context, present.Input) (present.Frame, error) {
		return present.Frame{}, errors.New("directory down")
	}), present.Input{Blocks: user})
	var pe *present.Error
	if !errors.As(err, &pe) || pe.Kind != present.ErrorPresenterFailed {
		t.Fatalf("presenter error = %v, want presenter_failed", err)
	}

	_, err = present.Run(context.Background(), presenterFunc(func(context.Context, present.Input) (present.Frame, error) {
		panic("boom")
	}), present.Input{Blocks: user})
	if !errors.As(err, &pe) || pe.Kind != present.ErrorPresenterFailed {
		t.Fatalf("panic = %v, want presenter_failed", err)
	}
}
```

**Step 2: Run it and watch it fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/present/... -run 'TestFrameValidate|TestRunClonesAndClassifies'
```

Expected: a build failure (`package github.com/looprig/harness/pkg/present` is not in
the module).

**Step 3: Implement `pkg/present/present.go`.**

```go
// Package present is the Message Presenter contract: a product hook that may frame
// a USER message with text before and after it so the model sees selected context
// (for example "[from: Alex (parent) · space: Family]").
//
// A presenter may only PREPEND and APPEND. The user's blocks are handed over as a
// clone, and any change to them is discarded. The session runs the presenter ONCE,
// when the message enters it, and journals the frame, so replay, restore and
// compaction reproduce what the model saw. Changing the presenter changes only new
// messages. Machine-originated input (subagent hand-backs, MessageAgent
// deliveries, every AgencyMachine input) is never presented.
//
// A presenter SHOULD be deterministic in its Input. It may do bounded lookups
// (subject -> display name). A redelivered command may be presented again before
// its durable prefix exists, and a nondeterministic presenter would then disagree
// with its own earlier rendering.
//
// The package imports no rig, session or loop package, so a product can test a
// presenter in isolation.
package present

import (
	"context"
	"fmt"
	"strconv"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// Limits on one frame. They bound the model-visible overhead a presenter adds and
// keep the frame well inside the durable inline payload.
const (
	// MaxFrameBlocks bounds Prefix and Suffix combined.
	MaxFrameBlocks = 8
	// MaxFrameTextBytes bounds the summed UTF-8 bytes of every text block in the frame.
	MaxFrameTextBytes = 8192
)

// Input is what a presenter may read. Principal is NIL when the command carried
// none. That is the case with no Factory stamping, in-process Submit, or a no-auth
// deployment, and every presenter MUST handle it. Metadata is nil when absent.
// Blocks is a clone of the user's blocks.
type Input struct {
	SessionID uuid.UUID
	LoopID    uuid.UUID
	AgentName identity.AgentName
	// Kind is runtimecommand.KindInput or runtimecommand.KindCreate. In-process user
	// input (Submit, SubmitToLoop, SubmitInput) is KindInput.
	Kind      runtimecommand.Kind
	Principal *sessionwire.Principal
	Metadata  sessionwire.MessageMetadata
	Blocks    []content.Block
}

// Frame is what a presenter returns: blocks placed before and after the user's.
// The zero Frame presents nothing.
type Frame struct {
	Prefix []content.Block
	Suffix []content.Block
}

// Empty reports whether the frame adds nothing.
func (f Frame) Empty() bool { return len(f.Prefix) == 0 && len(f.Suffix) == 0 }

// Validate refuses a frame the session will not journal. It allows only non-empty
// *content.TextBlock entries in v1 and applies the limits above. A failure is a
// *Error of kind ErrorFrameInvalid.
func (f Frame) Validate() error {
	if n := len(f.Prefix) + len(f.Suffix); n > MaxFrameBlocks {
		return frameInvalid(fmt.Errorf("%d blocks exceeds %d", n, MaxFrameBlocks))
	}
	total := 0
	for _, part := range [][]content.Block{f.Prefix, f.Suffix} {
		for i, b := range part {
			tb, ok := b.(*content.TextBlock)
			if !ok || tb == nil {
				return frameInvalid(fmt.Errorf("block %d is %T, want *content.TextBlock", i, b))
			}
			if tb.Text == "" {
				return frameInvalid(fmt.Errorf("block %d is empty", i))
			}
			total += len(tb.Text)
		}
	}
	if total > MaxFrameTextBytes {
		return frameInvalid(fmt.Errorf("%d text bytes exceeds %d", total, MaxFrameTextBytes))
	}
	return nil
}

// Presenter frames one user message.
type Presenter interface {
	Present(ctx context.Context, in Input) (Frame, error)
}

// ErrorKind classifies a presentation failure.
type ErrorKind string

const (
	// ErrorPresenterFailed: the presenter returned an error or panicked.
	ErrorPresenterFailed ErrorKind = "presenter_failed"
	// ErrorFrameInvalid: the presenter returned a frame Validate refuses.
	ErrorFrameInvalid ErrorKind = "frame_invalid"
)

// Error is the one typed presentation failure. The session refuses the command it
// was presenting: an admitted command settles `refused`, and SubmitInput returns
// this error. Cause is the presenter's own error. It is never logged verbatim,
// because a product error may name a person.
type Error struct {
	Kind  ErrorKind
	Cause error
}

func (e *Error) Error() string { return "present: " + string(e.Kind) }
func (e *Error) Unwrap() error { return e.Cause }

func frameInvalid(cause error) error { return &Error{Kind: ErrorFrameInvalid, Cause: cause} }

// Run invokes p once on a defensive copy of in and returns a validated, cloned
// frame. A presenter error or panic is ErrorPresenterFailed. An invalid frame is
// ErrorFrameInvalid. The returned blocks share nothing with the presenter.
func Run(ctx context.Context, p Presenter, in Input) (frame Frame, err error) {
	in.Blocks = content.CloneBlocks(in.Blocks)
	if in.Principal != nil {
		principal := *in.Principal
		in.Principal = &principal
	}
	if in.Metadata != nil {
		copied := make(sessionwire.MessageMetadata, len(in.Metadata))
		for k, v := range in.Metadata {
			copied[k] = v
		}
		in.Metadata = copied
	}
	defer func() {
		if r := recover(); r != nil {
			frame, err = Frame{}, &Error{Kind: ErrorPresenterFailed, Cause: fmt.Errorf("presenter panicked: %s", strconv.Quote(fmt.Sprint(r)))}
		}
	}()
	got, perr := p.Present(ctx, in)
	if perr != nil {
		return Frame{}, &Error{Kind: ErrorPresenterFailed, Cause: perr}
	}
	if err := got.Validate(); err != nil {
		return Frame{}, err
	}
	return Frame{Prefix: content.CloneBlocks(got.Prefix), Suffix: content.CloneBlocks(got.Suffix)}, nil
}
```

`pkg/present` must not import `command`, `event` or `internal/...`. Add a deps test
modelled on `pkg/sessionwire/deps_test.go`:
`TestPresentImportsNoRuntimePackage` lists `go list -deps` and fails on
`harness/internal`, `harness/pkg/rig`, `harness/pkg/command` or `harness/pkg/event`.

**Step 4: Run it and watch it pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/present/...
```

Expected: `ok  github.com/looprig/harness/pkg/present`.

**Step 5: Add a fuzz target.** The frame comes from product code, which is an external
input. Add `FuzzFrameValidate` (random strings/counts → `Validate` never panics, and
the result is consistent with the limits), then run it:

```bash
GOTOOLCHAIN=go1.26.8 go test ./pkg/present -run '^$' -fuzz=FuzzFrameValidate -fuzztime=30s
```

**Step 6: Commit.**

```bash
git add pkg/present
git commit -m "feat(present): add the Message Presenter contract and frame validation"
```

---

## Task 3: `rig.WithMessagePresenter` and the session wiring

**Files:**
- Modify: `pkg/rig/options.go`: add the key next to `keyToolResultCapture` (`:49`)
- Create: `pkg/rig/presenter.go`
- Modify: `pkg/rig/errors.go`: add `DefinitionInvalidMessagePresenter`
- Modify: `internal/sessionruntime/lifecycle.go` (next to `WithLifecycleToolResultObjects`, `:521`)
- Modify: `internal/sessionruntime/session.go` (`Session` struct: add `presenter present.Presenter`)
- Test: `pkg/rig/message_presenter_option_test.go`

**Step 1: Write the failing test.** It follows `pkg/rig/session_id_option_test.go` and
`tool_result_objects_test.go`. Use the existing minimal-definition helper; find it with
`grep -n "func minimalOptions\|func baseOptions" pkg/rig/*_test.go`.

```go
func TestWithMessagePresenterRefusesNilAndDuplicate(t *testing.T) {
	t.Parallel()
	var typedNil *stubPresenter
	tests := []struct {
		name     string
		opts     []Option
		wantKind DefinitionErrorKind
	}{
		{name: "nil presenter", opts: []Option{WithMessagePresenter(nil)}, wantKind: DefinitionInvalidMessagePresenter},
		{name: "typed nil presenter", opts: []Option{WithMessagePresenter(typedNil)}, wantKind: DefinitionInvalidMessagePresenter},
		{name: "duplicate", opts: []Option{WithMessagePresenter(&stubPresenter{}), WithMessagePresenter(&stubPresenter{})}, wantKind: DefinitionDuplicateOption},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(append(minimalOptions(t), tt.opts...)...)
			var de *DefinitionError
			if !errors.As(err, &de) || de.Kind != tt.wantKind {
				t.Fatalf("Define() = %v, want kind %q", err, tt.wantKind)
			}
		})
	}
}

func TestWithMessagePresenterDefines(t *testing.T) {
	t.Parallel()
	if _, err := Define(append(minimalOptions(t), WithMessagePresenter(&stubPresenter{}))...); err != nil {
		t.Fatalf("Define: %v", err)
	}
}

type stubPresenter struct{}

func (*stubPresenter) Present(context.Context, present.Input) (present.Frame, error) {
	return present.Frame{}, nil
}
```

**Step 2: Run it and watch it fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/rig/... -run 'TestWithMessagePresenter'
```

Expected: a build failure (`undefined: WithMessagePresenter`).

**Step 3: Implement.**

`pkg/rig/options.go`:
```go
	keyMessagePresenter                singletonKey = "message_presenter"
```

`pkg/rig/errors.go` (in the kind block):
```go
	// DefinitionInvalidMessagePresenter: WithMessagePresenter was called with a nil
	// presenter, including a typed nil behind the interface.
	DefinitionInvalidMessagePresenter DefinitionErrorKind = "invalid_message_presenter"
```

`pkg/rig/presenter.go`:
```go
package rig

import (
	"reflect"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/present"
)

// WithMessagePresenter installs the rig's Message Presenter. Every session the rig
// creates or restores presents each USER message through it once, when the message
// enters the session. See package present for the contract.
//
// It refuses a nil presenter, including a typed nil, and a second call. Like
// WithToolResultObjects (capture.go) it is a singleton: two presenters would have no
// rule for composing.
//
// The presenter is not part of the configuration fingerprint. A rendering is
// journaled, so changing the presenter changes only new messages and never makes a
// restored session drift.
func WithMessagePresenter(p present.Presenter) Option {
	return func(state *definitionState) error {
		if state.seen[keyMessagePresenter] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyMessagePresenter)}
		}
		if nilPresenter(p) {
			return &DefinitionError{Kind: DefinitionInvalidMessagePresenter}
		}
		state.seen[keyMessagePresenter] = true
		state.lifecycleOptions = append(state.lifecycleOptions, sessionruntime.WithLifecycleMessagePresenter(p))
		return nil
	}
}

func nilPresenter(p present.Presenter) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
```

`nilPresenter` duplicates `nilToolResultObjects` (`capture.go:176-186`). Extract both
into one `nilInterfaceValue(v any) bool` in `pkg/rig/options.go` and call it from both
(DRY). `any` is acceptable here because this is a reflection boundary.

`internal/sessionruntime/lifecycle.go`:
```go
// WithLifecycleMessagePresenter forwards the rig's presenter to every new and
// restored session. A restored session presents only messages that arrive after it
// is live. A re-offered input already carries its durable rendering.
func WithLifecycleMessagePresenter(p present.Presenter) LifecycleOption {
	return func(r *Lifecycle) {
		if p != nil {
			r.baseOpts = append(r.baseOpts, WithMessagePresenter(p))
		}
	}
}
```

`internal/sessionruntime/session.go`: add the field to `Session` (near
`toolResultReadable`, `:364`) and the option:
```go
	// presenter is the rig's Message Presenter, or nil. See present.go.
	presenter present.Presenter
```
```go
// WithMessagePresenter installs the session's Message Presenter.
func WithMessagePresenter(p present.Presenter) Option {
	return func(s *Session) { s.presenter = p }
}
```

**Step 4: Run it and watch it pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/rig/... ./internal/sessionruntime/... -run 'TestWithMessagePresenter|TestDefine'
```

**Step 5: Commit.**

```bash
git add pkg/rig internal/sessionruntime/lifecycle.go internal/sessionruntime/session.go
git commit -m "feat(rig): add WithMessagePresenter"
```

---

## Task 4: `runtimecommand.Admitted` principal and metadata

**Files:**
- Modify: `pkg/runtimecommand/command.go`: struct at `:183-224`, `Validate` at `:227-278`
- Test: `pkg/runtimecommand/principal_metadata_test.go`

**Step 1: Write the failing test.**

```go
package runtimecommand_test

import (
	"errors"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/runtimecommand"
)

func validPrincipal() *sessionwire.Principal {
	return &sessionwire.Principal{Tenant: "acme", Subject: "user_01", Kind: sessionwire.PrincipalKindActor}
}

func admittedOf(kind runtimecommand.Kind) runtimecommand.Admitted {
	a := runtimecommand.Admitted{
		CommandID: "cmd-1", RuntimeCommandID: testUUID(1), Kind: kind, LeaseEpoch: 1,
		AttemptID: "attempt-1",
	}
	switch kind {
	case runtimecommand.KindInput:
		a.Blocks = []content.Block{&content.TextBlock{Text: "hi"}}
	case runtimecommand.KindGateResponse:
		a.GateResponse = &gate.GateResponse{GateID: testUUID(2), Action: "approve"}
	}
	return a
}

func TestAdmittedPrincipalAndMetadata(t *testing.T) {
	t.Parallel()
	kinds := []runtimecommand.Kind{
		runtimecommand.KindInput, runtimecommand.KindCreate, runtimecommand.KindInterrupt,
		runtimecommand.KindRestore, runtimecommand.KindGateResponse,
	}
	for _, kind := range kinds {
		t.Run(string(kind)+"/principal accepted", func(t *testing.T) {
			t.Parallel()
			a := admittedOf(kind)
			a.Principal = validPrincipal()
			if err := a.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
		t.Run(string(kind)+"/invalid principal refused", func(t *testing.T) {
			t.Parallel()
			a := admittedOf(kind)
			a.Principal = &sessionwire.Principal{Tenant: "acme"} // no subject, no kind
			requireField(t, a.Validate(), "Principal")
		})
		metadataAllowed := kind == runtimecommand.KindInput || kind == runtimecommand.KindCreate
		t.Run(string(kind)+"/metadata", func(t *testing.T) {
			t.Parallel()
			a := admittedOf(kind)
			a.Metadata = sessionwire.MessageMetadata{"space": "family"}
			err := a.Validate()
			if metadataAllowed && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !metadataAllowed {
				requireField(t, err, "Metadata")
			}
		})
	}
	a := admittedOf(runtimecommand.KindInput)
	a.Metadata = sessionwire.MessageMetadata{"looprig_x": "v"} // held prefix (core rule)
	requireField(t, a.Validate(), "Metadata")
}

func requireField(t *testing.T, err error, field string) {
	t.Helper()
	var ve *runtimecommand.ValidationError
	if !errors.As(err, &ve) || ve.Field != field {
		t.Fatalf("error = %v, want *ValidationError{Field: %q}", err, field)
	}
}
```

`testUUID` is the existing helper in `pkg/runtimecommand/command_test.go:14` (core has no
`uuid.MustNew`). `gate.ID` is an alias of `uuid.UUID`. Drop the now-unused
`"github.com/looprig/core/uuid"` import if the compiler flags it. Check the
`gate.GateResponse` field names against `pkg/gate` before running.

**Step 2: Run it and watch it fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/runtimecommand/... -run TestAdmittedPrincipalAndMetadata
```

Expected: a build failure (`unknown field Principal in struct literal`).

**Step 3: Implement.** Add these fields to `Admitted` after `AttemptID`:

```go
	// Principal is the verified sender a Factory stamped, or nil. It is permitted on
	// every kind. Harness records it (the intent record, the interrupt record,
	// TurnInterrupted, GateResolved and the MessageInput on the message events) and
	// hands it to the presenter. It never places it on the model message. Harness
	// cannot verify it: it is Factory's recorded assertion, carried unchanged.
	Principal *coresessionwire.Principal
	// Metadata is the client's app-defined bag, permitted only on KindInput and
	// KindCreate, the kinds that carry a message to present. It is audit-only unless
	// the presenter renders it.
	Metadata coresessionwire.MessageMetadata
```

Import `coresessionwire "github.com/looprig/core/sessionwire/v1"`. Add this to
`Validate`, just before the final `return nil`:

```go
	if a.Principal != nil {
		if err := a.Principal.Validate(); err != nil {
			return &ValidationError{Field: "Principal", Reason: err.Error()}
		}
	}
	if a.Metadata != nil {
		if a.Kind != KindInput && a.Kind != KindCreate {
			return &ValidationError{Field: "Metadata", Reason: string(a.Kind) + " carries no message metadata"}
		}
		if err := a.Metadata.Validate(); err != nil {
			return &ValidationError{Field: "Metadata", Reason: err.Error()}
		}
	}
```

A non-nil empty map is valid shape but canonically absent. Treat `len(a.Metadata) == 0`
as absent everywhere downstream. Do not refuse it: Core's decoder produces nil for
absence.

`Application()` (`:334-341`) must **not** gain either field. The prefix is bodiless and
is correlated by the released reader. Add a comment saying so.

**Step 4: Run it and watch it pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/runtimecommand/...
```

**Step 5: Commit.**

```bash
git add pkg/runtimecommand
git commit -m "feat(runtimecommand): carry principal on every kind and metadata on create/input"
```

---

## Task 5: `command.UserInput` principal, metadata and the presented frame; `command.Interrupt` principal

**Files:**
- Modify: `pkg/command/submit.go`: `UserInput` at `:45-81`
- Modify: `pkg/command/interrupt.go`
- Modify: `pkg/command/marshal.go`: `userInputWire` `:176-183`, `marshalUserInput` `:185-202`, `decodeUserInput` `:371-388`
- Modify: `pkg/command/validate.go`: `case UserInput` `:73-91`
- Test: `pkg/command/attribution_test.go`

**Step 1: Write the failing test.**

```go
package command_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
)

func tb(s string) content.Block { return &content.TextBlock{Text: s} }

func TestUserInputAttributionRoundTrips(t *testing.T) {
	t.Parallel()
	in := command.UserInput{
		Header:    command.Header{CommandID: uuid.MustNew(), Agency: identity.AgencyUser},
		Blocks:    []content.Block{tb("Add milk")},
		Principal: &sessionwire.Principal{Tenant: "acme", Subject: "user_01", Kind: sessionwire.PrincipalKindActor},
		Metadata:  sessionwire.MessageMetadata{"space": "family"},
		Presented: &command.Presented{Prefix: []content.Block{tb("[from: Alex]")}},
	}
	b, err := command.MarshalCommand(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := command.UnmarshalCommand(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	again, err := command.MarshalCommand(got)
	if err != nil || !bytes.Equal(again, b) {
		t.Fatalf("not a fixed point:\n%s\n%s (%v)", b, again, err)
	}
	u := got.(command.UserInput)
	if blocks := u.ModelBlocks(); len(blocks) != 2 || blocks[0].(*content.TextBlock).Text != "[from: Alex]" {
		t.Fatalf("ModelBlocks = %#v", blocks)
	}
	mi := u.MessageInput()
	if mi == nil || mi.Prefix != 1 || mi.Suffix != 0 || mi.Principal.Subject != "user_01" || mi.Metadata["space"] != "family" {
		t.Fatalf("MessageInput = %#v", mi)
	}
}

// TestUserInputWithoutAttributionIsByteIdenticalToV0402 decodes the frozen v0.40.2
// golden and re-encodes it with the current codec.
func TestUserInputWithoutAttributionIsByteIdenticalToV0402(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"user_input.json", "interrupt.json"} {
		raw, err := os.ReadFile("../../internal/compat/testdata/pre_v0410/" + name)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.TrimSpace(raw)
		cmd, err := command.UnmarshalCommand(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := command.MarshalCommand(cmd)
		if err != nil || !bytes.Equal(out, raw) {
			t.Fatalf("%s drifted from v0.40.2:\n%s\n%s", name, raw, out)
		}
		if u, ok := cmd.(command.UserInput); ok && (u.Presented != nil || u.Principal != nil || u.Metadata != nil || u.MessageInput() != nil) {
			t.Fatalf("%s: pre-feature record decoded with attribution", name)
		}
	}
}

func TestUserInputAttributionValidation(t *testing.T) {
	t.Parallel()
	p := &sessionwire.Principal{Tenant: "acme", Subject: "u", Kind: sessionwire.PrincipalKindActor}
	base := func(agency identity.Agency) command.UserInput {
		return command.UserInput{Header: command.Header{CommandID: uuid.MustNew(), Agency: agency}, Blocks: []content.Block{tb("x")}}
	}
	tests := []struct {
		name    string
		mutate  func(*command.UserInput)
		agency  identity.Agency
		wantErr bool
	}{
		{name: "user with principal", agency: identity.AgencyUser, mutate: func(c *command.UserInput) { c.Principal = p }},
		{name: "machine with principal", agency: identity.AgencyMachine, mutate: func(c *command.UserInput) { c.Principal = p }, wantErr: true},
		{name: "machine with metadata", agency: identity.AgencyMachine, mutate: func(c *command.UserInput) { c.Metadata = sessionwire.MessageMetadata{"a": "b"} }, wantErr: true},
		{name: "machine presented", agency: identity.AgencyMachine, mutate: func(c *command.UserInput) { c.Presented = &command.Presented{Prefix: []content.Block{tb("x")}} }, wantErr: true},
		{name: "empty presented is not canonical", agency: identity.AgencyUser, mutate: func(c *command.UserInput) { c.Presented = &command.Presented{} }, wantErr: true},
		{name: "invalid principal", agency: identity.AgencyUser, mutate: func(c *command.UserInput) { c.Principal = &sessionwire.Principal{} }, wantErr: true},
		{name: "presented over frame cap", agency: identity.AgencyUser, mutate: func(c *command.UserInput) {
			c.Presented = &command.Presented{Prefix: make([]content.Block, 9)}
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := base(tt.agency)
			tt.mutate(&c)
			err := command.ValidateCommand(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateCommand() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestInterruptCarriesPrincipal(t *testing.T) {
	t.Parallel()
	p := &sessionwire.Principal{Tenant: "acme", Subject: "u", Kind: sessionwire.PrincipalKindActor}
	b, err := command.MarshalCommand(command.Interrupt{Header: command.Header{CommandID: uuid.MustNew(), Agency: identity.AgencyUser}, Principal: p})
	if err != nil {
		t.Fatal(err)
	}
	got, err := command.UnmarshalCommand(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.(command.Interrupt).Principal.Subject != "u" {
		t.Fatalf("principal lost: %s", b)
	}
}
```

**Step 2: Run it and watch it fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/command/... -run 'Attribution|ByteIdenticalToV0402|InterruptCarriesPrincipal'
```

Expected: a build failure (`unknown field Principal`, `undefined: command.Presented`).

**Step 3: Implement.**

`pkg/command/submit.go`: add these fields to `UserInput` after
`DelegateDeliveryPhase`, and add the new type and methods:

```go
	// Principal and Metadata are the attribution a Host-admitted (or in-process
	// SubmitInput) user message carried. They are durable on the intent record and
	// are copied onto the message events as event.MessageInput. Only AgencyUser input
	// may carry them. Machine input is never attributed.
	Principal *sessionwire.Principal      `json:"principal,omitzero"`
	Metadata  sessionwire.MessageMetadata `json:"metadata,omitempty"`
	// Presented is the ONE rendering the Message Presenter produced when the input
	// entered the session. nil means it was not presented or the frame was empty.
	// The loop sends Prefix ++ Blocks ++ Suffix to the model. A restore re-offer
	// reuses it verbatim and never presents again.
	Presented *Presented `json:"-"` // encoded by userInputWire
```

```go
// Presented is the journaled frame of one presented user message. It is never
// empty: an empty frame is stored as a nil *Presented, so an unpresented record is
// byte-identical to one written before presenters existed.
type Presented struct {
	Prefix []content.Block
	Suffix []content.Block
}

// ModelBlocks is the model-visible block sequence: Prefix ++ Blocks ++ Suffix. It
// does not clone. The loop clones when it builds the committed UserMessage.
func (c UserInput) ModelBlocks() []content.Block {
	if c.Presented == nil {
		return c.Blocks
	}
	out := make([]content.Block, 0, len(c.Presented.Prefix)+len(c.Blocks)+len(c.Presented.Suffix))
	out = append(out, c.Presented.Prefix...)
	out = append(out, c.Blocks...)
	return append(out, c.Presented.Suffix...)
}

// MessageInput is the attribution the message events record, or nil when the input
// carried none. nil keeps those events byte-identical to v0.40.2.
func (c UserInput) MessageInput() *event.MessageInput {
	if c.Principal == nil && len(c.Metadata) == 0 && c.Presented == nil {
		return nil
	}
	mi := &event.MessageInput{Principal: c.Principal, Metadata: c.Metadata}
	if c.Presented != nil {
		mi.Prefix, mi.Suffix = len(c.Presented.Prefix), len(c.Presented.Suffix)
	}
	return mi
}
```

(`event.MessageInput` is added in Task 6. Land Tasks 5 and 6 as one commit if the
build must stay green between them. `pkg/command` already imports `pkg/event`, in
`loop_tools.go`, so there is no cycle.)

`pkg/command/interrupt.go`:
```go
type Interrupt struct {
	Header
	// Principal is who stopped the session, when a Host-admitted interrupt carried
	// one. It is durable on each per-loop intent record, and the loop copies it onto
	// the TurnInterrupted of a turn this interrupt cancels.
	Principal *sessionwire.Principal `json:"principal,omitzero"`
	Ack       chan<- bool            `json:"-"`
}
```

`pkg/command/marshal.go`:
```go
type presentedWire struct {
	Prefix json.RawMessage `json:"prefix,omitempty"`
	Suffix json.RawMessage `json:"suffix,omitempty"`
}

type userInputWire struct {
	Header
	Blocks                json.RawMessage             `json:"blocks,omitempty"`
	NoFold                bool                        `json:"no_fold,omitzero"`
	TargetLoopID          uuid.UUID                   `json:"target_loop_id,omitzero"`
	BackgroundHandBack    bool                        `json:"background_hand_back,omitzero"`
	DelegateDeliveryPhase DelegateDeliveryPhase       `json:"delegate_delivery_phase,omitzero"`
	Principal             *sessionwire.Principal      `json:"principal,omitzero"`
	Metadata              sessionwire.MessageMetadata `json:"metadata,omitempty"`
	Presented             *presentedWire              `json:"presented,omitzero"`
}
```

In `marshalUserInput`, when `c.Presented != nil`, encode both halves with
`marshalBlocks` into a `presentedWire`, and set `Principal`/`Metadata`. In
`decodeUserInput`, when `w.Presented != nil`, decode both halves with `decodeBlocks`
into a `*Presented`. `mergeEnvelope` sorts map keys, so field order stays canonical.

`pkg/command/validate.go`: add this to `case UserInput` before `return nil`:
```go
		if c.Agency != identity.AgencyUser && (c.Principal != nil || len(c.Metadata) > 0 || c.Presented != nil) {
			return &CommandValidationError{Command: CommandUserInput, Field: FieldAttribution, Rule: RuleInvalid}
		}
		if c.Principal != nil && c.Principal.Validate() != nil {
			return &CommandValidationError{Command: CommandUserInput, Field: FieldPrincipal, Rule: RuleInvalid}
		}
		if len(c.Metadata) > 0 && c.Metadata.Validate() != nil {
			return &CommandValidationError{Command: CommandUserInput, Field: FieldMetadata, Rule: RuleInvalid}
		}
		if c.Presented != nil {
			frame := present.Frame{Prefix: c.Presented.Prefix, Suffix: c.Presented.Suffix}
			if frame.Empty() || frame.Validate() != nil {
				return &CommandValidationError{Command: CommandUserInput, Field: FieldPresented, Rule: RuleInvalid}
			}
		}
```

Add `FieldAttribution`, `FieldPrincipal`, `FieldMetadata` and `FieldPresented` to the
field constants (`:30-39`). Add `case Interrupt:` so an invalid non-nil `Principal` is
refused (`FieldPrincipal`). `pkg/command` importing `pkg/present` is allowed: present
imports only `runtimecommand`, `identity` and core.

`journal.NormalizedDeliveryFingerprint` (`pkg/journal/record.go:132-148`) fingerprints
`command.MarshalCommand(input)`, so the new members enter it automatically. Machine
inputs are the only phased ones, and they carry none, so existing fingerprints are
unchanged. No edit is needed. Add one assertion in
`pkg/journal/delivery_transition_test.go` that a machine phased record's fingerprint
is unchanged by this task (a fixed expected hex).

**Step 4: Run it and watch it pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/command/... ./pkg/journal/...
```

**Step 5: Commit** (together with Task 6 if needed to keep the build green).

```bash
git add pkg/command pkg/journal
git commit -m "feat(command): journal principal, metadata and the presented frame on user input"
```

---

## Task 6: event `MessageInput`; `Principal` on `TurnInterrupted` and `GateResolved`

**Files:**
- Modify: `pkg/event/turn.go`: `TurnStarted` `:86-92`, `TurnFoldedInto` `:179-185`, `InputCancelled` `:192-199`, `TurnInterrupted` `:292-297`
- Create: `pkg/event/message_input.go`
- Modify: `pkg/event/gate.go`: `GateResolved` `:77-101`
- Modify: `pkg/event/marshal.go`: `gateResolvedWire` `:1018-1026`, `marshalGateResolved`, `decodeGateResolved` `:1052-1073`
- Modify: `pkg/event/validate.go`: `validateEventBody` `:182`; add field names
- Test: `pkg/event/message_input_test.go`

Event `v` stays 1. See correction 1 for why a conditional bump is rejected.

**Step 1: Write the failing test.**

```go
package event_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/event"
)

// TestPreFeatureEventsAreByteIdentical decodes each frozen v0.40.2 golden and
// re-encodes it with the current codec.
func TestPreFeatureEventsAreByteIdentical(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"turn_started.json", "turn_folded_into.json", "input_cancelled.json", "turn_interrupted.json", "gate_resolved.json"} {
		raw, err := os.ReadFile("../../internal/compat/testdata/pre_v0410/" + name)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.TrimSpace(raw)
		ev, err := event.UnmarshalEvent(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := event.MarshalEvent(ev)
		if err != nil || !bytes.Equal(out, raw) {
			t.Fatalf("%s drifted from v0.40.2:\n%s\n%s", name, raw, out)
		}
	}
}

func TestMessageInputRoundTripAndValidation(t *testing.T) {
	t.Parallel()
	p := &sessionwire.Principal{Tenant: "acme", Subject: "u", Kind: sessionwire.PrincipalKindActor}
	msg := &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{
		&content.TextBlock{Text: "[from: A]"}, &content.TextBlock{Text: "Add milk"},
	}}}
	ok := turnStartedFixture(t) // copy the helper from an existing TurnStarted test in this package
	ok.Message = msg
	ok.Input = &event.MessageInput{Principal: p, Metadata: sessionwire.MessageMetadata{"space": "family"}, Prefix: 1}
	b, err := event.MarshalEvent(ok)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Fatalf("event version moved: %s", b)
	}
	back, err := event.UnmarshalEvent(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := back.(event.TurnStarted).Input; got == nil || got.Prefix != 1 || got.Principal.Subject != "u" {
		t.Fatalf("Input = %#v", got)
	}
	if user := event.UserBlocks(back.(event.TurnStarted).Message, back.(event.TurnStarted).Input); len(user) != 1 || user[0].(*content.TextBlock).Text != "Add milk" {
		t.Fatalf("UserBlocks = %#v", user)
	}

	bad := []struct {
		name  string
		input *event.MessageInput
	}{
		{name: "empty input is not canonical", input: &event.MessageInput{}},
		{name: "frame longer than message", input: &event.MessageInput{Prefix: 2, Suffix: 1}},
		{name: "invalid principal", input: &event.MessageInput{Principal: &sessionwire.Principal{}}},
		{name: "invalid metadata", input: &event.MessageInput{Metadata: sessionwire.MessageMetadata{"Bad Key": "v"}}},
	}
	for _, tt := range bad {
		ev := ok
		ev.Input = tt.input
		if _, err := event.MarshalEvent(ev); err == nil {
			t.Errorf("%s: MarshalEvent accepted it", tt.name)
		}
	}
}

func TestMessageInputDecodeIsStrict(t *testing.T) {
	t.Parallel()
	ok := turnStartedFixture(t)
	b, err := event.MarshalEvent(ok)
	if err != nil {
		t.Fatal(err)
	}
	forged := bytes.Replace(b, []byte(`{`), []byte(`{"input":{"prefix":0,"future":1},`), 1)
	if _, err := event.UnmarshalEvent(forged); err == nil {
		t.Fatal("an unknown member inside input must fail closed")
	}
}
```

Also add `TestGateResolvedAndTurnInterruptedCarryPrincipal` (marshal, unmarshal, assert
`Principal.Subject`), plus a case that a machine-agency `TurnStarted`
(`Cause.Agency == AgencyMachine`) with non-nil `Input` is refused.

**Step 2: Run it and watch it fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/event/... -run 'PreFeature|MessageInput|CarryPrincipal'
```

Expected: `TestPreFeatureEventsAreByteIdentical` passes already. The others fail to
build.

**Step 3: Implement.**

`pkg/event/message_input.go`:
```go
package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
)

// MessageInput is the attribution of one USER message on the events that carry
// it (TurnStarted, TurnFoldedInto, InputCancelled). Message.Blocks on those events
// is the ASSEMBLED model message: Prefix presenter blocks, then the user's blocks,
// then Suffix presenter blocks. Blocks[Prefix : len-Suffix] is exactly what the
// user sent. nil means none of the four members is set, so the event is
// byte-identical to v0.40.2.
//
// Its decoder is STRICT, unlike the plain event decoder around it, so a member a
// later release adds fails closed here rather than being dropped. The v0.40.2
// decoder drops this whole member silently; that is the documented lossy-rollback
// limit.
type MessageInput struct {
	Principal *sessionwire.Principal      `json:"principal,omitzero"`
	Metadata  sessionwire.MessageMetadata `json:"metadata,omitempty"`
	Prefix    int                         `json:"prefix,omitzero"`
	Suffix    int                         `json:"suffix,omitzero"`
}

type messageInputWire MessageInput

// UnmarshalJSON refuses unknown members and trailing data.
func (m *MessageInput) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w messageInputWire
	if err := dec.Decode(&w); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data after input")
	}
	*m = MessageInput(w)
	return nil
}

// UserBlocks returns the user's own blocks from an assembled message: all blocks
// when in is nil, else Blocks[Prefix : len-Suffix]. It returns nil when the counts
// do not fit, which validation already refuses.
func UserBlocks(msg *content.UserMessage, in *MessageInput) []content.Block {
	if msg == nil {
		return nil
	}
	if in == nil {
		return msg.Blocks
	}
	end := len(msg.Blocks) - in.Suffix
	if in.Prefix < 0 || in.Suffix < 0 || in.Prefix > end {
		return nil
	}
	return msg.Blocks[in.Prefix:end]
}

func validateMessageInput(name EventName, agency identity.Agency, msg *content.UserMessage, in *MessageInput) error {
	if in == nil {
		return nil
	}
	switch {
	case agency != identity.AgencyUser,
		in.Principal == nil && len(in.Metadata) == 0 && in.Prefix == 0 && in.Suffix == 0,
		in.Prefix < 0 || in.Suffix < 0,
		msg == nil || in.Prefix+in.Suffix > len(msg.Blocks),
		in.Principal != nil && in.Principal.Validate() != nil,
		len(in.Metadata) > 0 && in.Metadata.Validate() != nil:
		return &InvalidEventError{Event: name, Field: FieldInput, Rule: RuleInvalid}
	}
	return nil
}
```

`pkg/event` already imports `pkg/identity` in `validate.go:12`.

In `turn.go`, add this to `TurnStarted`, `TurnFoldedInto` and `InputCancelled`:
```go
	Input *MessageInput `json:"input,omitzero"`
```
and add this to `TurnInterrupted`:
```go
	// Principal is who interrupted this turn, when the interrupt carried one.
	Principal *sessionwire.Principal `json:"principal,omitzero"`
```

In `gate.go`, add `Principal *sessionwire.Principal` to `GateResolved` with a
comment: "who answered, when the admitted gate_response carried one". Add it to
`gateResolvedWire` (`json:"principal,omitzero"`) and copy it in `marshalGateResolved`
and `decodeGateResolved`.

In `validate.go`, add `FieldInput FieldName = "Input"` and `FieldPrincipal FieldName
= "Principal"`, then add these cases to `validateEventBody`:
```go
	case TurnStarted:
		return validateMessageInput("TurnStarted", e.Cause.Agency, e.Message, e.Input)
	case TurnFoldedInto:
		return validateMessageInput("TurnFoldedInto", e.Cause.Agency, e.Message, e.Input)
	case InputCancelled:
		return validateMessageInput("InputCancelled", e.Cause.Agency, e.Message, e.Input)
	case TurnInterrupted:
		if e.Principal != nil && e.Principal.Validate() != nil {
			return &InvalidEventError{Event: "TurnInterrupted", Field: FieldPrincipal, Rule: RuleInvalid}
		}
	case GateResolved:
		if e.Principal != nil && e.Principal.Validate() != nil {
			return &InvalidEventError{Event: "GateResolved", Field: FieldPrincipal, Rule: RuleInvalid}
		}
```

First check that none of these types already has a case in `validateEventBody`
(`grep -n "case TurnStarted\|case GateResolved" pkg/event/validate.go`). If one does,
merge into it.

Also extend the reflection/fill tests that enumerate event fields
(`grep -rln "reflect" pkg/event/*_test.go`) so the new fields are populated.

**Step 4: Run it and watch it pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/event/... ./pkg/command/...
```

**Step 5: Fuzz the strict decoder.**

```bash
GOTOOLCHAIN=go1.26.8 go test ./pkg/event -run '^$' -fuzz=FuzzUnmarshalEvent -fuzztime=30s
```

If there is no `FuzzUnmarshalEvent` (`grep -rn "func Fuzz" pkg/event`), add a seed of
the new `TurnStarted` encoding to the closest existing event fuzz target.

**Step 6: Commit.**

```bash
git add pkg/event
git commit -m "feat(event): record message attribution and the principal on interrupts and gate answers"
```

---

## Task 7: Present at the dispatch boundary; machine input bypasses

**Files:**
- Create: `internal/sessionruntime/present.go`
- Modify: `internal/sessionruntime/runtime_command.go`: `prepareAdmittedInput` `:760-798`
- Modify: `internal/sessionruntime/session.go`: `buildAndAuditUserInput` `:2605-2611`, `dispatchUserInput` `:2601-2603`, `submitToLoop` `:2536-2556`
- Test: `internal/sessionruntime/present_test.go`

**Step 1: Write the failing tests.** Use the fixture in
`internal/sessionruntime/runtime_command_test.go:91-129` and the disposition readers
in `attempt_disposition_test.go:23-63`.

```go
package sessionruntime

// countingPresenter records every call and returns a fixed prefix.
type countingPresenter struct {
	mu    sync.Mutex
	calls []present.Input
	frame present.Frame
	err   error
}

func (p *countingPresenter) Present(_ context.Context, in present.Input) (present.Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, in)
	return p.frame, p.err
}

func (p *countingPresenter) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.calls) }

func TestAdmittedInputIsPresentedOnceAndCarriesAttribution(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	p := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "[from: Alex]"}}}}
	WithMessagePresenter(p)(f.session)

	admitted := f.admittedInput("cmd-1", mustUUID(), "Add milk")
	admitted.AttemptID = "attempt-1"
	admitted.Principal = &coresessionwire.Principal{Tenant: "acme", Subject: "user_01", Kind: coresessionwire.PrincipalKindActor}
	admitted.Metadata = coresessionwire.MessageMetadata{"space": "family"}
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	in := f.drainOne(t).(command.UserInput)
	if p.count() != 1 {
		t.Fatalf("presenter calls = %d, want 1", p.count())
	}
	call := p.calls[0]
	if call.Kind != runtimecommand.KindInput || call.Principal.Subject != "user_01" || call.Metadata["space"] != "family" {
		t.Fatalf("presenter input = %#v", call)
	}
	if in.Presented == nil || len(in.Presented.Prefix) != 1 || in.Principal == nil || in.Metadata["space"] != "family" {
		t.Fatalf("dispatched input lacks attribution: %#v", in)
	}
	if got := in.Blocks[0].(*content.TextBlock).Text; got != "Add milk" {
		t.Fatalf("user blocks modified: %q", got)
	}

	// Redelivery after the prefix: a duplicate, not presented again.
	if d, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil || !d.Duplicate {
		t.Fatalf("redelivery = %+v, %v", d, err)
	}
	// NOTE: the redelivery path calls prepareAdmittedInput BEFORE the prefix (the
	// audit-first ordering). Assert the count the implementation actually yields and
	// pin it. See Step 3's "redelivery" note.
}

func TestUnpresentedInputHasNoAttribution(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t) // no presenter
	admitted := f.admittedInput("cmd-1", mustUUID(), "hi")
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil {
		t.Fatal(err)
	}
	in := f.drainOne(t).(command.UserInput)
	if in.Presented != nil || in.Principal != nil || in.Metadata != nil {
		t.Fatalf("attribution appeared from nowhere: %#v", in)
	}
}

func TestMachineInputIsNeverPresented(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	p := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "x"}}}}
	WithMessagePresenter(p)(f.session)
	if _, err := f.session.submitToLoop(context.Background(), f.session.activeLoopID,
		[]content.Block{&content.TextBlock{Text: "hand-back"}}, identity.AgencyMachine, false); err != nil {
		t.Fatal(err)
	}
	in := f.drainOne(t).(command.UserInput)
	if p.count() != 0 || in.Presented != nil {
		t.Fatalf("machine input presented: calls=%d presented=%v", p.count(), in.Presented)
	}
}
```

A `SubagentResult` is built on its own path (`delegation.go`, `grep -n
"command.SubagentResult{" internal/sessionruntime/*.go`) and has no attribution fields.
Assert that at the type level: add a compile-time comment test that `SubagentResult`
has no `Presented` field. A reflection check over `reflect.TypeOf(command.SubagentResult{})`
failing on a field named `Presented` or `Principal` is enough.

**Step 2: Run them and watch them fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/... -run 'Presented|MachineInputIsNeverPresented|UnpresentedInput'
```

**Step 3: Implement.**

`internal/sessionruntime/present.go`:
```go
package sessionruntime

import (
	"context"
	"time"

	"github.com/looprig/core/content"
	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/present"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// presentTimeout bounds one presenter call. A presenter may do a bounded lookup.
// An unbounded one would hold a Host's disposition attempt open indefinitely.
const presentTimeout = 5 * time.Second

// attribution is what a user input carries besides its blocks.
type attribution struct {
	principal *coresessionwire.Principal
	metadata  coresessionwire.MessageMetadata
	presented *command.Presented
}

// presentUserInput runs the session's presenter once for one USER message and
// returns its attribution. It is the ONLY place a presenter is invoked. Machine
// input never reaches it: every caller gates on AgencyUser. A restore re-offer
// never reaches it either, because the re-offer carries its durable Presented.
func (s *Session) presentUserInput(
	ctx context.Context,
	loopID uuid.UUID,
	kind runtimecommand.Kind,
	blocks []content.Block,
	principal *coresessionwire.Principal,
	metadata coresessionwire.MessageMetadata,
) (attribution, error) {
	attr := attribution{principal: principal}
	if len(metadata) > 0 {
		attr.metadata = metadata
	}
	if s.presenter == nil {
		return attr, nil
	}
	pctx, cancel := context.WithTimeout(ctx, presentTimeout)
	defer cancel()
	frame, err := present.Run(pctx, s.presenter, present.Input{
		SessionID: s.sessionID, LoopID: loopID, AgentName: s.agentNameFor(loopID),
		Kind: kind, Principal: principal, Metadata: attr.metadata, Blocks: blocks,
	})
	if err != nil {
		return attribution{}, err
	}
	if !frame.Empty() {
		attr.presented = &command.Presented{Prefix: frame.Prefix, Suffix: frame.Suffix}
	}
	return attr, nil
}

// agentNameFor is the loop's immutable attribution name, or "" for a loop with no
// bound definition (test doubles).
func (s *Session) agentNameFor(loopID uuid.UUID) identity.AgentName {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	if h, ok := s.loops[loopID]; ok && h != nil && h.bound != nil {
		return h.bound.Name()
	}
	return ""
}
```

`session.go`: change `buildAndAuditUserInput` to take the attribution. It has two
callers, `dispatchUserInput` and `prepareAdmittedInput`:
```go
func (s *Session) buildAndAuditUserInput(ctx context.Context, loopID uuid.UUID, blocks []content.Block, agency identity.Agency, noFold bool, id uuid.UUID, attr attribution) command.UserInput {
	cmd := command.UserInput{
		Header: command.Header{CommandID: id, Agency: agency, CreatedAt: s.stampNow()}, Blocks: blocks, NoFold: noFold,
		Principal: attr.principal, Metadata: attr.metadata, Presented: attr.presented,
	}
	s.appendCommand(ctx, loopID, cmd)
	return cmd
}
```

`dispatchUserInput` gains an `attr attribution` parameter and passes it through.
`submitToLoop` presents only for user agency, **before** minting the id:
```go
	var attr attribution
	if agency == identity.AgencyUser {
		var err error
		if attr, err = s.presentUserInput(ctx, loopID, runtimecommand.KindInput, blocks, nil, nil); err != nil {
			return uuid.UUID{}, err
		}
	}
```
Task 13 generalises this so `SubmitInput` can pass a principal and metadata. Keep the
signature change minimal here.

`runtime_command.go`, `prepareAdmittedInput`: insert presentation after the
loop-exited check (`:782`), before `if admitted.AttemptID == ""`:
```go
	attr, err := s.presentUserInput(ctx, active, admitted.Kind, admitted.Blocks, admitted.Principal, admitted.Metadata)
	if err != nil {
		return nil, err // *present.Error; ApplyRuntimeCommand refuses (Task 10)
	}
	if admitted.AttemptID == "" {
		cmd := s.buildAndAuditUserInput(ctx, active, admitted.Blocks, identity.AgencyUser, false, admitted.RuntimeCommandID, attr)
		return &pendingInput{backend: l, cmd: cmd}, nil
	}
	cmd := command.UserInput{
		Header:    command.Header{CommandID: admitted.RuntimeCommandID, Agency: identity.AgencyUser, CreatedAt: s.stampNow()},
		Blocks:    admitted.Blocks,
		Principal: attr.principal, Metadata: attr.metadata, Presented: attr.presented,
	}
```

**Redelivery note.** `prepareAdmittedInput` runs before the prefix append (the
audit-first ordering, `runtime_command.go:394-420`). So a redelivery of an
already-applied command **does** call the presenter, and the result is then discarded
when the prefix answers `Duplicate`. That breaks "once per input". Fix it cheaply: in
`ApplyRuntimeCommand`, before `prepareAdmittedInput`, check durable application.
`dispositions.ScanCommandEffect(ctx, admitted.CommandID, admitted.RuntimeCommandID)`
(`runtime_command.go:39-42`) reports `PrefixSeq != 0` for an applied command. Short-
circuit it to the same `Duplicate` disposition the prefix-collision path returns
(`:429-437`), without presenting and without appending the intent. Only do this when
`hasDispositions`. Legacy logs keep today's path, where the orphan-intent cost is
already documented at `:737-753`. The scan is a whole-journal walk. Guard it so it runs
only when `s.presenter != nil`, which keeps the unpresented path's cost unchanged, and
say so in a comment. Then the test asserts `p.count() == 1` after the redelivery.

The `create` arm reaches `prepareAdmittedInput` only when it has blocks
(`:413-420`), so a bare create is never presented. Test that too: a bare `KindCreate`
with a principal gives `p.count() == 0`.

**Step 4: Run them and watch them pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/...
```

**Step 5: Commit.**

```bash
git add internal/sessionruntime
git commit -m "feat(sessionruntime): present user input once at the dispatch boundary"
```

---

## Task 8: Loop composition and the message events

**Files:**
- Modify: `internal/loopruntime/loop.go`: `queuedInput` `:642-667`, `userMessageFromBlocks` `:1765-1767`, `UserInput` arm `:2591`, `TurnStarted` `:1657-1668`, `returnEntry` `:1778-1795`
- Modify: `internal/loopruntime/turn.go`: `TurnFoldedInto` `:785-800`
- Modify: `internal/loopruntime/message_clone.go`: add `cloneMessageInput`
- Test: `internal/loopruntime/presented_input_test.go`

**Step 1: Write the failing test.** It follows `agency_test.go:19-80` (`newLoop`,
`awaitReply`, `textBlocks`, `mustID`).

```go
func TestPresentedInputComposesTheModelMessage(t *testing.T) {
	t.Parallel()
	llm := &fakeLLM{chunks: []content.Chunk{textChunk("ok")}}
	l, rec, _ := newLoop(t, llm)
	id := mustID(t)
	principal := &sessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: sessionwire.PrincipalKindActor}
	user := textBlocks("Add milk")
	l.Commands <- command.UserInput{
		Header:    command.Header{CommandID: id, Agency: identity.AgencyUser},
		Blocks:    user,
		Principal: principal,
		Metadata:  sessionwire.MessageMetadata{"space": "family"},
		Presented: &command.Presented{Prefix: textBlocks("[from: Alex]"), Suffix: textBlocks("(sent from phone)")},
	}
	started := awaitReply(t, rec, id).(event.TurnStarted)
	blocks := started.Message.Blocks
	if len(blocks) != 3 || blocks[0].(*content.TextBlock).Text != "[from: Alex]" ||
		blocks[2].(*content.TextBlock).Text != "(sent from phone)" {
		t.Fatalf("assembled message = %#v", blocks)
	}
	if started.Input == nil || started.Input.Prefix != 1 || started.Input.Suffix != 1 || started.Input.Principal.Subject != "u1" {
		t.Fatalf("Input = %#v", started.Input)
	}
	if got := event.UserBlocks(started.Message, started.Input); len(got) != 1 || got[0].(*content.TextBlock).Text != "Add milk" {
		t.Fatalf("user blocks not recoverable exactly: %#v", got)
	}
	// The model request saw the assembled message.
	req := llm.lastRequest(t) // use the existing request-capture helper; grep "lastRequest\|requests" fake_llm_test.go
	last := req.Messages[len(req.Messages)-1].(*content.UserMessage)
	if len(last.Blocks) != 3 {
		t.Fatalf("model saw %d blocks, want 3", len(last.Blocks))
	}
}
```

Add a fold case in `fold_test.go`: a presented input queued behind a running turn
folds with `TurnFoldedInto.Input` set. Add a retraction case in
`cancel_queued_test.go`: `InputCancelled.Input` is set. Add a machine case: a
`SubagentResult` yields `Input == nil`.

**Step 2: Run it and watch it fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/loopruntime/... -run 'Presented'
```

**Step 3: Implement.**

- `queuedInput`: add `input *event.MessageInput` with the doc comment "the user
  message's attribution, copied onto TurnStarted/TurnFoldedInto/InputCancelled; nil
  for machine input".
- `:2591`:
  ```go
  qi := queuedInput{inputID: c.CommandHeader().CommandID, agency: c.CommandHeader().Agency,
      msg: userMessageFromBlocks(c.ModelBlocks()), input: c.MessageInput(), noFold: c.NoFold}
  ```
  `userMessageFromBlocks` already clones (`content.CloneBlocks`), so the composed
  slice shares nothing with the command.
- `TurnStarted` (`:1667`), `returnEntry` (`:1794`) and `turn.go:798`: add
  `Input: cloneMessageInput(qi.input)`.
- `message_clone.go`:
  ```go
  func cloneMessageInput(in *event.MessageInput) *event.MessageInput {
  	if in == nil {
  		return nil
  	}
  	out := *in
  	if in.Principal != nil {
  		p := *in.Principal
  		out.Principal = &p
  	}
  	if in.Metadata != nil {
  		out.Metadata = maps.Clone(in.Metadata)
  	}
  	return &out
  }
  ```
- The turn hook (`loop.go:1675-1678`) receives `started.Message`, the assembled
  message. That is correct: hooks see what the model sees. Say so in the hook doc
  (`pkg/hook`, `TurnData.Input`).

`internal/loopruntime/ownership_test.go:272` (`TestTurnStartedOwnsCommandAndHistoryGraphs`)
must be extended so mutating the command's `Presented`, `Principal` and `Metadata`
after send does not change the published event.

**Step 4: Run it and watch it pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/loopruntime/...
```

**Step 5: Commit.**

```bash
git add internal/loopruntime pkg/hook
git commit -m "feat(loopruntime): compose the presented frame and record message attribution"
```

---

## Task 9: A restore re-offer reuses `Presented`; the fold is byte-identical

No production change is expected: `planAppliedAdmittedInputs` re-offers the decoded
intent record verbatim (`admitted_input_replay.go:92-97`, `:143-167`), and `foldLoop`
appends `TurnStarted.Message` verbatim (`restore.go:866-877`). This task pins both.

**Files:**
- Test: `internal/sessionruntime/presented_restore_test.go`

**Step 1: Write the tests.** Base them on the existing durability tests in
`admitted_input_durability_test.go`. List them with `grep -n "^func Test"
internal/sessionruntime/admitted_input_durability_test.go`, and reuse the one that
applies an input, kills the runtime before `TurnStarted`, and restores.

```go
func TestReofferedPresentedInputIsNotPresentedAgain(t *testing.T) {
	t.Parallel()
	// 1. Build the durability fixture with a countingPresenter (prefix "[from: Alex]").
	// 2. Apply an attempt-bearing input with a principal; stop the runtime after the
	//    applied disposition and BEFORE TurnStarted (the fixture's existing seam).
	// 3. Restore with a SECOND countingPresenter that returns a DIFFERENT frame.
	// 4. Await the replayed TurnStarted.
	// Assert:
	//   - second presenter call count == 0;
	//   - TurnStarted.Message.Blocks[0] is "[from: Alex]" (the durable rendering);
	//   - TurnStarted.Input.Principal equals the admitted principal;
	//   - event.UserBlocks(...) equals the admitted blocks byte for byte
	//     (compare content.MarshalBlocks outputs).
}

func TestFoldReproducesPresentedMessagesVerbatim(t *testing.T) {
	t.Parallel()
	msg := &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{
		&content.TextBlock{Text: "[from: A]"}, &content.TextBlock{Text: "hi"},
	}}}
	started := event.TurnStarted{Message: msg, Input: &event.MessageInput{Prefix: 1}}
	got := foldLoop([]event.Event{started}).Msgs
	want, _ := content.MarshalMessages(content.AgenticMessages{msg}) // use the codec the fold tests use
	have, _ := content.MarshalMessages(got)
	if !bytes.Equal(want, have) {
		t.Fatalf("fold drifted:\n%s\n%s", want, have)
	}
}
```

Fill in step 1–4 of the first test with the fixture's own helpers. It must be a real
store-backed test (memstore via `sessionstoreOverMemstore`, `runtime_command_test.go:47`),
not a mock.

**Step 2: Run them.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/... -run 'Reoffered|FoldReproducesPresented'
```

Expected: PASS. If the re-offer test fails because `decodeUserInput` dropped
`presented`, Task 5's decoder is incomplete. Fix it there, not here.

**Step 3: Commit.**

```bash
git add internal/sessionruntime/presented_restore_test.go
git commit -m "test(sessionruntime): a restore re-offer reuses the journaled rendering"
```

---

## Task 10: A presenter failure refuses the command

**Files:**
- Modify: `internal/sessionruntime/runtime_command.go`: the `prepErr` branch at `:413-420`
- Test: `internal/sessionruntime/present_refusal_test.go`

**Step 1: Write the failing tests.**

```go
func TestPresenterFailureRefusesAnAttemptWithNoIntent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p    *countingPresenter
		kind present.ErrorKind
	}{
		{name: "presenter error", p: &countingPresenter{err: errors.New("directory down")}, kind: present.ErrorPresenterFailed},
		{name: "invalid frame", p: &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.ThinkingBlock{Thinking: "x"}}}}, kind: present.ErrorFrameInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			WithMessagePresenter(tc.p)(f.session)
			admitted := f.admittedInput("cmd-1", mustUUID(), "hi")
			admitted.AttemptID = "attempt-1"
			d, err := f.session.ApplyRuntimeCommand(context.Background(), admitted)
			var pe *present.Error
			if !errors.As(err, &pe) || pe.Kind != tc.kind {
				t.Fatalf("err = %v, want *present.Error{%s}", err, tc.kind)
			}
			if d.PrefixSequence == 0 {
				t.Fatal("the prefix must be durable so a redelivery deduplicates")
			}
			if got := onlyDisposition(t, f); got.Disposition != runtimecommand.DispositionRefused {
				t.Fatalf("disposition = %q, want refused", got.Disposition)
			}
			f.requireNoCommand(t, "a refused presentation")
			requireNoIntentRecord(t, f, admitted.RuntimeCommandID) // walk with OpenInternalRecordReplayer as readDispositions does
			// A redelivery is a duplicate and does not call the presenter again.
			if d2, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil || !d2.Duplicate {
				t.Fatalf("redelivery = %+v, %v", d2, err)
			}
			if tc.p.count() != 1 {
				t.Fatalf("presenter calls = %d, want 1", tc.p.count())
			}
		})
	}
}

func TestPresenterFailureOnALegacyCommandWritesNothing(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	WithMessagePresenter(&countingPresenter{err: errors.New("x")})(f.session)
	d, err := f.session.ApplyRuntimeCommand(context.Background(), f.admittedInput("cmd-1", mustUUID(), "hi"))
	var pe *present.Error
	if !errors.As(err, &pe) || d != (runtimecommand.Disposition{}) {
		t.Fatalf("got %+v, %v; want zero disposition and *present.Error", d, err)
	}
	if n := len(readDispositions(t, f)); n != 0 {
		t.Fatalf("%d dispositions written", n)
	}
}
```

Write `requireNoIntentRecord` next to `readDispositions`. It fails on any
`journal.CommandRecord` whose `command.UserInput.CommandID` equals the runtime id.

**Step 2: Run them and watch them fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/... -run 'PresenterFailure'
```

**Step 3: Implement.** Replace the `prepErr` return at `:417-419`:
```go
		if prepErr != nil {
			var presentErr *present.Error
			if errors.As(prepErr, &presentErr) {
				return s.refusePresentation(ctx, log, dispositions, admitted, presentErr)
			}
			return runtimecommand.Disposition{}, prepErr
		}
```
Then add:
```go
// refusePresentation settles a command whose message the presenter would not
// frame. No intent record exists, because presentation runs before it, so nothing
// durable holds the input.
//
// For an attempt-bearing command it writes the bodiless application prefix, then
// `refused` under this runtime's own grant. That is the existing "the effect failed
// after the prefix" arm. The prefix makes every redelivery of this attempt a
// Duplicate, so the presenter is not asked again and no second disposition is
// attempted. The disposition carries no reason: the durable frame has no reason
// member. The reason is on the returned error and on one WARN line holding only the
// error kind, never product text, which may name a person.
//
// A legacy command (no attempt) has no disposition to write. It returns the zero
// Disposition, so the command may be re-offered.
func (s *Session) refusePresentation(
	ctx context.Context, log runtimeCommandLog, dispositions dispositionLog,
	admitted runtimecommand.Admitted, cause *present.Error,
) (runtimecommand.Disposition, error) {
	slog.WarnContext(ctx, "session: message presenter refused an admitted input",
		"session", s.sessionID, "command_id", admitted.CommandID, "kind", cause.Kind)
	if admitted.AttemptID == "" {
		return runtimecommand.Disposition{}, cause
	}
	var res journal.AppendResult
	if err := s.sealedDurableWrite(func() error {
		var appendErr error
		res, appendErr = log.AppendCommandApplication(ctx, admitted.Application())
		return appendErr
	}); err != nil {
		return s.resolveApplicationConflict(ctx, log, admitted, err)
	}
	disposition := runtimecommand.Disposition{
		CommandID: admitted.CommandID, RuntimeCommandID: admitted.RuntimeCommandID, PrefixSequence: res.Sequence,
	}
	if !res.Appended {
		disposition.Duplicate = true
		return disposition, nil
	}
	return disposition, errors.Join(cause, s.recordDisposition(ctx, dispositions, admitted, runtimecommand.DispositionRefused))
}
```

Add a `create`-with-blocks presenter-failure case to the table (same outcome), and
update the per-kind table comment at `:449-462` with the row
`input/create, the presenter refused the message -> refused (prefix, no intent)`.

**Step 4: Run them and watch them pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/...
```

**Step 5: Commit.**

```bash
git add internal/sessionruntime
git commit -m "feat(sessionruntime): a presenter failure settles the command refused"
```

---

## Task 11: Principal on interrupt and gate_response

**Files:**
- Modify: `internal/sessionruntime/runtime_command.go`: `KindInterrupt` arm `:494-509`, `KindGateResponse` arm `:510-523`
- Modify: `internal/sessionruntime/interrupt.go`: `runInterrupt` `:283`, `runInterruptWithBarrier` `:296`, `fanoutInterrupt` `:211-250`
- Modify: `internal/sessionruntime/session.go`: `Interrupt` `:2719-2724`
- Modify: `internal/sessionruntime/gates.go`: `respondGateAsCaller` `:1007`, `respondGateCore` `:1029`, `buildGateResolved` `:1330`
- Modify: `internal/loopruntime/loop.go`: the `command.Interrupt` arm `:2683-2702` and `handleTurnResult` `:2316-2332`
- Tests: `internal/sessionruntime/principal_commands_test.go`, `internal/loopruntime/interrupt_principal_test.go`

**Step 1: Write the failing tests.**

Session level. Use `admittedInterrupt` and `ackInterrupts` from
`attempt_disposition_test.go:65-107`; the gate path follows the gate_response tests in
`pkg/runtimecommand/gate_response_test.go` and `internal/sessionruntime/*gate_response*_test.go`.

```go
func TestAdmittedInterruptJournalsItsPrincipal(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	p := &coresessionwire.Principal{Tenant: "acme", Subject: "parent", Kind: coresessionwire.PrincipalKindActor}
	got := make(chan command.Interrupt, 1)
	// A responder that captures the interrupt and acks false (idle).
	go func() {
		cmd := <-f.cmds
		in := cmd.(command.Interrupt)
		got <- in
		in.Ack <- false
	}()
	a := f.admittedInterrupt("int-1", mustUUID())
	a.Principal = p
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if in := <-got; in.Principal == nil || in.Principal.Subject != "parent" {
		t.Fatalf("interrupt principal = %#v", in.Principal)
	}
}
```

Add `TestAdmittedGateResponseStampsGateResolvedPrincipal`. Open a session-owned gate
the way the existing gate_response tests do, apply
`KindGateResponse` with a principal, then read the committed `GateResolved` and assert
`Principal.Subject`. Add `TestRespondGateLeavesPrincipalNil`: plain `RespondGate`
gives `Principal == nil`, and its bytes equal the frozen golden's shape (no
`principal` key). Add `TestRestoreAcceptsPrincipalAndJournalsNoRecord`: a
`KindRestore` with a principal is `applied`, and the journal gains only prefix +
disposition (correction 5).

Loop level:

```go
func TestInterruptPrincipalLandsOnTurnInterrupted(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)
	ack := make(chan bool, 1)
	p := &sessionwire.Principal{Tenant: "acme", Subject: "parent", Kind: sessionwire.PrincipalKindActor}
	l.Commands <- command.Interrupt{Ack: ack, Principal: p}
	if !<-ack {
		t.Fatal("ack false")
	}
	term, ok := drainToTerminal(t, rec).(event.TurnInterrupted)
	if !ok || term.Principal == nil || term.Principal.Subject != "parent" {
		t.Fatalf("terminal = %#v", term)
	}
}
```

Add a second case: an interrupt with **no** principal leaves `TurnInterrupted.Principal
== nil`, and a principal from an earlier interrupt does not leak onto a later turn.

**Step 2: Run them and watch them fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/... ./internal/loopruntime/... -run 'Principal'
```

**Step 3: Implement.**

- `interrupt.go`: thread a `principal *coresessionwire.Principal` parameter through
  `runInterrupt` → `runInterruptWithBarrier` → `fanoutInterrupt`, and set it on each
  `command.Interrupt` (`:225`). Every other caller passes `nil`: `Session.Interrupt`,
  the loop-scoped interrupts and `StopAgent` (`grep -n "runInterrupt\(|runInterruptWithBarrier(" internal/sessionruntime/*.go`).
- `session.go`: keep `Interrupt(ctx)` and add
  `func (s *Session) interruptAs(ctx context.Context, p *coresessionwire.Principal) (bool, error)`.
  `Interrupt` calls `interruptAs(ctx, nil)`.
- `runtime_command.go` `KindInterrupt` arm: call `s.interruptAs(ctx, admitted.Principal)`.
- `gates.go`: replace the `cause uuid.UUID` parameter of `respondGateAsCaller`,
  `respondGateCore`, `translateGateResponse` (check it with `grep -n "func (s \*Session) translateGateResponse"`)
  and `buildGateResolved` with a single `gateCause` value:
  ```go
  // gateCause is what an admitted gate_response carries into the answer: the
  // runtime command id (Cause.CommandID) and who answered. The zero value is
  // RespondGate's: no cause, no principal.
  type gateCause struct {
  	commandID uuid.UUID
  	principal *coresessionwire.Principal
  }
  ```
  `buildGateResolved` sets `Principal: cause.principal`. Every non-admitted caller
  passes `gateCause{}`: `RespondGate`, the classifier and timeout paths.
- `runtime_command.go` `KindGateResponse` arm: pass
  `gateCause{commandID: admitted.RuntimeCommandID, principal: admitted.Principal}`.
- `loop.go`: add `interruptedBy *sessionwire.Principal` to `loopState`. In the
  `command.Interrupt` arm, in the `state.cancelTurn != nil` branch only (`:2692-2695`),
  set `state.interruptedBy = c.Principal` before `state.cancelTurn()`. In
  `handleTurnResult`, before `commitBoundary` (`:2332`):
  ```go
  		if ti, ok := result.terminal.(event.TurnInterrupted); ok && state.interruptedBy != nil {
  			p := *state.interruptedBy
  			ti.Principal = &p
  			result.terminal = ti
  		}
  		state.interruptedBy = nil
  ```
  Also clear `interruptedBy` wherever a turn is installed (`installActiveTurn`,
  `:1431`) so a stale value cannot attach to a later turn. The second terminal site at
  `:3462` (a shutdown-drained terminal) gets no principal. That is a shutdown, not an
  interrupt, so leave it and note it in a comment.

**Step 4: Run them and watch them pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/... ./internal/loopruntime/...
```

**Step 5: Commit.**

```bash
git add internal/sessionruntime internal/loopruntime
git commit -m "feat(sessionruntime): journal who interrupted a session and who answered a gate"
```

---

## Task 12: Public redaction strips metadata and keeps the principal

**Files:**
- Modify: `pkg/sessionwire/privacy.go`: `redactPublicBody` `:41-81`
- Test: `pkg/sessionwire/privacy_test.go`

**Step 1: Write the failing test.** Follow the existing `GateResolved` audit case in
`privacy_test.go` (`grep -n "audit" pkg/sessionwire/privacy_test.go`).

```go
func TestPublicBodyStripsMetadataKeepsPrincipal(t *testing.T) {
	t.Parallel()
	p := &coresessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: coresessionwire.PrincipalKindActor}
	for _, ev := range []event.Event{
		turnStartedWithInput(t, p),     // Input{Principal, Metadata{"space":"family"}, Prefix: 1}
		turnFoldedIntoWithInput(t, p),
		inputCancelledWithInput(t, p),
	} {
		native, err := event.MarshalEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		public, err := redactPublicBody(ev, native)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Input map[string]json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(public, &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body.Input["metadata"]; ok {
			t.Fatalf("%T: public body leaked metadata: %s", ev, public)
		}
		for _, keep := range []string{"principal", "prefix"} {
			if _, ok := body.Input[keep]; !ok {
				t.Fatalf("%T: public body dropped %s: %s", ev, keep, public)
			}
		}
		if !bytes.Contains(native, []byte(`"space"`)) {
			t.Fatal("the native body must keep metadata for audit")
		}
	}
	// An event without input is returned unchanged, byte for byte.
	plain := turnStartedWithInput(t, nil)
	plain.Input = nil
	native, _ := event.MarshalEvent(plain)
	public, _ := redactPublicBody(plain, native)
	if !bytes.Equal(native, public) {
		t.Fatalf("unattributed event was re-encoded:\n%s\n%s", native, public)
	}
}
```

Also add: `TurnInterrupted` and `GateResolved` keep `principal` in public (GateResolved
still drops `audit`). A metadata-only input (`Input{Metadata: …}`) becomes public
with **no** `input` member at all, because `{}` is not canonical and a public reader
must never see an empty object.

**Step 2: Run it and watch it fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/sessionwire/... -run 'TestPublicBody'
```

**Step 3: Implement.** Add these cases to `redactPublicBody`:
```go
	case event.TurnStarted, event.TurnFoldedInto, event.InputCancelled:
		// A viewer sees what the model saw (the assembled message) and who sent it
		// (input.principal, input.prefix/suffix). Custom metadata is audit-only unless
		// the presenter rendered it into the message, so the public record never
		// carries input.metadata. An event with no input is returned unchanged.
		if !hasMessageMetadata(value) {
			return encoded, nil
		}
		return editObject(encoded, func(fields map[string]json.RawMessage) error {
			if err := editMember(fields, "input", func(input map[string]json.RawMessage) error {
				delete(input, "metadata")
				return nil
			}); err != nil {
				return err
			}
			if string(fields["input"]) == "{}" {
				delete(fields, "input")
			}
			return nil
		})
```
Here `hasMessageMetadata` switches on the three types and checks `Input != nil &&
len(Input.Metadata) > 0`. Checking before editing keeps an unattributed or
principal-only event byte-identical, because `editObject` re-encodes. `value` is
already the typed event in the switch. `TurnInterrupted` needs no case. `GateResolved`
keeps its existing `audit` deletion; the principal simply survives.

Update the file's header comment (`:10-22`) to list the new redaction.

**Step 4: Run it and watch it pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/sessionwire/... ./pkg/sessionstore/...
```

`pkg/sessionstore` runs because it writes the public projection beside the native
body.

**Step 5: Commit.**

```bash
git add pkg/sessionwire
git commit -m "feat(sessionwire): strip message metadata from public bodies, keep the principal"
```

---

## Task 13: `session.SubmitInput` as a segregated capability

**Files:**
- Modify: `pkg/session/session.go`: add `Input` and `InputSubmitter` after `LeaseEpochReporter` (`:408-412`)
- Modify: `internal/sessionruntime/session.go`: `SubmitInput`, and `submitToLoop` gains an attribution source
- Test: `internal/sessionruntime/submit_input_test.go`, `pkg/session/contracts_test.go`

**Step 1: Write the failing tests.**

```go
func TestSubmitInputPresentsAndAttributes(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t) // a live session with an active loop
	p := &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.TextBlock{Text: "[from: Alex]"}}}}
	WithMessagePresenter(p)(f.session)
	var submitter sessionapi.InputSubmitter = f.session
	principal := &coresessionwire.Principal{Tenant: "acme", Subject: "u1", Kind: coresessionwire.PrincipalKindActor}
	id, err := submitter.SubmitInput(context.Background(), sessionapi.Input{
		Blocks: []content.Block{&content.TextBlock{Text: "hi"}}, Principal: principal,
		Metadata: coresessionwire.MessageMetadata{"space": "family"},
	})
	if err != nil || id.IsZero() {
		t.Fatalf("SubmitInput = %v, %v", id, err)
	}
	in := f.drainOne(t).(command.UserInput)
	if in.Presented == nil || in.Principal.Subject != "u1" || p.calls[0].Principal.Subject != "u1" {
		t.Fatalf("input = %#v", in)
	}
}

func TestSubmitInputRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   sessionapi.Input
		p    present.Presenter
		want func(error) bool
	}{
		{name: "invalid metadata", in: sessionapi.Input{Blocks: textOnly("x"), Metadata: coresessionwire.MessageMetadata{"looprig_a": "b"}}},
		{name: "invalid principal", in: sessionapi.Input{Blocks: textOnly("x"), Principal: &coresessionwire.Principal{}}},
		{name: "no blocks", in: sessionapi.Input{}},
		{name: "presenter failure", in: sessionapi.Input{Blocks: textOnly("x")}, p: &countingPresenter{err: errors.New("x")},
			want: func(err error) bool { var pe *present.Error; return errors.As(err, &pe) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			if tt.p != nil {
				WithMessagePresenter(tt.p)(f.session)
			}
			id, err := f.session.SubmitInput(context.Background(), tt.in)
			if err == nil || !id.IsZero() || (tt.want != nil && !tt.want(err)) {
				t.Fatalf("SubmitInput = %v, %v", id, err)
			}
			f.requireNoCommand(t, "a refused submit")
		})
	}
}

func TestPlainSubmitIsPresentedWithNilPrincipal(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	p := &countingPresenter{}
	WithMessagePresenter(p)(f.session)
	if _, err := f.session.Submit(context.Background(), textOnly("hi")); err != nil {
		t.Fatal(err)
	}
	f.drainOne(t)
	if p.count() != 1 || p.calls[0].Principal != nil {
		t.Fatalf("calls = %#v", p.calls)
	}
}
```

In `pkg/session/contracts_test.go`, assert that `session.Session` did **not** gain
`SubmitInput`. Reflect over the interface's method set, following the existing
contract tests.

**Step 2: Run them and watch them fail.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/session/... ./internal/sessionruntime/... -run 'SubmitInput|PlainSubmit|Contract'
```

**Step 3: Implement.**

`pkg/session/session.go`:
```go
// Input is one user message submitted in-process with optional attribution.
// Principal is the verified sender as the CALLER established it: in-process there
// is no Factory, so the composition root owns its truth. Metadata is the app's
// custom fields, under Core's limits. Both are audit-only unless the rig's Message
// Presenter renders them.
type Input struct {
	Blocks    []content.Block
	Principal *sessionwire.Principal
	Metadata  sessionwire.MessageMetadata
}

// InputSubmitter is the capability to submit attributed user input to the active
// loop. It is segregated, like LeaseEpochReporter: Submit keeps its signature,
// and no Session implementer grows a method. Discover it by assertion:
//
//	submitter, ok := s.(session.InputSubmitter)
//
// SubmitInput validates Principal and Metadata with Core's rules and refuses empty
// Blocks. It presents the message through the rig's presenter; a presenter failure
// is a *present.Error, and nothing is sent. Otherwise it behaves exactly like
// Submit: the returned id is the Cause.CommandID of the outcome events.
type InputSubmitter interface {
	SubmitInput(context.Context, Input) (uuid.UUID, error)
}
```

`pkg/session` importing `core/sessionwire/v1` is a new import. Check `pkg/session`'s
deps test, if there is one (`grep -rn "deps" pkg/session/*_test.go`).

`internal/sessionruntime/session.go`: generalise `submitToLoop` to take an
`attributionSource` (principal, metadata) and present inside it for `AgencyUser`, as
in Task 7. `Submit` and `SubmitToLoop` pass the zero source. Then:

```go
// SubmitInput is the attributed form of Submit. See session.InputSubmitter.
func (s *Session) SubmitInput(ctx context.Context, in sessionapi.Input) (uuid.UUID, error) {
	if len(in.Blocks) == 0 {
		return uuid.UUID{}, &SessionError{Kind: SessionInvalidInput}
	}
	if in.Principal != nil {
		if err := in.Principal.Validate(); err != nil {
			return uuid.UUID{}, &SessionError{Kind: SessionInvalidInput, Cause: err}
		}
	}
	if len(in.Metadata) > 0 {
		if err := in.Metadata.Validate(); err != nil {
			return uuid.UUID{}, &SessionError{Kind: SessionInvalidInput, Cause: err}
		}
	}
	s.loopsMu.RLock()
	active := s.activeLoopID
	s.loopsMu.RUnlock()
	return s.submitToLoopAttributed(ctx, active, in.Blocks, identity.AgencyUser, false, in.Principal, in.Metadata)
}

var _ sessionapi.InputSubmitter = (*Session)(nil)
```

If `SessionInvalidInput` does not exist (`grep -n "Session[A-Z][A-Za-z]* *SessionErrorKind" internal/sessionruntime/errors.go`),
add it with a doc line and map it in `pkg/session/errors.go` the same way the other
kinds are mapped.

Check whether the rig's controller wrapper forwards methods
(`grep -rn "LeaseEpoch()" pkg/rig/*.go`). If rig wraps the runtime session, forward
`SubmitInput` there as well, and add a rig-level test that
`controller.(session.InputSubmitter)` succeeds.

**Step 4: Run them and watch them pass.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./pkg/session/... ./pkg/rig/... ./internal/sessionruntime/...
```

**Step 5: Commit.**

```bash
git add pkg/session pkg/rig internal/sessionruntime
git commit -m "feat(session): add the InputSubmitter capability for attributed in-process input"
```

---

## Task 14: A pre-feature journal restores identically (store-level golden)

Tasks 5 and 6 pin the codec against the frozen bytes. This task pins a **whole
restore**.

**Files:**
- Test: `internal/sessionruntime/prefeature_restore_test.go`

**Step 1: Write the test.**
1. Over memstore (`sessionstoreOverMemstore`), write a small journal: a `UserInput`
   intent, a `TurnStarted` and a `StepDone`. The event and command bodies come from
   `internal/compat/testdata/pre_v0410/*.json`, decoded with the current codecs and
   appended through the store's journal. Reuse whatever the restore tests already use
   to seed journals (`grep -n "func seedJournal\|func appendEvents" internal/sessionruntime/*_test.go`).
2. Restore it twice: once with no presenter, and once with a `countingPresenter`.
3. Assert:
   - the restored loop's messages (`foldLoop(...).Msgs`, encoded with the content
     codec) are byte-identical across both restores and equal the golden
     `TurnStarted.Message`;
   - the presenter call count is 0;
   - the stored bytes read back through `OpenInternalRecordReplayer` equal what was
     written.

```go
func TestPreFeatureJournalRestoresIdenticallyWithAndWithoutAPresenter(t *testing.T) {
	t.Parallel()
	// ... steps 1–3 above, using the fixture's own seeding helper ...
}
```

**Step 2: Run it.**

```bash
GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/... -run 'PreFeatureJournal'
```

Expected: PASS.

**Step 3: Commit.**

```bash
git add internal/sessionruntime/prefeature_restore_test.go
git commit -m "test(sessionruntime): a pre-feature journal restores identically"
```

---

## Task 15: Measure how v0.40.2 reads v0.41.0 records (the rollback probe)

This replaces the design's "old-decoder fail-closed test". Correction 1 explains why
v0.40.2 cannot fail closed. The test records what it does instead, so the release note
states a measured fact.

**Files:**
- Create: `internal/compat/testdata/v0410/*.json` (generated by the test below)
- Create: `internal/compat/v0410_golden_test.go`, which writes the v0.41.0 goldens and
  pins them

**Step 1: The v0.41.0 golden test.** It writes the attributed records with the current
codec when `-update` is set, and otherwise compares them.

```go
package compat_test

// TestV0410Goldens pins the attributed encodings, and they are the probe's input.
// Regenerate with: go test ./internal/compat -run TestV0410Goldens -update
```

Cover `TurnStarted`/`TurnFoldedInto`/`InputCancelled` with `input` (principal,
metadata, prefix 1, suffix 1), `TurnInterrupted` and `GateResolved` with `principal`,
a presented `UserInput`, and an `Interrupt` with `principal`. The package needs one
non-test file (`internal/compat/doc.go`, `package compat`, doc: "golden bytes for
cross-release codec checks").

**Step 2: Run the probe.**

```bash
GOTOOLCHAIN=go1.26.8 go test ./internal/compat -run TestV0410Goldens -update
GOTOOLCHAIN=go1.26.8 go test -race ./internal/compat
GOTOOLCHAIN=go1.26.8 make compat
```

Expected: every `pre_v0410` line is `IDENTICAL`, and every `v0410` line is `LOSSY`,
with the attribution member missing from the re-encoding. **If any v0410 line is
`REFUSED`,** the old reader fails closed for that record. Record that and word the
release note for it.

**Step 3: Record the result.** Paste the `make compat` output verbatim into
`internal/compat/testdata/v0410/PROBE_RESULT.txt`. The README upgrade note (Task 16)
cites it. Do not paraphrase the numbers.

**Step 4: Commit.**

```bash
git add internal/compat
git commit -m "test(compat): measure how v0.40.2 reads attributed v0.41.0 records"
```

---

## Task 16: Documentation

Harness has no `CHANGELOG`. Release text lives in the annotated tag (Task 18) and the
README upgrade notes (`README.md:296-446`). Do not create a CHANGELOG.

**Files:**
- Modify: `README.md`: add a new upgrade note after `:376-446`; update "How to use harness" (`:69`) with a presenter example
- Modify: `pkg/present/README.md` (Task 2), `pkg/session/README.md`, `pkg/command/README.md`, `pkg/event/README.md`
- Modify: `docs/plans/2026-09-25-message-principal-metadata-presenter-design.md` §8: set step 3 to "released v0.41.0" in Task 18, not before

**Step 1: README upgrade note** (verbatim structure):

```markdown
### Upgrade note: message principal, metadata and the Message Presenter (v0.41.0, one-way, LOSSY on rollback)

v0.41.0 carries an optional verified `principal` on every admitted runtime command
kind and optional `metadata` on `create` and `input`, and it adds
`rig.WithMessagePresenter`. A presenter may prepend and append text to a USER
message. It runs once, and the rendering is journaled on the intent record
(`presented`) and inside the committed `TurnStarted.Message`.
`TurnStarted`/`TurnFoldedInto`/`InputCancelled` record `input` (principal, metadata,
prefix/suffix counts). `TurnInterrupted` and `GateResolved` record `principal`. Event
`v` stays 1, and a record without the new members is byte-identical to v0.40.2.

**Do not roll a process back below v0.41.0 once a journal holds an attributed or
presented record.** harness v0.40.2 does NOT refuse such a journal. Its plain
decoders drop the new members silently (measured: `internal/compat/testdata/v0410/PROBE_RESULT.txt`):
committed turns restore unchanged, but the old process loses the attribution, and an
input that was applied but not yet started is re-offered WITHOUT its presenter frame,
so the model sees different text. The v0.41.0 decoders are strict on the new members,
so the next addition fails closed.

Consumer obligations:
- A presenter must handle `in.Principal == nil` and should be deterministic in its
  input: a command redelivered before its application prefix is durable is presented
  again.
- A presenter failure settles an attempt-bearing command `refused` (prefix, no intent
  record) and returns `*present.Error` from `SubmitInput`. The durable disposition
  carries no reason.
- The public projection strips `input.metadata` and keeps `input.principal`; metadata
  is audit-only unless the presenter renders it. Read it with `ReadRuntimeJournal`.
- `restore`'s principal is not journaled by harness; it lives in SessionStore's
  disposition descriptor.
- `session.InputSubmitter` is discovered by assertion; `session.Session` is unchanged.
- Hooks see the assembled message (`hook.TurnData.Input`), as the model does.
```

Add a matching note for the impl-04 content if impl-04's own plan left one
un-written. Check `README.md` for its note.

**Step 2: Check the docs.**

```bash
grep -n "v0.41.0" README.md | head
```

**Step 3: Commit.**

```bash
git add README.md pkg/*/README.md
git commit -m "docs: document principal, metadata and the Message Presenter (v0.41.0)"
```

---

## Task 17: Full verification (standalone, pinned)

This task needs core v0.12.0, sessionstore v0.14.0 and inference v0.14.0 **published**.
Until they are, run only Step 2 and stop.

**Step 1: Pin and tidy, with no workspace.**

```bash
cd /Users/ipotter/code/looprig/harness
GOWORK=off GOTOOLCHAIN=go1.26.8 go get github.com/looprig/core@v0.12.0 \
  github.com/looprig/sessionstore@v0.14.0 github.com/looprig/inference@v0.14.0
GOWORK=off GOTOOLCHAIN=go1.26.8 go mod tidy
git diff go.mod | grep -E '^\+|^-'
```

Expected: exactly the three version bumps plus any go.sum lines. There is no `replace`
(`grep -n replace go.mod` prints nothing).

**Step 2: The suite and native checks.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./... 2>&1 | tail -20
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race -tags integration ./... 2>&1 | tail -20
GOWORK=off GOTOOLCHAIN=go1.26.8 make check
GOWORK=off GOTOOLCHAIN=go1.26.8 make compat
```

Expected: every package `ok`, `make check` exit 0 (fmt-check, vet, staticcheck, gosec,
govulncheck, test, build), and `make compat` matching `PROBE_RESULT.txt`.

**Step 3: API diff.** Only additions are allowed, so this is a minor release.

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go run golang.org/x/exp/cmd/apidiff@latest -m \
  github.com/looprig/harness@v0.40.2 . 2>&1 | tee /tmp/harness-apidiff.txt
```

Expected: `Compatible changes` only. The listed additions are `pkg/present`,
`rig.WithMessagePresenter`, `rig.DefinitionInvalidMessagePresenter`, the new fields,
`session.Input`/`InputSubmitter`, `event.MessageInput`/`UserBlocks`,
`command.Presented`/`ModelBlocks`/`MessageInput`, and impl-04's additions. Any
`Incompatible change` stops the release. The `apidiff` tool is run with `go run` and
is not added to `go.mod`. If running it that way needs approval under the dependency
rule, ask first.

**Step 4: Remove the working go.work** (it was never committed):

```bash
rm -f go.work go.work.sum && git status --short
```

**Step 5: Commit the pin.**

```bash
git add go.mod go.sum
git commit -m "build: pin core v0.12.0, sessionstore v0.14.0 and inference v0.14.0"
```

---

## Task 18: Release harness v0.41.0 — REQUIRES OWNER CONFIRMATION before push/tag

v0.41.0 is a **minor** release (public API additions only), and it ships impl-04 and
this plan together. Do not push or tag until the owner confirms in the current
conversation. Also get the owner's acknowledgement of the "lossy on rollback" wording
(correction 1).

**Step 1: Pre-flight.**

```bash
cd /Users/ipotter/code/looprig/harness
git status --short                        # clean
git log --oneline v0.40.2..HEAD           # impl-04 + impl-05 commits only
git ls-remote --tags git@github.com:looprig/core.git v0.12.0
git ls-remote --tags git@github.com:looprig/sessionstore.git v0.14.0
git ls-remote --tags git@github.com:looprig/inference.git v0.14.0
```

**Step 2: STOP and ask the owner.** Show the log, the apidiff summary, the probe
result and the draft annotation below. Wait for an explicit yes.

**Step 3: Push, tag and verify** (only after the owner says yes).

```bash
git push origin main
git tag -a v0.41.0 -F - <<'EOF'
harness v0.41.0: message principal, metadata and the Message Presenter; unbounded execution and opaque tool input

- Principal on every admitted runtime command kind; metadata on create/input
  (runtimecommand.Admitted). Journaled on the intent record, the interrupt record,
  TurnStarted/TurnFoldedInto/InputCancelled (input), TurnInterrupted and
  GateResolved (principal). restore's principal lives in SessionStore only.
- rig.WithMessagePresenter / pkg/present: prepend/append text once, journaled; the
  loop sends Prefix ++ Blocks ++ Suffix. Machine input is never presented. A
  presenter failure settles the command refused (prefix, no intent record); the
  disposition carries no reason.
- Public bodies strip input.metadata, keep input.principal.
- session.InputSubmitter (SubmitInput) for attributed in-process input.
- loop.Unlimited (-1) for ToolLimits.Iterations/Calls and DelegationLimits.Quota
  (depth stays bounded; below -1 is refused); an explicit hustle.WithTimeout(0) means
  no execution deadline (an omitted WithTimeout is still refused); duplicate keys
  inside a tool_use block's input no longer make a journal unreplayable (the input is
  opaque; every other key stays strictly checked). [impl-04; confirm against its
  final commits before tagging.] The no-execution-ceiling inference call is
  inference v0.14.0's transport.WithoutExecutionTimeout, not a harness API; harness
  only pins it.
- Pins core v0.12.0, sessionstore v0.14.0, inference v0.14.0 (storage v0.7.0 and
  fsstore v0.6.0 test-only unchanged). apidiff vs v0.40.2: additions only.

ONE-WAY, LOSSY ON ROLLBACK: once a journal holds an attributed or presented record,
never run it under harness < v0.41.0. v0.40.2 does not refuse it — it drops the new
members silently, and re-offers an owed presented input without its frame (measured:
internal/compat/testdata/v0410/PROBE_RESULT.txt). Event v stays 1; absent members are
byte-identical to v0.40.2.
EOF
git push origin v0.41.0
git ls-remote origin refs/heads/main refs/tags/v0.41.0
git rev-parse HEAD v0.41.0^{}
```

Expected: the remote `main` equals the local `HEAD`, and the remote tag's peeled
commit equals `HEAD`.

**Step 4: Record it (outer workspace files are not committed by the agent).**
- Update `docs/plans/2026-09-25-message-principal-metadata-presenter-design.md` §8,
  step 3, to `released v0.41.0 (<sha>, tag object <obj>)`, and the master plan's
  progress rows for 04 and 05. Commit them in harness:
  `git commit -m "docs(plan): record harness v0.41.0"`, then push after the same
  confirmation.
- Tell the coordinator to update `/Users/ipotter/code/looprig/repositories.mk`,
  `go.work` and `AGENTS.md`. The tier-3 row gets harness v0.41.0 pins: core v0.12.0,
  inference v0.14.0, sessionstore v0.14.0. The one-way note goes too. Under MVS,
  harness v0.41.0 lifts every harness dependent to core v0.12.0 / sessionstore
  v0.14.0 on its next bump. Plan 06 (host v0.11.0) may now pin it.

---

## Self-review checklist (run before handing back)

- [ ] Every new member is `omitzero`/`omitempty`; the frozen v0.40.2 goldens re-encode
      byte-identically (Tasks 5, 6, 14).
- [ ] The presenter is called exactly once per user message: not on redelivery after
      the prefix, not on restore re-offer, not for machine input (Tasks 7, 9, 10).
- [ ] `Message.Blocks[Prefix:len-Suffix]` equals the admitted blocks byte for byte (Tasks 8, 9).
- [ ] A presenter failure → `refused` + prefix + no intent (attempt), and nothing
      durable (legacy) (Task 10).
- [ ] The interrupt principal reaches the intent records and `TurnInterrupted`; the
      gate principal reaches `GateResolved` (Task 11).
- [ ] Public bodies never contain `input.metadata`; unattributed events are
      byte-identical (Task 12).
- [ ] `session.Session` is unchanged; `InputSubmitter` is discovered by assertion (Task 13).
- [ ] The rollback behaviour was measured, not assumed, and the release note quotes
      the measurement (Tasks 15, 16, 18).
- [ ] No `replace`, no committed `go.work`, no Co-Authored-By trailer, no push or tag
      without owner confirmation.
