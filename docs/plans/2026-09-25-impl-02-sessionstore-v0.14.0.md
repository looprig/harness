# sessionstore v0.14.0: command principal and message metadata — implementation plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Let a disposition command carry the Factory-stamped `Principal` (on every
command kind) and the client's `MessageMetadata` (on `create`/`input` only) as recorded,
caller-asserted columns of the immutable command descriptor. An unattributed command's
stored bytes must stay byte-identical to v0.13.1.

**Architecture:** Two optional members are added to `DispositionCommandDescriptor`, to
its private wire DTO, and to `AdmitDispositionCommandRequest` / `AdmitPublicCreateRequest`.
The inbox record is written at **v2 when both members are absent** (so the bytes are
unchanged) and at **v3 only when either is present**. The reader accepts `{2, 3}` and
refuses `4` with `InboxErrorVersion`. It also refuses, as `InboxErrorMalformed`, a record
whose version disagrees with its members. Validation checks shape only: the store proves
nothing about who the principal is. An admission retry compares both members **by value**,
because a by-reference payload (`PayloadObject`) could otherwise let two descriptors with
different columns share one object digest. `ListSessionDispositionCommands` fails closed on
a v4 row.

**Tech Stack:** Go 1.26.8 (`GOTOOLCHAIN=go1.26.8 GOWORK=off`), `github.com/looprig/core`
`sessionwire/v1` (v0.12.0: `Principal`, `PrincipalKind*`, `SubjectID`, `MessageMetadata`),
`github.com/looprig/storage` v0.7.0, and `storage/memstore` in tests.

**Binding sources:**
- `harness/docs/plans/2026-09-25-message-principal-metadata-presenter-design.md`: §1.2,
  §2 (row "sessionstore disposition inbox row"), §6 step 2, and owner rulings 3, 9 and 10.
- `harness/docs/plans/2026-09-25-impl-00-master-plan.md`: rules for every plan.
- `/Users/ipotter/code/looprig/AGENTS.md`: release rules and the sessionstore
  field-authority rule. A value the store merely records may be caller-asserted.
- `sessionstore/CLAUDE.md`: production imports are limited to the stdlib, core and
  storage. No `replace` directives and no vendoring.

**Repository:** `/Users/ipotter/code/looprig/sessionstore`, local `main`. At plan time the
checkout is at `02c6418` (v0.13.1 + 1 README commit) and pins `core v0.11.0` and
`storage v0.7.0`. All line numbers below are from that tree.

**Command convention.** Every command is written `GOWORK=off GOTOOLCHAIN=go1.26.8 ...`.
If Task 0 takes **branch B** (core v0.12.0 not yet published), replace `GOWORK=off` with
`GOWORK=/tmp/sessionstore-core012.work` in Tasks 1–10. Task 11 runs under `GOWORK=off`
only, and only after the core bump.

**Commit convention:** conventional messages, **no Co-Authored-By trailer**. Commit
reviewed files only. Before every commit, run `git diff --check` and inspect
`git status --short`.

---

## Task 0: Preconditions and the core dependency (step 0)

The new types live in Core, so the first step is always to bump core to v0.12.0. It can
only happen once that tag exists on the remote.

**Step 1: Check the working tree.**

```bash
cd /Users/ipotter/code/looprig/sessionstore
git status --short          # expected: empty
git log --oneline -1        # expected: 02c6418 (or a later reviewed main commit)
git ls-remote --tags git@github.com:looprig/core.git 'refs/tags/v0.12.0*'
```

**Step 2A (branch A: core v0.12.0 IS published).**

```bash
cd /Users/ipotter/code/looprig/sessionstore
GOWORK=off GOTOOLCHAIN=go1.26.8 go get github.com/looprig/core@v0.12.0
GOWORK=off GOTOOLCHAIN=go1.26.8 go mod tidy
git diff go.mod             # expected: only core v0.11.0 -> v0.12.0
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./...
```

Expected: PASS, because core v0.12.0 is additive. Commit:

```bash
git add go.mod go.sum
git commit -m "chore(deps): adopt core v0.12.0"
```

