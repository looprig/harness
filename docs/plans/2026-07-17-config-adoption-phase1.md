# Configuration Adoption Phase 1 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build Phase 1 of `docs/plans/2026-07-16-session-versioning-migration-design.md`: `ConfigManifest` with canonical encoding and SHA-256 fingerprint, two-tier drift assessment, a `RestoreDecider` replacing the `WithAllowConfigMismatch` boolean, the `ConfigurationAdopted` event with epochs and baseline upgrades, and reading the event-envelope `v` on decode.

**Architecture:** New durable types (`ConfigManifest`, drift types, `ConfigurationAdopted`) live in `pkg/event` beside `ConfigFingerprint`; `pkg/rig` assembles the manifest the way `fingerprint.go` assembles the fingerprint today; the decider contract lives in `pkg/session`; `internal/sessionruntime` swaps its first-`SessionStarted` fingerprint check for latest-adopted-baseline + assessment + decision + adoption append. Everything is additive: the legacy `ConfigFingerprint` stays populated and readable throughout.

**Tech Stack:** Go stdlib only (crypto/sha256, encoding/binary, encoding/json). No new dependencies.

**Read first:** the design doc (sections "Configuration manifest" through "Configuration adoption event"), `pkg/event/config_fingerprint.go`, `pkg/rig/fingerprint.go`, `internal/sessionruntime/restore.go:147-195`.

**Ground rules (from CLAUDE.md):**
- TDD every task; run tests with `-race`.
- `gofmt`-clean; run `make fmt` before each commit.
- Do NOT run `make secure` casually — govulncheck spuriously expands `go.sum`; if you run it, revert `go.sum` before committing. Use `go vet ./...` + targeted `go test -race` as the per-task gate.
- Typed errors for anything callers classify. No `any` beyond JSON boundaries.

**Repo conventions to mirror (verify at the referenced lines before coding):**
- Event struct shape: copy `event.SessionStarted` (`pkg/event/event.go:239`) — `Header` embedding plus `enduring` + `sessionScoped` mixins.
- Header validation profiles: `pkg/event/validate.go:518-522` (the Restore* profiles).
- Codec registration: the `decodePayload` type switch (`pkg/event/marshal.go:555-566`) and the marshal path via `mergeEnvelope` (`marshal.go:322-336`).
- Canonical encoding style: `canonicalHustleTopologyMaterial` (`pkg/rig/fingerprint.go:156-179`) — explicit domain string, length-delimited big-endian.

---

### Task 1: ConfigManifest type with canonical encoding and fingerprint

**Files:**
- Create: `pkg/event/config_manifest.go`
- Test: `pkg/event/config_manifest_test.go`

**Step 1: Write the failing tests**

```go
package event

import (
	"strings"
	"testing"
)

func testManifest() ConfigManifest {
	return ConfigManifest{
		SchemaVersion:   ManifestSchemaVersion,
		AgentKind:       "coderig:operator",
		TopologyRev:     "aaaa",
		ModelID:         "claude-fable-5",
		SystemPromptRev: "bbbb",
		Tools: []ToolManifestEntry{
			{Name: "Bash", InputSchemaRev: "cc", OutputSchemaRev: "dd"},
			{Name: "Read", InputSchemaRev: "ee"},
		},
		RuntimeSkills:             true,
		WorkspaceRoot:             "/repo",
		WorkspaceTrust:            "trusted",
		AgentAdapter:              "",
		PermissionPosture:         "",
		NativePermissionPolicyRev: "ffff",
		PermissionStrictness:      3,
		ConfinementRev:            "gggg",
		ConfinementStrictness:     2,
		ExternalCapabilityRev:     "hhhh",
		AppFields:                 map[string]string{"b": "2", "a": "1"},
	}
}

func TestManifestFingerprint(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ConfigManifest)
		same   bool
	}{
		{name: "identical manifests match", mutate: func(*ConfigManifest) {}, same: true},
		{name: "app-field map order is irrelevant", mutate: func(m *ConfigManifest) {
			m.AppFields = map[string]string{"a": "1", "b": "2"}
		}, same: true},
		{name: "model change alters fingerprint", mutate: func(m *ConfigManifest) { m.ModelID = "other" }, same: false},
		{name: "tool schema change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.Tools[0].InputSchemaRev = "zz"
		}, same: false},
		{name: "strictness change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.PermissionStrictness = 1
		}, same: false},
		{name: "schema version change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.SchemaVersion = ManifestSchemaVersion + 1
		}, same: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := testManifest()
			other := testManifest()
			tt.mutate(&other)
			if got := base.Fingerprint() == other.Fingerprint(); got != tt.same {
				t.Errorf("fingerprint equality = %v, want %v", got, tt.same)
			}
		})
	}
}

// The canonical encoding is a durable contract: this golden vector pins it. If
// this test ever fails, the encoding changed — that is a manifest schema bump,
// not a test to update casually.
func TestManifestFingerprintGolden(t *testing.T) {
	got := testManifest().Fingerprint()
	if len(got) != 64 || strings.ToLower(got) != got {
		t.Fatalf("fingerprint %q is not lowercase hex sha256", got)
	}
	// Fill in the literal once on first run, then freeze it:
	const golden = "" // SET-ON-FIRST-RUN
	if golden != "" && got != golden {
		t.Errorf("canonical encoding drifted: fingerprint = %s, want %s", got, golden)
	}
}

func TestManifestFingerprintDomainSeparation(t *testing.T) {
	t.Parallel()
	// Empty manifest must not collide with the empty-string hash: the domain
	// tag guarantees it.
	empty := ConfigManifest{SchemaVersion: ManifestSchemaVersion}
	if empty.Fingerprint() == hexSHA256Event("") {
		t.Error("empty manifest fingerprint equals bare sha256 of empty string; domain tag missing")
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `go test -race ./pkg/event/ -run TestManifest -v`
Expected: FAIL — `ConfigManifest` undefined.

**Step 3: Implement**

`pkg/event/config_manifest.go` (complete file; comment density matches `config_fingerprint.go`):

```go
package event

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
)

// ManifestSchemaVersion is the current ConfigManifest schema version. Bumping it
// changes the canonical encoding, which changes every fingerprint — restore
// therefore never treats raw fingerprint inequality across schema versions as
// drift (see AssessDrift) and records a one-time baseline upgrade instead.
const ManifestSchemaVersion uint32 = 1

// manifestEncodingDomain separates manifest digests from every other SHA-256
// in the system. It is part of the durable contract; never change it without a
// schema bump.
const manifestEncodingDomain = "looprig/config-manifest/v1"

// ConfigEpoch orders the configurations explicitly adopted within one Session.
// SessionStarted is epoch 1; each ConfigurationAdopted increments it.
type ConfigEpoch uint64

// StrictnessLevel is an ordered security posture supplied by the composition
// root: higher is stricter. Zero means "unknown" — the posture exists only as
// an opaque digest, so a change cannot be direction-classified and drift
// assessment fails secure (Warn). Harness compares levels; it never computes
// them.
type StrictnessLevel uint8

// ToolManifestEntry is one model-facing tool's stable identity: its name plus
// content digests of its input and output schemas. Digests, never schemas —
// the manifest carries identity, not definitions.
type ToolManifestEntry struct {
	Name            string `json:"name"`
	InputSchemaRev  string `json:"input_schema_rev,omitzero"`
	OutputSchemaRev string `json:"output_schema_rev,omitzero"`
}

