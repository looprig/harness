# factory v0.12.0: principal stamping, message metadata, capability-gated admission — implementation plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

> **Commits:** conventional messages with **no Co-Authored-By trailer** (master plan rule; it overrides any tool default). Never commit a `go.work` and never add a `replace` directive.

> **Unverified code (cross-plan review, 2026-09-25).** The code blocks were written against
> factory v0.11.1 source but were **not compiled or run** while planning. Placeholders to
> resolve against the real tree: the admission fixture knobs (`f.auth.calls`,
> `f.commands.calls`/`lastAdmit`, `f.creates.prepares`/`lastAdmit`, `faultInjector.failing`),
> `mustNew`/`validOptions`/`withoutOption` (Task 5), the attach-fixture knobs
> `links.principal`/`principalCalls`/`attached` (Task 8), `newReadRouter`/`getJSON` (Task 10),
> `composeStampingWithHosts` (Task 9), and bodies elided as `// ...existing...` in Tasks 2, 6
> and 8. Task 9's probe asks over the SERVICE identity's tenant link; confirm a Host has a
> link for that tenant, or every probe will log "could not be asked".

**Goal:** Factory v0.12.0 accepts client `metadata` on create/input, refuses a
client-supplied `principal` on every kind, and, when a deployment opts in with
`factory.WithPrincipalStamping()`, stamps the verified principal on every command it
admits. It only hands a command carrying either member to a Host that advertises
`hostlink.attribution.principal`: refused at admission for an incapable resident
owner, withheld from placement on an incapable candidate. It exposes
`GET /v1/sessions/{sid}/commands/{cid}` with principal and metadata behind an
optional `AuditAuthorizer`.

**Architecture:** Admission is where every rule lives, so REST and ClientLink (both
routed to `internal/admission.Service`) cannot disagree. The stamped principal is written
into the decoded Core request, so the canonical payload, and with it the content digest
the store compares on retry, binds the sender to the `command_id`. The capability
plumbing copies the `gate_response` shape one for one: a predicate
(`hostlink.PrincipalCapable`), a pool query (`Pool.AcceptsCommandPrincipal`), an
admission seam (`PrincipalResponders`, `ownerAppliesPrincipal`), and a placement filter
(`appliesPrincipal` reusing `classifyGateCapability`).

**Tech Stack:** Go 1.26.8, `GOWORK=off GOTOOLCHAIN=go1.26.8`; core v0.12.0 and
sessionstore v0.14.0 (plans 01, 02). **No harness edge** (forbidden by
`import_boundary_test.go`).

**Design (binding):** `harness/docs/plans/2026-09-25-message-principal-metadata-presenter-design.md`
§1.1, §1.5, §2, §3, §4, §6 step 4. **Master plan:** `2026-09-25-impl-00-master-plan.md` row 07.
**Token rename (coordinator, 2026-09-25):** the design's `hostlink.command.principal` /
`HostLinkCapabilityCommandPrincipal` is now **`hostlink.attribution.principal`** /
**`sessionwire.HostLinkCapabilityAttributionPrincipal`**, because `hostlink.command.<kind>` is
reserved for runtime command kinds. Factory helper names (`PrincipalCapable`,
`AcceptsCommandPrincipal`) are unchanged.

---

## Evidence at factory v0.11.1 (verified 2026-09-25)

| Fact | Where (tag `v0.11.1`) |
|---|---|
| `identity.Principal{tenant, subject, kind}`, `NewPrincipal` bounds subject to `MaxIDBytes` and UTF-8, `Kind` ∈ {actor, service} | `identity/principal.go:54-127` |
| `AdmitCreate`/`AdmitInput`/`AdmitInterrupt`/`AdmitRestore`/`AdmitGateResponse` all run `req.Validate()` first, then authorize | `internal/admission/service.go:302-398` |
| `admitExisting`: canonical payload → `AuthorizeControl` → `retry` → `existingCompatible` → `admit` (no owner check for input/interrupt/restore) | `service.go:479-494` |
| Gate owner check `gateOwnerAnswers` → `ownerAppliesGateResponses` (nil seam refuses; fault is not a code) | `service.go:425-475` |
| `ErrGateResponderUnavailable` → HTTP 503 `unavailable` | `service.go:66-76`; `internal/httpapi/controls.go:495` |
| `admit` builds `AdmitDispositionCommandRequest` | `service.go:526-551` |
| Retry = re-admit under the winner's binding; the store compares kind/digest/size → `command_mismatch` → `command_rejected` | `service.go:560-572`, `:584-594` |
| `canonicalCommand = json.Marshal(request)` | `service.go:635-641` |
| Create: `createIdentity` digests the canonical payload; `PublicCreateIdentity` has no principal/metadata member at sessionstore v0.13.1 | `internal/admission/create.go:118-131`; `sessionstore/public_create.go:27-36` |
| `GateResponseCapable(reply) = reply.Supports(token)`; `Pool.AcceptsGateResponses` | `internal/realtime/hostlink/capability.go:39-41`, `:149-176` |
| Placement capable-only filter + `classifyGateCapability` | `internal/placement/attach.go:462-481`, `:527-574` |
| Dedicated-path capability check | `attach.go:202-229` |
| Wake withholds gate responses from an incapable owner | `attach.go:747-777` |
| Pending sweeper marks gate responses per session from `descriptor.Kind` | `internal/placement/pending.go:280-300`; `Request.GateResponses` `reconciler.go:221-226` |
| Composition: `gateResponders{pool}`, `placementLinks.AcceptsGateResponses`, `classifyCapabilityRead` | `compose.go:103-128`, `:446-490` |
| `WithCredentialVerifier` is a **required** seam (`New` refuses its absence with `*MissingSeamsError`) | `server.go:256`; `options.go:187-195` |
| Start-time warnings (`warnPlacementUncomposed`) | `serve.go:540-575`, called at `serve.go:292` |
| `/v1/agents` and `/v1/capabilities` share one handler | `internal/httpapi/routes.go:931-933`; `reads.go:198-230` |
| REST status for `runtime_unavailable` is **422**, not 409 (`command.RefusalStatus`) | `internal/command/refusal.go:108-125` |
| `TenantAuthorizer` embeds `internalidentity.Authorizer`, method sets pinned equal | `exports.go:33-75` (`TestTenantAuthorizerIsTheInternalAuthorizer`) |
| Released-version allowlist | `module_pin_test.go:15-21` (`releasedLooprigVersions`) |
| No `apidiff` target; releases ran `apidiff -m` by hand | `Makefile`; `~/go/bin/apidiff` |

### Two corrections to the design, stated so the executor does not trip on them

1. **"409 runtime_unavailable"** (design §3) does not match the code. Factory's single status
   authority maps `runtime_unavailable` to **422 Unprocessable Entity**, and a refusal for an
   incapable owner is the same kind of statement as every other `runtime_unavailable`
   ("well formed, this deployment cannot carry it out"). This plan keeps the code
   `runtime_unavailable` and the ruled status **422**, and does not change the shared table.
   The "different-subject retry" answer is `command_rejected`, which **is** 409. If the owner
   wants 409 for the incapable-owner case, that needs a new ruling on `refusal.go`. Flag it in
   the release notes.
2. **"Compose refuses stamping without a Verifier"**: Factory's constructor is `factory.New`,
   and `WithCredentialVerifier` is already a required seam, so a Verifier cannot be absent. The
   refusal is discharged by the existing `*MissingSeamsError`, and Task 5 pins that with a
   test. No unreachable second check is added (host/factory CLAUDE.md: a guard must be
   reachable).

## Pre-flight

### Task 0: Workspace, pins, upstream API check

**Files:** `go.mod`, `go.sum`, `module_pin_test.go:15-21`; uncommitted `go.work`.

**Step 1: Confirm the upstream APIs.**

```bash
cd /Users/ipotter/code/looprig
grep -n 'HostLinkCapabilityAttributionPrincipal' core/sessionwire/v1/*.go
grep -n 'type Principal struct\|type SubjectID\|type PrincipalKind\|type MessageMetadata' core/sessionwire/v1/*.go
grep -n 'Principal \*sessionwire.Principal\|Metadata  *sessionwire.MessageMetadata' sessionstore/disposition_inbox.go sessionstore/public_create.go
```

Expected: the constant; the four types; `Principal`/`Metadata` on
`DispositionCommandDescriptor` and `AdmitDispositionCommandRequest`. **Also required, and not
spelled out in the design:** the create path must fill the same descriptor columns, or
placement cannot see that a pending create carries a principal. Expect
`AdmitPublicCreateRequest.Principal/Metadata` in sessionstore v0.14.0 (impl-02 Task 7;
design §9.11). If it does not exist, **STOP and raise it with the coordinator**:
sessionstore v0.14.0 owes it (plan 02), and Factory must not work around it by parsing
payloads in the sweeper.

**Step 2: Uncommitted workspace.**

```bash
cd /Users/ipotter/code/looprig/factory
cat > go.work <<'EOF'
go 1.26.8

use (
	.
	../core
	../sessionstore
)
EOF
grep -qx 'go.work' .git/info/exclude || echo 'go.work' >> .git/info/exclude
grep -qx 'go.work.sum' .git/info/exclude || echo 'go.work.sum' >> .git/info/exclude
GOTOOLCHAIN=go1.26.8 go build ./...
```