**Step 2B (branch B: core v0.12.0 is NOT yet published).** Develop against the local core
checkout (`/Users/ipotter/code/looprig/core`, carrying impl-01's work) through a temporary
workspace file outside the repository:

```bash
cat > /tmp/sessionstore-core012.work <<'EOF'
go 1.26.8

use (
	/Users/ipotter/code/looprig/sessionstore
	/Users/ipotter/code/looprig/core
)
EOF
```

Rules for branch B:
- **Never add a `replace` directive.**
- **Never create or commit a `go.work` inside `sessionstore`.** The file lives in `/tmp`
  so it cannot be staged.
- `go.mod` keeps `core v0.11.0` until Task 11.
- Task commits made under branch B do not build under `GOWORK=off`. They stay local and
  unpushed until Task 11 bumps core and verifies standalone.

**Step 3: Confirm that the Core API this plan names exists** (in whichever core is in use):

```bash
grep -n "type Principal struct\|PrincipalKindActor\|PrincipalKindService\|type MessageMetadata\|func (p Principal) Validate\|func (m MessageMetadata) Validate" \
  /Users/ipotter/code/looprig/core/sessionwire/v1/*.go
```

Expected: all six match. This plan also relies on:
- `Principal`'s JSON member order `tenant, subject, kind`;
- its strict `UnmarshalJSON`;
- `Principal` being comparable (three string-typed fields);
- metadata key grammar `^[a-z][a-z0-9_]{0,63}$`, with the `looprig` prefix refused.

If any name or behaviour differs from design §1.1, stop and reconcile with impl-01
before continuing. Do not guess.

---

## Task 1: Declare the members on the descriptor, the requests and the wire DTO

**Files:**
- Create: `sessionstore/disposition_attribution_test.go`
- Modify: `sessionstore/wire_dto_test.go` (the `fillProbeMembers` switch, currently lines 111–145)
- Modify: `sessionstore/disposition_inbox.go`: the descriptor (40–57), the request (98–109),
  the descriptor DTO (375–387) and both conversions (401–413, 465–477)
- Modify: `sessionstore/public_create.go` (71–75)

**Step 1: Write the failing test.** Create `disposition_attribution_test.go`:

```go
package sessionstore

import (
	"reflect"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
)

// testPrincipal is a Factory-stamped actor in the fixture's own tenant.
func testPrincipal() *sessionwire.Principal {
	return &sessionwire.Principal{Tenant: catalogTenant, Subject: "user/alex", Kind: sessionwire.PrincipalKindActor}
}

// testMetadata is a client bag with two keys, so map ordering is exercised.
func testMetadata() sessionwire.MessageMetadata {
	return sessionwire.MessageMetadata{"space": "family", "client": "oxy-ios"}
}

// Principal and Metadata are declared, with Core's types, on the descriptor and on
// both admission requests that build one.
func TestDispositionAttributionIsDeclared(t *testing.T) {
	t.Parallel()
	want := map[string]reflect.Type{
		"Principal": reflect.TypeOf((*sessionwire.Principal)(nil)),
		"Metadata":  reflect.TypeOf(sessionwire.MessageMetadata(nil)),
	}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(DispositionCommandDescriptor{}),
		reflect.TypeOf(AdmitDispositionCommandRequest{}),
		reflect.TypeOf(AdmitPublicCreateRequest{}),
	} {
		for name, wantType := range want {
			field, ok := typ.FieldByName(name)
			if !ok || field.Type != wantType {
				t.Errorf("%s.%s: present %v type %v, want %v", typ.Name(), name, ok, field.Type, wantType)
			}
		}
	}
}
```

Also extend `fillProbeMembers` in `wire_dto_test.go`. Insert this case directly after the
`case value.Kind() == reflect.Slice:` arm, so the conversion probe can fill a map member:

```go
	case value.Kind() == reflect.Map:
		filled := reflect.MakeMapWithSize(value.Type(), 1)
		key := reflect.New(value.Type().Key()).Elem()
		fillProbeMembers(t, key, path+"{key}")
		elem := reflect.New(value.Type().Elem()).Elem()
		fillProbeMembers(t, elem, path+"{value}")
		filled.SetMapIndex(key, elem)
		value.Set(filled)
```

**Step 2: Run it and watch it fail.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionAttributionIsDeclared' .
```

Expected: FAIL with six `present false` errors.

**Step 3: Add the exported members.** In `disposition_inbox.go`, append to
`DispositionCommandDescriptor` (after `PayloadObject`, line 56):

```go
	// Principal is the command's sender as its admitting caller (a Factory)
	// recorded it. It is a caller-asserted value this store merely records: the
	// store validates its shape and proves nothing about who it names. Permitted
	// on every kind. Nil means absent, and an unattributed record is stored at
	// DispositionInboxRecordVersion with bytes identical to v0.13.1's.
	Principal *sessionwire.Principal `json:"principal,omitempty"`
	// Metadata is the client's app-defined bag. It is permitted only on the
	// message kinds "create" and "input", and is shape-validated by Core.
	// Empty is normalized to nil (absent).
	Metadata sessionwire.MessageMetadata `json:"metadata,omitempty"`
```

Append to `AdmitDispositionCommandRequest` (after `ApplyDeadline`, line 108):

```go
	// Principal and Metadata become the descriptor's columns. A retry compares
	// them by value, so the same CommandID admitted with a different principal
	// or metadata is InboxErrorCommandMismatch even when the payload is equal.
	Principal *sessionwire.Principal
	Metadata  sessionwire.MessageMetadata
```

Append to `AdmitPublicCreateRequest` in `public_create.go` (after `PayloadObject`, line 74):

```go
	// Principal and Metadata are carried onto the inbox descriptor. They are NOT
	// part of the reservation identity (whose record version is unchanged): a
	// caller whose payload canonically includes them, as Factory's does, binds
	// them through PayloadDigest, and the inbox compares them by value on retry.
	Principal *sessionwire.Principal
	Metadata  sessionwire.MessageMetadata
```

**Step 4: Run the mirror guard and watch it fail.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionAttributionIsDeclared|TestWireDTOsMirrorExportedRecords' .
```

Expected: the first test PASSES. `TestWireDTOsMirrorExportedRecords/DispositionCommandDescriptor`
FAILS: the exported record has `Metadata` and `Principal` but the DTO does not. That shows
the guard works.

**Step 5: Add the durable members to the DTO.** Append to `dispositionDescriptorWire`
(after `PayloadObject`, line 386). Order matters, because it is the stored member order:

```go
	Principal        *sessionwire.Principal      `json:"principal,omitempty"`
	Metadata         sessionwire.MessageMetadata `json:"metadata,omitempty"`
```

Also extend the DTO comment above `dispositionInboxRecordWire` (lines 327–334) with this
sentence: "principal and metadata are omitempty, and their presence selects record v3
(see dispositionInboxRecordVersionFor)."

**Step 6: Run the conversion guard and watch it fail.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestWireDTOsMirrorExportedRecords|TestWireConversionsCarryEveryMember' .
```

Expected: the mirror test PASSES. `TestWireConversionsCarryEveryMember/DispositionInboxRecord`
FAILS with "conversion dropped or altered a member", because neither conversion carries
the members yet.

**Step 7: Carry the members in both conversions.** In `dispositionInboxToWire`, the
descriptor literal (lines 405–409) becomes:

```go
		Descriptor: dispositionDescriptorWire{
			PublicCreate: d.PublicCreate, TenantID: d.TenantID, SessionID: d.SessionID, CommandID: d.CommandID,
			Binding: d.Binding, RuntimeCommandID: d.RuntimeCommandID, Kind: d.Kind,
			PayloadDigest: d.PayloadDigest, PayloadSize: d.PayloadSize, Payload: d.Payload, PayloadObject: d.PayloadObject,
			Principal: d.Principal, Metadata: d.Metadata,
		},
```

In `record()` (lines 469–473):

```go
		Descriptor: DispositionCommandDescriptor{
			PublicCreate: d.PublicCreate, TenantID: d.TenantID, SessionID: d.SessionID, CommandID: d.CommandID,
			Binding: d.Binding, RuntimeCommandID: d.RuntimeCommandID, Kind: d.Kind,
			PayloadDigest: d.PayloadDigest, PayloadSize: d.PayloadSize, Payload: d.Payload, PayloadObject: d.PayloadObject,
			Principal: d.Principal, Metadata: d.Metadata,
		},
```

**Step 8: Run the tests and watch them pass.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionAttributionIsDeclared|TestWireDTOsMirrorExportedRecords|TestWireConversionsCarryEveryMember|TestDispositionInboxWireGolden' .
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./...
```

Expected: PASS. The unchanged v2 golden still passes, because both members are omitempty
and nil.

**Step 9: Commit.**

```bash
git add disposition_attribution_test.go wire_dto_test.go disposition_inbox.go public_create.go
git commit -m "feat(disposition): declare principal and metadata on the command descriptor"
```

---

## Task 2: Validate the shape at admission (metadata refused on non-message kinds)

**Files:**
- Modify: `sessionstore/disposition_inbox.go`: imports (lines 3–14), `dispositionDescriptor`
  (153–169), `canonicalDispositionInboxRecord` (513–555)
- Modify: `sessionstore/disposition_attribution_test.go`

**Step 1: Write the failing tests.** Add `"context"`, `"maps"` and `"reflect"` to the test
file's imports, then append:

```go
func TestDispositionAttributionShape(t *testing.T) {
	t.Parallel()
	principal := func(r *AdmitDispositionCommandRequest) { r.Principal = testPrincipal() }
	metadata := func(r *AdmitDispositionCommandRequest) { r.Metadata = testMetadata() }
	both := func(r *AdmitDispositionCommandRequest) { principal(r); metadata(r) }
	for _, tc := range []struct {
		name  string
		kind  CommandKind
		set   func(*AdmitDispositionCommandRequest)
		field string // "" means accepted
	}{
		{"principal on create", "create", principal, ""},
		{"principal on input", "input", principal, ""},
		{"principal on interrupt", "interrupt", principal, ""},
		{"principal on restore", "restore", principal, ""},
		{"principal on gate_response", "gate_response", principal, ""},
		{"principal on an opaque kind", "Kind/A:B", principal, ""},
		{"metadata on create", "create", metadata, ""},
		{"metadata on input", "input", metadata, ""},
		{"both on create", "create", both, ""},
		{"both on input", "input", both, ""},
		{"metadata on interrupt", "interrupt", metadata, "metadata"},
		{"metadata on restore", "restore", metadata, "metadata"},
		{"metadata on gate_response", "gate_response", metadata, "metadata"},
		{"metadata on an opaque kind", "Kind/A:B", metadata, "metadata"},
		{"both on interrupt", "interrupt", both, "metadata"},
		{"principal with an unknown kind", "input", func(r *AdmitDispositionCommandRequest) {
			p := testPrincipal()
			p.Kind = "admin"
			r.Principal = p
		}, "principal"},
		{"principal without a subject", "interrupt", func(r *AdmitDispositionCommandRequest) {
			p := testPrincipal()
			p.Subject = ""
			r.Principal = p
		}, "principal"},
		{"principal without a tenant", "restore", func(r *AdmitDispositionCommandRequest) {
			p := testPrincipal()
			p.Tenant = ""
			r.Principal = p
		}, "principal"},
		{"metadata with an invalid key", "input", func(r *AdmitDispositionCommandRequest) {
			r.Metadata = sessionwire.MessageMetadata{"Space": "family"}
		}, "metadata"},
		{"metadata with a reserved key", "create", func(r *AdmitDispositionCommandRequest) {
			r.Metadata = sessionwire.MessageMetadata{"looprig_x": "v"}
		}, "metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openTestStore(t)
			createDispositionCatalog(t, s)
			req := dispositionRequest()
			req.Kind = tc.kind
			tc.set(&req)
			entry, created, err := s.AdmitDispositionCommand(ctx, req)
			get := GetDispositionCommandRequest{TenantID: req.TenantID, SessionID: req.SessionID, CommandID: req.CommandID}
			if tc.field != "" {
				if got := assertInboxCode(t, err, InboxErrorInvalid); got.Field != tc.field {
					t.Fatalf("field = %q, want %q", got.Field, tc.field)
				}
				if created {
					t.Fatal("a refused command reported created")
				}
				_, err := s.GetDispositionCommand(ctx, get)
				assertInboxCode(t, err, InboxErrorNotFound)
				return
			}
			if err != nil || !created {
				t.Fatalf("admit: %v created=%v", err, created)
			}
			d := entry.Record.Descriptor
			if !reflect.DeepEqual(d.Principal, req.Principal) || !maps.Equal(d.Metadata, req.Metadata) {
				t.Fatalf("descriptor attribution = %+v %v, want %+v %v", d.Principal, d.Metadata, req.Principal, req.Metadata)
			}
			read, err := s.GetDispositionCommand(ctx, get)
			if err != nil || !reflect.DeepEqual(read, entry) {
				t.Fatalf("read back: %+v %v; want %+v", read, err, entry)
			}
		})
	}
}

// The store keeps its own copy, and treats an empty bag as absent on every kind.
func TestDispositionAttributionIsCopiedAndNormalized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	createDispositionCatalog(t, s)

	req := dispositionRequest()
	req.Principal, req.Metadata = testPrincipal(), testMetadata()
	entry, _, err := s.AdmitDispositionCommand(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Principal.Subject = "user/mallory"
	req.Metadata["space"] = "work"
	if entry.Record.Descriptor.Principal.Subject != "user/alex" || entry.Record.Descriptor.Metadata["space"] != "family" {
		t.Fatalf("returned descriptor aliases the request: %+v", entry.Record.Descriptor)
	}

	empty := dispositionRequest()
	empty.CommandID, empty.Kind, empty.Metadata = "public/command:empty", "interrupt", sessionwire.MessageMetadata{}
	got, _, err := s.AdmitDispositionCommand(ctx, empty)
	if err != nil {
		t.Fatalf("an empty bag on a non-message kind is absent, not refused: %v", err)
	}
	if got.Record.Descriptor.Metadata != nil {
		t.Fatalf("empty metadata stored as %#v, want nil", got.Record.Descriptor.Metadata)
	}
}
```

**Step 2: Run them and watch them fail.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionAttributionShape|TestDispositionAttributionIsCopiedAndNormalized' .
```

Expected: FAIL. The accepted cases report nil attribution, because the request is not
wired yet. The refusal cases report "error = <nil>, want *InboxError". The copy test
panics on a nil `Principal` or reports aliasing.

**Step 3: Implement.** Add `"maps"` to `disposition_inbox.go`'s imports. Append after
`dispositionDescriptor`:

```go
// The message kinds are the only kinds with a message a presenter could frame,
// so they are the only kinds that may carry client metadata. CommandKind stays
// opaque everywhere else. This is a positive list for ONE member, not a closed
// kind set: an unknown kind is still admitted, just without metadata.
const (
	commandKindCreate CommandKind = "create"
	commandKindInput  CommandKind = "input"
)

func commandKindCarriesMessage(kind CommandKind) bool {
	return kind == commandKindCreate || kind == commandKindInput
}

// validateDispositionAttribution holds the descriptor's two recorded,
// caller-asserted members to their SHAPE and nothing more. The store proves
// nothing about who the principal names (the workspace rule: a value the store
// merely records may be caller-asserted, like PlacementTermination.Kind). It
// also canonicalizes: a copied principal, a cloned map, and empty metadata as nil,
// so a validated descriptor never aliases its caller and has one spelling.
// Admission (before any I/O) and the record codec (encode and decode) both call it.
func validateDispositionAttribution(d *DispositionCommandDescriptor) error {
	if d.Principal != nil {
		if err := d.Principal.Validate(); err != nil {
			return inboxInvalid("principal", err)
		}
		principal := *d.Principal
		d.Principal = &principal
	}
	if len(d.Metadata) == 0 {
		d.Metadata = nil
		return nil
	}
	if !commandKindCarriesMessage(d.Kind) {
		return inboxInvalid("metadata", nil)
	}
	if err := d.Metadata.Validate(); err != nil {
		return inboxInvalid("metadata", err)
	}
	d.Metadata = maps.Clone(d.Metadata)
	return nil
}
```

Replace `dispositionDescriptor` (lines 153–169) with:

```go
func dispositionDescriptor(req AdmitDispositionCommandRequest) (DispositionCommandDescriptor, error) {
	d := DispositionCommandDescriptor{TenantID: req.TenantID, SessionID: req.SessionID, CommandID: req.CommandID, Binding: req.Binding, RuntimeCommandID: req.ProposedRuntimeCommandID, Kind: req.Kind, Payload: req.Payload, PayloadObject: req.PayloadObject, Principal: req.Principal, Metadata: req.Metadata}
	if err := validateDispositionAttribution(&d); err != nil {
		return DispositionCommandDescriptor{}, err
	}
	if d.PayloadObject == nil {
		digest := sha256.Sum256(d.Payload)
		d.PayloadDigest, d.PayloadSize = hex.EncodeToString(digest[:]), uint64(len(d.Payload))
	} else {
		parsed, err := parseObjectMetadata(*d.PayloadObject)
		if err != nil {
			return DispositionCommandDescriptor{}, err
		}
		if parsed.kind != ObjectKindCommandPayload || !d.PayloadObject.CreatedAt.IsZero() {
			return DispositionCommandDescriptor{}, inboxInvalid("payload_object", nil)
		}
		d.PayloadDigest, d.PayloadSize = hex.EncodeToString(parsed.digest[:]), d.PayloadObject.SizeBytes
	}
	return d, nil
}
```

In `canonicalDispositionInboxRecord`, insert the following immediately before
`r.AcceptedAt, r.ApplyDeadline = base.AcceptedAt, base.ApplyDeadline` (line 553):

```go
	if err := validateDispositionAttribution(d); err != nil {
		return DispositionInboxRecord{}, err
	}
```

**Step 4: Run the tests and watch them pass.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionAttribution' .
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./...
```

Expected: PASS. (Attributed rows are still written at v2 in this task. Task 3 moves them to v3.)

**Step 5: Mutation probes.** Revert each after it has been seen to fail:
- Delete the `commandKindCarriesMessage` check. The four "metadata on …" non-message cases must FAIL.
- Delete `d.Metadata = nil` in the empty branch. `TestDispositionAttributionIsCopiedAndNormalized` must FAIL.
- Delete `d.Metadata = maps.Clone(...)`. The alias assertion must FAIL.

**Step 6: Commit.**

```bash
git add disposition_inbox.go disposition_attribution_test.go
git commit -m "feat(disposition): validate command principal and metadata shape at admission"
```

---

## Task 3: Write v3 only when attributed; read {2, 3}

**Files:**
- Modify: `sessionstore/disposition_inbox.go`: the constant (16–24), `encodeDispositionInboxRecord`
  (479–492) and `decodeDispositionInboxRecord` (494–511)
- Modify: `sessionstore/catalog.go`: `decodeVersionedRecord` (1100–1133). `slices` is
  already imported at line 10.
- Modify: `sessionstore/disposition_attribution_test.go`

**Step 1: Write the failing tests.** Add `"bytes"` to the test imports and append:

```go
// These literals pin v3's stored bytes independently of the structs and DTOs. The
// unattributed rows are the v2 control: they must equal what v0.13.1 wrote, which
// the unchanged TestDispositionInboxWireGolden pins as well.
func TestDispositionInboxWireGoldenAttributed(t *testing.T) {
	t.Parallel()
	const identityHead = `"tenant_id":"Tenant/A:B","session_id":"Session/A:B","command_id":"Create/A:B","binding":{"storage_binding_id":"agent-pool/east","binding_version":"config-2026-09","runtime_session_id":"runtime/session-a","protocol_mode":"disposition"},"runtime_command_id":"2f1c7d1e-0f3a-4c5b-9f21-000000000001","kind":"`
	const payload = `","payload_digest":"47ffa3ea45a70b8a41c2c0825df323c00a8b7a01c1ea06083cc41dddcc001123","payload_size":3,"payload":"AP8B"`
	const principalWire = `,"principal":{"tenant":"Tenant/A:B","subject":"user/A:B","kind":"actor"}`
	const metadataWire = `,"metadata":{"client":"oxy-ios","space":"family"}`
	const tail = `},"accepted_at":"2026-08-30T11:30:00Z","apply_deadline":"2026-08-30T12:30:00Z","state":"pending"}`
	const v2, v3 = `{"record_version":2,"descriptor":{`, `{"record_version":3,"descriptor":{`
	principal := &sessionwire.Principal{Tenant: "Tenant/A:B", Subject: "user/A:B", Kind: sessionwire.PrincipalKindActor}
	metadata := sessionwire.MessageMetadata{"space": "family", "client": "oxy-ios"}
	for _, tc := range []struct {
		name      string
		kind      CommandKind
		principal *sessionwire.Principal
		metadata  sessionwire.MessageMetadata
		want      string
	}{
		{"principal and metadata on input", "input", principal, metadata, v3 + identityHead + "input" + payload + principalWire + metadataWire + tail},
		{"principal only on interrupt", "interrupt", principal, nil, v3 + identityHead + "interrupt" + payload + principalWire + tail},
		{"principal only on gate_response", "gate_response", principal, nil, v3 + identityHead + "gate_response" + payload + principalWire + tail},
		{"metadata only on create", "create", nil, metadata, v3 + identityHead + "create" + payload + metadataWire + tail},
		{"neither stays v2", "input", nil, nil, v2 + identityHead + "input" + payload + tail},
		{"empty metadata stays v2", "input", nil, sessionwire.MessageMetadata{}, v2 + identityHead + "input" + payload + tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := DispositionCommandDescriptor{
				TenantID: "Tenant/A:B", SessionID: "Session/A:B", CommandID: "Create/A:B",
				Binding: testSessionBinding(), RuntimeCommandID: inboxRuntime, Kind: tc.kind,
				Payload: []byte{0, 255, 1}, PayloadDigest: "47ffa3ea45a70b8a41c2c0825df323c00a8b7a01c1ea06083cc41dddcc001123", PayloadSize: 3,
				Principal: tc.principal, Metadata: tc.metadata,
			}
			r := DispositionInboxRecord{Descriptor: d, AcceptedAt: inboxAcceptedAt, ApplyDeadline: inboxDeadline, State: InboxStatePending}
			got, canonical, err := encodeDispositionInboxRecord(r)
			if err != nil || string(got) != tc.want {
				t.Fatalf("encode:\n got %s %v\nwant %s", got, err, tc.want)
			}
			decoded, err := decodeDispositionInboxRecord([]byte(tc.want))
			if err != nil || !reflect.DeepEqual(decoded, canonical) {
				t.Fatalf("decode: %+v %v; want %+v", decoded, err, canonical)
			}
		})
	}
}