// ConfigManifest is the canonical, bounded, secret-free description of the
// behavior a Session runs under. It is a strict superset of the legacy
// ConfigFingerprint (see ManifestFromLegacy) and the input to both the
// SHA-256 fingerprint and typed drift assessment. Credentials, raw prompts,
// tool schemas, and environment contents never enter a manifest.
//
// SchemaVersion 0 marks a legacy projection built by ManifestFromLegacy; it is
// never persisted and never fingerprinted.
type ConfigManifest struct {
	SchemaVersion   uint32              `json:"schema_version"`
	AgentKind       string              `json:"agent_kind,omitzero"`
	TopologyRev     string              `json:"topology_rev,omitzero"`
	ModelID         string              `json:"model_id,omitzero"`
	SystemPromptRev string              `json:"system_prompt_rev,omitzero"`
	Tools           []ToolManifestEntry `json:"tools,omitzero"`
	RuntimeSkills   bool                `json:"runtime_skills,omitzero"`
	WorkspaceRoot   string              `json:"workspace_root,omitzero"`
	WorkspaceTrust  string              `json:"workspace_trust,omitzero"`
	AgentAdapter    string              `json:"agent_adapter,omitzero"`
	// PermissionPosture is the foreign-agent posture string; native sessions
	// use NativePermissionPolicyRev + PermissionStrictness instead.
	PermissionPosture         string         `json:"permission_posture,omitzero"`
	NativePermissionPolicyRev string         `json:"native_permission_policy_rev,omitzero"`
	PermissionStrictness      StrictnessLevel `json:"permission_strictness,omitzero"`
	ConfinementRev            string         `json:"confinement_rev,omitzero"`
	ConfinementStrictness     StrictnessLevel `json:"confinement_strictness,omitzero"`
	ExternalCapabilityRev     string         `json:"external_capability_rev,omitzero"`
	// AppFields are application-defined, secret-free compatibility fields.
	// Canonically encoded in sorted key order.
	AppFields map[string]string `json:"app_fields,omitzero"`
}

// Fingerprint is SHA-256 over the canonical encoding: explicit domain,
// schema version, stable field order, length-delimited values, deterministic
// collection ordering. Equal fingerprints of the same SchemaVersion identify
// behaviorally identical configurations.
func (m ConfigManifest) Fingerprint() string {
	return hexSHA256EventBytes(m.canonical())
}

