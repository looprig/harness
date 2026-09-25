# tests v0.14.0 lane, Carbon re-pin, Oxy switch-over, workspace docs

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

> **Commits:** conventional messages with **no Co-Authored-By trailer** (master plan rule; it overrides any tool default). Never commit a `go.work` and never add a `replace` directive.

> **Unverified code (cross-plan review, 2026-09-25).** None of this plan's code was
> **executed** while planning, and parts are outlines, not full code: Task A2's
> `admittedFor` switch (`// ... the existing arms unchanged ...`); Task A3's
> interrupt/gate-response and different-subject-retry subtests (bodies are comments); Task
> A4 (prose only, no code); Task A5's `startJetStream`/`startOldHost` helpers and the kit
> accessors it names (`AwaitAdvertised`, `HostAdvertises`, `Registration`,
> `JournalEvents`, `SawInRequest`, `CommandState`'s absent sentinel). Write each in full and
> watch it fail before implementing. The old-Host probe deliberately runs a sessionstore
> v0.13.1 reader beside v3 rows, which the one-way rule forbids in production; that is the
> point of the probe, not a deployment pattern.

**Goal:** Prove, across real released modules, that the principal/metadata/presenter
feature and the unbounded-execution changes behave as the designs promise. Then move
the two consumers onto them:

- **Carbon**: re-pin and copy the new members through its runtime adapter.
- **Oxy**: delete its forks, pin plain releases, move its hustles to structured
  output, and adopt stamping plus a presenter.

Finish with the workspace bookkeeping.

**Architecture:** Four independent parts, in this order.

| Part | What | Where |
|---|---|---|
| A | tests v0.14.0: the cross-module lane (design §6 step 6) and the unbounded-execution smoke | `looprig/tests` |
| B | Carbon v0.30.0: pins, the adapter copy, the pin ledger | `looprig/carbon` |
| C | Oxy Phase 8: plain releases, structured output, stamping plus presenter | `/Users/ipotter/code/oxy` |
| D | AGENTS.md, repositories.mk, go.work, `.github` docs corpus | outer workspace and `.github` |

Part A's lane and Part B are the release gates for Carbon. Part C depends on B only
through the shared lessons (the adapter-copy rule). Every release step **REQUIRES
OWNER CONFIRMATION**.

**Tech stack:** Go 1.26.8 (`GOTOOLCHAIN=go1.26.8 GOWORK=off`). The lane uses the
existing `internal/orchestrationtest` kit (real Factory, real Host, real harness rig,
scripted model). The old-Host probe uses a nested build-only module, a released
host v0.10.3 binary, and a NATS JetStream server as the shared durable plane.

**Binding designs:** `2026-09-25-message-principal-metadata-presenter-design.md` (§2
one-way rules, §3 rollout, §4 visibility, §5 presenter semantics, §6 steps 6-7, the
Oxy migration list) and `2026-09-25-unbounded-execution-and-opaque-tool-input-design.md`.
Master plan: `2026-09-25-impl-00-master-plan.md` (row 09).

**Naming (coordinator ruling, supersedes the design text):** the Host capability token
is **`hostlink.attribution.principal`**, Core constant
**`sessionwire.HostLinkCapabilityAttributionPrincipal`**. The design's
`hostlink.command.principal` / `HostLinkCapabilityCommandPrincipal` is superseded.
Use the new name everywhere below. Use the Host-side alias impl-06 defines (expected
`CapabilityAttributionPrincipal`).

---

## Preconditions (all parts)

Every one of these tags must exist on its remote before the part that names it:

```bash
for r in core:v0.12.0 sessionstore:v0.14.0 inference:v0.14.0 harness:v0.41.0 host:v0.11.0 factory:v0.12.0 wui:v0.4.0 host:v0.10.3; do
  git ls-remote --tags "git@github.com:looprig/${r%%:*}.git" "${r#*:}" | grep -q . && echo "ok $r" || echo "MISSING $r"
done
```

Expected: eight `ok` lines. Part A needs all but wui. Part B needs all eight. A
`MISSING` line means the dependency release is still owed; stop (AGENTS.md
leaf-to-root rule).

Record, from the published modules, the exact names this plan assumes. Correct
every later code block to match before you write it:

| Assumed name | Where to check |
|---|---|
| `present.Presenter`, `present.Input`, `present.Frame` | harness v0.41.0 `pkg/present` |
| `rig.WithMessagePresenter` | harness `pkg/rig` |
| `runtimecommand.Admitted.Principal`, `.Metadata` | harness `pkg/runtimecommand` |
| `event.TurnStarted.Input` (`*event.MessageInput`), `event.TurnInterrupted.Principal`, `event.GateResolved.Principal` | harness `pkg/event` |
| `loop.Unlimited` | harness `pkg/loop` |
| `department.RuntimeCommand.Principal`, `.Metadata` | host v0.11.0 `department` |
| `factory.WithPrincipalStamping`, `factory.AuditAuthorizer`, `identity.ErrClientPrincipal`, `identity.ErrMetadataUnsupported` (both in the public `factory/identity` package; `internal/admission` is not importable) | factory v0.12.0 |
| the audit route `GET /v1/sessions/{sid}/commands/{cid}` and its JSON members `principal`, `metadata` | factory v0.12.0 |
| `sessionstore.DispositionCommandRecord` attempt field | sessionstore v0.14.0 |
| `transport.WithoutExecutionTimeout` | inference v0.14.0 |

```bash
cd /Users/ipotter/code/looprig/tests
for m in harness@v0.41.0 host@v0.11.0 factory@v0.12.0 sessionstore@v0.14.0 core@v0.12.0; do
  GOWORK=off go doc -all github.com/looprig/${m%%@*}/... 2>/dev/null | head -0
done
GOWORK=off go doc github.com/looprig/harness/pkg/present
```

(Run `go doc` after Task A1 has pinned the versions.)

---

# Part A: tests v0.14.0

Work on `tests` local `main` (last tag v0.13.2, `main` at `24935a2`). Preserve
`.worktrees/`. The suite needs the sibling checkouts (Makefile header). Run it from
the full workspace, with `GOWORK=off`.

## Task A1: Re-pin to the new releases

**Files:** `go.mod`, `go.sum`, plus whatever fails to compile.

**Step 1:**

```bash
cd /Users/ipotter/code/looprig/tests
GOWORK=off GOTOOLCHAIN=go1.26.8 go get \
  github.com/looprig/core@v0.12.0 github.com/looprig/sessionstore@v0.14.0 \
  github.com/looprig/inference@v0.14.0 github.com/looprig/harness@v0.41.0 \
  github.com/looprig/host@v0.11.0 github.com/looprig/factory@v0.12.0
GOWORK=off go mod tidy
GOWORK=off go vet -tags integration ./...
```

Expected: `vet` passes, or fails only where a kit type implements an interface that
gained a method. Fix each by adding the method, following the pattern already in the
file. Do not bump controller, tools, llm or the others unless MVS forces them.
`go mod tidy` is correct here: this module's `mod-check` requires a tidy `go.mod`.

**Step 2:** Run the whole existing suite before adding anything:

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 make test
```

Expected: PASS. There is one intended exception:
`TestFactoryHostWireGoldens` / `TestSessionwireGolden` fail on
`testdata/sessionwire/hostlink_connect_negotiation.json`, because every
v0.11.0 Host now advertises `hostlink.attribution.principal`.

**Step 3:** Regenerate the goldens (the Makefile's intended-wire-change path):

```bash
make sessionwire-goldens || true
git diff -- testdata/sessionwire
```

Expected: the only diff is one added `"hostlink.attribution.principal"` entry in
the `hostlink_methods` array (or arrays) of the connect-negotiation fixtures. **Any
other diff is a wire change nobody booked: stop and report it.** Then run
`make test` again. Expected: PASS.

**Step 4: Commit.**

```bash
git add go.mod go.sum testdata/sessionwire internal/ *.go
git commit -m "chore(deps): pin core v0.12.0, sessionstore v0.14.0, harness v0.41.0, host v0.11.0, factory v0.12.0"
```

## Task A2: Kit support: copy the members, presenter, stamping, limits, hustles

**Files:**
- Modify: `internal/orchestrationtest/pooled.go` (`pooledSession.ApplyCommand`,
  `PooledWorldOptions`, `defineRig`)
- Modify: `internal/orchestrationtest/admission.go` (`PooledFactoryConfig`), and
  `pooledFactoryOptions` in `pooled.go`
- Create: `internal/orchestrationtest/presenter.go`
- Test: `internal/orchestrationtest/presenter_test.go` (package-level, integration tag)

**Why the adapter copy is not optional.** `pooledSession.ApplyCommand`
(`pooled.go:1236`) builds `runtimecommand.Admitted` **field by field**. On the bump
alone it would silently drop `cmd.Principal` and `cmd.Metadata`: the presenter would
see `nil` and nothing would be stamped into the journal. This is the same
explicit-copy hazard AGENTS.md records for `hustleruntime.ownInferenceRequest`, and
Carbon (Part B) and Oxy (Part C) carry the identical code.

**Step 1: Write the failing kit test** `internal/orchestrationtest/presenter_test.go`:

```go
//go:build integration

package orchestrationtest

import (
	"context"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/present"
)

func TestCountingPresenterFramesOnlyStampedInputAndCountsEachPresentation(t *testing.T) {
	p := NewCountingPresenter()
	alex := &sessionwire.Principal{Tenant: PooledTenantA, Subject: "user_alex", Kind: sessionwire.PrincipalKindActor}
	blocks := []content.Block{&content.TextBlock{Text: "add milk"}}

	frame, err := p.Present(context.Background(), present.Input{Principal: alex, Metadata: sessionwire.MessageMetadata{"space": "family"}, Blocks: blocks})
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if len(frame.Prefix) != 1 || len(frame.Suffix) != 0 {
		t.Fatalf("frame = %+v, want one prefix block", frame)
	}
	if got := frame.Prefix[0].(*content.TextBlock).Text; got != "[from: user_alex · space: family]" {
		t.Fatalf("prefix = %q", got)
	}
	empty, err := p.Present(context.Background(), present.Input{Blocks: blocks})
	if err != nil || len(empty.Prefix)+len(empty.Suffix) != 0 {
		t.Fatalf("an unstamped input must get an empty frame, got %+v, %v", empty, err)
	}
	if got := p.Count("add milk"); got != 2 {
		t.Fatalf("Count(add milk) = %d, want 2", got)
	}
}

func TestPooledSessionCopiesPrincipalAndMetadataIntoTheAdmittedCommand(t *testing.T) {
	cmd := departmentCommandFixture(t) // input kind, with Principal and Metadata set
	admitted, err := admittedFor(cmd, 7)
	if err != nil {
		t.Fatalf("admittedFor: %v", err)
	}
	if admitted.Principal == nil || *admitted.Principal != *cmd.Principal {
		t.Fatalf("principal dropped: %+v", admitted.Principal)
	}
	if admitted.Metadata["space"] != "family" {
		t.Fatalf("metadata dropped: %+v", admitted.Metadata)
	}
}
```

`admittedFor` is the pure part of `ApplyCommand`, extracted in Step 3 so it can be
tested without a lease. `departmentCommandFixture` builds a
`department.RuntimeCommand{Kind: PooledKindInput, CommandID: "cmd-1", RuntimeCommandID: uuid.New(), AttemptID: "a-1", Payload: <InputRequest JSON with one text block>, Principal: &alex, Metadata: {"space":"family"}}`.
Put it in `presenter_test.go`.

**Step 2: Run:**

```bash
GOWORK=off go test -tags integration -run 'TestCountingPresenter|TestPooledSessionCopies' ./internal/orchestrationtest/
```

Expected: FAIL (undefined: `NewCountingPresenter`, `admittedFor`).

**Step 3: Implement.** `internal/orchestrationtest/presenter.go`:

```go
//go:build integration

package orchestrationtest

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/present"
)