func rawDispositionRow(t *testing.T, s *Store, command sessionwire.CommandID) []byte {
	t.Helper()
	scope, err := s.deriveSessionScope(catalogTenant, catalogSession)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.backend.OrderedIndex.Get(context.Background(), dispositionInboxID(scope, command))
	if err != nil {
		t.Fatal(err)
	}
	return stored.Value
}

// Through the real admission path, an unattributed command is stored at v2 with
// no new member names, and an attributed one at v3.
func TestDispositionAdmissionSelectsRecordVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	createDispositionCatalog(t, s)

	plain := dispositionRequest()
	if _, _, err := s.AdmitDispositionCommand(ctx, plain); err != nil {
		t.Fatal(err)
	}
	raw := rawDispositionRow(t, s, plain.CommandID)
	if !bytes.HasPrefix(raw, []byte(`{"record_version":2,`)) || bytes.Contains(raw, []byte(`"principal"`)) || bytes.Contains(raw, []byte(`"metadata"`)) {
		t.Fatalf("unattributed row is not v0.13.1's shape: %s", raw)
	}

	attributed := dispositionRequest()
	attributed.CommandID, attributed.Principal = "public/command:2", testPrincipal()
	if _, _, err := s.AdmitDispositionCommand(ctx, attributed); err != nil {
		t.Fatal(err)
	}
	if raw := rawDispositionRow(t, s, attributed.CommandID); !bytes.HasPrefix(raw, []byte(`{"record_version":3,`)) {
		t.Fatalf("attributed row is not v3: %s", raw)
	}
}
```

**Step 2: Run them and watch them fail.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionInboxWireGoldenAttributed|TestDispositionAdmissionSelectsRecordVersion' .
```