func (m ConfigManifest) canonical() []byte {
	material := appendManifestString(nil, manifestEncodingDomain)
	material = binary.BigEndian.AppendUint64(material, uint64(m.SchemaVersion))
	material = appendManifestString(material, m.AgentKind)
	material = appendManifestString(material, m.TopologyRev)
	material = appendManifestString(material, m.ModelID)
	material = appendManifestString(material, m.SystemPromptRev)
	tools := append([]ToolManifestEntry(nil), m.Tools...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	material = binary.BigEndian.AppendUint64(material, uint64(len(tools)))
	for _, entry := range tools {
		material = appendManifestString(material, entry.Name)
		material = appendManifestString(material, entry.InputSchemaRev)
		material = appendManifestString(material, entry.OutputSchemaRev)
	}
	flag := uint64(0)
	if m.RuntimeSkills {
		flag = 1
	}
	material = binary.BigEndian.AppendUint64(material, flag)
	material = appendManifestString(material, m.WorkspaceRoot)
	material = appendManifestString(material, m.WorkspaceTrust)
	material = appendManifestString(material, m.AgentAdapter)
	material = appendManifestString(material, m.PermissionPosture)
	material = appendManifestString(material, m.NativePermissionPolicyRev)
	material = binary.BigEndian.AppendUint64(material, uint64(m.PermissionStrictness))
	material = appendManifestString(material, m.ConfinementRev)
	material = binary.BigEndian.AppendUint64(material, uint64(m.ConfinementStrictness))
	material = appendManifestString(material, m.ExternalCapabilityRev)
	keys := make([]string, 0, len(m.AppFields))
	for key := range m.AppFields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	material = binary.BigEndian.AppendUint64(material, uint64(len(keys)))
	for _, key := range keys {
		material = appendManifestString(material, key)
		material = appendManifestString(material, m.AppFields[key])
	}
	return material
}

func appendManifestString(material []byte, value string) []byte {
	material = binary.BigEndian.AppendUint64(material, uint64(len(value)))
	return append(material, value...)
}

func hexSHA256Event(value string) string {
	return hexSHA256EventBytes([]byte(value))
}

func hexSHA256EventBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
```

**Step 4: Run tests, capture the golden**

Run: `go test -race ./pkg/event/ -run TestManifest -v`
Expected: PASS. Print the fingerprint once (temporary `t.Log`), paste it into the `golden` constant, re-run, confirm PASS.

**Step 5: Commit**

```bash
make fmt && git add pkg/event/config_manifest.go pkg/event/config_manifest_test.go
git commit -m "feat(event): add ConfigManifest with canonical encoding and fingerprint"
```

---

### Task 2: Determinism fuzz target

**Files:**
- Test: `pkg/event/config_manifest_fuzz_test.go`

**Step 1: Write the fuzz target** (the encoder takes arbitrary field values from external config, so it gets a fuzz target per CLAUDE.md):

```go
package event

import "testing"

func FuzzManifestCanonical(f *testing.F) {
	f.Add("kind", "model", "root", "tool", "rev", uint8(3))
	f.Fuzz(func(t *testing.T, kind, model, root, tool, rev string, level uint8) {
		m := ConfigManifest{
			SchemaVersion:        ManifestSchemaVersion,
			AgentKind:            kind,
			ModelID:              model,
			WorkspaceRoot:        root,
			Tools:                []ToolManifestEntry{{Name: tool, InputSchemaRev: rev}},
			PermissionStrictness: StrictnessLevel(level),
			AppFields:            map[string]string{kind: model},
		}
		first, second := m.Fingerprint(), m.Fingerprint()
		if first != second {
			t.Fatalf("non-deterministic fingerprint: %s != %s", first, second)
		}
		if len(first) != 64 {
			t.Fatalf("fingerprint length = %d, want 64", len(first))
		}
	})
}
```

**Step 2: Run**

Run: `go test -race ./pkg/event/ -run FuzzManifestCanonical -fuzz FuzzManifestCanonical -fuzztime 30s`
Expected: no crashers.

**Step 3: Commit**

```bash
git add pkg/event/config_manifest_fuzz_test.go
git commit -m "test(event): fuzz manifest canonical encoding for determinism"
```

---

### Task 3: Legacy fingerprint projection

**Files:**
- Modify: `pkg/event/config_manifest.go`
- Test: `pkg/event/config_manifest_test.go`

**Step 1: Write the failing test**

```go
func TestManifestFromLegacy(t *testing.T) {
	t.Parallel()
	legacy := ConfigFingerprint{
		TopologyRev: "t", AgentKind: "k", ModelID: "m", SystemPromptRev: "s",
		ToolPolicyRev: "tp", RuntimeSkills: true, WorkspaceRoot: "/r",
		AgentAdapter: "a", PermissionPosture: "p",
		NativePermissionPolicyRev: "n", ExternalCapabilityRev: "x",
	}
	got := ManifestFromLegacy(legacy)
	if got.SchemaVersion != 0 {
		t.Errorf("SchemaVersion = %d, want 0 (legacy projection marker)", got.SchemaVersion)
	}
	// Every legacy field must survive the projection (superset requirement).
	if got.TopologyRev != "t" || got.AgentKind != "k" || got.ModelID != "m" ||
		got.SystemPromptRev != "s" || !got.RuntimeSkills || got.WorkspaceRoot != "/r" ||
		got.AgentAdapter != "a" || got.PermissionPosture != "p" ||
		got.NativePermissionPolicyRev != "n" || got.ExternalCapabilityRev != "x" {
		t.Errorf("legacy fields dropped in projection: %+v", got)
	}
	if got.legacyToolPolicyRev != "tp" {
		t.Errorf("legacyToolPolicyRev = %q, want %q", got.legacyToolPolicyRev, "tp")
	}
}

func TestLegacyToolPolicyRevDerivation(t *testing.T) {
	t.Parallel()
	m := ConfigManifest{
		SchemaVersion: ManifestSchemaVersion,
		Tools:         []ToolManifestEntry{{Name: "Read"}, {Name: "Bash"}},
	}
	// Must reproduce rig's names-only digest: sha256("Bash\nRead").
	if got, want := m.ToolNamesRev(), hexSHA256Event("Bash\nRead"); got != want {
		t.Errorf("ToolNamesRev() = %s, want %s", got, want)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test -race ./pkg/event/ -run 'TestManifestFromLegacy|TestLegacyToolPolicyRev' -v`
Expected: FAIL — undefined functions.

**Step 3: Implement** (append to `config_manifest.go`):

```go
// ManifestFromLegacy projects a legacy ConfigFingerprint into a partial
// manifest for drift assessment against a live candidate. SchemaVersion 0
// marks the projection: it is never persisted, never fingerprinted, and
// limits assessment to the fields the legacy fingerprint can distinguish
// (tool identity is names-only; permission and confinement are digest-only,
// so their changes classify Warn).
func ManifestFromLegacy(f ConfigFingerprint) ConfigManifest {
	return ConfigManifest{
		SchemaVersion:             0,
		AgentKind:                 f.AgentKind,
		TopologyRev:               f.TopologyRev,
		ModelID:                   f.ModelID,
		SystemPromptRev:           f.SystemPromptRev,
		legacyToolPolicyRev:       f.ToolPolicyRev,
		RuntimeSkills:             f.RuntimeSkills,
		WorkspaceRoot:             f.WorkspaceRoot,
		AgentAdapter:              f.AgentAdapter,
		PermissionPosture:         f.PermissionPosture,
		NativePermissionPolicyRev: f.NativePermissionPolicyRev,
		ExternalCapabilityRev:     f.ExternalCapabilityRev,
	}
}

// ToolNamesRev reproduces the legacy names-only tool digest from the manifest's
// tool entries, so a full manifest can be compared against a legacy baseline.
// It MUST stay byte-identical to rig's toolPolicyRev (sorted names joined by \n).
func (m ConfigManifest) ToolNamesRev() string {
	names := make([]string, 0, len(m.Tools))
	for _, entry := range m.Tools {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return hexSHA256Event(strings.Join(names, "\n"))
}
```

Add the unexported field to `ConfigManifest` (with `json:"-"`) and `strings` to imports:

```go
	// legacyToolPolicyRev carries a legacy baseline's names-only tool digest
	// through ManifestFromLegacy. Never persisted, never canonical.
	legacyToolPolicyRev string `json:"-"`
```

**Step 4: Run tests**

Run: `go test -race ./pkg/event/ -v -run 'TestManifest|TestLegacy'`
Expected: PASS (golden unchanged — the unexported field is not encoded).

**Step 5: Commit**

```bash
make fmt && git add pkg/event/config_manifest.go pkg/event/config_manifest_test.go
git commit -m "feat(event): project legacy ConfigFingerprint into partial manifest"
```

---

### Task 4: Drift assessment types and classification

**Files:**
- Create: `pkg/event/drift.go`
- Test: `pkg/event/drift_test.go`

**Step 1: Write the failing table-driven matrix** (cover every category; both directions for direction-sensitive fields; digest-only rule; legacy baseline):

```go
package event

import "testing"

func TestAssessDrift(t *testing.T) {
	base := testManifest()
	tests := []struct {
		name         string
		mutate       func(*ConfigManifest)
		wantCategory DriftCategory
		wantSeverity DriftSeverity
	}{
		{name: "model change is info", mutate: func(m *ConfigManifest) { m.ModelID = "x" },
			wantCategory: DriftModel, wantSeverity: DriftInfo},
		{name: "prompt change is info", mutate: func(m *ConfigManifest) { m.SystemPromptRev = "x" },
			wantCategory: DriftPrompt, wantSeverity: DriftInfo},
		{name: "tool schema change is info", mutate: func(m *ConfigManifest) { m.Tools[0].InputSchemaRev = "x" },
			wantCategory: DriftTool, wantSeverity: DriftInfo},
		{name: "tool removed is info", mutate: func(m *ConfigManifest) { m.Tools = m.Tools[:1] },
			wantCategory: DriftTool, wantSeverity: DriftInfo},
		{name: "topology change is info", mutate: func(m *ConfigManifest) { m.TopologyRev = "x" },
			wantCategory: DriftTopology, wantSeverity: DriftInfo},
		{name: "external catalog change is info", mutate: func(m *ConfigManifest) { m.ExternalCapabilityRev = "x" },
			wantCategory: DriftExternal, wantSeverity: DriftInfo},
		{name: "confinement stricter is info", mutate: func(m *ConfigManifest) {
			m.ConfinementRev = "x"
			m.ConfinementStrictness = base.ConfinementStrictness + 1
		}, wantCategory: DriftConfinement, wantSeverity: DriftInfo},
		{name: "confinement broadened is warn", mutate: func(m *ConfigManifest) {
			m.ConfinementRev = "x"
			m.ConfinementStrictness = base.ConfinementStrictness - 1
		}, wantCategory: DriftConfinement, wantSeverity: DriftWarn},
		{name: "permission narrowed is info", mutate: func(m *ConfigManifest) {
			m.NativePermissionPolicyRev = "x"
			m.PermissionStrictness = base.PermissionStrictness + 1
		}, wantCategory: DriftPermission, wantSeverity: DriftInfo},
		{name: "permission broadened is warn", mutate: func(m *ConfigManifest) {
			m.NativePermissionPolicyRev = "x"
			m.PermissionStrictness = base.PermissionStrictness - 1
		}, wantCategory: DriftPermission, wantSeverity: DriftWarn},
		{name: "digest-only permission change is warn", mutate: func(m *ConfigManifest) {
			m.NativePermissionPolicyRev = "x"
			m.PermissionStrictness = 0 // unknown direction -> fail secure
		}, wantCategory: DriftPermission, wantSeverity: DriftWarn},
		{name: "workspace move is warn", mutate: func(m *ConfigManifest) { m.WorkspaceRoot = "/other" },
			wantCategory: DriftWorkspace, wantSeverity: DriftWarn},
		{name: "trust mode change is warn", mutate: func(m *ConfigManifest) { m.WorkspaceTrust = "untrusted" },
			wantCategory: DriftTrust, wantSeverity: DriftWarn},
		{name: "agent kind change is warn", mutate: func(m *ConfigManifest) { m.AgentKind = "x" },
			wantCategory: DriftAgentKind, wantSeverity: DriftWarn},
		{name: "adapter change is warn", mutate: func(m *ConfigManifest) { m.AgentAdapter = "x" },
			wantCategory: DriftAdapter, wantSeverity: DriftWarn},
		{name: "runtime skills flip is warn", mutate: func(m *ConfigManifest) { m.RuntimeSkills = false },
			wantCategory: DriftRuntimeSkills, wantSeverity: DriftWarn},
		{name: "app field change is info", mutate: func(m *ConfigManifest) { m.AppFields["a"] = "x" },
			wantCategory: DriftApp, wantSeverity: DriftInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			candidate := testManifest()
			tt.mutate(&candidate)
			assessment := AssessDrift(testManifest(), candidate)
			if len(assessment.Changes) != 1 {
				t.Fatalf("Changes = %d entries (%+v), want exactly 1", len(assessment.Changes), assessment.Changes)
			}
			change := assessment.Changes[0]
			if change.Category != tt.wantCategory || change.Severity != tt.wantSeverity {
				t.Errorf("change = {%s %s}, want {%s %s}",
					change.Category, change.Severity, tt.wantCategory, tt.wantSeverity)
			}
		})
	}
}

func TestAssessDriftNoChanges(t *testing.T) {
	t.Parallel()
	assessment := AssessDrift(testManifest(), testManifest())
	if len(assessment.Changes) != 0 || assessment.AnyWarn() {
		t.Errorf("identical manifests produced drift: %+v", assessment)
	}
}

func TestAssessDriftLegacyBaseline(t *testing.T) {
	t.Parallel()
	legacy := ManifestFromLegacy(ConfigFingerprint{
		ModelID: "m", ToolPolicyRev: testManifest().ToolNamesRev(),
		NativePermissionPolicyRev: "old",
	})
	candidate := testManifest()
	candidate.ModelID = "m"
	assessment := AssessDrift(legacy, candidate)
	// Tool names unchanged -> no tool drift despite the legacy baseline having
	// no schema digests; schema-only info is invisible to a legacy baseline.
	for _, change := range assessment.Changes {
		if change.Category == DriftTool {
			t.Errorf("tool drift reported against name-identical legacy baseline: %+v", change)
		}
		// Legacy permission is digest-only: change must be Warn.
		if change.Category == DriftPermission && change.Severity != DriftWarn {
			t.Errorf("legacy permission drift severity = %s, want warn", change.Severity)
		}
	}
	if !assessment.BaselineUpgrade {
		t.Error("BaselineUpgrade = false for legacy baseline, want true")
	}
}
```

**Step 2: Run to verify failure**

Run: `go test -race ./pkg/event/ -run TestAssessDrift -v`
Expected: FAIL — types undefined.

**Step 3: Implement `pkg/event/drift.go`**

```go
package event

// DriftSeverity is the two-tier classification of one configuration change,
// answering one question: does the change expand what the session can touch?
// Severity is advisory input to application policy, not authority.
type DriftSeverity string

const (
	// DriftInfo changes are auto-accepted and recorded by default policy.
	DriftInfo DriftSeverity = "info"
	// DriftWarn changes require an explicit restore decision by default policy.
	DriftWarn DriftSeverity = "warn"
)

// DriftCategory names the manifest field family a change belongs to.
type DriftCategory string

const (
	DriftTool          DriftCategory = "tool"
	DriftModel         DriftCategory = "model"
	DriftPrompt        DriftCategory = "prompt"
	DriftTopology      DriftCategory = "topology"
	DriftExternal      DriftCategory = "external"
	DriftConfinement   DriftCategory = "confinement"
	DriftPermission    DriftCategory = "permission"
	DriftWorkspace     DriftCategory = "workspace"
	DriftTrust         DriftCategory = "trust"
	DriftAgentKind     DriftCategory = "agent_kind"
	DriftAdapter       DriftCategory = "adapter"
	DriftRuntimeSkills DriftCategory = "runtime_skills"
	DriftApp           DriftCategory = "app"
)

// DriftChange is one typed configuration change: safe identities only (names,
// digests, levels), never raw configuration. It is durable — ConfigurationAdopted
// persists the accepted assessment.
type DriftChange struct {
	Category DriftCategory `json:"category"`
	// Field disambiguates within a category (a tool name, an app-field key).
	Field    string        `json:"field,omitzero"`
	Old      string        `json:"old,omitzero"`
	New      string        `json:"new,omitzero"`
	Severity DriftSeverity `json:"severity"`
}

// DriftAssessment is the typed comparison of the latest adopted baseline
// against the candidate live manifest.
type DriftAssessment struct {
	Changes []DriftChange `json:"changes,omitzero"`
	// BaselineUpgrade reports that the baseline is a legacy projection or an
	// older manifest schema, so an accepting restore appends a one-time
	// baseline-upgrade ConfigurationAdopted even when Changes is empty.
	BaselineUpgrade bool `json:"baseline_upgrade,omitzero"`
}

// AnyWarn reports whether any change requires an explicit decision under
// default policy.
func (a DriftAssessment) AnyWarn() bool {
	for _, change := range a.Changes {
		if change.Severity == DriftWarn {
			return true
		}
	}
	return false
}

// AssessDrift compares baseline (the latest adopted manifest, possibly a
// legacy projection from ManifestFromLegacy) against candidate (the frozen
// live manifest) and reports typed changes. Direction-sensitive categories
// (confinement, permission) classify Info when the posture tightened and Warn
// when it broadened; when direction is unknowable — a change visible only
// through an opaque digest — they fail secure to Warn.
func AssessDrift(baseline, candidate ConfigManifest) DriftAssessment {
	assessment := DriftAssessment{
		BaselineUpgrade: baseline.SchemaVersion < candidate.SchemaVersion,
	}
	add := func(category DriftCategory, field, old, new string, severity DriftSeverity) {
		assessment.Changes = append(assessment.Changes, DriftChange{
			Category: category, Field: field, Old: old, New: new, Severity: severity,
		})
	}
	if baseline.ModelID != candidate.ModelID {
		add(DriftModel, "", baseline.ModelID, candidate.ModelID, DriftInfo)
	}
	if baseline.SystemPromptRev != candidate.SystemPromptRev {
		add(DriftPrompt, "", baseline.SystemPromptRev, candidate.SystemPromptRev, DriftInfo)
	}
	if baseline.TopologyRev != candidate.TopologyRev {
		add(DriftTopology, "", baseline.TopologyRev, candidate.TopologyRev, DriftInfo)
	}
	if baseline.ExternalCapabilityRev != candidate.ExternalCapabilityRev {
		add(DriftExternal, "", baseline.ExternalCapabilityRev, candidate.ExternalCapabilityRev, DriftInfo)
	}
	assessTools(baseline, candidate, add)
	assessDirectional(DriftConfinement,
		baseline.ConfinementRev, candidate.ConfinementRev,
		baseline.ConfinementStrictness, candidate.ConfinementStrictness, add)
	assessDirectional(DriftPermission,
		baseline.NativePermissionPolicyRev, candidate.NativePermissionPolicyRev,
		baseline.PermissionStrictness, candidate.PermissionStrictness, add)
	if baseline.PermissionPosture != candidate.PermissionPosture {
		add(DriftPermission, "posture", baseline.PermissionPosture, candidate.PermissionPosture, DriftWarn)
	}
	if baseline.WorkspaceRoot != candidate.WorkspaceRoot {
		add(DriftWorkspace, "", baseline.WorkspaceRoot, candidate.WorkspaceRoot, DriftWarn)
	}
	if baseline.WorkspaceTrust != candidate.WorkspaceTrust {
		add(DriftTrust, "", baseline.WorkspaceTrust, candidate.WorkspaceTrust, DriftWarn)
	}
	if baseline.AgentKind != candidate.AgentKind {
		add(DriftAgentKind, "", baseline.AgentKind, candidate.AgentKind, DriftWarn)
	}
	if baseline.AgentAdapter != candidate.AgentAdapter {
		add(DriftAdapter, "", baseline.AgentAdapter, candidate.AgentAdapter, DriftWarn)
	}
	if baseline.RuntimeSkills != candidate.RuntimeSkills {
		add(DriftRuntimeSkills, "", boolID(baseline.RuntimeSkills), boolID(candidate.RuntimeSkills), DriftWarn)
	}
	assessAppFields(baseline.AppFields, candidate.AppFields, add)
	return assessment
}

func assessTools(baseline, candidate ConfigManifest, add func(DriftCategory, string, string, string, DriftSeverity)) {
	if baseline.SchemaVersion == 0 {
		// Legacy baseline: only the names-only digest is comparable.
		if baseline.legacyToolPolicyRev != candidate.ToolNamesRev() {
			add(DriftTool, "", baseline.legacyToolPolicyRev, candidate.ToolNamesRev(), DriftInfo)
		}
		return
	}
	old := make(map[string]ToolManifestEntry, len(baseline.Tools))
	for _, entry := range baseline.Tools {
		old[entry.Name] = entry
	}
	for _, entry := range candidate.Tools {
		prior, existed := old[entry.Name]
		switch {
		case !existed:
			add(DriftTool, entry.Name, "", entry.InputSchemaRev, DriftInfo)
		case prior != entry:
			add(DriftTool, entry.Name, prior.InputSchemaRev, entry.InputSchemaRev, DriftInfo)
		}
		delete(old, entry.Name)
	}
	for name, prior := range old {
		add(DriftTool, name, prior.InputSchemaRev, "", DriftInfo)
	}
}

func assessDirectional(category DriftCategory, oldRev, newRev string, oldLevel, newLevel StrictnessLevel, add func(DriftCategory, string, string, string, DriftSeverity)) {
	if oldRev == newRev && oldLevel == newLevel {
		return
	}
	severity := DriftWarn // unknown direction fails secure
	if oldLevel != 0 && newLevel != 0 {
		if newLevel >= oldLevel {
			severity = DriftInfo
		}
	}
	add(category, "", oldRev, newRev, severity)
}

func assessAppFields(baseline, candidate map[string]string, add func(DriftCategory, string, string, string, DriftSeverity)) {
	for key, value := range candidate {
		if old, existed := baseline[key]; !existed || old != value {
			add(DriftApp, key, baseline[key], value, DriftInfo)
		}
	}
	for key, old := range baseline {
		if _, still := candidate[key]; !still {
			add(DriftApp, key, old, "", DriftInfo)
		}
	}
}

func boolID(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
```

Note: `assessTools` ordering is map-driven for adds/removes — if the sweep test
flakes on ordering, sort `assessment.Changes` by (Category, Field) before
returning. `assessAppFields` iterates maps: sort keys first (deterministic
output is required because the assessment is persisted). Fix that in this task,
not later: collect keys, `sort.Strings`, then range.

**Step 4: Run tests**

Run: `go test -race ./pkg/event/ -run TestAssessDrift -v`
Expected: PASS.

**Step 5: Commit**

```bash
make fmt && git add pkg/event/drift.go pkg/event/drift_test.go
git commit -m "feat(event): typed drift assessment with two-tier severity"
```

---

### Task 5: Read the event-envelope `v` on decode

**Files:**
- Modify: `pkg/event/marshal.go` (`UnmarshalEvent`, ~line 345; `schemaVersion` const at line 22)
- Create error in: `pkg/event/marshal.go` or the package's existing error file (mirror where `*EphemeralNotPersistableError` lives)
- Test: `pkg/event/marshal_test.go` (append)

**Step 1: Write the failing test**

```go
func TestUnmarshalEventSchemaVersion(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]json.RawMessage)
		wantErr bool
	}{
		{name: "current version decodes", mutate: func(map[string]json.RawMessage) {}},
		{name: "missing v decodes as version 1", mutate: func(env map[string]json.RawMessage) {
			delete(env, "v")
		}},
		{name: "future version fails typed", mutate: func(env map[string]json.RawMessage) {
			env["v"] = json.RawMessage(`2`)
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Marshal any simple Enduring event the package's existing tests use
			// (mirror their fixture), unmarshal to a map, apply mutate, re-marshal,
			// then UnmarshalEvent.
			raw := marshalTestEventEnvelope(t) // helper: reuse existing fixture pattern
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			tt.mutate(envelope)
			mutated, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			_, err = UnmarshalEvent(mutated)
			if (err != nil) != tt.wantErr {
				t.Fatalf("UnmarshalEvent() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var unsupported *UnsupportedSchemaError
				if !errors.As(err, &unsupported) {
					t.Fatalf("error type = %T, want *UnsupportedSchemaError", err)
				}
				if unsupported.Version != 2 {
					t.Errorf("Version = %d, want 2", unsupported.Version)
				}
			}
		})
	}
}
```

(Write `marshalTestEventEnvelope` by copying whatever fixture the existing
`marshal_test.go` round-trip tests construct — do not invent a new event shape.)

**Step 2: Run to verify failure**

Run: `go test -race ./pkg/event/ -run TestUnmarshalEventSchemaVersion -v`
Expected: FAIL — `UnsupportedSchemaError` undefined.

**Step 3: Implement**

Add the typed error (next to the package's other exported errors):

```go
// UnsupportedSchemaError reports a durable record whose schema version is
// newer than this binary supports. It is the dispatch hook a future migration
// layer catches; today it is a hard, typed restore failure.
type UnsupportedSchemaError struct {
	Kind    string // "event"
	Version uint32
}

func (e *UnsupportedSchemaError) Error() string {
	return fmt.Sprintf("event: unsupported %s schema version %d (max %d)", e.Kind, e.Version, schemaVersion)
}
```

In `UnmarshalEvent` (marshal.go:345), before dispatching on `"type"`: decode the
`"v"` field from the envelope; treat absent as 1; if `v > schemaVersion`, return
`&UnsupportedSchemaError{Kind: "event", Version: v}`.

**Step 4: Run the full event package** (regression: every existing decode test
must still pass, proving absent-v tolerance):

Run: `go test -race ./pkg/event/`
Expected: PASS.

**Step 5: Commit**

```bash
make fmt && git add pkg/event/
git commit -m "feat(event): fail typed on unsupported event schema version at decode"
```

---

### Task 6: ConfigurationAdopted event

**Files:**
- Modify: `pkg/event/event.go` (append near the Restore* events, line ~271)
- Modify: `pkg/event/validate.go` (profile near line 518)
- Modify: `pkg/event/marshal.go` (`decodePayload` switch at ~555; marshal registration mirroring SessionStarted)
- Test: `pkg/event/event_test.go` / the package's existing round-trip test file

**Step 1: Write the failing round-trip test** (copy the SessionStarted round-trip test pattern exactly — same helper, same assertions):

```go
func TestConfigurationAdoptedRoundTrip(t *testing.T) {
	t.Parallel()
	original := ConfigurationAdopted{
		Header:    testSessionScopedHeader(t), // reuse the fixture SessionStarted tests use
		SessionID: testSessionID,
		Epoch:     2,
		PreviousFingerprint: "prev",
		AdoptedFingerprint:  "next",
		Manifest:  testManifest(),
		Drift: []DriftChange{{Category: DriftModel, Old: "a", New: "b", Severity: DriftInfo}},
		Source:  DecisionSourcePolicy,
		Actor:   "op@host",
		AppVersion: "coderig/1.2.3",
		Message: "accepted model change",
	}
	raw, err := MarshalEvent(original)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	decoded, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent() error = %v", err)
	}
	got, ok := decoded.(ConfigurationAdopted)
	if !ok {
		t.Fatalf("decoded type = %T, want ConfigurationAdopted", decoded)
	}
	if got.Epoch != 2 || got.Source != DecisionSourcePolicy || len(got.Drift) != 1 {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.Class() != Enduring || got.Scope() != ScopeSession {
		t.Errorf("Class/Scope = %v/%v, want Enduring/ScopeSession", got.Class(), got.Scope())
	}
}
```

**Step 2: Run to verify failure**

Run: `go test -race ./pkg/event/ -run TestConfigurationAdoptedRoundTrip -v`
Expected: FAIL — type undefined.

**Step 3: Implement**

In `event.go`, mirroring `SessionStarted`'s embedding (verify at line 239 —
Header + `enduring` + `sessionScoped` mixins, same doc-comment style):

```go
// DecisionSource records who or what accepted a configuration adoption.
type DecisionSource string

const (
	DecisionSourceUser     DecisionSource = "user"
	DecisionSourcePolicy   DecisionSource = "policy"
	DecisionSourceOperator DecisionSource = "operator"
	// DecisionSourceMigration is stamped only by Harness itself when a Phase 2
	// migration adopts a configuration; a RestoreDecider never produces it.
	DecisionSourceMigration DecisionSource = "migration"
)

// ConfigurationAdopted commits a new configuration epoch: the durable record
// of an accepted restore drift or a one-time baseline upgrade. It is appended
// under the restore lease after the decision validates and before RestoreDone;
// the latest SessionStarted or ConfigurationAdopted is the baseline for the
// next restore's drift assessment.
type ConfigurationAdopted struct {
	Header
	enduring
	sessionScoped
	SessionID           uuid.UUID      `json:"session_id"`
	Epoch               ConfigEpoch    `json:"epoch"`
	PreviousFingerprint string         `json:"previous_fingerprint,omitzero"`
	AdoptedFingerprint  string         `json:"adopted_fingerprint"`
	Manifest            ConfigManifest `json:"manifest"`
	Drift               []DriftChange  `json:"drift,omitzero"`
	Source              DecisionSource `json:"source"`
	Actor               string         `json:"actor,omitzero"`
	AppVersion          string         `json:"app_version,omitzero"`
	// Message is durable user-authored data, not an instruction: it never
	// gains authority during future prompt construction.
	Message string `json:"message,omitzero"`
}
```

Register the codec in `decodePayload` (marshal.go:555 switch) and the marshal
side exactly as SessionStarted is registered. Add a validation profile beside
the Restore* profiles (validate.go:518): session-scoped header, required
SessionID, `Epoch >= 2` (epoch 1 is SessionStarted), non-empty
`AdoptedFingerprint`, `Source` one of the four constants, and bounds — cap
`len(Drift)` at 256 entries, `Message` at 4096 bytes, reject a `Manifest` whose
`SchemaVersion` is 0 (legacy projections are never persisted). Follow the exact
mechanism the neighboring profiles use.

**Step 4: Run tests**

Run: `go test -race ./pkg/event/`
Expected: PASS.

**Step 5: Commit**

```bash
make fmt && git add pkg/event/
git commit -m "feat(event): ConfigurationAdopted event with epochs and drift summary"
```

---

### Task 7: SessionStarted carries the manifest; rig assembles it

**Files:**
- Modify: `pkg/event/event.go` (SessionStarted struct)
- Modify: `pkg/rig/fingerprint.go` (manifest assembly beside `frozenFingerprint`)
- Modify: `pkg/rig` call sites that build `SessionStarted.Config` (find with `grep -rn "Config:" pkg/rig internal/sessionruntime | grep -i fingerprint`)
- Tests: `pkg/rig/fingerprint_test.go` (append), `pkg/event` round-trip

**Step 1: Write the failing tests**

In `pkg/event`: extend the existing SessionStarted round-trip test with a
populated `Manifest` field and assert it survives; also assert a legacy record
*without* the field still decodes (delete the key from the envelope map, decode,
expect zero Manifest — this proves additivity).

In `pkg/rig`:

```go
func TestManifestMatchesFingerprint(t *testing.T) {
	t.Parallel()
	// Build the same fixture frozenFingerprint tests use, produce both the
	// legacy fingerprint and the manifest, and assert every shared field agrees
	// (superset requirement: the manifest must never disagree with the legacy
	// fingerprint it supersedes).
	fields := ConfigFingerprintFields{AgentKind: "k", RuntimeSkills: true, WorkspaceRoot: "/r"}
	legacy := frozenFingerprint(fields, definitions, primers, active)   // reuse existing fixture vars
	manifest := frozenManifest(fields, definitions, primers, active)
	if manifest.SchemaVersion != event.ManifestSchemaVersion {
		t.Fatalf("SchemaVersion = %d", manifest.SchemaVersion)
	}
	if manifest.AgentKind != legacy.AgentKind || manifest.ModelID != legacy.ModelID ||
		manifest.SystemPromptRev != legacy.SystemPromptRev ||
		manifest.TopologyRev != legacy.TopologyRev ||
		manifest.ToolNamesRev() != legacy.ToolPolicyRev {
		t.Errorf("manifest disagrees with legacy fingerprint:\n%+v\nvs\n%+v", manifest, legacy)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test -race ./pkg/rig/ -run TestManifestMatchesFingerprint -v`
Expected: FAIL — `frozenManifest` undefined.

**Step 3: Implement**

- `SessionStarted` gains `Manifest ConfigManifest \`json:"manifest,omitzero"\``
  (additive; the existing `Config ConfigFingerprint` stays populated — the doc's
  deprecation window).
- In `pkg/rig/fingerprint.go` add `frozenManifest(...)` and a live counterpart
  mirroring `fingerprintWithTopology` / `frozenFingerprint` exactly: same
  inputs, filling manifest fields from the same sources, tool entries from
  `initial.ToolNames` (schema digests empty until the tool-info plumbing lands
  — see note), `WorkspaceTrust`/strictness levels from new optional
  `ConfigFingerprintFields` members (additive fields: `WorkspaceTrust string`,
  `PermissionStrictness`, `ConfinementStrictness`, `ConfinementRev`,
  `AppFields map[string]string` — all zero-safe).
- Populate `SessionStarted.Manifest` wherever `Config` is populated today.

**Note on tool schema digests:** `toolPolicyRev` (fingerprint.go:235) reads
`tool.InvokableTool.Info()`. Check whether `Info()` exposes the input/output
schema; if it does, digest the canonical JSON of each schema
(`hexSHA256` of the marshaled schema) into `ToolManifestEntry`. If it does not,
leave the digests empty in this task, and file the follow-up — empty digests
compare equal, so drift stays names-only until the plumbing exists. Do NOT
invent a new method on the tool interface in this task.

**Step 4: Run tests**

Run: `go test -race ./pkg/rig/ ./pkg/event/`
Expected: PASS.

**Step 5: Commit**

```bash
make fmt && git add pkg/event/ pkg/rig/
git commit -m "feat(rig): assemble ConfigManifest and stamp it on SessionStarted"
```

---

### Task 8: RestoreDecider contract, default policy decider, rig option

**Files:**
- Create: `pkg/session/decider.go`
- Modify: `pkg/session/errors.go` (add `RestoreRejectedError`; mark `ConfigMismatchError` Deprecated)
- Modify: `pkg/rig/options.go` (~line 161: add `WithRestoreDecider`, re-implement `WithAllowConfigMismatch` as shim)
- Modify: `internal/sessionruntime/lifecycle.go` (~line 303: plumb the decider the way `WithLifecycleAllowConfigMismatch` is plumbed)
- Test: `pkg/session/decider_test.go`

**Step 1: Write the failing test**

```go
package session

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

func TestDefaultPolicyDecider(t *testing.T) {
	tests := []struct {
		name       string
		assessment event.DriftAssessment
		wantAccept bool
	}{
		{name: "no drift accepts", wantAccept: true},
		{name: "info-only accepts", assessment: event.DriftAssessment{Changes: []event.DriftChange{
			{Category: event.DriftModel, Severity: event.DriftInfo},
		}}, wantAccept: true},
		{name: "any warn rejects", assessment: event.DriftAssessment{Changes: []event.DriftChange{
			{Category: event.DriftModel, Severity: event.DriftInfo},
			{Category: event.DriftWorkspace, Severity: event.DriftWarn},
		}}, wantAccept: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			decision, err := DefaultPolicyDecider{}.DecideRestore(context.Background(), tt.assessment)
			if err != nil {
				t.Fatalf("DecideRestore() error = %v", err)
			}
			if decision.Accept != tt.wantAccept {
				t.Errorf("Accept = %v, want %v", decision.Accept, tt.wantAccept)
			}
			if decision.Source != event.DecisionSourcePolicy {
				t.Errorf("Source = %s, want policy", decision.Source)
			}
		})
	}
}

func TestAcceptAllDecider(t *testing.T) {
	t.Parallel()
	warn := event.DriftAssessment{Changes: []event.DriftChange{
		{Category: event.DriftWorkspace, Severity: event.DriftWarn},
	}}
	decision, err := AcceptAllDecider{}.DecideRestore(context.Background(), warn)
	if err != nil || !decision.Accept {
		t.Fatalf("AcceptAllDecider = (%+v, %v), want unconditional accept", decision, err)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test -race ./pkg/session/ -run 'TestDefaultPolicy|TestAcceptAll' -v`
Expected: FAIL.

**Step 3: Implement `pkg/session/decider.go`**

```go
package session

import (
	"context"

	"github.com/looprig/harness/pkg/event"
)

// RestoreDecision is an application's answer to a drift assessment. Source and
// Actor are recorded durably on the resulting ConfigurationAdopted.
type RestoreDecision struct {
	Accept bool
	Source event.DecisionSource // user | policy | operator (never migration)
	// Actor is an optional stable identity for the decision maker.
	Actor string
	// Message is an optional user-authored note recorded on the adoption event.
	Message string
}

// RestoreDecider answers a restore drift assessment. It runs while the restore
// lease is held and the lease fence is appended; ctx carries the restore
// deadline, and a timeout is a rejection. Harness owns the assessment and the
// durable record; the decider owns presentation and the policy choice.
type RestoreDecider interface {
	DecideRestore(ctx context.Context, assessment event.DriftAssessment) (RestoreDecision, error)
}

// DefaultPolicyDecider is the fail-secure headless default: accept when every
// change is Info, reject when any change is Warn. Deployments that must accept
// specific Warn categories install their own decider.
type DefaultPolicyDecider struct{}

func (DefaultPolicyDecider) DecideRestore(_ context.Context, assessment event.DriftAssessment) (RestoreDecision, error) {
	return RestoreDecision{
		Accept: !assessment.AnyWarn(),
		Source: event.DecisionSourcePolicy,
	}, nil
}

// AcceptAllDecider accepts every assessment. It backs the deprecated
// WithAllowConfigMismatch shim; the adoption record still shows what was
// accepted and that policy, not a person, decided.
type AcceptAllDecider struct{}

func (AcceptAllDecider) DecideRestore(_ context.Context, _ event.DriftAssessment) (RestoreDecision, error) {
	return RestoreDecision{Accept: true, Source: event.DecisionSourcePolicy}, nil
}
```

Add to `pkg/session/errors.go` (mirror `ConfigMismatchError`'s style at line 75):

```go
// RestoreRejectedError reports a restore refused by the configured
// RestoreDecider (or by default policy). It carries the full typed assessment
// so callers and operators see exactly which fields drifted and how severely.
type RestoreRejectedError struct {
	Assessment event.DriftAssessment
	Source     event.DecisionSource
}

func (e *RestoreRejectedError) Error() string { /* list Warn categories, then Info count */ }
```

Mark `ConfigMismatchError` with a `// Deprecated: superseded by
RestoreRejectedError; remove after the deprecation window.` comment. Do not
delete it.

In `pkg/rig/options.go`, add `WithRestoreDecider(d session.RestoreDecider)`
delegating to a new `sessionruntime.WithLifecycleRestoreDecider`, and
re-implement `WithAllowConfigMismatch` as `WithRestoreDecider(session.AcceptAllDecider{})`
with a `// Deprecated:` comment. In `lifecycle.go`, plumb the decider field
exactly as `allowConfigMismatch` is plumbed today (restore-only, not NewSession);
default it to `session.DefaultPolicyDecider{}` when unset — check the import
direction first: if `internal/sessionruntime` cannot import `pkg/session`
(cycle), define the decider interface in `internal/sessionruntime` and have
`pkg/session` alias it (`type RestoreDecider = sessionruntime.RestoreDecider`),
mirroring how the package split handles `ConfigMismatchError` today.

**Step 4: Run tests**

Run: `go test -race ./pkg/session/ ./pkg/rig/ ./internal/sessionruntime/`
Expected: PASS (existing sessionruntime tests still pass — the boolean still
works through the shim).

**Step 5: Commit**

```bash
make fmt && git add pkg/session/ pkg/rig/ internal/sessionruntime/
git commit -m "feat(session): RestoreDecider contract with fail-secure default policy"
```

---

### Task 9: Latest-adopted baseline scanner

**Files:**
- Modify: `internal/sessionruntime/restore.go` (beside `firstConfigFingerprint`, line 188)
- Test: `internal/sessionruntime/restore_test.go` (append; follow the file's existing fixture style)

**Step 1: Write the failing test**

```go
func TestLatestAdoptedBaseline(t *testing.T) {
	tests := []struct {
		name        string
		events      []event.Event
		wantEpoch   event.ConfigEpoch
		wantLegacy  bool
		wantErr     bool
	}{
		{name: "no session started fails closed", wantErr: true},
		{name: "session started with manifest is epoch 1",
			events:    []event.Event{sessionStartedWithManifest(t)},
			wantEpoch: 1},
		{name: "legacy session started yields legacy projection",
			events:     []event.Event{sessionStartedLegacyOnly(t)},
			wantEpoch:  1,
			wantLegacy: true},
		{name: "latest adoption wins",
			events: []event.Event{
				sessionStartedWithManifest(t),
				configurationAdopted(t, 2),
				configurationAdopted(t, 3),
			},
			wantEpoch: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			baseline, err := latestAdoptedBaseline(tt.events)
			if (err != nil) != tt.wantErr {
				t.Fatalf("latestAdoptedBaseline() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if baseline.Epoch != tt.wantEpoch {
				t.Errorf("Epoch = %d, want %d", baseline.Epoch, tt.wantEpoch)
			}
			if got := baseline.Manifest.SchemaVersion == 0; got != tt.wantLegacy {
				t.Errorf("legacy projection = %v, want %v", got, tt.wantLegacy)
			}
		})
	}
}
```

(Build the fixture helpers from the file's existing SessionStarted test
fixtures; `sessionStartedLegacyOnly` populates only `Config`.)

**Step 2: Run to verify failure**

Run: `go test -race ./internal/sessionruntime/ -run TestLatestAdoptedBaseline -v`
Expected: FAIL.

**Step 3: Implement** (in `restore.go`, beside `firstConfigFingerprint`, which
stays for now):

```go
// adoptedBaseline is the restore comparison baseline: the manifest and epoch of
// the LATEST committed SessionStarted or ConfigurationAdopted. A legacy
// SessionStarted (no manifest) yields a SchemaVersion-0 projection, which
// limits drift assessment and forces a baseline upgrade on acceptance.
type adoptedBaseline struct {
	Manifest event.ConfigManifest
	Epoch    event.ConfigEpoch
}

func latestAdoptedBaseline(events []event.Event) (adoptedBaseline, error) {
	var baseline adoptedBaseline
	found := false
	for _, ev := range events {
		switch typed := ev.(type) {
		case event.SessionStarted:
			if !found { // only the first SessionStarted seeds epoch 1
				found = true
				baseline.Epoch = 1
				if typed.Manifest.SchemaVersion != 0 {
					baseline.Manifest = typed.Manifest
				} else {
					baseline.Manifest = event.ManifestFromLegacy(typed.Config)
				}
			}
		case event.ConfigurationAdopted:
			found = true
			baseline.Manifest = typed.Manifest
			baseline.Epoch = typed.Epoch
		}
	}
	if !found {
		return adoptedBaseline{}, &RestoreDiscoveryError{Kind: RestoreNoSessionStarted}
	}
	return baseline, nil
}
```

**Step 4: Run tests**

Run: `go test -race ./internal/sessionruntime/`
Expected: PASS.

**Step 5: Commit**

```bash
make fmt && git add internal/sessionruntime/
git commit -m "feat(sessionruntime): latest-adopted configuration baseline scanner"
```

---

### Task 10: Wire assessment, decision, adoption, and rejection into restore

This is the integration task. Read `internal/sessionruntime/restore_constructor.go`
in full first — especially the documented sequence at lines 25-39, the
fingerprint seam near line 128/229, `RestoreStarted` at ~304, `RestoreDone` at
~416, and `recordErrored` at ~172-195.

**Files:**
- Modify: `internal/sessionruntime/restore_constructor.go`
- Modify: `internal/sessionruntime/restore.go` (`checkFingerprint`,
  `restoredContextDisposition`, `checkAgentName` call sites)
- Modify: `internal/sessionruntime/session.go` (~158: replace the
  `allowConfigMismatch bool` field with the decider)
- Test: the package's existing restore integration tests plus new cases

**Step 1: Write the failing tests** (in the existing restore test file, using
its session-store fixtures; one test per behavior):

1. `TestRestoreAcceptsInfoDriftAndAdopts` — build a session, restore with a
   changed model (Info): restore succeeds; journal contains
   `ConfigurationAdopted` with `Epoch: 2`, `Source: policy`, one Info change,
   appended after `RestoreStarted` and before `RestoreDone`.
2. `TestRestoreRejectsWarnDriftHeadless` — restore with a changed workspace
   root and the default decider: restore fails with `*RestoreRejectedError`
   (via `errors.As`); journal contains `RestoreErrored` and NO
   `ConfigurationAdopted`; the lease is released (a second restore can acquire).
3. `TestRestoreNoDriftAppendsNoEpoch` — identical config, full-manifest
   baseline: no `ConfigurationAdopted` in the journal.
4. `TestRestoreLegacyBaselineUpgrades` — session created with legacy-only
   `Config` (no manifest), restored with identical config: restore succeeds and
   appends a baseline-upgrade `ConfigurationAdopted` (Epoch 2, Source policy,
   empty Warn set). A second restore appends nothing (baseline now complete).
5. `TestRestoreShimAcceptsWarn` — `WithAllowConfigMismatch()` + workspace
   change: succeeds; adoption recorded with `Source: policy`.
6. `TestRestoreDeciderErrorRejects` — a stub decider returning an error:
   restore fails, `RestoreErrored` appended.
7. `TestRestoreAgentNameMismatchIsWarnDrift` — persisted root AgentName differs
   from configured: default decider rejects; `AcceptAllDecider` accepts (this
   replaces the old boolean-shared path).
8. `TestRestoreEpochMonotonic` — journal already at epoch 3: next adoption is 4.

**Step 2: Run to verify failures**

Run: `go test -race ./internal/sessionruntime/ -run TestRestore -v`
Expected: new tests FAIL; existing ones PASS.

**Step 3: Implement, in this order**

1. `session.go`: replace `allowConfigMismatch bool` with
   `restoreDecider RestoreDecider` (defaulted to the policy decider);
   `WithAllowConfigMismatch()` (command_journal.go:135) sets the accept-all
   decider. Keep the exported option names working.
2. In the constructor, replace the `firstConfigFingerprint` +
   `checkFingerprint` seam with:

```go
baseline, err := latestAdoptedBaseline(all)
// ... error handling as today ...
candidate := frozenManifestForRestore(...) // the Task 7 frozen manifest, same inputs as the frozen fingerprint today
assessment := event.AssessDrift(baseline.Manifest, candidate)
// Fold the agent-name check into the assessment instead of its own boolean gate:
if persistedName != configuredName {
	assessment.Changes = append(assessment.Changes, event.DriftChange{
		Category: event.DriftAgentKind, Field: "agent_name",
		Old: string(persistedName), New: string(configuredName),
		Severity: event.DriftWarn,
	})
}
decision, err := s.restoreDecider.DecideRestore(ctx, assessment)
if err != nil || !decision.Accept {
	rejection := &RestoreRejectedError{Assessment: assessment, Source: decision.Source}
	// recordErrored appends RestoreErrored and releases the lease, as for any
	// restore failure today.
	return recordErroredPath(rejection)
}
```

3. After `RestoreStarted` is appended (constructor ~line 304), append the
   adoption when needed:

```go
if len(assessment.Changes) > 0 || assessment.BaselineUpgrade {
	adopted := event.ConfigurationAdopted{
		// Header/IDs stamped the same way RestoreStarted's are here.
		SessionID:           sessionID,
		Epoch:               baseline.Epoch + 1,
		PreviousFingerprint: previousFingerprintOf(baseline), // empty for legacy projections
		AdoptedFingerprint:  candidate.Fingerprint(),
		Manifest:            candidate,
		Drift:               assessment.Changes,
		Source:              decision.Source,
		Actor:               decision.Actor,
		Message:             decision.Message,
	}
	// Append via the same journal appender RestoreStarted uses; a failed
	// append aborts the restore (recordErrored path) — the session must not
	// come up under an unrecorded configuration.
}
```

4. `restoredContextDisposition` (restore.go:160): reimplement on the new seam —
   context is stale iff the restore was accepted *with* drift (any change
   present), preserving today's semantics ("merely enabling the override does
   not discard matching state").
5. `checkFingerprint` and `checkAgentName` become unused by the constructor;
   delete them and their direct tests only if nothing else references them
   (grep first) — otherwise leave with Deprecated comments.

**Step 4: Run the full package plus the public surface**

Run: `go test -race ./internal/sessionruntime/ ./pkg/rig/ ./pkg/session/ ./pkg/event/`
Expected: PASS, including all 8 new tests.

**Step 5: Commit**

```bash
make fmt && git add internal/sessionruntime/ pkg/
git commit -m "feat(sessionruntime): drift-assessed restore with durable adoption"
```

---

### Task 11: Lease-loss and multi-epoch integration cases

**Files:**
- Test: `internal/sessionruntime/` restore test file (append)

**Step 1: Write the failing tests**

1. `TestRestoreAdoptionLeaseLost` — using the package's existing lease-loss
   fixture (grep for `JournalLeaseLostError` in tests): force the lease to move
   between `RestoreStarted` and the adoption append; restore must fail and the
   session must not come up.
2. `TestRestoreMultiEpochBaselineSelection` — journal with epochs 1..3 where
   epoch 3's manifest matches the live config but epoch 1's does not: restore
   succeeds with no new adoption (proves the baseline is the latest, not the
   first — the design's core behavioral change).

**Step 2: Run to verify failure, implement any fix, re-run**

Run: `go test -race ./internal/sessionruntime/ -run 'TestRestoreAdoptionLeaseLost|TestRestoreMultiEpoch' -v`
Expected: these may pass immediately if Task 10 is correct — that is fine;
they pin the invariants either way. If `TestRestoreAdoptionLeaseLost` fails,
the adoption append is not using the lease-checked journal path — fix that in
Task 10's code, not by weakening the test.

**Step 3: Commit**

```bash
git add internal/sessionruntime/
git commit -m "test(sessionruntime): pin adoption lease safety and latest-baseline selection"
```

---

### Task 12: Full verification sweep

**Step 1: Format and vet**

Run: `make fmt && go vet ./...`
Expected: no output/errors.

**Step 2: Full test suite**

Run: `go test -race ./...`
Expected: PASS everywhere.

**Step 3: Build**

Run: `CGO_ENABLED=0 go build -trimpath ./...`
Expected: clean build.

**Step 4: Lint (targeted, NOT `make secure` — see ground rules)**

Run: `make lint` if it completes without touching go.sum; otherwise run
staticcheck/gosec directly on changed packages. If anything expanded `go.sum`,
`git checkout -- go.sum` before committing.

**Step 5: Update the design doc status and commit**

Edit `docs/plans/2026-07-16-session-versioning-migration-design.md` status line
to note Phase 1 implemented (list the landed pieces; note tool schema digests
if deferred in Task 7). Then:

```bash
git add -A && git commit -m "feat: session configuration adoption (Phase 1)

Implements docs/plans/2026-07-16-session-versioning-migration-design.md
Phase 1: ConfigManifest + canonical fingerprint, typed drift assessment,
RestoreDecider with fail-secure default, ConfigurationAdopted epochs with
baseline upgrades, and typed UnsupportedSchemaError on future event schema
versions."
```

---

## Deliberately out of scope (do not build)

- The Phase 2 migration framework (`pkg/migration`, `Migrator`, migration
  events) — reserved names only.
- Blob-backed manifests (open question 2) — inline only; the validation bounds
  in Task 6 keep the event bounded.
- Tool input/output schema digests if `tool.InvokableTool.Info()` does not
  already expose schemas (Task 7 note) — file a follow-up instead.
- Interactive deciders for the TUI/serve — they implement `RestoreDecider`
  downstream; only the seam ships here.
- Removing `ConfigMismatchError`, `checkFingerprint`, or the
  `WithAllowConfigMismatch` option — deprecate, don't delete.