// CountingPresenter is the lane's Message Presenter. For a stamped input it
// prepends one text block naming the sender's subject and the "space" metadata
// field. For an unstamped one it returns an empty frame, which every presenter
// must do (the principal is optional in harness). It counts every call by the
// input's text, so a case can prove the presenter ran EXACTLY ONCE per message
// across restore, failover and redelivery (design §5).
//
// It is deterministic, a function of its Input only, as the design requires:
// a nondeterministic presenter would break the delivery-fingerprint dedup.
type CountingPresenter struct {
	mu    sync.Mutex
	calls map[string]int
}

func NewCountingPresenter() *CountingPresenter { return &CountingPresenter{calls: map[string]int{}} }

func (p *CountingPresenter) Present(_ context.Context, in present.Input) (present.Frame, error) {
	p.mu.Lock()
	p.calls[presentedText(in.Blocks)]++
	p.mu.Unlock()
	if in.Principal == nil {
		return present.Frame{}, nil
	}
	return present.Frame{Prefix: []content.Block{&content.TextBlock{
		Text: fmt.Sprintf("[from: %s · space: %s]", in.Principal.Subject, in.Metadata["space"]),
	}}}, nil
}

// Count is how many times the presenter ran for an input whose text is text.
func (p *CountingPresenter) Count(text string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[text]
}