**Step 3 (only once core v0.12.0 and sessionstore v0.14.0 tags exist):**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go get github.com/looprig/core@v0.12.0 github.com/looprig/sessionstore@v0.14.0
GOWORK=off GOTOOLCHAIN=go1.26.8 go mod tidy
```

In `module_pin_test.go` set `"github.com/looprig/core": "v0.12.0"` and
`"github.com/looprig/sessionstore": "v0.14.0"`. If sessionstore v0.14.0 moved storage, update
that row as well to match `go.mod`. Run
`GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'Pin|Module|Boundary' .` and expect `ok`. Commit:

```bash
git add go.mod go.sum module_pin_test.go
git commit -m "chore(deps): pin core v0.12.0 and sessionstore v0.14.0"
```

Until the tags exist, work through `go.work` and make this commit last, before Task 13.

## Identity

### Task 1: `identity.Principal.Wire`, `PrincipalFromWire`, sentinels

**Files:** Modify `identity/principal.go`; Test `identity/principal_test.go`.

**Step 1: Failing tests.**

```go
func TestWireAndBackRoundTripEveryKind(t *testing.T) {
	for _, kind := range []identity.Kind{identity.KindActor, identity.KindService} {
		p, err := identity.NewPrincipal("acme", "user_01H", kind)
		if err != nil {
			t.Fatal(err)
		}
		wire := p.Wire()
		want := sessionwire.Principal{Tenant: "acme", Subject: "user_01H", Kind: sessionwire.PrincipalKind(kind)}
		if wire != want {
			t.Fatalf("Wire() = %+v, want %+v", wire, want)
		}
		back, err := identity.PrincipalFromWire(wire)
		if err != nil || back != p {
			t.Fatalf("PrincipalFromWire = (%+v, %v), want %+v", back, err, p)
		}
	}
}

func TestServiceKindIsServiceOnTheWire(t *testing.T) {
	p, _ := identity.NewPrincipal("acme", "factory-sweeper", identity.KindService)
	if p.Wire().Kind != sessionwire.PrincipalKindService {
		t.Fatalf("Wire().Kind = %q", p.Wire().Kind)
	}
}

func TestPrincipalFromWireRefusesAnInvalidPrincipal(t *testing.T) {
	for _, wire := range []sessionwire.Principal{
		{},
		{Tenant: "acme", Subject: "u"},
		{Tenant: "acme", Subject: "u", Kind: "robot"},
		{Tenant: "", Subject: "u", Kind: sessionwire.PrincipalKindActor},
		{Tenant: "acme", Subject: "", Kind: sessionwire.PrincipalKindActor},
	} {
		p, err := identity.PrincipalFromWire(wire)
		if !errors.Is(err, identity.ErrInvalidPrincipal) || p != (identity.Principal{}) {
			t.Fatalf("PrincipalFromWire(%+v) = (%+v, %v)", wire, p, err)
		}
	}
}

func TestTheAdmissionSentinelsAreDistinct(t *testing.T) {
	if errors.Is(identity.ErrClientPrincipal, identity.ErrMetadataUnsupported) {
		t.Fatal("the two refusals must be distinguishable")
	}
}
```

**Step 2:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./identity` fails (undefined).

**Step 3: Implement** (append to `principal.go`):

```go
// ErrClientPrincipal is the cause of the invalid_request refusal a command gets
// when its body carries a principal. The principal is Factory-stamped from a
// verified credential; a client may never supply one, WHETHER OR NOT this
// deployment stamps, so a client cannot choose the sender an audit records.
var ErrClientPrincipal = errors.New("admission: principal is Factory-stamped; a client may not supply it")

// ErrMetadataUnsupported is the cause of the runtime_unavailable refusal a
// command carrying a principal or metadata gets when the session's resident
// owner does not advertise sessionwire.HostLinkCapabilityAttributionPrincipal:
// that Host would refuse the body after its attempt began.
var ErrMetadataUnsupported = errors.New("admission: the session's Host does not apply command principal or metadata")

// Wire is this principal as Core's sessionwire/v1 record, the value Factory
// stamps on a command. It carries only verified facts: tenant, subject, kind.
func (p Principal) Wire() sessionwire.Principal {
	return sessionwire.Principal{
		Tenant:  p.tenant,
		Subject: sessionwire.SubjectID(p.subject),
		Kind:    sessionwire.PrincipalKind(p.kind),
	}
}

// PrincipalFromWire converts a Core principal read back from a durable record.
// It validates exactly as NewPrincipal does and returns the ZERO Principal on
// refusal. The value is a recorded assertion, not a fresh verification.
func PrincipalFromWire(wire sessionwire.Principal) (Principal, error) {
	if err := wire.Validate(); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidPrincipal, err)
	}
	return NewPrincipal(wire.Tenant, string(wire.Subject), Kind(wire.Kind))
}
```

Add a compile-time check that the two vocabularies agree, beside the `Kind` constants:

```go
// The two spellings of a principal's kind are one vocabulary.
var (
	_ = [1]struct{}{}[len(KindActor)-len(sessionwire.PrincipalKindActor)]
	_ = [1]struct{}{}[len(KindService)-len(sessionwire.PrincipalKindService)]
)
```

The table test is the real guard, so if this check reads as too clever, drop it and keep
the test.

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./identity` → `ok`.

**Step 5:** `git commit -am "feat(identity): convert Principal to and from Core's sessionwire Principal"`

## Admission

### Task 2: Refuse a client principal for every kind; stamp when enabled

**Files:** Modify `internal/admission/service.go`, `internal/admission/create.go`;
Create `internal/admission/principal_test.go`.

**Step 1: Failing tests.**

```go
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory/identity"
)

var clientPrincipal = &sessionwire.Principal{Tenant: "tenant-a", Subject: "someone-else", Kind: sessionwire.PrincipalKindActor}

// admitAll drives every kind once with a request built by mutate.
func admitAll(f *serviceFixture, mutate func(kind string, req any)) map[string]error {
	ctx := context.Background()
	create := sessionwire.CreateRequest{CommandEnvelope: envelope("create-1"), SessionID: "session-new", AgentID: "agent-a"}
	input := sessionwire.InputRequest{CommandEnvelope: envelope("input-1"), SessionID: "session-a", Blocks: json.RawMessage(`[{"type":"text","text":"hi"}]`)}
	interrupt := sessionwire.InterruptRequest{CommandEnvelope: envelope("interrupt-1"), SessionID: "session-a"}
	restore := sessionwire.RestoreRequest{CommandEnvelope: envelope("restore-1"), SessionID: "session-a"}
	gate := gateAnswer("answer-1")
	mutate("create", &create)
	mutate("input", &input)
	mutate("interrupt", &interrupt)
	mutate("restore", &restore)
	mutate("gate_response", &gate)
	errs := map[string]error{}
	_, _, errs["create"] = f.service.AdmitCreate(ctx, f.principal, create)
	_, _, errs["input"] = f.service.AdmitInput(ctx, f.principal, input)
	_, _, errs["interrupt"] = f.service.AdmitInterrupt(ctx, f.principal, interrupt)
	_, _, errs["restore"] = f.service.AdmitRestore(ctx, f.principal, restore)
	_, _, errs["gate_response"] = f.service.AdmitGateResponse(ctx, f.principal, gate)
	return errs
}

func setPrincipal(_ string, req any) {
	switch r := req.(type) {
	case *sessionwire.CreateRequest:
		r.Principal = clientPrincipal
	case *sessionwire.InputRequest:
		r.Principal = clientPrincipal
	case *sessionwire.InterruptRequest:
		r.Principal = clientPrincipal
	case *sessionwire.RestoreRequest:
		r.Principal = clientPrincipal
	case *sessionwire.GateResponseRequest:
		r.Principal = clientPrincipal
	}
}

func TestAClientPrincipalIsRefusedForEveryKindWithAndWithoutStamping(t *testing.T) {
	for _, stamping := range []bool{false, true} {
		f := newServiceFixture(t)
		resolvableSession(f)
		f.rebuild(t, func(cfg *Config) {
			cfg.Binding = SessionBindingTemplate{StorageBindingID: "storage-a", BindingVersion: "v1"}
			cfg.StampPrincipal = stamping
		})
		authBefore, storeBefore := f.auth.calls, f.commands.calls
		for kind, err := range admitAll(f, setPrincipal) {
			if !IsCode(err, sessionwire.ErrorCodeInvalidRequest) || !errors.Is(err, identity.ErrClientPrincipal) {
				t.Fatalf("stamping=%v %s: err = %v, want invalid_request wrapping ErrClientPrincipal", stamping, kind, err)
			}
		}
		if f.auth.calls != authBefore || f.commands.calls != storeBefore || f.creates.prepares != 0 {
			t.Fatalf("stamping=%v: a client principal reached the authorizer or the store", stamping)
		}
	}
}