Expected: FAIL. The four attributed golden cases got `{"record_version":2,…` where v3 was
wanted, and the store test reports "attributed row is not v3". The two v2 cases pass.

**Step 3: Implement the version selection.** Replace the constant block at lines 16–24 of
`disposition_inbox.go` with:

```go
// DispositionInboxRecordVersion identifies the disposition inbox codec. Legacy
// v1 records retain their original codec and PayloadRef equality. This version
// carries the claim, attempt and outcome of the settlement protocol as members
// absent until the state that requires them, so a pending record's bytes are
// exactly what they were before those states existed. It is a record format and
// not an authority: it supplies no claim, dispatch or settlement permission,
// and a further durable member still requires a version bump because decoding
// demands exact canonical re-encoding.
//
// It remains the version of every UNATTRIBUTED record (no principal, no
// metadata), so those bytes are identical to the ones v0.13.1 writes and every
// released reader keeps reading them.
const DispositionInboxRecordVersion uint8 = 2

// DispositionInboxRecordVersionAttributed is the version of a record whose
// descriptor carries a Principal or Metadata. It is written only then. A
// sessionstore ≤ v0.13.1 refuses it as InboxErrorVersion ("upgrade"), not as
// malformed, and that is the one-way rule: once any v3 row is stored, every
// process that reads the store must be on sessionstore ≥ v0.14.0.
const DispositionInboxRecordVersionAttributed uint8 = 3

// dispositionInboxRecordVersionFor derives the record version from the canonical
// descriptor. The version is a function of the content, never a caller choice,
// so one record has exactly one spelling.
func dispositionInboxRecordVersionFor(d DispositionCommandDescriptor) uint8 {
	if d.Principal != nil || len(d.Metadata) != 0 {
		return DispositionInboxRecordVersionAttributed
	}
	return DispositionInboxRecordVersion
}
```

In `encodeDispositionInboxRecord` (line 484), change the marshal line to:

```go
	value, err := json.Marshal(dispositionInboxWire{RecordVersion: dispositionInboxRecordVersionFor(r.Descriptor), dispositionInboxRecordWire: dispositionInboxToWire(r)})
```

**Step 4: Let the shared decoder accept a set of versions.** In `catalog.go`, replace
`decodeVersionedRecord` (lines 1100–1133) with the two functions below. Keep the existing
doc comment above the first one, and do not change the other callers.

```go
func decodeVersionedRecord[T any](
	value []byte,
	maxBytes int,
	wantVersion uint8,
	fields versionedRecordFields,
	fail func(failure versionedRecordFailure, field string, cause error) error,
) (T, error) {
	return decodeVersionedRecordOf[T](value, maxBytes, []uint8{wantVersion}, fields, fail)
}

// decodeVersionedRecordOf is decodeVersionedRecord for a record that has more
// than one current codec version. The disposition inbox is the only one: it
// writes v2 for an unattributed command and v3 for an attributed one. Every rule
// above holds unchanged. A version outside the set is a version failure, and an
// empty set accepts nothing. Which version a record SHOULD carry is the record's
// own decoder's check, not this one's.
func decodeVersionedRecordOf[T any](
	value []byte,
	maxBytes int,
	versions []uint8,
	fields versionedRecordFields,
	fail func(failure versionedRecordFailure, field string, cause error) error,
) (T, error) {
	var wire T
	if len(value) > maxBytes {
		return wire, fail(versionedRecordTooLarge, fields.Record, nil)
	}
	if len(value) == 0 {
		return wire, fail(versionedRecordMalformed, fields.Record, nil)
	}
	var probe struct {
		RecordVersion uint8 `json:"record_version"`
	}
	if err := json.Unmarshal(value, &probe); err != nil {
		return wire, fail(versionedRecordMalformed, fields.Record, err)
	}
	if !slices.Contains(versions, probe.RecordVersion) {
		return wire, fail(versionedRecordVersion, fields.Version, nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		// Deliberately the zero value rather than wire: a failed Decode may
		// have populated some members before it stopped, and handing a caller a
		// half-decoded record beside an error is how one of them ends up used.
		var zero T
		return zero, fail(versionedRecordMalformed, fields.Record, err)
	}
	return wire, nil
}
```

In `decodeDispositionInboxRecord` (line 495), change the decode call to:

```go
	wire, err := decodeVersionedRecordOf[dispositionInboxWire](value, MaxInboxRecordBytes, []uint8{DispositionInboxRecordVersion, DispositionInboxRecordVersionAttributed}, versionedRecordFields{Record: "record", Version: "record_version"}, inboxRecordFailure)
```

Update that function's comment ("V2 is a canonical stored codec…") to say "V2 and V3 are
canonical stored codecs…".