func presentedText(blocks []content.Block) string {
	var parts []string
	for _, block := range blocks {
		if text, ok := block.(*content.TextBlock); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}
```

In `pooled.go`, split `pooledSession.ApplyCommand`. Its current body after the lease
check moves into:

```go
// admittedFor is the pure half of ApplyCommand: the harness record for cmd under
// epoch. It COPIES Principal and Metadata. Admitted is built field by field, so
// an omitted member is dropped in silence, not refused.
func admittedFor(cmd department.RuntimeCommand, epoch uint64) (runtimecommand.Admitted, error) {
	admitted := runtimecommand.Admitted{
		CommandID:        runtimecommand.CommandID(cmd.CommandID),
		RuntimeCommandID: cmd.RuntimeCommandID,
		LeaseEpoch:       epoch,
		AttemptID:        runtimecommand.AttemptID(cmd.AttemptID),
		Principal:        cmd.Principal,
	}
	switch cmd.Kind {
	// ... the existing arms unchanged, except:
	case PooledKindCreate, PooledKindInput:
		// (each existing arm) plus:
		admitted.Metadata = cmd.Metadata
	}
	return admitted, nil
}
```

Keep the `refusingCreates()` check in `ApplyCommand` before calling `admittedFor`,
because it needs the rig. Set `Metadata` only in the create and input arms, since
harness `Admitted.Validate` refuses metadata on other kinds. `Principal` goes on
every kind.

Add to `PooledWorldOptions`:

```go
	// Presenter, when set, is registered on every rig with rig.WithMessagePresenter.
	Presenter present.Presenter
	// ToolLimits, when non-zero, replaces the loop's tool limits (ToolResults' own
	// limits win when both are set). The unbounded smoke uses loop.Unlimited here.
	ToolLimits loop.ToolLimits
	// Hustles are registered on every rig with rig.WithHustles.
	Hustles []hustle.Definition
```

Store them on `PooledWorld`. In `defineRig`:
- if `w.presenter != nil`, append `rig.WithMessagePresenter(w.presenter)`;
- if `len(w.hustles) > 0`, append `rig.WithHustles(w.hustles...)`;
- if `w.toolLimits != (loop.ToolLimits{})` and `w.toolResults == nil`, append
  `loop.WithToolLimits(w.toolLimits)`.

Add to `PooledFactoryConfig`:

```go
	// PrincipalStamping composes factory.WithPrincipalStamping(): every admitted
	// command carries the verified principal (subject "user-<tenant>", kind actor,
	// from the kit's pooledVerifier).
	PrincipalStamping bool
	// AuditAuthorizer, when set, is the Authorizer Factory discovers the optional
	// factory.AuditAuthorizer seam on. Nil keeps pooledAuthorizer, which does NOT
	// implement it, so the audit route omits principal and metadata.
	AuditAuthorizer factory.Authorizer
```

In `pooledFactoryOptions`, append `factory.WithPrincipalStamping()` when
`PrincipalStamping` is set. Use `cfg.AuditAuthorizer` in place of the default when
it is non-nil. Add the kit type:

```go
// AuditingAuthorizer permits everything and implements factory.AuditAuthorizer
// as tenant equality -- the TenantAuthorizer rule -- and records each audit call.
type AuditingAuthorizer struct {
	pooledAuthorizer
	mu    sync.Mutex
	Audit []sessionwire.SessionID
}

func (a *AuditingAuthorizer) AuthorizeAuditRead(_ context.Context, p identity.Principal, s sessionwire.SessionID) error {
	a.mu.Lock()
	a.Audit = append(a.Audit, s)
	a.mu.Unlock()
	return nil
}
```

**Step 4:** Run the Step 2 command again (PASS), then `make test` (PASS: no
existing case composes a presenter or stamping, so nothing else moves).

**Step 5: Commit.**

```bash
git add internal/orchestrationtest
git commit -m "test(kit): copy principal and metadata into admitted commands; presenter, stamping and limit seams"
```

## Task A3: The principal lane (stamped kinds, visibility, audit, refusals)

**Files:** Create `principal_presenter_integration_test.go` (`//go:build integration`, package `tests`).

**Step 1: Write the lane.** One world, one stamping Factory, one Host, and
subtests that share the world:

```go
//go:build integration

// This file is design §6 step 6 of 2026-09-25-message-principal-metadata-presenter-design.md:
// a real Factory with WithPrincipalStamping, a real Host (v0.11.0) and a real
// harness rig with a Message Presenter. Principal is stamped on every command
// kind, metadata rides create/input, the presenter's frame is journaled once,
// the public journal keeps the principal and strips metadata, and the audit
// route discloses both only through AuditAuthorizer.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/sessionstore"
	"github.com/looprig/tests/internal/orchestrationtest"
)

func textBlocks(t *testing.T, texts ...string) json.RawMessage {
	t.Helper()
	var blocks []map[string]string
	for _, text := range texts {
		blocks = append(blocks, map[string]string{"type": "text", "text": text})
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("encoding blocks: %v", err)
	}
	return encoded
}

func blocksText(blocks []content.Block) []string {
	var out []string
	for _, block := range blocks {
		if text, ok := block.(*content.TextBlock); ok {
			out = append(out, text.Text)
		}
	}
	return out
}

func TestPrincipalMetadataAndPresenterAcrossFactoryHostHarness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	tenant := orchestrationtest.PooledTenantA
	presenter := orchestrationtest.NewCountingPresenter()
	world := orchestrationtest.NewPooledWorld(t, ctx, orchestrationtest.PooledWorldOptions{
		Tenants:     []sessionwire.TenantID{tenant},
		WithAskTool: true,
		Presenter:   presenter,
	})
	world.LLM.Respond(func(request inference.Request) orchestrationtest.PooledTurn {
		return orchestrationtest.PooledTurn{Text: "ok"}
	})
	orchestrationtest.StartPooledHost(t, ctx, world, "i-principal-host", 1)
	auditor := &orchestrationtest.AuditingAuthorizer{}
	served := orchestrationtest.StartPooledFactoryWith(t, ctx, world, orchestrationtest.PooledFactoryConfig{
		Replica: "i-principal", PrincipalStamping: true, AuditAuthorizer: auditor,
	})
	// the verified principal pooledVerifier mints for tenant A
	want := sessionwire.Principal{Tenant: tenant, Subject: sessionwire.SubjectID("user-" + string(tenant)), Kind: sessionwire.PrincipalKindActor}
	const s = sessionwire.SessionID("i-principal-one")

	t.Run("a stamped create with metadata is presented once and journaled assembled", func(t *testing.T) {
		status, body := served.Post(t, ctx, tenant, "/v1/sessions", sessionwire.CreateRequest{
			CommandEnvelope: orchestrationtest.PooledEnvelope("p-create"),
			SessionID:       s,
			AgentID:         orchestrationtest.PooledAgent,
			Blocks:          textBlocks(t, "add milk"),
			Metadata:        sessionwire.MessageMetadata{"space": "family", "client": "lane"},
		})
		if status != http.StatusCreated {
			t.Fatalf("create answered %d: %s", status, body)
		}
		orchestrationtest.PooledWait(t, "p-create applied", 120*time.Second, func() bool {
			return world.CommandState(ctx, tenant, s, "p-create") == sessionstore.InboxStateApplied
		})
		runtime := world.RuntimeSessionID(t, ctx, tenant, s)
		started := orchestrationtest.JournalEvents[event.TurnStarted](t, world, tenant, runtime)
		if len(started) != 1 {
			t.Fatalf("want one TurnStarted, got %d", len(started))
		}
		got := started[0]
		if got.Input == nil || got.Input.Principal == nil || *got.Input.Principal != want {
			t.Fatalf("TurnStarted.Input = %+v, want principal %+v", got.Input, want)
		}
		if got.Input.Metadata["space"] != "family" || got.Input.Prefix != 1 || got.Input.Suffix != 0 {
			t.Fatalf("TurnStarted.Input = %+v", got.Input)
		}
		if texts := blocksText(got.Message.Blocks); strings.Join(texts, "|") != "[from: "+string(want.Subject)+" · space: family]|add milk" {
			t.Fatalf("assembled message = %q", texts)
		}
		// The user's own blocks are recoverable exactly: Blocks[Prefix : len-Suffix].
		own := got.Message.Blocks[got.Input.Prefix : len(got.Message.Blocks)-got.Input.Suffix]
		if strings.Join(blocksText(own), "|") != "add milk" {
			t.Fatalf("user blocks = %q", blocksText(own))
		}
		if presenter.Count("add milk") != 1 {
			t.Fatalf("presenter ran %d times, want 1", presenter.Count("add milk"))
		}
		// The model saw the frame.
		if !world.LLM.SawInRequest(0, "[from: "+string(want.Subject)) {
			t.Fatal("the model request did not carry the presenter frame")
		}
	})

	t.Run("a stamped input with metadata", func(t *testing.T) {
		status, body := served.Post(t, ctx, tenant, "/v1/sessions/"+string(s)+"/input", sessionwire.InputRequest{
			CommandEnvelope: orchestrationtest.PooledEnvelope("p-input"),
			SessionID:       s,
			Blocks:          textBlocks(t, "and eggs"),
			Metadata:        sessionwire.MessageMetadata{"space": "family"},
		})
		if status != http.StatusOK {
			t.Fatalf("input answered %d: %s", status, body)
		}
		orchestrationtest.PooledWait(t, "p-input applied", 120*time.Second, func() bool {
			return world.CommandState(ctx, tenant, s, "p-input") == sessionstore.InboxStateApplied
		})
		if presenter.Count("and eggs") != 1 {
			t.Fatalf("presenter ran %d times for the input", presenter.Count("and eggs"))
		}
	})

	t.Run("the public journal keeps the principal and frame counts and strips metadata", func(t *testing.T) {
		status, page := served.Get(t, ctx, tenant, "/v1/sessions/"+string(s)+"/journal")
		if status != http.StatusOK {
			t.Fatalf("journal answered %d: %s", status, page)
		}
		if !bytes.Contains(page, []byte(`"subject":"`+string(want.Subject)+`"`)) {
			t.Fatalf("public journal lacks the principal: %s", page)
		}
		if !bytes.Contains(page, []byte(`"prefix":1`)) {
			t.Fatalf("public journal lacks the frame count: %s", page)
		}
		for _, secret := range []string{`"metadata"`, `"client":"lane"`} {
			if bytes.Contains(page, []byte(secret)) {
				t.Fatalf("public journal leaked %s: %s", secret, page)
			}
		}
	})

	t.Run("the audit route discloses principal and metadata only through AuditAuthorizer", func(t *testing.T) {
		status, body := served.Get(t, ctx, tenant, "/v1/sessions/"+string(s)+"/commands/p-create")
		if status != http.StatusOK {
			t.Fatalf("audit route answered %d: %s", status, body)
		}
		var audit struct {
			CommandID string                      `json:"command_id"`
			Status    string                      `json:"status"`
			Principal *sessionwire.Principal      `json:"principal"`
			Metadata  sessionwire.MessageMetadata `json:"metadata"`
		}
		if err := json.Unmarshal(body, &audit); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
		if audit.Principal == nil || *audit.Principal != want || audit.Metadata["client"] != "lane" {
			t.Fatalf("audit = %+v", audit)
		}
		if len(auditor.Audit) == 0 {
			t.Fatal("AuthorizeAuditRead was never consulted")
		}
		// A replica whose Authorizer lacks the seam omits both members.
		plain := orchestrationtest.StartPooledFactoryWith(t, ctx, world, orchestrationtest.PooledFactoryConfig{Replica: "i-principal-plain", PrincipalStamping: true})
		_, bare := plain.Get(t, ctx, tenant, "/v1/sessions/"+string(s)+"/commands/p-create")
		if bytes.Contains(bare, []byte(`"principal"`)) || bytes.Contains(bare, []byte(`"metadata"`)) {
			t.Fatalf("without AuditAuthorizer the route disclosed audit members: %s", bare)
		}
	})

	t.Run("a client-supplied principal is refused before any durable write", func(t *testing.T) {
		forged := want
		forged.Subject = "someone-else"
		status, body := served.Post(t, ctx, tenant, "/v1/sessions/"+string(s)+"/input", sessionwire.InputRequest{
			CommandEnvelope: orchestrationtest.PooledEnvelope("p-forged"),
			SessionID:       s,
			Blocks:          textBlocks(t, "forged"),
			Principal:       &forged,
		})
		if status != http.StatusBadRequest || !strings.Contains(body, "principal") {
			t.Fatalf("forged principal answered %d: %s", status, body)
		}
		if world.CommandState(ctx, tenant, s, "p-forged") != "" {
			t.Fatal("a refused command left a durable record")
		}
	})

	t.Run("interrupt and gate response carry the principal into the journal", func(t *testing.T) {
		// Drive the ask tool, answer its gate, then interrupt a held turn.
		// Build the gate the way factory_gate_integration_test.go builds one:
		// script PooledAskToolName, wait for OpenGates, post a gate_response to
		// /v1/sessions/{s}/gates/{gid}. Then script a turn with Hold, post
		// /v1/sessions/{s}/interrupt with command "p-interrupt", and close Hold.
		runtime := world.RuntimeSessionID(t, ctx, tenant, s)
		// ... (steps above)
		resolved := orchestrationtest.JournalEvents[event.GateResolved](t, world, tenant, runtime)
		if len(resolved) == 0 || resolved[len(resolved)-1].Principal == nil || *resolved[len(resolved)-1].Principal != want {
			t.Fatalf("GateResolved.Principal = %+v", resolved)
		}
		interrupted := orchestrationtest.JournalEvents[event.TurnInterrupted](t, world, tenant, runtime)
		if len(interrupted) == 0 || interrupted[len(interrupted)-1].Principal == nil {
			t.Fatalf("TurnInterrupted carries no principal: %+v", interrupted)
		}
		if presenter.Count("") != 0 {
			t.Fatal("an interrupt or gate response was presented; neither carries a message")
		}
	})

	t.Run("a retry of one command_id by a different verified subject is command_rejected", func(t *testing.T) {
		// Replay p-input's exact body as tenant A's second bearer (add
		// PooledBearersAlt with subject "user-alt-<tenant>" to the kit's
		// pooledVerifier). Expect 409 and a body naming command_rejected
		// (Factory's public code for the store's command_mismatch; design §9.9,
		// impl-07 Task 3); the stored descriptor still names want. Add the same
		// row for a create (replay p-create's body): its reservation digest
		// differs, so it must also be 409 command_rejected, never re-attributed.
	})
}
```

The two subtests whose bodies are outlines must be written in full before
committing. Model them on the existing cases the comments name:
- `factory_gate_integration_test.go` (gate open, answer);
- `host_gate_failover_integration_test.go` (Hold, interrupt);
- the kit's `PooledBearers` (a second bearer per tenant).

`CommandState` returning `""` for an absent record is an assumption; check its
body in `pooled.go:2595` and use the absent sentinel it actually returns.
`inference` is `github.com/looprig/inference`.

**Step 2: Run.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -count=1 -tags integration -race -run '^TestPrincipalMetadataAndPresenterAcrossFactoryHostHarness$' .
```

Expected: PASS. If a subtest fails, the lane found a cross-module defect. File it
against the owning module (harness, host or factory) with the failing assertion.
Do **not** weaken the assertion.

Prove the lane is not vacuous: temporarily delete `Principal: cmd.Principal` from
`admittedFor` and re-run. Expected: the first subtest fails on
`TurnStarted.Input = <nil>`. Restore the line.

**Step 3: Commit.**

```bash
git add principal_presenter_integration_test.go internal/orchestrationtest
git commit -m "test(lane): principal and metadata across Factory, Host and harness with a presenter"
```

## Task A4: Restart-and-restore byte equality, presenter once across failover, redelivery dedup

**Files:** Add to `principal_presenter_integration_test.go` a second top-level test,
`TestPresenterRenderingSurvivesRestoreFailoverAndRedelivery`.

Three cases, each in a fresh world with `Presenter` set and a stamping Factory:

1. **Restore reproduces the rendering byte for byte.**
   - Create `r-1` with text `restore me` and metadata `space=family`, and wait for it to be applied.
   - Record `before := content.MarshalBlocks(started[0].Message.Blocks)` and the model request's user message bytes.
   - `host.Stop()` (warm release), start a second pooled Host, and post input `r-2`.
   - When the model request for `r-2` arrives, its first user message must marshal to `before` exactly (`bytes.Equal`).
   - The journal's first TurnStarted must also marshal to `before`.
   - `presenter.Count("restore me") == 1`. There is no second presentation on restore.
2. **Presenter count 1 across failover.**
   - Start a **Mortal** Host (`StartLifecycleHost(..., PooledHostConfig{Mortal: true})`).
   - Script the model reply for input `f-1` with `Hold`.
   - After `TurnStarted` for `f-1` commits, `Kill` the Host and start a successor.
   - Close `Hold`, and wait for `f-1` to settle and the turn to end on the successor.
   - `presenter.Count("f-1 text") == 1`.
   - Every `TurnStarted` naming `f-1` (the cause command id) has identical marshalled `Message` bytes.
3. **Double redelivery is deduplicated.**
   - `dropper := orchestrationtest.NewReplyDropper()` and
     `StartPooledHostWith(t, ctx, world, "i-redeliver", 1, dropper.Wrap)`.
   - Post input `d-1`, and wait until `dropper.Deliveries()` holds at least two entries for `d-1` with `ReplyDropped` on the first.
     Let the dropper drop twice if its API allows (see `factory_admission_recovery_integration_test.go`).
   - Then: `d-1` applied, exactly one `TurnStarted` caused by `d-1`, and `presenter.Count("d-1 text") == 1`.
   - `dropper.Faults()` is empty.

Run:

```bash
GOWORK=off go test -count=1 -tags integration -race -run '^TestPresenterRenderingSurvivesRestoreFailoverAndRedelivery$' .
```

Expected: PASS. Commit:
`test(lane): presenter rendering survives restore, failover and redelivery`.

## Task A5: Old-Host probe with released host v0.10.3

**Why a separate process.** One module graph can hold one host version, and tests
v0.14.0 pins host v0.11.0. The probe runs a **real released v0.10.3 Host** as a
subprocess, built from a nested build-only module. The nested module reuses the
**published tests v0.13.2 kit**, which composes exactly host v0.10.3 over
harness v0.40.2.
- The import is legal because Go's `internal` rule is by path. The importer
  `github.com/looprig/tests/oldhostlane/cmd/oldhost` is inside the tree rooted at
  `github.com/looprig/tests`. gopls importing `golang.org/x/tools/internal` is the
  standing precedent.
- The durable plane both processes share is a NATS JetStream server on loopback,
  through natsstore, which the kind lane already uses as the SessionStore backend.

**REQUIRES OWNER CONFIRMATION (dependency):** the test starts the JetStream server
in process via `github.com/nats-io/nats-server/v2/server`. That promotes an
existing `// indirect` requirement (v2.14.5, pulled in by natsstore) to a direct
test import. Ask before Step 3. The fallback, if refused: gate the probe behind a
`LOOPRIG_OLDHOST_NATS_URL` env var pointing at an operator-started `nats-server -js`,
and skip closed without it.

**Files:**
- Create: `oldhostlane/go.mod`, `oldhostlane/go.sum`, `oldhostlane/cmd/oldhost/main.go`
- Create: `old_host_probe_integration_test.go` (`//go:build integration`)
- Modify: `scripts/check-release-modfile.sh` or `release_modfile_guard_test.go` so
  that `oldhostlane/go.mod` is also checked for `replace` directives
- Modify: `Makefile`: `mod-check` also runs `(cd oldhostlane && GOWORK=off go mod verify)`

**Step 1: The nested module.**

```bash
cd /Users/ipotter/code/looprig/tests && mkdir -p oldhostlane/cmd/oldhost
cd oldhostlane && GOWORK=off go mod init github.com/looprig/tests/oldhostlane
GOWORK=off go mod edit -go=1.26.8
```

`oldhostlane/cmd/oldhost/main.go`:

```go
//go:build integration && kind

// Command oldhost is the old-Host probe's RELEASED v0.10.3 Host: the tests
// v0.13.2 kit's pooled Host (host v0.10.3 over harness v0.40.2), serving one
// tenant over a SessionStore on the NATS URL it is given, so a v0.12.0 Factory
// in another process sees it in the pool. It advertises no
// hostlink.attribution.principal, which is what the probe is about.
//
// Environment: OLDHOST_STORE_URL (nats://...), OLDHOST_ID. It prints one JSON
// line {"id","base"} on stdout once serving, and drains on SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/natsstore"
	"github.com/looprig/tests/internal/kindlane"
	"github.com/looprig/tests/internal/orchestrationtest"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	store, err := natsstore.Open(openCtx, natsstore.Options{URL: os.Getenv("OLDHOST_STORE_URL")})
	cancel()
	if err != nil {
		log.Error("open store", "error", err.Error())
		os.Exit(1)
	}
	defer func() { _ = store.Close(context.Background()) }()
	tb := kindlane.NewProcessTB("oldhost", log)
	defer tb.RunCleanups()
	world := orchestrationtest.NewPooledWorld(tb, ctx, orchestrationtest.PooledWorldOptions{
		Tenants: []sessionwire.TenantID{orchestrationtest.PooledTenantA},
		Backend: store.Composite,
	})
	h := orchestrationtest.StartPooledHost(tb, ctx, world, sessionwire.HostID(os.Getenv("OLDHOST_ID")), 1)
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"id": string(h.ID), "base": string(h.Base)}); err != nil {
		os.Exit(1)
	}
	<-ctx.Done()
}
```

Then:

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go get github.com/looprig/tests@v0.13.2 github.com/looprig/natsstore@v0.5.3
GOWORK=off go mod tidy
GOWORK=off go build -tags 'integration kind' -o /tmp/oldhost ./cmd/oldhost && GOWORK=off go list -m github.com/looprig/host github.com/looprig/harness
```

Expected: the build succeeds and prints `github.com/looprig/host v0.10.3` and
`github.com/looprig/harness v0.40.2`.
- If the build fails with `use of internal package ... not allowed`, the path rule
  does not hold for your Go version. Fall back to copying `kindlane.NewProcessTB` and
  the kit's pooled-Host composition into `oldhostlane/internal/` from tests v0.13.2
  source, with a header naming the source tag.
- If `kindlane.NewProcessTB` or `ProcessTB.RunCleanups` differ from the v0.13.2 names
  used above, correct `main.go` to match the tag.

The v0.14.0 kit and the v0.13.2 kit must agree on `PooledServiceToken`,
`PooledTenantA` and `PooledBearers`, or Factory and the old Host will not
authenticate each other. **Do not change those constants in Task A2.**

**Step 2: The probe test** `old_host_probe_integration_test.go`:

```go
//go:build integration

// The old-Host probe (design §3 and §6 step 6): a released host v0.10.3, which
// does not advertise hostlink.attribution.principal, in the pool of a stamping
// factory v0.12.0. A create is never placed on it (it waits, NoCapacity, until
// a capable Host exists); an input to a session already resident on it is
// refused runtime_unavailable before any durable write; the old Host never
// begins an attempt for either.
package tests

func TestOldHostNeverReceivesAStampedCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	natsURL := startJetStream(t) // server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()}); ReadyForConnections(10s); ClientURL()
	store, err := natsstore.Open(ctx, natsstore.Options{URL: natsURL})
	if err != nil {
		t.Fatalf("natsstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	tenant := orchestrationtest.PooledTenantA
	world := orchestrationtest.NewPooledWorld(t, ctx, orchestrationtest.PooledWorldOptions{
		Tenants: []sessionwire.TenantID{tenant}, Backend: store.Composite,
	})
	old := startOldHost(t, ctx, natsURL, "i-oldhost-v0103") // go build (Step 1 command, into t.TempDir()), exec with env, read the JSON line, SIGTERM + Wait on cleanup

	// 1. Its connect reply lacks the token: the probe is about an incapable Host.
	report := orchestrationtest.AwaitAdvertised(t, world, old.ID)
	_ = report // the registration exists; the capability is on the connect reply
	if orchestrationtest.HostAdvertises(t, ctx, old.Base, sessionwire.HostLinkCapabilityAttributionPrincipal) {
		t.Fatal("host v0.10.3 advertised the attribution token")
	}

	// 2. With stamping OFF a bare create becomes resident on the old Host (the only Host).
	plain := orchestrationtest.StartPooledFactoryWith(t, ctx, world, orchestrationtest.PooledFactoryConfig{Replica: "old-plain"})
	const resident = sessionwire.SessionID("old-resident")
	if status, body := plain.Post(t, ctx, tenant, "/v1/sessions", sessionwire.CreateRequest{
		CommandEnvelope: orchestrationtest.PooledEnvelope("old-create-1"), SessionID: resident, AgentID: orchestrationtest.PooledAgent,
	}); status != http.StatusCreated {
		t.Fatalf("plain create answered %d: %s", status, body)
	}
	orchestrationtest.PooledWait(t, "the bare create applied on the old Host", 120*time.Second, func() bool {
		return world.CommandState(ctx, tenant, resident, "old-create-1") == sessionstore.InboxStateApplied
	})
	if owner, ok := world.Registration(t, ctx, tenant, resident); !ok || owner.HostID != old.ID {
		t.Fatalf("resident owner = %+v, %v; want %s", owner, ok, old.ID)
	}
	plain.Stop()

	// 3. With stamping ON, an input to that session is refused before any write.
	stamping := orchestrationtest.StartPooledFactoryWith(t, ctx, world, orchestrationtest.PooledFactoryConfig{Replica: "old-stamping", PrincipalStamping: true})
	status, body := stamping.Post(t, ctx, tenant, "/v1/sessions/"+string(resident)+"/input", sessionwire.InputRequest{
		CommandEnvelope: orchestrationtest.PooledEnvelope("old-input-1"), SessionID: resident, Blocks: textBlocks(t, "hello"),
	})
	// 422, not 409: Factory's single status table maps runtime_unavailable to
	// 422 (design §9.9, impl-07 "Two corrections").
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "runtime_unavailable") {
		t.Fatalf("input to an incapable owner answered %d: %s", status, body)
	}
	if world.CommandState(ctx, tenant, resident, "old-input-1") != "" {
		t.Fatal("the refused input left a durable record")
	}

	// 4. A stamped create waits (NoCapacity): admitted, pending, never attempted.
	const waiting = sessionwire.SessionID("old-waiting")
	if status, body := stamping.Post(t, ctx, tenant, "/v1/sessions", sessionwire.CreateRequest{
		CommandEnvelope: orchestrationtest.PooledEnvelope("old-create-2"), SessionID: waiting, AgentID: orchestrationtest.PooledAgent,
		Blocks: textBlocks(t, "wait for a capable host"),
	}); status != http.StatusCreated {
		t.Fatalf("stamped create answered %d: %s", status, body)
	}
	deadline := time.Now().Add(5 * orchestrationtest.ReconcileSweepInterval)
	for time.Now().Before(deadline) {
		if state := world.CommandState(ctx, tenant, waiting, "old-create-2"); state != sessionstore.InboxStatePending {
			t.Fatalf("the stamped create left pending (%s) with only an incapable Host", state)
		}
		time.Sleep(orchestrationtest.ReconcileSweepInterval / 2)
	}
	assertNoAttempt(t, ctx, world, tenant, waiting, "old-create-2") // GetDispositionCommand: the attempt field is empty

	// 5. A capable Host arrives and takes it.
	capable := orchestrationtest.StartPooledHost(t, ctx, world, "i-newhost-v0110", 1)
	orchestrationtest.PooledWait(t, "the stamped create applied on the capable Host", 120*time.Second, func() bool {
		return world.CommandState(ctx, tenant, waiting, "old-create-2") == sessionstore.InboxStateApplied
	})
	if owner, _ := world.Registration(t, ctx, tenant, waiting); owner.HostID != capable.ID {
		t.Fatalf("the stamped create was placed on %s", owner.HostID)
	}
	if old.Attempts(t) != 0 { // the old Host's stderr carries no attempt log line for either command
		t.Fatal("the old Host began an attempt")
	}
}
```

Write the helpers in the same file:
- `startJetStream`;
- `startOldHost`, which returns `{ID, Base, cmd, stderr bytes.Buffer}` plus an
  `Attempts` method that counts the v0.10.3 Host's attempt log lines on stderr for
  the two command ids. Pass `HostLogs` through the kit's JSON logger. Check the
  v0.10.3 Host's log message for "attempt begun" and name it exactly.
- `assertNoAttempt`.

Add `HostAdvertises` to the kit. It dials the base with Core's connect codec, the
way `IncapableHost`'s tests do, and returns
`reply.Supports(sessionwire.HostLinkCapabilityAttributionPrincipal)`.

The authoritative "never begins an attempt" check is the store: the disposition
record for `old-create-2` has no attempt, and `old-input-1` has no record at all.
The stderr count is a second, independent observation.

**Step 3: Run.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -count=1 -tags integration -race -run '^TestOldHostNeverReceivesAStampedCommand$' .
```

Expected: PASS.

Prove it is not vacuous: set `PrincipalStamping: false` on the `stamping` replica.
Expected: step 3 then answers 200 and the input is applied on the old Host. That
proves the refusal came from the stamping gate. Revert.

**Step 4: Commit.**

```bash
git add oldhostlane old_host_probe_integration_test.go internal/orchestrationtest Makefile scripts release_modfile_guard_test.go go.mod go.sum
git commit -m "test(lane): released host v0.10.3 never receives a stamped command"
```

## Task A6: Unbounded-execution smoke

**Files:** Create `unbounded_execution_integration_test.go` (`//go:build integration`).

Three cases on a `WithWorkspace` world (file tools) with one pooled Host and an
unstamped Factory:

1. **`loop.Unlimited` runs past the former cap.**
   - Set `ToolLimits: loop.ToolLimits{Iterations: loop.Unlimited, Calls: loop.Unlimited}`.
   - Script 30 tool turns (`PooledTurn{ToolName: orchestrationtest.PooledReadToolName, ToolInput: `{"path":"a.txt"}`}`), then one text turn.
   - The former default cap is 25 (`pkg/loop/mode.go` `defaultMaxToolIterations`).
   - Expect the turn to end in `TurnDone`, not `TurnFailed` with kind `tool_limit`, and
     `len(world.WorkspaceTools.Reads()) == 30`.
   - Control run with `ToolLimits{}`: the same script ends `TurnFailed{tool_limit}`.
     This proves the case exercises the cap.
2. **A zero-timeout hustle is definable, journaled and restorable.**
   - Set `Hustles: []hustle.Definition{mustDefine(hustle.WithName("lane.idle"), hustle.WithParticipation(hustle.ParticipationBackground), hustle.WithCurrentLoopModel(), hustle.WithTimeout(0), hustle.WithSystemPrompt("x", "lane-v1"), hustle.WithPolicyRevision("lane-v1"))}`.
   - Create a session, `Stop` the Host, start a successor, and post an input.
   - Expect the restore to succeed and the input to be applied. That proves
     `DefinitionDescriptor.Validate` accepts `TimeoutNanos: 0` on replay.
   - If the hustle can be invoked in the kit, also assert it completes with no
     deadline error. Otherwise state in a comment that execution-without-deadline is
     covered by harness's own unit tests.
3. **Duplicate keys inside `tool_use.input` survive restore.**
   - Script one tool turn with `ToolInput: `{"path":"a.txt","path":"b.txt","content":"x"}``
     against `PooledWriteToolName`, then a text turn.
   - After it settles, `Stop` the Host, start a successor and post an input.
   - Expect the input to be applied (restore replayed the journal), and the
     `StepDone`'s tool_use `Input` bytes to equal the scripted string byte for byte.
   - Control, documented in a comment: on harness v0.40.2 this restore fails
     (Oxy's `TestOxyOpaqueInputInGatePreparedResume` is the regression it
     reproduces).

Run:

```bash
GOWORK=off go test -count=1 -tags integration -race -run '^TestUnbounded' .
```

Expected: PASS. Commit: `test(lane): unbounded tool loops, zero hustle timeout and opaque tool input across restore`.

## Task A7: Full verification and release tests v0.14.0 — **REQUIRES OWNER CONFIRMATION**

```bash
cd /Users/ipotter/code/looprig/tests
GOTOOLCHAIN=go1.26.8 make check
GOTOOLCHAIN=go1.26.8 make release-check
GOTOOLCHAIN=go1.26.8 make secure
git status --short
```

Expected: all pass, and the tree is clean except `evidence/` if you record the run.

Why a minor: new lanes, a new direct test-only NATS server import, and the nested
build-only module. Release notes must state:
- the pins;
- the lanes added;
- the `oldhostlane` nested module: never tagged, build-only, pins tests v0.13.2 /
  host v0.10.3;
- the one-way rule proven: stamped and presented journals are unreadable below
  harness v0.41.0.

After confirmation, run `git push origin main`, then
`git tag -a v0.14.0 -m "<notes>"` and `git push origin v0.14.0`, and verify with
`git ls-remote`.

---

# Part B: Carbon v0.30.0

Carbon v0.29.1 pins factory v0.11.1, host v0.10.3, harness v0.40.2 and wui v0.3.0.

**What changes, and why each item:**

| Change | Why |
|---|---|
| Re-pin: core v0.12.0, sessionstore v0.14.0, inference v0.14.0, harness v0.41.0, host v0.11.0, factory v0.12.0, wui v0.4.0 | the feature; MVS lifts core/sessionstore on the harness bump anyway |
| **`internal/app/department.go` `carbonRuntime.ApplyCommand` copies `cmd.Principal` (every kind) and `cmd.Metadata` (create/input) into `runtimecommand.Admitted`** | the adapter builds `Admitted` field by field (`department.go:647`), so a browser's metadata would be dropped silently, the same hazard as the tests kit |
| Pin ledger rows in `internal/app/dependency_test.go` | the ledger names every pin and why |
| No presenter and no stamping by default | Carbon is single-user; stamping stays an explicit deployment opt-in (design ruling 4) |

Nothing else needs to change:
- `carbonInputBlocks` decodes with Core's `sessionwire.InputRequest`, so a
  metadata-bearing payload decodes;
- the wui bundle change arrives through the pin;
- `WithSessionJournalResolver` and `host.NewPublicJournals` stay as they are.

## Task B1: Failing adapter test

**Files:** `internal/app/department_test.go` (or the file holding
`carbonRuntime.ApplyCommand` tests; find it with
`git grep -n 'ApplyCommand' internal/app/*_test.go`).

```go
func TestCarbonRuntimeCopiesPrincipalAndMetadata(t *testing.T) {
	alex := sessionwire.Principal{Tenant: "t", Subject: "user_alex", Kind: sessionwire.PrincipalKindActor}
	for _, kind := range []string{carbonKindCreate, carbonKindInput, carbonKindInterrupt} {
		t.Run(kind, func(t *testing.T) {
			applier := &recordingApplier{} // implements runtimecommand.Applier; records the Admitted
			runtime := newTestCarbonRuntime(t, applier, 3 /* held epoch */)
			cmd := carbonCommand(t, kind) // RuntimeCommand with a real Core payload for create/input
			cmd.Principal = &alex
			if kind != carbonKindInterrupt {
				cmd.Metadata = sessionwire.MessageMetadata{"space": "family"}
			}
			if err := runtime.ApplyCommand(context.Background(), cmd); err != nil {
				t.Fatalf("ApplyCommand: %v", err)
			}
			got := applier.last
			if got.Principal == nil || *got.Principal != alex {
				t.Fatalf("principal = %+v", got.Principal)
			}
			if kind != carbonKindInterrupt && got.Metadata["space"] != "family" {
				t.Fatalf("metadata = %+v", got.Metadata)
			}
			if kind == carbonKindInterrupt && got.Metadata != nil {
				t.Fatalf("metadata on an interrupt: %+v", got.Metadata)
			}
		})
	}
}
```

Reuse the file's existing fake controller and lease stubs for
`recordingApplier` / `newTestCarbonRuntime` / `carbonCommand`. If none exist,
write them against `carbonRuntime`'s fields.

Run `GOWORK=off go test -run TestCarbonRuntimeCopiesPrincipalAndMetadata ./internal/app/`.
Expected: FAIL to compile before the re-pin, and FAIL on `principal = <nil>` after it.

## Task B2: Re-pin, copy, ledger

```bash
cd /Users/ipotter/code/looprig/carbon
GOWORK=off GOTOOLCHAIN=go1.26.8 go get github.com/looprig/core@v0.12.0 github.com/looprig/sessionstore@v0.14.0 \
  github.com/looprig/inference@v0.14.0 github.com/looprig/harness@v0.41.0 github.com/looprig/host@v0.11.0 \
  github.com/looprig/factory@v0.12.0 github.com/looprig/wui@v0.4.0
GOWORK=off go mod tidy
```

In `ApplyCommand`, add `Principal: cmd.Principal` to the `admitted` literal. In
the create and input arms, add `admitted.Metadata = cmd.Metadata`. Comment it:
"COPIED: Admitted is built field by field; an omitted member is dropped in silence."

Update the ledger rows in `dependency_test.go` for core, sessionstore, harness,
host, factory, wui and inference. Each "why" names the principal/metadata/presenter
feature and the one-way rule:

> once a journal holds a stamped or presented record (or metadata from a browser),
> never roll Carbon back below harness v0.41.0; once a store holds a v3 inbox row,
> never below sessionstore v0.14.0

Keep the existing pairing guards (`host >= v0.5.0` with `harness >= v0.36.0`), and
add a pairing guard `host >= v0.11.0` with `harness >= v0.41.0`.

Run:

```bash
GOTOOLCHAIN=go1.26.8 make check && GOWORK=off go test ./... && GOWORK=off go test -tags integration ./browser/...
```

Expected: PASS, including the Task B1 test.

Commit: `feat(app): carry command principal and metadata into the runtime; pin harness v0.41.0`.

## Task B3: Release Carbon v0.30.0 — **REQUIRES OWNER CONFIRMATION**

Why a minor: new behaviour (metadata reaches the journal; the wui v0.4.0 bundle).
Release notes:
- the one-way rule above;
- "no stamping by default; `factory.WithPrincipalStamping` is a deployment opt-in";
- "`make install` must be re-run for the binary to change".

Push `main`, tag `v0.30.0` annotated, push the tag and verify.

---

# Part C: Oxy switch-over (Oxy Phase 8)

Repo `/Users/ipotter/code/oxy`. Oxy's plan is
`docs/plans/2026-09-25-looprig-factory-host-migration.md` on `feat/looprig-current`.
Its phase table puts this at **Phase 8** ("after Looprig plans 01-09"). Phase 3 (Host
launch target) is done on `feat/host-department`. Phases 4-7 are Oxy's own and are
not in this plan.

**Starting state (`feat/host-department`):**
- `go.mod` pins harness v0.40.2, inference v0.13.0, host v0.10.3, core v0.11.0,
  sessionstore v0.13.1 and wui v0.3.0;
- it carries `replace github.com/looprig/harness => ./third_party/looprig-harness`
  and `replace github.com/looprig/inference => ./third_party/looprig-inference`;
- the forks' patches are listed in each fork's `OXY-PATCHES.md`.

**Do Part C on a new branch `feat/looprig-plain` cut from wherever Phase 7 ended.**
If the owner wants it earlier, cut it from `feat/host-department`. Ask which; do not
pick silently.

## Task C1: Inventory what the forks carry and what replaces each

Map each patch to its replacement:

| Fork patch (OXY-PATCHES.md) | Replacement | Oxy action |
|---|---|---|
| 1 `loop.Unlimited` | harness v0.41.0 `loop.Unlimited` | none (same name, same `-1`) |
| 2 zero hustle timeout | harness v0.41.0 | none; Oxy journals holding `TimeoutNanos: 0` become readable by stock harness |
| 3 `session.HustleHost` | **not upstreamed** | confirm no production file names it: `git grep -n HustleHost -- ':!third_party'` must return only docs/comments. `titles.go:544` already says the title service "never needs session.HustleHost" |
| 4 legacy JSON-text extractor tolerating thinking | **not upstreamed** | Task C4: `hustle.WithOutputSchema` |
| 5 opaque `tool_use.input` | harness v0.41.0 (patch-level fix) | none |
| 6 `transport.WithoutExecutionTimeout` | inference v0.14.0, same name | check the import path of Oxy's cancellable inference client is unchanged. **Also check every provider Oxy can select** (impl-03 known limit): `llm`'s gemini, bedrock and chutes providers build their own `http.Client`, so the marker does not lift their ceilings. If Oxy selects one of them for a long call, **FLAG** it to the owner as an `llm` follow-up; do not patch `llm` here |

## Task C2: Port the fork's Oxy regression tests as consumer tests

The upstream tests now live in harness/inference. Oxy keeps **consumer** tests that
pin the behaviour it relies on through the public API only:

| Fork test | Oxy consumer test | Asserts via public API |
|---|---|---|
| `pkg/loop/unlimited_test.go` | `internal/oxi/looprig_contract_test.go` `TestLoopUnlimitedAccepted` | `loop.Define(loop.WithToolLimits(loop.ToolLimits{Iterations: loop.Unlimited, Calls: loop.Unlimited}))` succeeds; `-2` is refused |
| `internal/sessionruntime/unlimited_test.go` (quota) | `TestDelegationQuotaUnlimited` | `rig.Define(rig.WithDelegationLimits(rig.DelegationLimits{Quota: loop.Unlimited, ...}))` succeeds |
| `pkg/hustle/unlimited_test.go` | `TestHustleZeroTimeoutAccepted` | `hustle.Define(... hustle.WithTimeout(0) ...)` succeeds; negative refused |
| `pkg/event/oxy_opaque_input_test.go` incl. `TestOxyOpaqueInputInGatePreparedResume` | `TestDuplicateKeyToolInputRoundTrips` | `event.MarshalEvent` / `event.UnmarshalEvent` round-trip a `StepDone`, and a `GatePrepared` with `Resume`, whose `tool_use` input has duplicate keys; the envelope with a duplicate key is still refused |
| `transport/execution_timeout_test.go` | `TestInferenceClientMarksNoExecutionTimeout` | Oxy's cancellable client wraps each call's context with `transport.WithoutExecutionTimeout`; test through an `httptest.Server` that delays the headers past 60 s using a fake clock if the transport allows, else assert the marker with a RoundTripper probe |
| `internal/hustleruntime/reasoning_output_test.go` | replaced by Task C4's structured-output test | — |
| `pkg/session/hustle_host_test.go`, `internal/sessionruntime/hustle_host_test.go` | dropped | HustleHost is gone |

Also keep Oxy's existing `internal/oxi/compaction_test.go:141` zero-timeout case,
which now runs against stock harness.

**Steps:**
1. Write the consumer tests first **while the forks are still in place**. Expected: PASS.
2. Commit: `test(oxi): consumer tests for the upstreamed Looprig behaviours`.

## Task C3: Delete the forks, drop the replaces, pin plain releases

```bash
cd /Users/ipotter/code/oxy
git rm -r third_party/looprig-harness third_party/looprig-inference
GOWORK=off go mod edit -dropreplace github.com/looprig/harness -dropreplace github.com/looprig/inference
GOWORK=off GOTOOLCHAIN=go1.26.8 go get github.com/looprig/harness@v0.41.0 github.com/looprig/inference@v0.14.0 \
  github.com/looprig/host@v0.11.0 github.com/looprig/core@v0.12.0 github.com/looprig/sessionstore@v0.14.0 \
  github.com/looprig/factory@v0.12.0 github.com/looprig/wui@v0.4.0
GOWORK=off go mod tidy
grep -n replace go.mod; git grep -n third_party -- ':!docs'
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./...
```

Expected:
- no `replace` line;
- no remaining `third_party` references outside docs (fix Dockerfile, Makefile and
  CI paths that copied the forks);
- tests pass, the Task C2 consumer tests included.

Commit: `chore(deps): drop the Looprig forks; pin harness v0.41.0 and inference v0.14.0`.

Also update `docs/LOOPRIG-INTEGRATION.md` (it mentions HustleHost) and the Phase 8
row of the Oxy plan.

## Task C4: Move Oxy's hustles to `hustle.WithOutputSchema`, provider by provider

The fork's patch 4 existed because the legacy JSON-text extractor refused a text
block beside thinking blocks. `hustle.WithOutputSchema`'s reader ignores thinking
(inference `structured_result.go`). **It requires each provider to support native
structured output.**

**Step 1: Inventory the hustles and providers.**

```bash
git grep -n 'hustle.Define' -- '*.go' ':!*_test.go'
git grep -n 'Provider' internal/oxi/config.go
```

Known today:
- `context.compact` (`internal/oxi/compaction.go:55`, policy revision
  `harness-compaction-parser-v2`, JSON-text output);
- `sessionTitleDefinition` (`titles.go:538`). Titles no longer run through it; it is
  registered **only on serve rigs** so existing journals and the serve topology
  revision stay unchanged.

For each provider Oxy's config can select, record whether its `llm` provider
declares native structured output. Check `llm v0.15.0` capability metadata: a model
capability such as `model.WithStructuredOutput` or the provider's structured-output
support table. A provider that lacks it is a **FLAG**: stop, report it to the owner,
and leave that hustle on the legacy extractor. Without the fork, the legacy extractor
will now **refuse** a reply that carries thinking beside the text.

**Step 2: Compatibility check before changing a definition.** A hustle's descriptor
is journaled and validated on replay. Changing `context.compact`'s output mode or
policy revision can change the rig's topology or compatibility revision. That could
refuse restore of existing sessions, or strand them on a Host whose compatibility ID
differs. Oxy's Phase 3 notes say the compatibility ID uses harness restore-critical
fields only.

Write a test first: an Oxy journal recorded under the old definition
(`internal/oxi/testdata`, or one produced by the migration fixture) restores under
the new rig. If it does not, the switch needs a new hustle **name**. Keep the old
definition registered for replay only, and **ask the owner** before choosing.

**Step 3: TDD per hustle.**
1. Failing test: a fake inference client returns a structured-output response with
   a thinking block beside the JSON. The hustle yields the parsed result.
2. Change the definition to `hustle.WithOutputSchema(<schema for the compaction summary>)`
   and bump the policy revision (for example `oxi-compaction-structured-v1`).
3. Run: PASS. Commit per hustle.

The serve-only title definition is replay-only and is deleted when Oxy Phase 4
removes the serve mount. Do not migrate it.

## Task C5: Principal stamping and the presenter; retire the interim handler

Design "Oxy migration" steps 1-7, adapted to Oxy's composition (Factory + one pooled
Host in one process, Oxy plan "Target architecture"):