func TestStampingPutsTheVerifiedPrincipalOnEveryKind(t *testing.T) {
	f := newServiceFixture(t)
	resolvableSession(f)
	f.rebuild(t, func(cfg *Config) {
		cfg.Binding = SessionBindingTemplate{StorageBindingID: "storage-a", BindingVersion: "v1"}
		cfg.StampPrincipal = true
		cfg.PrincipalResponders = &servicePrincipalResponders{}
	})
	want := f.principal.Wire()
	for kind, err := range admitAll(f, func(string, any) {}) {
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	for id, entry := range f.commands.records {
		assertStampedPayload(t, string(id), entry.Record.Descriptor.Payload, want)
		if got := entry.Record.Descriptor.Principal; got == nil || *got != want {
			t.Fatalf("%s: descriptor principal = %+v, want %+v", id, got, want)
		}
	}
	assertStampedPayload(t, "create-1", f.creates.lastAdmit.Payload, want)
}

func TestStampingOffLeavesThePayloadByteIdentical(t *testing.T) {
	f := newServiceFixture(t)
	resolvableSession(f)
	input := sessionwire.InputRequest{CommandEnvelope: envelope("input-1"), SessionID: "session-a", Blocks: json.RawMessage(`[{"type":"text","text":"hi"}]`)}
	if _, _, err := f.service.AdmitInput(context.Background(), f.principal, input); err != nil {
		t.Fatal(err)
	}
	const golden = `{"version":1,"command_id":"input-1","session_id":"session-a","blocks":[{"type":"text","text":"hi"}]}`
	if got := string(f.commands.lastAdmit.Payload); got != golden {
		t.Fatalf("payload = %s, want the v0.11.1 bytes %s", got, golden)
	}
	if f.commands.lastAdmit.Principal != nil || f.commands.lastAdmit.Metadata != nil {
		t.Fatal("columns set with stamping off and no metadata")
	}
}

func TestAServicePrincipalStampsService(t *testing.T) {
	f := newServiceFixture(t)
	resolvableSession(f)
	f.rebuild(t, func(cfg *Config) { cfg.StampPrincipal = true; cfg.PrincipalResponders = &servicePrincipalResponders{} })
	svc, _ := identity.NewPrincipal("tenant-a", "factory-ops", identity.KindService)
	if _, _, err := f.service.AdmitInterrupt(context.Background(), svc, sessionwire.InterruptRequest{CommandEnvelope: envelope("i-1"), SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if got := f.commands.lastAdmit.Principal; got == nil || got.Kind != sessionwire.PrincipalKindService {
		t.Fatalf("principal = %+v, want kind service", got)
	}
}

func assertStampedPayload(t *testing.T, id string, payload []byte, want sessionwire.Principal) {
	t.Helper()
	var probe struct {
		Principal *sessionwire.Principal `json:"principal"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil || probe.Principal == nil || *probe.Principal != want {
		t.Fatalf("%s: payload principal = %+v (%v), want %+v", id, probe.Principal, err, want)
	}
}
```

The fixture fakes need: a `calls` counter on `serviceAuthorizer`, one on
`servicePublicCreates` (`prepares`), and `lastAdmit` capture there. Add them if they are
missing, keeping the file's style. `servicePrincipalResponders` is added in Task 3. Declare
it in this step with a `refuse bool` so this file compiles; Task 3 grows it. Take the golden
from running today's code once, not from working it out by hand.

**Step 2:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./internal/admission -run 'Principal|Stamp' -v` fails.

**Step 3: Implement.**

`Config` gains:

```go
	// StampPrincipal makes every command this service admits carry the
	// verified principal (factory.WithPrincipalStamping). OFF by default, and
	// never partial: when on, every kind is stamped, so the audit trail has no
	// gaps. A client principal is refused whether it is on or off.
	StampPrincipal bool

	// PrincipalResponders decides whether a session's resident owner can apply
	// a command carrying a principal or metadata. OPTIONAL; nil refuses every
	// such command addressed to a session with a live owner
	// (ErrMetadataUnsupported), which is the safe default.
	PrincipalResponders PrincipalResponders
```

Add one helper that every entry point calls right after `req.Validate()` and **before**
`canonicalCommand` or `AuthorizeControl`:

```go
// attribute is the principal rule for every kind, in the design's order: a
// client principal is refused first, whether or not this deployment stamps,
// and before any authorizer or store call; then, when stamping is on, the
// verified principal is written into the request so the canonical payload --
// and the content digest the store compares on retry -- binds the sender to
// the command id. A retry of the same id by a different subject is therefore
// command_rejected, never re-attributed.
func (s *Service) attribute(principal identity.Principal, carried **sessionwire.Principal) error {
	if *carried != nil {
		return refusal(sessionwire.ErrorCodeInvalidRequest, fmt.Errorf("%w (field principal)", identity.ErrClientPrincipal))
	}
	if s.cfg.StampPrincipal {
		wire := principal.Wire()
		*carried = &wire
	}
	return nil
}
```

and in each `Admit*`:

```go
	if err := req.Validate(); err != nil { ...unchanged... }
	if err := s.attribute(principal, &req.Principal); err != nil {
		return sessionstore.DispositionInboxEntry{}, false, err
	}
```

Thread the columns to the store. `admit` gains a `members` parameter:

```go
// members are the two optional descriptor columns. They restate what the
// canonical payload already carries so the store can compare them by value on
// a by-reference payload, and so the pending sweeper can see them without
// reading any payload.
type members struct {
	principal *sessionwire.Principal
	metadata  sessionwire.MessageMetadata
}
```

and sets `req.Principal = m.principal; req.Metadata = m.metadata` on the
`AdmitDispositionCommandRequest`. `admitExisting` and `retry` take and pass it. Callers pass
`members{req.Principal, req.Metadata}` for input and `members{principal: req.Principal}` for
interrupt, restore and gate_response. For a create, `admitPublicCreate`
(`create.go:162`) sets the columns on `sessionstore.AdmitPublicCreateRequest`, which is
where sessionstore v0.14.0 carries them (impl-02 Task 1 Step 3 and Task 7; resolved in
design §9.11): `admit := sessionstore.AdmitPublicCreateRequest{Identity: identity,
Principal: req.Principal, Metadata: req.Metadata}`. `PublicCreateIdentity` does **not**
gain them. The reservation still binds them, because `createIdentity` digests the
canonical (stamped) payload, and a different-subject create retry is refused by the
reservation/inbox comparison as `command_mismatch`, which `createRefusal` maps to
`command_rejected` (409). `AdmitPublicCreate` copies them onto the create's own
disposition inbox descriptor, which is the row the pending sweeper reads (Task 8).
Assert `f.creates.lastAdmit.Principal`/`.Metadata` in
`TestStampingPutsTheVerifiedPrincipalOnEveryKind` and in a create row with metadata.

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./internal/admission` → `ok`. Pre-existing tests
must be unchanged, because stamping defaults off.

**Step 5:** `git commit -am "feat(admission): refuse a client principal on every kind and stamp the verified one when enabled"`

### Task 3: A different-subject retry is `command_rejected`

**Files:** Test `internal/admission/principal_test.go` (append). There is no production
change if Task 2 is right; this task pins the property.

```go
func TestARetryByADifferentSubjectIsRejectedNotReattributed(t *testing.T) {
	f := newServiceFixture(t)
	resolvableSession(f)
	f.rebuild(t, func(cfg *Config) { cfg.StampPrincipal = true; cfg.PrincipalResponders = &servicePrincipalResponders{} })
	input := sessionwire.InputRequest{CommandEnvelope: envelope("input-1"), SessionID: "session-a", Blocks: json.RawMessage(`[{"type":"text","text":"hi"}]`)}
	if _, _, err := f.service.AdmitInput(context.Background(), f.principal, input); err != nil {
		t.Fatal(err)
	}
	other, _ := identity.NewPrincipal("tenant-a", "actor-b", identity.KindActor)
	_, _, err := f.service.AdmitInput(context.Background(), other, input)
	if !IsCode(err, sessionwire.ErrorCodeCommandRejected) {
		t.Fatalf("retry by another subject = %v, want command_rejected", err)
	}
	stored := f.commands.records["input-1"].Record.Descriptor.Principal
	if stored == nil || string(stored.Subject) != "actor-a" {
		t.Fatalf("the stored sender changed to %+v", stored)
	}
	// The same subject's retry is idempotent.
	if _, _, err := f.service.AdmitInput(context.Background(), f.principal, input); err != nil {
		t.Fatalf("same-subject retry = %v", err)
	}
}
```

Run `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./internal/admission -run DifferentSubject -v`. Expect PASS.
Then mutate: delete `*carried = &wire` from `attribute`. Expect FAIL. Revert. Add the same
test against the real store in `service_store_integration_test.go`, following that file's
build tag and setup, so the digest comparison is sessionstore's own and not the fake's.

`git commit -am "test(admission): a different subject's retry of a stamped command is command_rejected"`

### Task 4: Capability-gated admission for a resident owner

**Files:** Modify `internal/admission/service.go`; Test `internal/admission/principal_test.go`.

**Step 1: Failing tests.**

```go
type servicePrincipalResponders struct {
	faultInjector
	wrap   error
	refuse bool
	calls  int
}

func (p *servicePrincipalResponders) AcceptsCommandPrincipal(_ context.Context, _ sessionwire.HostLinkRegistryObservation) (bool, error) {
	p.calls++
	if err := p.enter("AcceptsCommandPrincipal"); err != nil {
		if p.wrap != nil {
			return false, fmt.Errorf("%w: %w", p.wrap, err)
		}
		return false, err
	}
	return !p.refuse, nil
}

func withMetadata(kind string, req any) {
	if r, ok := req.(*sessionwire.InputRequest); ok {
		r.Metadata = sessionwire.MessageMetadata{"space": "family"}
	}
}

func TestACommandCarryingAMemberIsRefusedForAnIncapableResidentOwner(t *testing.T) {
	for name, row := range map[string]struct {
		configure func(*serviceFixture, *servicePrincipalResponders)
		admitted  bool
	}{
		"incapable owner":        {func(_ *serviceFixture, p *servicePrincipalResponders) { p.refuse = true }, false},
		"no responders composed": {func(f *serviceFixture, _ *servicePrincipalResponders) { f.rebuild(t, func(cfg *Config) { cfg.PrincipalResponders = nil }) }, false},
		"capable owner":          {func(*serviceFixture, *servicePrincipalResponders) {}, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newServiceFixture(t)
			resolvableSession(f)
			responders := &servicePrincipalResponders{}
			f.rebuild(t, func(cfg *Config) { cfg.PrincipalResponders = responders })
			row.configure(f, responders)
			before := f.commands.calls
			input := sessionwire.InputRequest{CommandEnvelope: envelope("input-1"), SessionID: "session-a",
				Blocks: json.RawMessage(`[{"type":"text","text":"hi"}]`), Metadata: sessionwire.MessageMetadata{"space": "family"}}
			_, _, err := f.service.AdmitInput(context.Background(), f.principal, input)
			if row.admitted {
				if err != nil {
					t.Fatalf("capable owner: %v", err)
				}
				return
			}
			if !IsCode(err, sessionwire.ErrorCodeRuntimeUnavailable) || !errors.Is(err, identity.ErrMetadataUnsupported) {
				t.Fatalf("err = %v, want runtime_unavailable wrapping ErrMetadataUnsupported", err)
			}
			if f.commands.calls != before {
				t.Fatal("an incapable owner's command reached AdmitDispositionCommand")
			}
		})
	}
}

func TestAnUnstampedMemberlessCommandNeverAsks(t *testing.T) {
	f := newServiceFixture(t)
	resolvableSession(f)
	responders := &servicePrincipalResponders{refuse: true}
	f.rebuild(t, func(cfg *Config) { cfg.PrincipalResponders = responders })
	if _, _, err := f.service.AdmitInterrupt(context.Background(), f.principal,
		sessionwire.InterruptRequest{CommandEnvelope: envelope("i-1"), SessionID: "session-a"}); err != nil {
		t.Fatalf("a plain interrupt was refused: %v", err)
	}
	if responders.calls != 0 {
		t.Fatalf("a plain command asked the capability %d times", responders.calls)
	}
}

func TestNoLiveOwnerAdmitsAndLeavesItToPlacement(t *testing.T) {
	f := newServiceFixture(t)
	resolvableSession(f)
	f.directory.ok = false // no resident owner
	responders := &servicePrincipalResponders{refuse: true}
	f.rebuild(t, func(cfg *Config) { cfg.PrincipalResponders = responders; cfg.StampPrincipal = true })
	if _, _, err := f.service.AdmitInput(context.Background(), f.principal, sessionwire.InputRequest{
		CommandEnvelope: envelope("input-1"), SessionID: "session-a", Blocks: json.RawMessage(`[{"type":"text","text":"hi"}]`)}); err != nil {
		t.Fatalf("an unplaced session's stamped input was refused: %v", err)
	}
	if responders.calls != 0 {
		t.Fatal("asked a Host that does not own the session")
	}
}

func TestAnUnreachableOwnerIsAFaultWithNothingWritten(t *testing.T) {
	f := newServiceFixture(t)
	resolvableSession(f)
	responders := &servicePrincipalResponders{wrap: ErrPrincipalResponderUnavailable}
	responders.failing = "AcceptsCommandPrincipal"
	f.rebuild(t, func(cfg *Config) { cfg.PrincipalResponders = responders; cfg.StampPrincipal = true })
	before := f.commands.calls
	_, _, err := f.service.AdmitInterrupt(context.Background(), f.principal,
		sessionwire.InterruptRequest{CommandEnvelope: envelope("i-1"), SessionID: "session-a"})
	var coded *Error
	if errors.As(err, &coded) || !errors.Is(err, ErrPrincipalResponderUnavailable) || f.commands.calls != before {
		t.Fatalf("err = %v (writes %d), want an uncoded fault wrapping ErrPrincipalResponderUnavailable and no write", err, f.commands.calls-before)
	}
}
```

Before relying on `faultInjector.failing`, check that it is the field
`gate_capability_test.go:90` sets; use the actual field name. Add a stamped gate_response row
to the first test as well. A gate response asks both capabilities, gate first.

**Step 2:** Run and see it fail.

**Step 3: Implement** in `service.go`:

```go
// ErrPrincipalResponderUnavailable classifies a failure to ASK the owner
// whether it applies command principal and metadata that is a transient
// condition of the path to it. It is ErrGateResponderUnavailable's twin and is
// answered the same way: a fault with no public code, HTTP 503 retryable,
// nothing written.
var ErrPrincipalResponderUnavailable = errors.New("admission: the session's owner could not be reached to ask whether it applies command principal and metadata")

// PrincipalResponders answers whether a session's live owner can apply a
// command carrying a principal or metadata. The composition implements it
// over the HostLink pool with hostlink.PrincipalCapable. (false, nil) is a
// decision; an error is a fault.
type PrincipalResponders interface {
	AcceptsCommandPrincipal(ctx context.Context, owner sessionwire.HostLinkRegistryObservation) (bool, error)
}

// ownerAppliesPrincipal is the admission gate for a command carrying a
// principal or metadata, asked BEFORE anything is written. A session with no
// fresh matching owner is admitted: nothing can read the command until
// placement puts the session on a capable Host (appliesPrincipal). A live
// owner that cannot apply it would refuse the body after its attempt, so the
// command is refused runtime_unavailable. The catalog's LeaseEpoch and Host
// version strings are never read as a capability (Fable ruling 2026-09-19).
func (s *Service) ownerAppliesPrincipal(ctx context.Context, tenant sessionwire.TenantID, session sessionwire.SessionID, record sessionstore.CatalogRecord, m members) error {
	if m.principal == nil && len(m.metadata) == 0 {
		return nil
	}
	owner, ok, err := s.cfg.Directory.Owner(ctx, tenant, session)
	if err != nil {
		return fmt.Errorf("admission: observe the session's owner: %w", err)
	}
	if !ok || !freshMatchingOwner(owner, record, s.cfg.Clock.Now()) {
		return nil
	}
	if s.cfg.PrincipalResponders == nil {
		return refusal(sessionwire.ErrorCodeRuntimeUnavailable, identity.ErrMetadataUnsupported)
	}
	accepts, err := s.cfg.PrincipalResponders.AcceptsCommandPrincipal(ctx, owner)
	if err != nil {
		return fmt.Errorf("admission: ask the owner whether it applies command principal and metadata: %w", err)
	}
	if !accepts {
		return refusal(sessionwire.ErrorCodeRuntimeUnavailable, identity.ErrMetadataUnsupported)
	}
	return nil
}
```

Call it in `admitExisting` after `existingCompatible` and before `admit`. In
`AdmitGateResponse`, call it after `gateOwnerAnswers` and before `admit`. A retry that was
already stored returns through `retry` before either check, so a retry answers from its
record, as the gate rule does. `AdmitCreate` does not call it: a new session has no owner,
and placement enforces the rule (Task 8).

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./internal/admission` → `ok`, including
`TestTheCapabilityIsAskedOnlyForAGateResponse`. That test still holds because a plain input
never asks either question.

**Step 5:** `git commit -am "feat(admission): refuse a principal- or metadata-bearing command for an incapable resident owner"`

## Composition options

### Task 5: `WithPrincipalStamping`, and the Verifier requirement pinned

**Files:** Modify `options.go`, `server.go` (`config`), `compose.go` (`admission.Config`);
Test `options_test.go`.

**Step 1: Failing tests.**

```go
func TestWithPrincipalStampingIsOffByDefaultAndOnWhenSupplied(t *testing.T) {
	off := mustNew(t, validOptions(t)...) // this file's helpers
	if off.cfg.stampPrincipal {
		t.Fatal("stamping is on by default")
	}
	on := mustNew(t, append(validOptions(t), factory.WithPrincipalStamping())...)
	if !on.cfg.stampPrincipal {
		t.Fatal("WithPrincipalStamping did not enable stamping")
	}
}

func TestWithPrincipalStampingTwiceIsADuplicate(t *testing.T) {
	_, err := factory.New(append(validOptions(t), factory.WithPrincipalStamping(), factory.WithPrincipalStamping())...)
	if !errors.Is(err, factory.ErrDuplicateOption) {
		t.Fatalf("err = %v, want ErrDuplicateOption", err)
	}
}

// THE DESIGN'S "REFUSED WITHOUT A VERIFIER" IS THE REQUIRED-SEAM CHECK. A
// Verifier is mandatory for every composition, so stamping can never be
// composed without one; this pins that the refusal names it.
func TestStampingWithoutAVerifierIsRefused(t *testing.T) {
	_, err := factory.New(append(withoutOption(validOptions(t), "WithCredentialVerifier"), factory.WithPrincipalStamping())...)
	var missing *factory.MissingSeamsError
	if !errors.As(err, &missing) || !slices.Contains(missing.Options, "WithCredentialVerifier") {
		t.Fatalf("err = %v, want MissingSeamsError naming WithCredentialVerifier", err)
	}
}
```

Match `mustNew`, `validOptions` and `withoutOption` to the helpers `options_test.go` and
`server_test.go` already define (read them first). Internal-field assertions belong in a
`package factory` test file, and there is precedent in `compose_*_internal_test.go`.

**Step 2:** fails (undefined).

**Step 3: Implement.** In `options.go`:

```go
// WithPrincipalStamping makes Factory stamp the VERIFIED principal (tenant,
// subject, kind) on every command it admits -- create, input, interrupt,
// restore and gate_response -- so the audit trail and a harness presenter know
// who sent each one. It is an explicit per-deployment opt-in and never a
// default, and it is all-or-nothing: no command is admitted unstamped while it
// is on.
//
// It is meaningful only with a verified credential, and WithCredentialVerifier
// is already a required seam, so New refuses a composition without one.
//
// ROLLOUT: upgrade every Host to one advertising
// hostlink.attribution.principal (host >= v0.11.0), then Factory, then enable
// this. While it is on, a session resident on an older Host is refused
// runtime_unavailable until it is re-placed, and placement puts sessions only
// on capable Hosts; Start logs a WARN per registered Host lacking the token.
//
// Enabling it turns the retry of a command admitted BEFORE it was enabled into
// command_rejected (the stamped payload differs), so enable it at a quiet
// moment.
//
// ONE-WAY: once a stamped command is stored, every Factory and Host sharing the
// store must stay on sessionstore >= v0.14.0.
func WithPrincipalStamping() Option {
	return option("WithPrincipalStamping", func(c *config) error {
		c.stampPrincipal = true
		return nil
	})
}
```

Add `stampPrincipal bool` to `config`. In `composeComponents`, set
`StampPrincipal: cfg.stampPrincipal` and
`PrincipalResponders: principalResponders{pool: pool}` on `admission.Config`. The adapter is
defined in Task 7; commit the two together if the build needs it.

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'PrincipalStamping|Verifier' .` → `ok`.

**Step 5:** `git commit -am "feat(factory): add WithPrincipalStamping"`

## HostLink capability

### Task 6: `hostlink.PrincipalCapable` and `Pool.AcceptsCommandPrincipal`

**Files:** Modify `internal/realtime/hostlink/capability.go`; Test
`internal/realtime/hostlink/capability_test.go` (or the file that tests
`AcceptsGateResponses`; find it with `grep -ln AcceptsGateResponses internal/realtime/hostlink/*_test.go`).

**Step 1: Failing tests.**

```go
func TestPrincipalCapableIsAnExactTokenMatch(t *testing.T) {
	for _, row := range []struct {
		methods []string
		want    bool
	}{
		{[]string{sessionwire.HostLinkCapabilityAttributionPrincipal}, true},
		{[]string{sessionwire.HostLinkCapabilityGateResponse}, false},
		{[]string{"hostlink.v1." + sessionwire.HostLinkCapabilityAttributionPrincipal}, false},
		{[]string{sessionwire.HostLinkCapabilityAttributionPrincipal + "x"}, false},
		{nil, false},
	} {
		reply := sessionwire.VersionNegotiationResponse{}.WithHostLinkMethods(row.methods...)
		if got := hostlink.PrincipalCapable(reply); got != row.want {
			t.Fatalf("PrincipalCapable(%v) = %v, want %v", row.methods, got, row.want)
		}
	}
}
```

Build the reply the same way the existing `GateResponseCapable` test does, since the
constructor for a valid response may need a version. Then copy the existing pool-level
`AcceptsGateResponses` tests (capable, incapable, reconnecting → error, terminal → evicted,
no Negotiator → false) to `AcceptsCommandPrincipal`, with a fake Host advertising the new
token.

**Step 2:** fails.

**Step 3: Implement.** Factor the pool query so both capabilities share one path:

```go
// PrincipalCapable is THE ONE STATEMENT of "the Host at the other end of this
// link reads a command's principal and metadata before its attempt", read from
// its connect reply: Core's token HostLinkCapabilityAttributionPrincipal, which
// host >= v0.11.0 advertises unconditionally. Exact match, like
// GateResponseCapable, and the same one-reply staleness applies.
func PrincipalCapable(reply sessionwire.VersionNegotiationResponse) bool {
	return reply.Supports(sessionwire.HostLinkCapabilityAttributionPrincipal)
}

// AcceptsGateResponses ...keep the existing doc...
func (p *Pool) AcceptsGateResponses(ctx context.Context, target Target, tenantID sessionwire.TenantID) (bool, error) {
	return p.accepts(ctx, target, tenantID, p.gateResponses)
}

// AcceptsCommandPrincipal reports whether the Host at target reads command
// principal and metadata, asked of tenantID's link exactly as
// AcceptsGateResponses asks its question.
func (p *Pool) AcceptsCommandPrincipal(ctx context.Context, target Target, tenantID sessionwire.TenantID) (bool, error) {
	return p.accepts(ctx, target, tenantID, PrincipalCapable)
}

// accepts is the one capability read: acquire the tenant's link without holding
// the pool lock across a dial, read the last verified reply, answer by
// predicate. See AcceptsGateResponses for every branch's reason.
func (p *Pool) accepts(ctx context.Context, target Target, tenantID sessionwire.TenantID, capable func(sessionwire.VersionNegotiationResponse) bool) (bool, error) {
	// ...the body of today's AcceptsGateResponses, with the last line
	// `return capable(reply), nil`...
}
```

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./internal/realtime/hostlink` → `ok`.

**Step 5:** `git commit -am "feat(hostlink): PrincipalCapable and Pool.AcceptsCommandPrincipal"`

### Task 7: Composition adapters and the 503 mapping

**Files:** Modify `compose.go:446-490`, `internal/httpapi/controls.go:495`; Tests
`compose_gate_capability_internal_test.go` (pattern), `internal/httpapi/gate_responder_unavailable_test.go` (pattern).

**Step 1: Failing tests.** Copy, in the style of `compose_gate_capability_internal_test.go`,
a test showing that `principalResponders{pool}` answers true for a fake Host advertising the
token and false for one that does not. A reconnecting link must come back wrapped in
`admission.ErrPrincipalResponderUnavailable`. In `internal/httpapi`, copy
`gate_responder_unavailable_test.go` into a test asserting that an admitter returning
`fmt.Errorf("x: %w", admission.ErrPrincipalResponderUnavailable)` is answered 503
`unavailable`. Add a REST test asserting that `refusal(runtime_unavailable,
identity.ErrMetadataUnsupported)` is answered **422** `runtime_unavailable` (see "Two
corrections").

**Step 2:** fails.

**Step 3: Implement.**

```go
// principalResponders is the command-principal capability question asked of
// the pool, gateResponders' twin: admission asks it before writing a command
// carrying a principal or metadata to a session with a live owner.
type principalResponders struct{ pool *hostlink.Pool }

func (p principalResponders) AcceptsCommandPrincipal(ctx context.Context, owner sessionwire.HostLinkRegistryObservation) (bool, error) {
	capable, err := p.pool.AcceptsCommandPrincipal(ctx, hostlink.Target{Host: owner.HostID, Endpoint: owner.InternalEndpoint, Generation: owner.HostGeneration}, owner.TenantID)
	return capable, classifyCapabilityRead(err, admission.ErrPrincipalResponderUnavailable)
}

func (l placementLinks) AcceptsCommandPrincipal(ctx context.Context, owner sessionwire.HostLinkRegistryObservation) (bool, error) {
	capable, err := principalResponders(l).AcceptsCommandPrincipal(ctx, owner)
	if errors.Is(err, hostlink.ErrNoTenantEndpoint) {
		return false, fmt.Errorf("%w: %w", placement.ErrTenantUnaddressable, err)
	}
	if errors.Is(err, admission.ErrPrincipalResponderUnavailable) {
		return false, fmt.Errorf("%w: %w", placement.ErrHostUnreachable, err)
	}
	return capable, err
}
```

Change `classifyCapabilityRead(err error)` to `classifyCapabilityRead(err, unavailable error)`
and pass `admission.ErrGateResponderUnavailable` at the existing call. In
`controls.go` `admissionFailure`:

```go
	if errors.Is(err, admission.ErrGateResponderUnavailable) || errors.Is(err, admission.ErrPrincipalResponderUnavailable) {
```

The ClientLink maps any uncoded fault to its temporary internal error, so it needs no
change. Confirm with the ClientLink test that drives `ErrGateResponderUnavailable`, and add
the twin row.

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test . ./internal/httpapi ./internal/realtime/clientlink` → `ok`.

**Step 5:** `git commit -am "feat(factory): compose the command-principal capability for admission"`

## Placement

### Task 8: Capable-only placement and withheld wakes

**Files:** Modify `internal/placement/reconciler.go` (`Request`, `Result`),
`internal/placement/attach.go` (`HostLinks`, pooled loop, dedicated path, `bindAndDeliver`,
`reportIncapable` reuse), `internal/placement/pending.go` (`collect`); every test fake
implementing `HostLinks` (`grep -ln 'func (.*) AcceptsGateResponses' internal/placement/*_test.go`
— for example `fixround_test.go:82` — gains `AcceptsCommandPrincipal`); Tests
`internal/placement/principal_placement_test.go`, `internal/placement/pending_test.go`.

**Step 1: Failing tests.** Model them on the existing gate-response placement tests. Find
them with `grep -ln 'GateResponses:' internal/placement/*_test.go` and reuse their fixture
constructor, fake links and candidate pages.

```go
func TestAPrincipalBearingSessionIsPlacedOnlyOnACapableHost(t *testing.T) {
	f := newAttachFixture(t) // the existing pooled-attach fixture
	f.candidates("host-old", "host-new")
	f.links.principal = map[sessionwire.HostID]bool{"host-old": false, "host-new": true}
	result, err := f.reconciler.Reconcile(t.Context(), Request{
		TenantID: f.tenant, SessionID: f.session, Wake: []sessionwire.CommandID{"input-1"},
		PrincipalCommands: []sessionwire.CommandID{"input-1"},
	})
	if err != nil || result.Decision.Outcome != OutcomeAttachPooled || result.Decision.Target.HostID != "host-new" {
		t.Fatalf("Reconcile = (%+v, %v), want attach to host-new", result, err)
	}
	if !slices.Contains(result.Incapable, "host-old") || f.links.attached("host-old") {
		t.Fatal("the incapable Host was attached or not reported")
	}
}

func TestNoCapableHostMeansNoCapacityNotAPlacement(t *testing.T) {
	f := newAttachFixture(t)
	f.candidates("host-old")
	f.links.principal = map[sessionwire.HostID]bool{"host-old": false}
	result, err := f.reconciler.Reconcile(t.Context(), Request{TenantID: f.tenant, SessionID: f.session,
		PrincipalCommands: []sessionwire.CommandID{"create-1"}})
	if err != nil || result.Decision.Outcome != OutcomeNoCapacity || f.links.attached("host-old") {
		t.Fatalf("Reconcile = (%+v, %v), want NoCapacity with no attach", result, err)
	}
}

func TestAPlainSessionNeverAsksThePrincipalCapability(t *testing.T) {
	f := newAttachFixture(t)
	f.candidates("host-old")
	f.links.principal = map[sessionwire.HostID]bool{"host-old": false}
	if _, err := f.reconciler.Reconcile(t.Context(), Request{TenantID: f.tenant, SessionID: f.session,
		Wake: []sessionwire.CommandID{"input-1"}}); err != nil {
		t.Fatal(err)
	}
	if f.links.principalCalls != 0 {
		t.Fatal("a plain session asked the principal capability")
	}
}

func TestAWakeWithholdsAPrincipalCommandFromAnIncapableOwner(t *testing.T) {
	f := newAttachFixture(t)
	f.reusableOwner("host-old")
	f.links.principal = map[sessionwire.HostID]bool{"host-old": false}
	result, err := f.reconciler.Reconcile(t.Context(), Request{TenantID: f.tenant, SessionID: f.session,
		Wake: []sessionwire.CommandID{"plain-1", "stamped-1"}, PrincipalCommands: []sessionwire.CommandID{"stamped-1"}})
	if err != nil || result.Delivered != 1 || result.WithheldPrincipalCommands != 1 {
		t.Fatalf("Reconcile = (%+v, %v), want 1 delivered and 1 withheld", result, err)
	}
}
```

Add a dedicated-path row too, copying the dedicated gate-capability test: an incapable
dedicated endpoint is `Incapable` with no attach. Add to `pending_test.go` a due page holding
a pending live input whose descriptor has `Principal` set, a claimed live create with
`Metadata`, and a plain input. Assert that `Request.PrincipalCommands` holds exactly the
first two, and that `Wake` still holds only the pending ones.

The field names on the fake links (`principal`, `principalCalls`, `attached`) are
placeholders for the attach fixture. Add them to the existing fake in the same style as its
gate-response knobs.

**Step 2:** fails.

**Step 3: Implement.**

`reconciler.go` `Request`:

```go
	// PrincipalCommands names the open commands (pending and claimed, live)
	// whose descriptor carries a principal or metadata. A non-empty list means
	// the session may be placed only on a Host advertising
	// hostlink.attribution.principal (HostLinks.AcceptsCommandPrincipal), and
	// those of them in Wake are withheld from an owner that cannot apply them.
	// A claimed one is listed because a successor must read its body before
	// the attempt; an applying one is not, because recovery reads no body.
	PrincipalCommands []sessionwire.CommandID
```

`Result` gains `WithheldPrincipalCommands int` beside `WithheldGateResponses`.

`attach.go`: `HostLinks` gains

```go
	// AcceptsCommandPrincipal reports whether the Host an observation names
	// reads command principal and metadata (hostlink.PrincipalCapable). An
	// error means it could not be asked, and is never read as "can".
	AcceptsCommandPrincipal(ctx context.Context, owner sessionwire.HostLinkRegistryObservation) (bool, error)
```

In the pooled candidate loop, after the gate block:

```go
			if len(req.PrincipalCommands) > 0 {
				switch verdict, err := r.appliesPrincipal(ctx, req, candidate); verdict {
				case gateUnaddressable:
					result.Unaddressable = append(result.Unaddressable, candidate.HostID)
					r.logUnaddressable(ctx, req, candidate, err)
					continue
				case gateIncapable:
					result.Incapable = append(result.Incapable, candidate.HostID)
					incapableWhy[candidate.HostID] = "does not advertise " + sessionwire.HostLinkCapabilityAttributionPrincipal
					continue
				case gateUnreachable:
					result.Unreachable = append(result.Unreachable, candidate.HostID)
					continue
				case gateAbort:
					return result, err
				}
			}
```

```go
// appliesPrincipal is the capable-only placement filter for a session with an
// open command carrying a principal or metadata, appliesGateResponses' twin
// over the same classification (classifyGateCapability): such a session waits
// (OutcomeNoCapacity) rather than landing on a Host that would refuse the
// command's body.
func (r *Reconciler) appliesPrincipal(ctx context.Context, req Request, candidate sessionwire.HostLinkCapacityReport) (gateCapability, error) {
	capable, err := r.cfg.Links.AcceptsCommandPrincipal(ctx, sessionwire.HostLinkRegistryObservation{
		TenantID: req.TenantID, SessionID: req.SessionID,
		HostID: candidate.HostID, HostGeneration: candidate.HostGeneration,
		InternalEndpoint: candidate.InternalEndpoint,
	})
	return classifyGateCapability(capable, err)
}
```

In `placeDedicated`, after the `len(req.GateResponses) > 0` block, add the same check
against the dedicated endpoint, with the same four outcomes and the same catalog re-read.
The simplest way is to fold the two into one loop over `[]func(...) (bool, error)` so there
is a single re-read.

In `bindAndDeliver`, beside the gate withholding:

```go
	principal := commandSet(req.PrincipalCommands)
	var (
		askedPrincipal   bool
		principalCapable bool
		principalErr     error
	)
	for _, command := range req.Wake {
		// ...existing gate arm...
		if _, stamped := principal[command]; stamped {
			if !askedPrincipal {
				principalCapable, principalErr = r.cfg.Links.AcceptsCommandPrincipal(ctx, observation)
				askedPrincipal = true
			}
			if principalErr != nil || !principalCapable {
				result.WithheldPrincipalCommands++
				continue
			}
		}
		// ...deliver...
	}
```

Generalise `gateResponseWake` into `commandSet(ids []sessionwire.CommandID) map[...]struct{}`
and use it for both.

`pending.go`: add `principals []sessionwire.CommandID` to `openSession`, and in `collect`:

```go
			carries := descriptor.Principal != nil || len(descriptor.Metadata) > 0
			switch entry.Record.State {
			case sessionstore.InboxStatePending:
				if live {
					// ...existing...
					if carries {
						session.principals = append(session.principals, descriptor.CommandID)
					}
				}
			case sessionstore.InboxStateClaimed:
				if live {
					session.needsHost = true
					if carries {
						session.principals = append(session.principals, descriptor.CommandID)
					}
				}
```

and pass `PrincipalCommands: session.principals` in `Sweep`.

The wake withholding does **not** stop an old resident Host from reading its durable
stream. Admission's refusal (Task 4) is what protects a resident session, and this withhold
only avoids waking it. State that in the `WithheldPrincipalCommands` doc.

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./internal/placement .` → `ok`. Every `HostLinks`
fake, including root-package ones, must compile with the new method. A fake answers `true`
by default so pre-existing tests are unchanged.

**Step 5:** Mutation: make `appliesPrincipal` return `gateCapable` unconditionally. Expect
`TestNoCapableHostMeansNoCapacityNotAPlacement` to FAIL. Revert.

**Step 6:** `git commit -am "feat(placement): place principal- and metadata-bearing sessions only on capable Hosts"`

## Startup warning and capabilities flags

### Task 9: WARN at Start for each registered Host lacking the token

**Files:** Modify `serve.go` (Start, new `warnIncapableHosts`); Test
`compose_startup_warning_test.go`.

**Step 1: Failing test.** Using that file's log-capturing composition and the root test
fake dialer (`cfg.hostDialer`, precedent `compose_gate_capability_internal_test.go`), compose
with `WithPrincipalStamping()`, a Directory whose `Candidates` returns `host-old` (no token)
and `host-new` (token), then Start:

```go
func TestStartWarnsForEachRegisteredHostLackingThePrincipalToken(t *testing.T) {
	logs, server := composeStampingWithHosts(t, map[sessionwire.HostID]bool{"host-old": false, "host-new": true})
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	eventually(t, func() bool { return logs.count(warnHostLacksPrincipal, "host_id", "host-old") == 1 })
	if logs.count(warnHostLacksPrincipal, "host_id", "host-new") != 0 {
		t.Fatal("warned about a capable Host")
	}
}

func TestStartDoesNotProbeWithoutStamping(t *testing.T) {
	logs, server, dials := composeWithHostsNoStamping(t)
	_ = server.Start(t.Context())
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	if dials() != 0 || logs.count(warnHostLacksPrincipal) != 0 {
		t.Fatal("probed Hosts with stamping off")
	}
}
```

**Step 2:** fails.

**Step 3: Implement.**

```go
const (
	warnHostLacksPrincipal   = "factory: WithPrincipalStamping is on and a registered Host does not advertise hostlink.attribution.principal; sessions on it are refused runtime_unavailable and none are placed on it"
	warnHostPrincipalUnasked = "factory: WithPrincipalStamping is on and a registered Host could not be asked whether it advertises hostlink.attribution.principal"
	principalProbeTimeout    = 5 * time.Second
	principalProbePageLimit  = 64
)

// warnIncapableHosts is the rollout check WithPrincipalStamping documents:
// once, at Start, off the Start path, each pooled Host the directory lists for
// a configured target is asked the capability over the SERVICE identity's
// tenant link, and one WARN is written per Host that answers no (or cannot
// be asked). It changes nothing; placement and admission enforce the rule.
// Dedicated Hosts do not exist before placement creates them and are not
// probed. Bounded: one page per pooled target, one timeout per Host.
func (s *Server) warnIncapableHosts(ctx context.Context) {
	if !s.cfg.stampPrincipal {
		return
	}
	seen := map[sessionwire.HostID]bool{}
	for _, template := range s.cfg.department {
		if template.normalized().Key.Placement != sessionwire.HostPlacementPooled {
			continue
		}
		page, err := s.cfg.directory.Candidates(ctx, sessionstore.ListCompatibleHostsRequest{Key: template.normalized().Key, Limit: principalProbePageLimit})
		if err != nil {
			logger(s.cfg).WarnContext(ctx, warnHostPrincipalUnasked, slog.String("error", err.Error()))
			continue
		}
		for _, host := range page.Hosts {
			if seen[host.HostID] {
				continue
			}
			seen[host.HostID] = true
			probe, cancel := context.WithTimeout(ctx, principalProbeTimeout)
			capable, err := s.components.pool.AcceptsCommandPrincipal(probe, hostlink.Target{Host: host.HostID, Endpoint: host.InternalEndpoint, Generation: host.HostGeneration}, s.cfg.service.Tenant())
			cancel()
			switch {
			case err != nil:
				logger(s.cfg).WarnContext(ctx, warnHostPrincipalUnasked, slog.String("host_id", string(host.HostID)), slog.String("error", err.Error()))
			case !capable:
				logger(s.cfg).WarnContext(ctx, warnHostLacksPrincipal, slog.String("host_id", string(host.HostID)), slog.Uint64("host_generation", host.HostGeneration))
			}
		}
	}
}
```

In `Start`, launch it on `loopCtx` under the same `WaitGroup` as the sweeps, so Stop waits
for it:

```go
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.warnIncapableHosts(loopCtx)
	}()
```

Before relying on `LaunchTemplate.normalized()` and the `components.pool` field name, check
that they are what `compose.go` and `composition.go` define.

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race -run 'Warn|Start' .` → `ok`.

**Step 5:** `git commit -am "feat(factory): warn at Start for each Host lacking hostlink.attribution.principal while stamping"`

### Task 10: `/v1/capabilities` flags

**Files:** Modify `internal/httpapi/routes.go:931-933`, `internal/httpapi/reads.go`
(`serveAgents`), `RouterConfig`; `server.go` (router config); Test `internal/httpapi/reads_test.go`.

**Step 1: Failing tests.**

```go
func TestCapabilitiesAdvertisesTheCommandFeatures(t *testing.T) {
	for _, stamping := range []bool{false, true} {
		rt := newReadRouter(t, func(cfg *RouterConfig) { cfg.StampsPrincipal = stamping }) // this file's helper
		body := getJSON(t, rt, "/v1/capabilities")
		var flags struct {
			MessageMetadata  *bool `json:"message_metadata"`
			CommandPrincipal *bool `json:"command_principal"`
		}
		if err := json.Unmarshal(body, &flags); err != nil {
			t.Fatal(err)
		}
		if flags.MessageMetadata == nil || !*flags.MessageMetadata || flags.CommandPrincipal == nil || *flags.CommandPrincipal != stamping {
			t.Fatalf("stamping=%v: flags = %s", stamping, body)
		}
		var summary sessionwire.DepartmentCapabilitySummary
		if err := json.Unmarshal(body, &summary); err != nil {
			t.Fatalf("the body is no longer a Core summary: %v", err)
		}
	}
}

func TestAgentsBodyIsUnchanged(t *testing.T) {
	rt := newReadRouter(t, nil)
	body := getJSON(t, rt, "/v1/agents")
	if bytes.Contains(body, []byte("message_metadata")) || bytes.Contains(body, []byte("command_principal")) {
		t.Fatalf("/v1/agents gained members: %s", body)
	}
}
```

**Step 2:** fails.

**Decision this task makes (owner to confirm):** design §3 lists the flags as
`message_metadata: true, command_principal: true`. This plan reads `command_principal` as
"this deployment stamps" (false without `WithPrincipalStamping`), because a client that
feature-detects on it wants to know whether sends will be attributed; `message_metadata`
is always true on v0.12.0. wui v0.4.0 (impl-08) and Oxy (impl-09) read only
`message_metadata`. If the owner wants a constant `true`, change the one line in
`withFeatures` and the test row.

**Step 3: Implement.** `RouterConfig` gains:

```go
	// StampsPrincipal is published on /v1/capabilities as command_principal:
	// whether this deployment stamps the verified sender on every command.
	StampsPrincipal bool
```

Make `serveAgents` take `features bool`. `/v1/agents` passes `false` and `/v1/capabilities`
passes `true`. When `features` is true, splice the two members onto Core's marshalled summary
before the ETag is computed:

```go
// withFeatures appends Factory's command-feature flags to a marshalled Core
// summary. message_metadata is true on every Factory >= v0.12.0 (it accepts a
// client's metadata on create and input); command_principal says whether this
// deployment stamps. They are appended after Core's members, in a fixed order,
// so the body -- and its ETag -- is deterministic; Core's decoder retains them
// as additional fields, so a Core reader is unaffected.
func withFeatures(body []byte, stamps bool) []byte {
	if len(body) < 2 || body[len(body)-1] != '}' {
		return body
	}
	flags := `,"command_principal":` + strconv.FormatBool(stamps) + `,"message_metadata":true}`
	return append(body[:len(body)-1:len(body)-1], flags...)
}
```

Put `/v1/capabilities` on its own `served(authAuthenticated, ... rt.serveAgents(true))` rule.
In `server.go`, set `StampsPrincipal: cfg.stampPrincipal`. If
`TestEveryRouteDeclaresWhatItsShapeRequires` restates handlers, it needs no change because
the rules are the same. If it compares handler identity, update its restatement.

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./internal/httpapi .` → `ok`.

**Step 5:** `git commit -am "feat(httpapi): publish message_metadata and command_principal on /v1/capabilities"`

## Audit read

### Task 11: `AuditAuthorizer` and `GET /v1/sessions/{sid}/commands/{cid}`

**Files:** Modify `server.go` (public `AuditAuthorizer`), `exports.go` (`TenantAuthorizer`),
`internal/identity/authorize.go` (`AuthorizeAuditRead`), `internal/httpapi/deps.go`
(narrow `AuditAuthorizer`, `CommandReader`), `internal/httpapi/routes.go` (route, config),
`internal/httpapi/errors.go` (`command_not_found`), create `internal/httpapi/commands.go`;
Tests `internal/httpapi/commands_test.go`, `exports_test.go`.

**Step 1: Failing tests** (`internal/httpapi/commands_test.go`, reusing `routes_test.go`'s
router harness):

```go
func TestCommandStatusWithoutAnAuditAuthorizerOmitsTheMembers(t *testing.T) {
	rt := newCommandRouter(t, stampedEntry(), plainAuthorizer{}) // AuthorizeSessionRead only
	status, body := get(t, rt, "/v1/sessions/session-a/commands/input-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if bytes.Contains(body, []byte(`"principal"`)) || bytes.Contains(body, []byte(`"metadata"`)) {
		t.Fatalf("members disclosed without AuditAuthorizer: %s", body)
	}
	var cs sessionwire.CommandStatus
	if err := json.Unmarshal(body, &cs); err != nil || cs.CommandID != "input-1" {
		t.Fatalf("not a Core CommandStatus: %s (%v)", body, err)
	}
}

func TestCommandStatusWithAGrantingAuditAuthorizerCarriesThem(t *testing.T) {
	rt := newCommandRouter(t, stampedEntry(), auditAuthorizer{grant: true})
	_, body := get(t, rt, "/v1/sessions/session-a/commands/input-1")
	var got struct {
		Principal *sessionwire.Principal      `json:"principal"`
		Metadata  sessionwire.MessageMetadata `json:"metadata"`
	}
	if err := json.Unmarshal(body, &got); err != nil || got.Principal == nil || got.Principal.Subject != "actor-a" || got.Metadata["space"] != "family" {
		t.Fatalf("body = %s", body)
	}
}

func TestADenyingAuditAuthorizerOmitsThemAndStillAnswers(t *testing.T) {
	rt := newCommandRouter(t, stampedEntry(), auditAuthorizer{grant: false})
	status, body := get(t, rt, "/v1/sessions/session-a/commands/input-1")
	if status != http.StatusOK || bytes.Contains(body, []byte(`"principal"`)) {
		t.Fatalf("status %d body %s", status, body)
	}
}

func TestCommandStatusRefusals(t *testing.T) {
	rt := newCommandRouter(t, stampedEntry(), plainAuthorizer{})
	for _, row := range []struct {
		path string
		want int
	}{
		{"/v1/sessions/session-a/commands/" + strings.Repeat("x", 257), http.StatusBadRequest},
		{"/v1/sessions/session-a/commands/absent", http.StatusNotFound},
		{"/v1/sessions/session-other/commands/input-1", http.StatusNotFound},
	} {
		if status, _ := get(t, rt, row.path); status != row.want {
			t.Fatalf("%s: status %d, want %d", row.path, status, row.want)
		}
	}
}

func TestNoCommandReaderIsUnavailable(t *testing.T) {
	rt := newCommandRouter(t, nil, plainAuthorizer{}) // CommandReads nil
	if status, _ := get(t, rt, "/v1/sessions/session-a/commands/input-1"); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
}
```

In `exports_test.go`, assert
`var _ factory.AuditAuthorizer = factory.TenantAuthorizer{}`, and that a same-tenant principal
is granted and another tenant's is refused `ErrUnauthorized`. `TestTenantAuthorizerIsTheInternalAuthorizer`
must still pass, so add the method to both.

**Step 2:** fails.

**Step 3: Implement.**

Root (`server.go`, beside `Authorizer`):

```go
// AuditAuthorizer is an OPTIONAL authorization seam, discovered by assertion on
// the Authorizer a composition supplies (precedent: harness
// session.LeaseEpochReporter), so no existing Authorizer implementation breaks.
// It decides whether a principal may read a command's audit members -- the
// stamped principal and the client metadata -- on
// GET /v1/sessions/{sid}/commands/{cid}. Absent, or denying, the route answers
// the command's public status and omits both members; their existence is not
// disclosed. AuthorizeSessionRead alone NEVER unlocks custom metadata.
type AuditAuthorizer interface {
	AuthorizeAuditRead(ctx context.Context, principal identity.Principal, session sessionwire.SessionID) error
}
```

`internal/identity/authorize.go`:

```go
// AuthorizeAuditRead authorizes a command's audit members within the
// principal's tenant: tenant equality, as every other decision here.
func (Authorizer) AuthorizeAuditRead(_ context.Context, principal factoryidentity.Principal, _ sessionwire.SessionID) error {
	return authorizeTenantPrincipal(principal)
}
```

`exports.go`:

```go
var _ AuditAuthorizer = TenantAuthorizer{}

// AuthorizeAuditRead implements AuditAuthorizer as tenant equality.
func (TenantAuthorizer) AuthorizeAuditRead(ctx context.Context, principal identity.Principal, session sessionwire.SessionID) error {
	return internalidentity.Authorizer{}.AuthorizeAuditRead(ctx, principal, session)
}
```

`internal/httpapi/deps.go`: declare the narrow `AuditAuthorizer` (same method) and

```go
// CommandReader is the one durable command read the commands route makes. A
// *sessionstore.Store satisfies it.
type CommandReader interface {
	GetDispositionCommand(ctx context.Context, req sessionstore.GetDispositionCommandRequest) (sessionstore.DispositionInboxEntry, error)
}
```

`RouterConfig` gains `CommandReads CommandReader`, where nil is answered 503. In `server.go`
set it from `cfg.commands`. `TestPublicSeamsAreExactlyTheUnionOfTheirConsumers` may then
require `Commands` to include it, which it already does, so no public seam widens.

`errors.go`: `ErrorCodeCommandNotFound sessionwire.ErrorCode = "command_not_found"` and

```go
// commandNotFound is the ONE construction of a command read's 404. A command
// in another session, another tenant, or never admitted answers the same bytes.
func commandNotFound() apiError {
	return apiError{status: http.StatusNotFound, code: ErrorCodeCommandNotFound, message: "there is no such command"}
}
```

If a test pins the set of local codes, add it there.

`routes.go` route table, after the gates route:

```go
		{pattern: "/v1/sessions/{sid}/commands/{cid}",
			rules:   served(authSessionRead, func(rt *Router) http.Handler { return rt.serveCommandStatus() }),
			session: true},
```

Update `TestEveryRouteDeclaresWhatItsShapeRequires`'s independent restatement with the new row.

`internal/httpapi/commands.go`:

```go
// serveCommandStatus answers one admitted command's public status and, only
// for a principal an AuditAuthorizer grants, its audit members.
func (rt *Router) serveCommandStatus() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operation, authenticated := internalidentity.OperationContextFrom(r.Context())
		if !authenticated {
			writeAPIError(w, internalFailure())
			return
		}
		if rt.commandReads == nil {
			writeAPIError(w, controlUnavailable())
			return
		}
		command := sessionwire.CommandID(r.PathValue("cid"))
		if err := command.Validate(); err != nil {
			writeAPIError(w, refusedRequestError("the command identifier is not valid"))
			return
		}
		session := sessionwire.SessionID(r.PathValue("sid"))
		entry, err := rt.commandReads.GetDispositionCommand(r.Context(), sessionstore.GetDispositionCommandRequest{
			TenantID: operation.Principal.Tenant(), SessionID: session, CommandID: command,
		})
		if err != nil {
			writeAPIError(w, commandReadFailure(err))
			return
		}
		status, readable := command.StatusForDisposition(entry) // internal/command, aliased on import
		if !readable {
			writeAPIError(w, internalFailure())
			return
		}
		body, err := status.MarshalJSON()
		if err != nil {
			writeAPIError(w, internalFailure())
			return
		}
		if rt.auditGranted(r.Context(), operation.Principal, session) {
			body, err = withAuditMembers(body, entry.Record.Descriptor.Principal, entry.Record.Descriptor.Metadata)
			if err != nil {
				writeAPIError(w, internalFailure())
				return
			}
		}
		writeJSONBytes(w, http.StatusOK, body)
	})
}

// auditGranted asks the optional seam. A missing seam or ANY error is "no":
// a denial and an authorizer fault both withhold, because this read has a
// complete answer without the members.
func (rt *Router) auditGranted(ctx context.Context, principal identity.Principal, session sessionwire.SessionID) bool {
	audit, ok := rt.authorizer.(AuditAuthorizer)
	return ok && audit.AuthorizeAuditRead(ctx, principal, session) == nil
}

// withAuditMembers appends principal and metadata, when present, after Core's
// members of a marshalled CommandStatus, in a fixed order.
func withAuditMembers(body []byte, principal *sessionwire.Principal, metadata sessionwire.MessageMetadata) ([]byte, error) {
	if principal == nil && len(metadata) == 0 {
		return body, nil
	}
	if len(body) < 2 || body[len(body)-1] != '}' {
		return nil, errors.New("httpapi: a command status is not a JSON object")
	}
	out := append([]byte(nil), body[:len(body)-1]...)
	if principal != nil {
		encoded, err := json.Marshal(principal)
		if err != nil {
			return nil, err
		}
		out = append(append(out, `,"principal":`...), encoded...)
	}
	if len(metadata) > 0 {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		out = append(append(out, `,"metadata":`...), encoded...)
	}
	return append(out, '}'), nil
}

// commandReadFailure maps the store's answer: an absent command, or an absent
// session in any of the store's spellings, is the one 404; a draining store is
// 503; everything else is a fault.
func commandReadFailure(err error) apiError {
	var inbox *sessionstore.InboxError
	if errors.As(err, &inbox) && inbox.Code == sessionstore.InboxErrorNotFound || command.SessionAbsent(err) {
		return commandNotFound()
	}
	if failure, ok := storeUnavailable(err); ok {
		return failure
	}
	return internalFailure()
}
```

`internal/command` clashes with the local variable named `command`, so import it as
`internalcommand` in this file. Use this package's existing constructor for the 400 invalid
identifier (the one `invalidObjectRequest` or `invalidSessionID` is built from) instead of
`refusedRequestError`. A legacy-bound session's command read also answers 404: the store
refuses a disposition read on it, and that refusal is not absence, so decide it explicitly.
Add a row, and if the store reports `binding.protocol_mode` for it, map that to
`commandNotFound()` too.

**Step 4:** `GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./internal/httpapi ./internal/identity .` → `ok`.

**Step 5:** `git commit -am "feat(httpapi): GET /v1/sessions/{sid}/commands/{cid} with optional AuditAuthorizer"`

### Task 12: Docs

Update `README.md` with a "Principal stamping and message metadata (v0.12.0)" section that
covers:

- `WithPrincipalStamping` (opt-in, all kinds, needs a Verifier, which is always required).
- A client `principal` gets 400 `invalid_request` on REST and ClientLink.
- Metadata limits come from Core: 16 fields, 64-byte keys, 1024-byte values, 4096 bytes in
  total, strings only, and the `looprig` prefix is refused.
- A different-subject retry gets 409 `command_rejected`.
- An incapable resident owner gets **422** `runtime_unavailable` (`ErrMetadataUnsupported`),
  with nothing written. An owner that cannot be asked gets 503.
- Placement is capable-only (`NoCapacity` until a capable Host exists).
- The Start WARN.
- `/v1/capabilities` flags.
- The commands route and `AuditAuthorizer`.
- Operator rollout order: every Host to v0.11.0, then Factory, check the tokens, then
  stamping.
- **ONE-WAY:** once a stamped command is stored, no Factory or Host sharing the store may
  roll back below sessionstore v0.14.0.
- Enabling stamping makes an in-flight retry of an unstamped command `command_rejected`.

Add one line to `CLAUDE.md`: "Principal and metadata rules live in `internal/admission`
only; the capability predicate is `hostlink.PrincipalCapable`, asked through
`Pool.AcceptsCommandPrincipal`, and nothing else decides." Run
`GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run Documented .` (the boundary-rules doc guard). Commit:
`git commit -am "docs: principal stamping, metadata, audit route and rollout"`.

### Task 13: Full verification

```bash
cd /Users/ipotter/code/looprig/factory
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./...
GOWORK=off GOTOOLCHAIN=go1.26.8 make check
git diff --check
git status --short
```

Expected: all `ok`, and `make check` exit 0 (fmt-check, vet, staticcheck, gosec, vuln, test,
stress, fuzz, build). Only the untracked `go.work`/`go.work.sum` show in status. Run the
store-backed integration lane under its tag the way `service_store_integration_test.go`
declares it (for example `GOWORK=off GOTOOLCHAIN=go1.26.8 go test -tags integration -race ./internal/admission/...`).
This must run **after Task 0 Step 3**.

**apidiff (additions only):**

```bash
rm -rf /tmp/factory-v0.11.1 && git -C /Users/ipotter/code/looprig/factory worktree add /tmp/factory-v0.11.1 v0.11.1
(cd /tmp/factory-v0.11.1 && GOWORK=off GOTOOLCHAIN=go1.26.8 ~/go/bin/apidiff -m -w /tmp/factory-old.api github.com/looprig/factory)
(cd /Users/ipotter/code/looprig/factory && GOWORK=off GOTOOLCHAIN=go1.26.8 ~/go/bin/apidiff -m -w /tmp/factory-new.api github.com/looprig/factory)
~/go/bin/apidiff -m -incompatible /tmp/factory-old.api /tmp/factory-new.api
~/go/bin/apidiff -m /tmp/factory-old.api /tmp/factory-new.api
git -C /Users/ipotter/code/looprig/factory worktree remove /tmp/factory-v0.11.1
```

Expected: `-incompatible` is empty. The full report lists only additions:
`WithPrincipalStamping`, `AuditAuthorizer`, `TenantAuthorizer.AuthorizeAuditRead`,
`identity.ErrClientPrincipal`, `identity.ErrMetadataUnsupported`, `identity.PrincipalFromWire`,
and `identity.Principal.Wire`. Any change to an existing signature is a defect.

### Task 14: Release factory v0.12.0 — REQUIRES OWNER CONFIRMATION

Preconditions: core v0.12.0 and sessionstore v0.14.0 tags on their remotes; Tasks 0 Step 3
and 13 green on the committed tree. Factory can release in parallel with host v0.11.0, but
the operator rollout still upgrades Hosts first.

**REQUIRES OWNER CONFIRMATION** before each of:

```bash
cd /Users/ipotter/code/looprig/factory
git push origin main
git tag -a v0.12.0 -m "factory v0.12.0: principal stamping and message metadata

Accepts client metadata on create/input (Core limits). Refuses a
client-supplied principal on every kind (400 invalid_request,
identity.ErrClientPrincipal), with or without stamping. WithPrincipalStamping()
(opt-in) stamps the verified principal on every command; a retry of the same
command_id by a different subject is 409 command_rejected. A command carrying
either member is refused for a resident owner lacking
hostlink.attribution.principal (422 runtime_unavailable,
identity.ErrMetadataUnsupported, nothing written; 503 if the owner cannot be
asked) and is placed only on a capable Host. Start WARNs per registered Host
lacking the token while stamping. /v1/capabilities adds message_metadata and
command_principal. New GET /v1/sessions/{sid}/commands/{cid}; principal and
metadata only behind the optional AuditAuthorizer (TenantAuthorizer implements
it).

Pins core v0.12.0, sessionstore v0.14.0.

ONE-WAY: once a stamped command is stored, every Factory and Host sharing the
store must stay on sessionstore >= v0.14.0.

Rollout: upgrade every Host to host v0.11.0, then Factory, confirm each Host
advertises hostlink.attribution.principal, then enable WithPrincipalStamping."
git push origin v0.12.0
git ls-remote origin refs/heads/main refs/tags/v0.12.0
```

After the tag exists, the coordinator updates `repositories.mk`, `go.work` and `AGENTS.md` in
the outer workspace and marks row 07 in the master plan and step 4b in the design's §8 as
released. The outer repository is not committed by the agent.