**Step 5: Run the tests and watch them pass.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionInboxWireGolden|TestDispositionAdmissionSelectsRecordVersion|TestDispositionAttribution' .
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./...
```

Expected: PASS. That includes the **unchanged** `TestDispositionInboxWireGolden` (proof
that v2 bytes are identical) and `TestDispositionCodecRejectsCorruptionAndLegacy`. Its
"future version" case (2→3 on an unattributed row) is still refused, because the
canonical re-encode is v2. Task 4 makes that refusal explicit.

**Step 6: Commit.**

```bash
git add disposition_inbox.go catalog.go disposition_attribution_test.go
git commit -m "feat(disposition): write inbox record v3 only for attributed commands"
```

---

## Task 4: Refuse v4 as a version failure, and refuse a version that disagrees with the members

**Files:**
- Modify: `sessionstore/disposition_inbox.go` (`decodeDispositionInboxRecord`)
- Modify: `sessionstore/disposition_codec_test.go` (lines 30–33)
- Modify: `sessionstore/disposition_attribution_test.go`

**Step 1: Write the failing test.** Append:

```go
func TestDispositionCodecVersionAgreesWithAttribution(t *testing.T) {
	t.Parallel()
	req := dispositionRequest()
	descriptor, err := dispositionDescriptor(req)
	if err != nil {
		t.Fatal(err)
	}
	base := DispositionInboxRecord{Descriptor: descriptor, AcceptedAt: req.AcceptedAt, ApplyDeadline: req.ApplyDeadline, State: InboxStatePending}
	plain, _, err := encodeDispositionInboxRecord(base)
	if err != nil {
		t.Fatal(err)
	}
	attributedRecord := base
	attributedRecord.Descriptor.Principal, attributedRecord.Descriptor.Metadata = testPrincipal(), testMetadata()
	attributed, _, err := encodeDispositionInboxRecord(attributedRecord)
	if err != nil {
		t.Fatal(err)
	}
	principalOnly := base
	principalOnly.Descriptor.Principal = testPrincipal()
	principalRow, _, err := encodeDispositionInboxRecord(principalOnly)
	if err != nil {
		t.Fatal(err)
	}
	replace := func(v []byte, old, new string) []byte {
		out := bytes.Replace(bytes.Clone(v), []byte(old), []byte(new), 1)
		if bytes.Equal(out, v) {
			t.Fatalf("vacuous mutation: %q not found in %s", old, v)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		value []byte
		code  InboxErrorCode
		field string
	}{
		{"v4 of an unattributed row", replace(plain, `"record_version":2`, `"record_version":4`), InboxErrorVersion, "record_version"},
		{"v4 of an attributed row", replace(attributed, `"record_version":3`, `"record_version":4`), InboxErrorVersion, "record_version"},
		{"v3 with neither member", replace(plain, `"record_version":2`, `"record_version":3`), InboxErrorMalformed, "record_version"},
		{"v2 carrying attribution", replace(attributed, `"record_version":3`, `"record_version":2`), InboxErrorMalformed, "record_version"},
		{"v3 with only an empty bag", replace(replace(plain, `"record_version":2`, `"record_version":3`), `,"payload":`, `,"metadata":{},"payload":`), InboxErrorMalformed, "record_version"},
		{"unknown member inside principal", replace(principalRow, `"kind":"actor"}`, `"kind":"actor","name":"Alex"}`), InboxErrorMalformed, "record"},
		{"non-string metadata value", replace(attributed, `"space":"family"`, `"space":7`), InboxErrorMalformed, "record"},
		{"invalid principal kind", replace(principalRow, `"kind":"actor"`, `"kind":"admin"`), InboxErrorInvalid, "principal"},
		{"metadata on a non-message kind", replace(attributed, `"kind":"input"`, `"kind":"interrupt"`), InboxErrorInvalid, "metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeDispositionInboxRecord(tc.value)
			if got := assertInboxCode(t, err, tc.code); got.Field != tc.field {
				t.Fatalf("field = %q, want %q (%v)", got.Field, tc.field, err)
			}
		})
	}
}
```

(The `"kind":"input"` literal first appears as the descriptor's kind. The principal's
`"kind":"actor"` comes later in the bytes, so `bytes.Replace(..., 1)` hits the command
kind.)

Also edit the existing case in `disposition_codec_test.go` (lines 30–33) so it keeps
meaning "future":

```go
		"future version": func(v []byte) []byte {
			return bytes.Replace(v, []byte(`"record_version":2`), []byte(`"record_version":4`), 1)
		},
```

**Step 2: Run it and watch it fail.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionCodecVersionAgreesWithAttribution|TestDispositionCodecRejectsCorruptionAndLegacy' .
```

Expected: FAIL on the two `record_version`-field Malformed cases, which currently report
field `record` from the canonical byte comparison. The v4 cases already PASS, because
the Task 3 accept set gives them `InboxErrorVersion`. They are kept as the pin.

**Step 3: Implement.** Replace `decodeDispositionInboxRecord`'s body after the decode
call with:

```go
	decoded := wire.record()
	// The version is a function of the content. A v2 row carrying attribution,
	// or a v3 row carrying none, is not a spelling this package writes, so it is
	// corrupt rather than old or new. It is reported against the version member
	// because that is the member that lies.
	if wire.RecordVersion != dispositionInboxRecordVersionFor(decoded.Descriptor) {
		return DispositionInboxRecord{}, inboxErr(InboxErrorMalformed, "record_version", nil)
	}
	canonical, record, err := encodeDispositionInboxRecord(decoded)
	if err != nil {
		return DispositionInboxRecord{}, err
	}
	// V2 and V3 are canonical stored codecs, not user JSON input formats. Exact
	// re-encoding rejects duplicate members and nested unknown fields (including
	// fields dropped by additive Core projection decoders), not just top-level
	// unknowns. Noncanonical spellings require an explicit offline conversion.
	if !bytes.Equal(canonical, value) {
		return DispositionInboxRecord{}, inboxErr(InboxErrorMalformed, "record", nil)
	}
	return record, nil
```

(`len(decoded.Descriptor.Metadata) != 0` is false for `"metadata":{}`, and that row has
no principal, so "v3 with only an empty bag" fails at the version check, with field
`record_version`.)

**Core-dependent expectation.** The "invalid principal kind" case expects
`InboxErrorInvalid`/`principal`, which assumes Core's `Principal.UnmarshalJSON` is strict
about **members** only and leaves value rules to `Validate()`. If impl-01's `UnmarshalJSON`
also validates the kind, the decode fails earlier as `InboxErrorMalformed`/`record`. In
that case change the expectation to match, since both are fail-closed. Record which one
applies in the commit message body.

**Step 4: Run the tests and watch them pass.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionCodec' .
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./...
```

Expected: PASS.

**Step 5: Mutation probes.** Revert each after it has been seen to fail:
- Add `4` to the accept set. Both v4 cases must FAIL.
- Delete the version-agreement check. The two `record_version`/Malformed cases must FAIL.
- Make `dispositionInboxRecordVersionFor` always return 3. `TestDispositionInboxWireGolden`
  (the v2 golden) must FAIL.

**Step 6: Commit.**

```bash
git add disposition_inbox.go disposition_codec_test.go disposition_attribution_test.go
git commit -m "feat(disposition): refuse an inbox record whose version disagrees with its attribution"
```

---

## Task 5: Transitions keep an attributed record at v3

Every transition re-encodes the whole record through `encodeDispositionInboxRecord`, so
the version follows the immutable descriptor. This task pins that behaviour.

**Files:** Modify `sessionstore/disposition_attribution_test.go`.

**Step 1: Write the test.**

```go
func TestDispositionTransitionsKeepAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	createDispositionCatalog(t, s)
	req := dispositionRequest()
	req.Principal, req.Metadata = testPrincipal(), testMetadata()
	admitted, _, err := s.AdmitDispositionCommand(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	rejected, ok, err := s.RejectDispositionCommand(ctx, rejectRequest(admitted, 0))
	if err != nil || !ok || rejected.Record.State != InboxStateRejected {
		t.Fatalf("reject: %+v %v %v", rejected, ok, err)
	}
	if !reflect.DeepEqual(rejected.Record.Descriptor, admitted.Record.Descriptor) {
		t.Fatalf("transition rewrote the descriptor:\n got %+v\nwant %+v", rejected.Record.Descriptor, admitted.Record.Descriptor)
	}
	if raw := rawDispositionRow(t, s, req.CommandID); !bytes.HasPrefix(raw, []byte(`{"record_version":3,`)) {
		t.Fatalf("rejected row left v3: %s", raw)
	}
}
```

**Step 2: Run it.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionTransitionsKeepAttribution' .
```

Expected: PASS. Prove it is not vacuous: temporarily change
`encodeDispositionInboxRecord` to marshal with the literal `DispositionInboxRecordVersion`.
The test must FAIL (the rejected row is refused or left at v2). Then revert.

**Step 3: Commit.**

```bash
git add disposition_attribution_test.go
git commit -m "test(disposition): pin that transitions keep an attributed record at v3"
```

---

## Task 6: Compare principal and metadata by value on retry (inline and by-reference)

**Files:**
- Modify: `sessionstore/disposition_inbox.go` (`admitDispositionCommand`, lines 231–234)
- Modify: `sessionstore/disposition_attribution_test.go`

**Step 1: Write the failing test.**