1. **Runtime adapter copy.** `internal/oxi/department/department.go:354` (it exists on
   `feat/host-department`; `feat/looprig-current` and `feat/state-migration` have no
   department adapter, so confirm the branch Part C starts from carries it) builds
   `runtimecommand.Admitted` field by field (`oxyRuntime.ApplyCommand`; its test helper
   `helpers_test.go:148` builds `hostdepartment.RuntimeCommand` and must gain the members). Add `Principal: cmd.Principal` (all
   kinds), plus `admitted.Metadata = cmd.Metadata` in the create and input arms.
   Test first, exactly like Task B1: `TestOxyRuntimeCopiesPrincipalAndMetadata`.
2. **Presenter.** New `internal/oxi/presenter.go`:

   ```go
   // Presenter renders "[from: <name> (<role>) · space: <space>]" as one prefix
   // block for a stamped input. It returns an empty frame for a nil principal,
   // which every presenter must handle. It is deterministic in its Input, as
   // the design requires: Members is a snapshot read, not a live query.
   type Presenter struct{ Members MemberDirectory }

   type MemberDirectory interface {
   	Member(ctx context.Context, subject sessionwire.SubjectID) (name, role string, ok bool, err error)
   }

   func (p Presenter) Present(ctx context.Context, in present.Input) (present.Frame, error) {
   	if in.Principal == nil {
   		return present.Frame{}, nil
   	}
   	name, role, ok, err := p.Members.Member(ctx, in.Principal.Subject)
   	if err != nil {
   		return present.Frame{}, err // refused disposition; the message is not applied
   	}
   	if !ok {
   		name, role = string(in.Principal.Subject), "unknown"
   	}
   	label := fmt.Sprintf("[from: %s (%s)", name, role)
   	if space := in.Metadata["space"]; space != "" {
   		label += " · space: " + space
   	}
   	return present.Frame{Prefix: []content.Block{&content.TextBlock{Text: label + "]"}}}, nil
   }
   ```

   Table tests cover:
   - nil principal gives an empty frame;
   - a known member;
   - an unknown member;
   - no space;
   - a directory error returns an error;
   - the frame is within `present.MaxFrameBlocks` / `MaxFrameTextBytes`.

   Register it with `rig.WithMessagePresenter(oxi.Presenter{Members: ...})` in the
   shared rig builder. **Changing the rig option list may change the rig's
   compatibility ID.** Check against Phase 3's compatibility test; the presenter is
   not restore-critical by design (§5: no re-rendering on restore).
