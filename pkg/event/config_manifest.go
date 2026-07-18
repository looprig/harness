package event

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
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
	PermissionPosture         string          `json:"permission_posture,omitzero"`
	NativePermissionPolicyRev string          `json:"native_permission_policy_rev,omitzero"`
	PermissionStrictness      StrictnessLevel `json:"permission_strictness,omitzero"`
	ConfinementRev            string          `json:"confinement_rev,omitzero"`
	ConfinementStrictness     StrictnessLevel `json:"confinement_strictness,omitzero"`
	ExternalCapabilityRev     string          `json:"external_capability_rev,omitzero"`
	// AppFields are application-defined, secret-free compatibility fields.
	// Canonically encoded in sorted key order.
	AppFields map[string]string `json:"app_fields,omitzero"`
	// legacyToolPolicyRev carries a legacy baseline's names-only tool digest
	// through ManifestFromLegacy. Never persisted, never canonical.
	legacyToolPolicyRev string `json:"-"`
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