```go
func TestDispositionRetryComparesAttributionByValue(t *testing.T) {
	t.Parallel()
	none := func(*AdmitDispositionCommandRequest) {}
	principal := func(r *AdmitDispositionCommandRequest) { r.Principal = testPrincipal() }
	metadata := func(r *AdmitDispositionCommandRequest) { r.Metadata = testMetadata() }
	both := func(r *AdmitDispositionCommandRequest) { principal(r); metadata(r) }
	for _, representation := range []string{"inline", "by reference"} {
		for _, tc := range []struct {
			name         string
			first, retry func(*AdmitDispositionCommandRequest)
			field        string // "" means an idempotent retry
		}{
			{"same attribution", both, both, ""},
			{"principal added", none, principal, "principal"},
			{"principal dropped", principal, none, "principal"},
			{"different subject", principal, func(r *AdmitDispositionCommandRequest) {
				p := testPrincipal()
				p.Subject = "user/sam"
				r.Principal = p
			}, "principal"},
			{"different principal kind", principal, func(r *AdmitDispositionCommandRequest) {
				p := testPrincipal()
				p.Kind = sessionwire.PrincipalKindService
				r.Principal = p
			}, "principal"},
			{"metadata added", principal, both, "metadata"},
			{"metadata dropped", both, principal, "metadata"},
			{"metadata value differs", metadata, func(r *AdmitDispositionCommandRequest) {
				m := testMetadata()
				m["space"] = "work"
				r.Metadata = m
			}, "metadata"},
		} {
			t.Run(representation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				s := openTestStore(t)
				createDispositionCatalog(t, s)
				first := dispositionRequest()
				retry := dispositionRequest()
				if representation == "by reference" {
					// Two INDEPENDENT uploads of the same bytes share a digest,
					// which is exactly why the columns need their own comparison.
					one, two := uploadCommandPayload(t, s, inboxPayload), uploadCommandPayload(t, s, inboxPayload)
					first.Payload, first.PayloadObject = nil, &one
					retry.Payload, retry.PayloadObject = nil, &two
				}
				tc.first(&first)
				tc.retry(&retry)
				want, created, err := s.AdmitDispositionCommand(ctx, first)
				if err != nil || !created {
					t.Fatalf("first: %v %v", created, err)
				}
				got, created, err := s.AdmitDispositionCommand(ctx, retry)
				if tc.field == "" {
					if err != nil || created || !reflect.DeepEqual(got, want) {
						t.Fatalf("idempotent retry: %+v %v %v; want %+v", got, created, err, want)
					}
					return
				}
				if e := assertInboxCode(t, err, InboxErrorCommandMismatch); e.Field != tc.field {
					t.Fatalf("field = %q, want %q", e.Field, tc.field)
				}
				if created {
					t.Fatal("a mismatched retry reported created")
				}
				stored, err := s.GetDispositionCommand(ctx, GetDispositionCommandRequest{TenantID: first.TenantID, SessionID: first.SessionID, CommandID: first.CommandID})
				if err != nil || !reflect.DeepEqual(stored, want) {
					t.Fatalf("the winner moved: %+v %v", stored, err)
				}
			})
		}
	}
}
```

**Step 2: Run it and watch it fail.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionRetryComparesAttributionByValue' .
```

Expected: FAIL on every mismatch case in both representations with
"error = <nil>, want *InboxError". The retry silently returns the first winner. The two
"same attribution" cases PASS.

**Step 3: Implement.** In `admitDispositionCommand`, directly after the
`InboxErrorCommandMismatch, "command"` check (line 233–234), insert:

```go
	// The columns are compared by value, separately from the payload identity.
	// A by-reference payload is identified by digest alone, so two descriptors
	// whose columns differ could otherwise share one object and be taken as the
	// same command, re-attributing a command to whoever retried it.
	if !samePrincipal(winner.Principal, d.Principal) {
		return DispositionInboxEntry{}, false, inboxErr(InboxErrorCommandMismatch, "principal", nil)
	}
	if !maps.Equal(winner.Metadata, d.Metadata) {
		return DispositionInboxEntry{}, false, inboxErr(InboxErrorCommandMismatch, "metadata", nil)
	}