3. **Factory.** Order matters: upgrade the Host, then Factory, then enable stamping.
   In the one-process composition that means:
   - the pinned host v0.11.0 advertises `hostlink.attribution.principal`;
   - add a startup check that the composed Host's advertised methods `Supports` the
     token before `factory.WithPrincipalStamping()` is passed;
   - fail startup otherwise.
   - `WithPrincipalStamping` needs a verified credential; `factory.New` already refuses any
     composition without `WithCredentialVerifier` (`*MissingSeamsError`, design §9.10), so
     there is no separate stamping check to satisfy.
4. **Audit.** Oxy's Casbin `Authorizer` implements `factory.AuditAuthorizer`
   (`AuthorizeAuditRead`) with Oxy's audit policy (owner decision). The audit screen
   reads `GET /v1/sessions/{sid}/commands/{cid}`.
5. **Client.** Oxy's UI sends `metadata` (for example `space`) on create/input
   through wui v0.4.0's command plane, never `principal`, and feature-detects
   `message_metadata` on `/v1/capabilities`.
6. **Retire the interim front handler.** If Phase 5 built it (the command_id to
   member audit table plus the server-written header block), delete it, its table
   migration and its tests in the same commit that enables stamping. Keep a one-off
   read-only export of the interim table if the owner wants the historic
   attribution. **Ask.** If Phase 5 did not build it, record "nothing to retire".
7. **One-way note in Oxy's release/deploy notes:** once one stamped message lands, no
   rollback below harness v0.41.0 / sessionstore v0.14.0 for any process sharing
   `state/`.

Verify: `GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./...`, plus Oxy's own
end-to-end smoke. Two browsers as two members see `from <name>` on each other's
messages after reload (Oxy acceptance list).

Commits: one per numbered step. Oxy pushes follow Oxy's own rules and **REQUIRE
OWNER CONFIRMATION**.

---

# Part D: Workspace bookkeeping checklist

The outer `looprig` repository is **never committed by the agent** (master plan;
memory "ignore the outer repo"). Edit the files and leave them for the owner.

**`repositories.mk`** (after each tag exists on its remote):
- `tests` becomes v0.14.0, `wui` v0.4.0 and `carbon` v0.30.0, alongside the
  impl-01..07 rows: core v0.12.0, sessionstore v0.14.0, inference v0.14.0,
  harness v0.41.0, host v0.11.0 and factory v0.12.0.
- `oldhostlane` is **not** added to `NESTED_MODULE_RELEASES`: it is build-only and
  never tagged. Add a comment saying so beside the tests row.

**`go.work`:** each `use` entry is unchanged. Do not add `tests/oldhostlane`. It
must resolve its own pins (tests v0.13.2, host v0.10.3), and a workspace entry would
mask exactly that.