```

And append after `validateDispositionAttribution`:

```go
// samePrincipal is value equality over an optional principal: both absent, or
// both present and equal member by member.
func samePrincipal(a, b *sessionwire.Principal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
```

**Step 4: Run the tests and watch them pass.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestDispositionRetry|TestDispositionAdmission' .
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./...
```

Expected: PASS.

**Step 5: Mutation probes.** Replace `samePrincipal`'s body with `return true`, then
delete the `maps.Equal` check. Each change must make its own cases FAIL. Revert both.

**Step 6: Commit.**

```bash
git add disposition_inbox.go disposition_attribution_test.go
git commit -m "feat(disposition): compare principal and metadata by value on admission retry"
```

---

## Task 7: Carry the members through AdmitPublicCreate

Factory admits every `create` through `AdmitPublicCreate` (factory
`internal/admission/create.go:162-172`), so the create path needs the same members.

**Files:**
- Modify: `sessionstore/public_create.go` (lines 339 and 365)
- Modify: `sessionstore/disposition_attribution_test.go`

**Step 1: Write the failing test.** Add
`"github.com/looprig/storage/memstore"` to the test imports.

```go
func TestPublicCreateCarriesAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, memstore.New())
	prep := publicCreateRequest()
	if _, err := s.PreparePublicCreate(ctx, prep); err != nil {
		t.Fatal(err)
	}
	admit := AdmitPublicCreateRequest{Identity: prep.Identity, Payload: bytes.Clone(inboxPayload), Principal: testPrincipal(), Metadata: testMetadata()}
	entry, created, err := s.AdmitPublicCreate(ctx, admit)
	if err != nil || !created {
		t.Fatalf("admit: %v %v", created, err)
	}
	d := entry.Record.Descriptor
	if !d.PublicCreate || !reflect.DeepEqual(d.Principal, admit.Principal) || !maps.Equal(d.Metadata, admit.Metadata) {
		t.Fatalf("public create dropped attribution: %+v", d)
	}
	if raw := rawDispositionRow(t, s, prep.Identity.CommandID); !bytes.HasPrefix(raw, []byte(`{"record_version":3,`)) {
		t.Fatalf("attributed public create is not v3: %s", raw)
	}
	again, created, err := s.AdmitPublicCreate(ctx, admit)
	if err != nil || created || !reflect.DeepEqual(again, entry) {
		t.Fatalf("idempotent retry: %+v %v %v", again, created, err)
	}
	spoof := admit
	p := testPrincipal()
	p.Subject = "user/sam"
	spoof.Principal = p
	_, _, err = s.AdmitPublicCreate(ctx, spoof)
	if e := assertInboxCode(t, err, InboxErrorCommandMismatch); e.Field != "principal" {
		t.Fatalf("field = %q, want principal", e.Field)
	}
}

// THE PENDING SWEEPER'S VIEW (design §9.11). Factory's placement sweeper reads
// only ListDueDispositionCommands (factory internal/placement/pending.go:267) and
// classifies from entry.Record.Descriptor, never from payload bytes. A public
// create's inbox row must therefore carry the columns in that view, or a pending
// stamped create could be placed on a Host that cannot read it.
func TestPublicCreateAttributionIsVisibleToTheDueView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, memstore.New())
	prep := publicCreateRequest()
	if _, err := s.PreparePublicCreate(ctx, prep); err != nil {
		t.Fatal(err)
	}
	admit := AdmitPublicCreateRequest{Identity: prep.Identity, Payload: bytes.Clone(inboxPayload), Principal: testPrincipal(), Metadata: testMetadata()}
	if _, _, err := s.AdmitPublicCreate(ctx, admit); err != nil {
		t.Fatal(err)
	}
	scope, err := s.deriveSessionScope(prep.Identity.TenantID, prep.Identity.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	due, err := s.ListDueDispositionCommands(ctx, ListDueDispositionCommandsRequest{
		Shard: int(scope.ControlShard), DueAtOrBefore: inboxDeadline.Add(time.Hour), Limit: 10,
	})
	if err != nil || due.Unreadable != 0 || len(due.Commands) != 1 {
		t.Fatalf("due view: %+v %v", due, err)
	}
	d := due.Commands[0].Record.Descriptor
	if !d.PublicCreate || d.Principal == nil || *d.Principal != *admit.Principal || !maps.Equal(d.Metadata, admit.Metadata) {
		t.Fatalf("the due view lost the create's attribution: %+v", d)
	}
}

// Metadata on a non-message kind is refused before any provider I/O: with no
// reservation prepared, the answer is the shape refusal, not not_found.
func TestPublicCreateRefusesMetadataBeforeIO(t *testing.T) {
	t.Parallel()
	s := openStore(t, memstore.New())
	identity := publicCreateRequest().Identity
	identity.Kind = "interrupt"
	_, _, err := s.AdmitPublicCreate(context.Background(), AdmitPublicCreateRequest{Identity: identity, Payload: bytes.Clone(inboxPayload), Metadata: testMetadata()})
	if e := assertInboxCode(t, err, InboxErrorInvalid); e.Field != "metadata" {
		t.Fatalf("field = %q, want metadata", e.Field)
	}
}
```

**Step 2: Run them and watch them fail.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestPublicCreateCarriesAttribution|TestPublicCreateAttributionIsVisibleToTheDueView|TestPublicCreateRefusesMetadataBeforeIO' .
```

Expected: FAIL. The first test reports "public create dropped attribution", the second
"the due view lost the create's attribution", and the third gets `not_found` (field
`reservation`) instead of `invalid`/`metadata`. (Add `"time"` to the test imports if the
file lacks it; check the helper names `deriveSessionScope`/`ControlShard` against
`consumption_test.go`, which Task 8 also uses.)

**Step 3: Implement.** In `public_create.go`, append
`Principal: req.Principal, Metadata: req.Metadata` to both `AdmitDispositionCommandRequest`
literals. Line 339 becomes:

```go
	d, err := dispositionDescriptor(AdmitDispositionCommandRequest{TenantID: i.TenantID, SessionID: i.SessionID, CommandID: i.CommandID, Binding: i.Binding, Kind: i.Kind, Payload: req.Payload, PayloadObject: req.PayloadObject, Principal: req.Principal, Metadata: req.Metadata})
```

Line 365 becomes:

```go
	entry, created, err := s.admitDispositionCommand(opCtx, AdmitDispositionCommandRequest{TenantID: i.TenantID, SessionID: i.SessionID, CommandID: i.CommandID, Binding: i.Binding, ProposedRuntimeCommandID: r.RuntimeCommandID, Kind: i.Kind, Payload: req.Payload, PayloadObject: req.PayloadObject, Principal: req.Principal, Metadata: req.Metadata, AcceptedAt: r.AcceptedAt, ApplyDeadline: r.ApplyDeadline}, &r)
```

**Step 4: Run the tests and watch them pass.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestPublicCreate' .
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./...
```

Expected: PASS. `TestPublicCreateWireGolden` is unchanged, because the reservation record
did not move.

**Step 5: Commit.**

```bash
git add public_create.go disposition_attribution_test.go
git commit -m "feat(public-create): carry principal and metadata through AdmitPublicCreate"
```

---

## Task 8: ListSessionDispositionCommands fails closed at a v4 row

**Files:** Modify `sessionstore/consumption_test.go` (append after
`TestASessionCommandThisReaderCannotDecodeFailsThePage`, which ends near line 598).

**Step 1: Write the test.**

```go
// A row from a NEWER codec is the case a consumer meets during a rollout, and it
// must fail the page as "upgrade", never be stepped over: a consumer that skipped
// it would advance its cursor past a command it can never apply. The weak due
// view keeps stepping over it, as it does for any row it cannot read.
func TestASessionCommandFromANewerCodecFailsThePage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := consumptionFixture(t, memstore.New())
	admitted := admitSessionCommands(t, store, catalogTenant, catalogSession, "mine", 3)
	scope, err := store.deriveSessionScope(catalogTenant, catalogSession)
	if err != nil {
		t.Fatalf("deriveSessionScope: %v", err)
	}
	walkExactly(t, store, catalogTenant, catalogSession, 0, 100, 3)

	id := dispositionInboxID(scope, admitted[1])
	stored, err := store.backend.OrderedIndex.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	future := bytes.Replace(stored.Value, []byte(`"record_version":2`), []byte(`"record_version":4`), 1)
	if bytes.Equal(future, stored.Value) {
		t.Fatal("vacuous: the fixture row is not v2")
	}
	if _, err := store.backend.OrderedIndex.Update(ctx, id, stored.Revision, future, stored.Rank, stored.Due); err != nil {
		t.Fatalf("Update: %v", err)
	}

	_, err = store.ListSessionDispositionCommands(ctx, ListSessionDispositionCommandsRequest{
		TenantID: catalogTenant, SessionID: catalogSession, Limit: 100,
	})
	if e := assertInboxCode(t, err, InboxErrorVersion); e.Field != "record_version" {
		t.Fatalf("field = %q, want record_version", e.Field)
	}
	_, err = store.GetDispositionCommand(ctx, GetDispositionCommandRequest{TenantID: catalogTenant, SessionID: catalogSession, CommandID: admitted[1]})
	assertInboxCode(t, err, InboxErrorVersion)

	due, err := store.ListDueDispositionCommands(ctx, ListDueDispositionCommandsRequest{
		Shard: int(scope.ControlShard), DueAtOrBefore: inboxDeadline.Add(time.Hour), Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListDueDispositionCommands: %v", err)
	}
	if due.Unreadable != 1 || len(due.Commands) != 2 {
		t.Fatalf("due view: %d unreadable, %d commands; want 1 and 2", due.Unreadable, len(due.Commands))
	}
}

// A v3 row is an ordinary member of the stream and carries its attribution.
func TestSessionCommandStreamCarriesAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := consumptionFixture(t, memstore.New())
	req := dispositionRequest()
	req.Principal, req.Metadata = testPrincipal(), testMetadata()
	if _, _, err := store.AdmitDispositionCommand(ctx, req); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListSessionDispositionCommands(ctx, ListSessionDispositionCommandsRequest{TenantID: catalogTenant, SessionID: catalogSession, Limit: 10})
	if err != nil || len(page.Commands) != 1 {
		t.Fatalf("page: %+v %v", page, err)
	}
	if d := page.Commands[0].Record.Descriptor; *d.Principal != *req.Principal || d.Metadata["space"] != "family" {
		t.Fatalf("stream dropped attribution: %+v", d)
	}
}
```

**Step 2: Run the tests.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'TestASessionCommandFromANewerCodecFailsThePage|TestSessionCommandStreamCarriesAttribution' .
```

Expected: PASS. The behaviour came from Tasks 3–4, and this task pins the consumer-facing
contract. Show that it is not vacuous: temporarily add `4` to the accept set in
`decodeDispositionInboxRecord`. The first test must FAIL (it gets Malformed or no error
instead of Version). Then revert.

**Step 3: Commit.**

```bash
git add consumption_test.go
git commit -m "test(consumption): a v4 inbox row fails the session command page closed"
```

---

## Task 9: Seed the codec fuzzer with attributed and future records

**Files:** Modify `sessionstore/disposition_codec_test.go` (imports at lines 3–7; the
`FuzzDispositionInboxCodec` seed loop ends just before `f.Fuzz` near line 119).

**Step 1: Add the seeds and an invariant.** Add
`sessionwire "github.com/looprig/core/sessionwire/v1"` to the imports. Insert before
`f.Fuzz(`:

```go
	// v3 is a durable shape too. Seed it, and seed its v4 spelling, so random
	// mutation starts from both sides of the version gate.
	attributed := r
	attributed.State, attributed.Claim, attributed.Attempt, attributed.Outcome = InboxStatePending, nil, nil, nil
	attributed.Descriptor.Principal = &sessionwire.Principal{Tenant: req.TenantID, Subject: "user/A:B", Kind: sessionwire.PrincipalKindActor}
	attributed.Descriptor.Metadata = sessionwire.MessageMetadata{"space": "family"}
	v, _, err = encodeDispositionInboxRecord(attributed)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(v)
	f.Add(bytes.Replace(v, []byte(`"record_version":3`), []byte(`"record_version":4`), 1))
```

Inside the `f.Fuzz` body, after the "codec grants progress" checks, add:

```go
		wantV3 := r.Descriptor.Principal != nil || len(r.Descriptor.Metadata) != 0
		if bytes.HasPrefix(value, []byte(`{"record_version":3,`)) != wantV3 {
			t.Fatal("record version disagrees with attribution")
		}
		if len(r.Descriptor.Metadata) != 0 && !commandKindCarriesMessage(r.Descriptor.Kind) {
			t.Fatal("metadata decoded on a non-message kind")
		}
```

(Check that `r`'s kind at that point is `req.Kind` = `"input"`, so the metadata seed is
valid. If the seed loop reassigned `r.Descriptor`, rebuild `attributed` from `req`.)

**Step 2: Run it.**

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run 'FuzzDispositionInboxCodec' .
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -run '^$' -fuzz '^FuzzDispositionInboxCodec$' -fuzztime 60s .
```

Expected: PASS and no new files under `testdata/fuzz`. If the fuzzer writes a
reproducer, it is a finding: commit it and fix the codec. Never delete it (see the
Makefile `fuzz` notes).

**Step 3: Commit.**

```bash
git add disposition_codec_test.go
git commit -m "test(fuzz): seed the disposition codec with attributed and future records"
```

---

## Task 10: Documentation

The repository has **no CHANGELOG file**, so do not create one. The release notes are
`README.md` plus the annotated tag message (Task 12).

**Files:** Modify `sessionstore/README.md`.

**Step 1: Status paragraph** (lines 24–31). Change "public-create admission," to
"public-create admission, command attribution (a recorded principal on every kind, client
metadata on create/input),".

**Step 2: New section.** Insert the following before `## Residency-only grants` (line 268):

```markdown
### Command attribution: principal and metadata (v0.14.0)

`AdmitDispositionCommandRequest` and `AdmitPublicCreateRequest` carry two optional
members that become columns of the immutable `DispositionCommandDescriptor`:

| Member | Type | Allowed on | Meaning |
|---|---|---|---|
| `Principal` | `*sessionwire.Principal` | every kind | the sender as the admitting caller (Factory) recorded it |
| `Metadata` | `sessionwire.MessageMetadata` | `create`, `input` only | the client's app-defined string bag |

- **Shape only.** Both are validated with Core's rules, and a failure is `InboxErrorInvalid`
  with field `principal` or `metadata`. Metadata on any other kind (including an opaque
  one) is refused before any I/O. **The store proves nothing about who the principal is**:
  it is a recorded, caller-asserted value, like `PlacementTermination.Kind`, and the store
  does not check it against the command's tenant.
- **Record versions.** A command with neither member is stored at record version **2**
  with bytes identical to v0.13.1's. A command with either member is stored at **3**. The
  reader accepts {2, 3}. A 4 is `InboxErrorVersion` (upgrade), and so is a v3 row in the
  eyes of sessionstore ≤ v0.13.1. A version that disagrees with the members (v2 carrying
  one, v3 carrying none) is `InboxErrorMalformed`.
- **Retries compare by value.** The same `CommandID` admitted again with a different
  principal or metadata is `InboxErrorCommandMismatch` (field `principal` / `metadata`),
  even when the payload is equal. A by-reference payload is identified by digest alone,
  so without the column compare a different sender could re-attribute a command.
- **Public create.** The members are not part of the reservation identity (its record is
  unchanged). A caller whose payload canonically includes them binds them through
  `PayloadDigest`, as Factory does, and the inbox compares them on retry.
- **Consumers.** `ListSessionDispositionCommands` and `GetDispositionCommand` return both
  members, and fail closed on a row from a newer codec. The due view steps over one and
  counts it `Unreadable`.
- **One-way.** Once any v3 row is stored, every Factory and Host reading that store must be
  on sessionstore ≥ v0.14.0. An older reader refuses the row, and a rollback wedges every
  session holding an attributed command.
```

**Step 3: Check and commit.**

```bash
git diff --check
git add README.md
git commit -m "docs: document command attribution and inbox record v3"
```

---

## Task 11: Core bump (if Task 0 took branch B) and full verification

**Step 1: Bump core.** Skip this step if Task 0 already did it. It requires core v0.12.0
on the remote:

```bash
cd /Users/ipotter/code/looprig/sessionstore
git ls-remote --tags git@github.com:looprig/core.git refs/tags/v0.12.0   # must print a ref
GOWORK=off GOTOOLCHAIN=go1.26.8 go get github.com/looprig/core@v0.12.0
GOWORK=off GOTOOLCHAIN=go1.26.8 go mod tidy
grep -n "looprig/core\|looprig/storage\|^go \|toolchain" go.mod
# expected: core v0.12.0, storage v0.7.0, go 1.26.8, no toolchain line, no replace
rm -f /tmp/sessionstore-core012.work
git add go.mod go.sum
git commit -m "chore(deps): adopt core v0.12.0"
```

**Step 2: Standalone verification** (no workspace masking):

```bash
GOWORK=off GOTOOLCHAIN=go1.26.8 go build ./...
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race -count=1 ./...
GOWORK=off GOTOOLCHAIN=go1.26.8 make check
git diff --check
git status --short            # expected: empty; no go.work anywhere in the repo
test ! -e go.work && echo "no go.work"
grep -n "replace" go.mod || echo "no replace"
```

Expected: every command succeeds. `make check` runs fmt-check, vet, staticcheck, gosec,
vuln, the race tests, a 30s run per fuzz target, and build. `deps_test.go` still passes,
because production code imports only the stdlib, core and storage (`maps` is stdlib).

**Step 3: API diff against v0.13.1.**

```bash
git worktree add /tmp/sessionstore-v0131 v0.13.1
(cd /tmp/sessionstore-v0131 && GOWORK=off GOTOOLCHAIN=go1.26.8 go run golang.org/x/exp/cmd/apidiff@latest -m -w /tmp/sessionstore-v0131.export .)
GOWORK=off GOTOOLCHAIN=go1.26.8 go run golang.org/x/exp/cmd/apidiff@latest -m /tmp/sessionstore-v0131.export .
git worktree remove /tmp/sessionstore-v0131
```

Expected: **compatible changes only**:
- the new const `DispositionInboxRecordVersionAttributed`;
- the new fields `Principal` and `Metadata` on `DispositionCommandDescriptor`,
  `AdmitDispositionCommandRequest` and `AdmitPublicCreateRequest`.

Any incompatible line blocks the release. Together with the new durable codec version,
this is a **minor** bump: **v0.14.0**.

**Step 4: Record the evidence.** Keep the command outputs (test summary, `make check`
tail, apidiff) for the release report. Do not write them to a file in this repo.

---

## Task 12: Release v0.14.0 — REQUIRES OWNER CONFIRMATION

**Do not run any step of this task until the owner explicitly confirms the push and the
tag.** Present the Task 11 evidence and the annotation below, and wait.

**Step 1 (after confirmation): Push main.**

```bash
cd /Users/ipotter/code/looprig/sessionstore
git push origin main
git ls-remote origin refs/heads/main     # must equal: git rev-parse HEAD
```

**Step 2 (after confirmation): Create and push the annotated tag.**

```bash
git tag -a v0.14.0 -F - <<'EOF'
sessionstore v0.14.0: command principal and message metadata

Additive (minor). Pins core v0.12.0 and storage v0.7.0.

- DispositionCommandDescriptor, AdmitDispositionCommandRequest and
  AdmitPublicCreateRequest gain optional Principal (*sessionwire.Principal,
  every kind) and Metadata (sessionwire.MessageMetadata, create/input only).
- Validation is shape only (InboxErrorInvalid, field principal/metadata);
  metadata on any other kind is refused before any I/O. The store records the
  principal as a caller-asserted value and proves nothing about who it names.
- Inbox record v2 is still written, byte-identical, when both are absent; v3
  only when either is present (DispositionInboxRecordVersionAttributed). The
  reader accepts {2,3}, refuses 4 with InboxErrorVersion, and refuses a
  version that disagrees with its members as InboxErrorMalformed.
- An admission retry compares principal and metadata by value
  (InboxErrorCommandMismatch, field principal/metadata), including for
  by-reference payloads that share one object digest.
- ListSessionDispositionCommands and GetDispositionCommand fail closed on a
  newer-codec row; the due view steps over it and counts it Unreadable.

ONE-WAY: once any v3 row is stored, every Factory and Host reading that store
must be on sessionstore >= v0.14.0. sessionstore <= v0.13.1 refuses a v3 row
(InboxErrorVersion), so a rollback wedges every session holding an attributed
command.
EOF
git push origin v0.14.0
git ls-remote origin refs/tags/v0.14.0 refs/tags/v0.14.0^{}
# the peeled ^{} ref must equal: git rev-parse HEAD
```

**Step 3: Workspace bookkeeping.** These edits are to files outside the sessionstore repo.
The agent does not commit the outer repository.
- `/Users/ipotter/code/looprig/repositories.mk` line 38: `sessionstore|…|v0.13.1` → `v0.14.0`.
- `/Users/ipotter/code/looprig/go.work` already `use`s `./sessionstore`, so no change.
- `/Users/ipotter/code/looprig/AGENTS.md`: add a v0.14.0 entry with the release
  commit, the tag object and the one-way rule. Tier and edges are unchanged
  (`sessionstore -> core, storage`).
- harness repo: in `2026-09-25-impl-00-master-plan.md` Progress, set
  `02 sessionstore v0.14.0 | released (v0.14.0)`. In the design doc §8, set step 2 to
  released. Commit in harness as `docs(plan): record sessionstore v0.14.0`, and push only
  with owner confirmation.

---

## Consumer notes

- **One-way rule.** Once a store holds any v3 inbox row, **every process that reads it
  (every Factory and Host, and any tool calling `GetDispositionCommand` or
  `ListSessionDispositionCommands`) must be on sessionstore ≥ v0.14.0**. A v0.13.1 reader
  answers `InboxErrorVersion` for that row, and the command stream **fails the whole
  session's page**, so a rollback wedges every session with an attributed command. Upgrade
  every reader before any writer admits one.
- **Nothing changes until a writer sets a member.** With both members absent the bytes are
  identical to v0.13.1's, so a fleet can take v0.14.0 in any order. A v3 row appears only
  once Factory v0.12.0 has `WithPrincipalStamping()` enabled or a client sends metadata.
  The deployment order is master plan steps 1–4: Hosts, then Factory, then check the
  capability token, then enable stamping.
- **Factory (impl-07)** must set `Principal`/`Metadata` on **both**
  `AdmitDispositionCommandRequest` (input, interrupt, restore, gate_response) and
  `AdmitPublicCreateRequest` (create). The values must be exactly the ones in its canonical
  payload, so the digest and the columns agree. A retry by a different verified subject is
  `InboxErrorCommandMismatch` field `principal`, which Factory maps to its public code
  `command_rejected` (HTTP 409), as it already does for kind/digest/size mismatches.
  Factory's `Commands` seam signatures are unchanged (additive fields only).
- **Host / harness (impl-05/06)** can read the columns from `GetDispositionCommand`, but the
  design routes principal and metadata through the **payload** (Core's strict decoder).
  Treat the descriptor columns as the audit copy, not a second source to reconcile.
- **Error vocabulary is unchanged** (still 20 codes). No new `InboxErrorCode`, so a
  consumer pinning the code set does not fail on this bump.
- **Not verified by the store, by design:** that the principal's tenant equals the
  command's tenant, that the subject exists, or that the metadata was client-supplied.
  These belong to Factory's admission.
- **Harness v0.41.0** pins sessionstore v0.14.0. Under MVS, every harness dependent is
  lifted onto it at its next bump. All changes are additive.