**`AGENTS.md`:**
- Table rows:
  - tier 6 `tests` pins; the new direct test-only NATS server import (if approved);
    the nested build-only `oldhostlane` module pinning tests v0.13.2 / host v0.10.3;
  - `carbon` v0.30.0 pins;
  - `wui` v0.4.0 (core v0.12.0, 36 schemas).
  - The design says tiers and edges are unchanged. Verify that is still true after
    Part A, whose only new edge is external.
- One release paragraph each for tests v0.14.0, wui v0.4.0 and carbon v0.30.0, in
  the house style: commit, tag object, what changed, consumer obligations. Include:
  - **the adapter-copy obligation**: every product runtime that builds
    `runtimecommand.Admitted` field by field must copy `Principal` and `Metadata`,
    or they are dropped in silence (measured in the tests kit, Carbon and Oxy);
  - the one-way rules (§2);
  - the rollout order: every Host, then Factory, then `WithPrincipalStamping`;
  - the token name `hostlink.attribution.principal`.
- Replace the stale sentence "`wui` v0.3.0 re-vendored its contract from core v0.11.0 (35 schemas)".

**Plans:** tick row 09 in `2026-09-25-impl-00-master-plan.md`, rows 6-7 in the
design §8 progress table, and the "Oxy drops extractor patch" row in the
unbounded-execution design's progress table.

**`.github` docs corpus** (`/Users/ipotter/code/looprig/.github`, currently at v0.4.0;
its own repo; release **REQUIRES OWNER CONFIRMATION**):
- `docs/modules/{core,sessionstore,harness,host,factory,wui,inference}.md`: new API
  sections (Principal, MessageMetadata and the capability token; descriptor members;
  `pkg/present` and `WithMessagePresenter`; `loop.Unlimited`; zero hustle timeout;
  `WithoutExecutionTimeout`; `WithPrincipalStamping`, `AuditAuthorizer` and the audit
  route; the wui transcript frame).
- `docs/guides/harness*` and `docs/guides/web-ui`: a "Message presenter" guide with
  Oxy's household example (nil-principal handling, determinism, once-only rendering,
  the failure-means-refused rule).
- `docs/_data/modules.json`, `dependencies.json`, `packages.json`: versions and new
  packages (`harness/pkg/present`).
- `docs/_data/evidence.json`: the tests v0.14.0 lanes.
- Grep for stale versions: `git grep -n 'v0\.40\.2\|v0\.11\.1\|v0\.10\.3\|wui v0\.3\.0' docs`.
  Update each hit that describes "current". Leave historical release notes alone.
- Release `.github` as a patch or minor per its own convention. Then re-pin `www` to
  it (the AGENTS.md W6 row shows `www` pins the corpus).
