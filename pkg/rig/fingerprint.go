package rig

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// ConfigFingerprintFields are immutable rig-level behavior inputs that are not part
// of a loop.Definition. Define freezes them for both session creation and restoration.
type ConfigFingerprintFields struct {
	AgentKind     string
	RuntimeSkills bool
	WorkspaceRoot string
	// AdapterID identifies a foreign-agent adapter. Empty means native.
	AdapterID string
	// Posture identifies a foreign agent's non-interactive permission posture.
	Posture string
	// NativePermissionPolicyRev is the digest of native permission configuration.
	NativePermissionPolicyRev string
	// ExternalCapabilityRev is the digest of the identity of external capabilities
	// the composition root attached to the session — tools served by processes
	// Harness does not own, such as MCP servers. Empty means none, which is what
	// keeps it additive for every rig that attaches nothing.
	//
	// The rig neither computes nor interprets it: only the composition root knows
	// what it attached. The canonical producer is github.com/looprig/mcp's
	// mcpharness.Manager.ConfigDigest, taken after the Manager has started.
	ExternalCapabilityRev string

	// The fields below are carried ONLY by the richer event.ConfigManifest; the
	// legacy event.ConfigFingerprint has no home for them. Each is optional and
	// zero-safe: a caller that supplies nothing leaves the manifest field empty,
	// which keeps the manifest additive against a journal that predates it.

	// WorkspaceTrust is an opaque, secret-free label for the workspace's trust
	// posture (e.g. "trusted"/"untrusted"). Empty means unspecified.
	WorkspaceTrust string
	// PermissionStrictness is the ordered native-permission posture level; higher is
	// stricter. Zero means unknown (drift assessment fails secure). It complements
	// NativePermissionPolicyRev, which is the digest-only identity.
	PermissionStrictness event.StrictnessLevel
	// ConfinementRev is a content digest of the confinement (sandbox) configuration.
	// Empty means none. Harness compares it, never parses it.
	ConfinementRev string
	// ConfinementStrictness is the ordered confinement posture level; higher is
	// stricter. Zero means unknown.
	ConfinementStrictness event.StrictnessLevel
	// AppFields are application-defined, secret-free compatibility fields the
	// composition root attaches. Canonically encoded in sorted key order by the
	// manifest. Nil means none.
	AppFields map[string]string
}

// FingerprintFrom derives the stable, secret-free behavior fingerprint of a bound loop.
func FingerprintFrom(definition loop.BoundDefinition) event.ConfigFingerprint {
	return event.ConfigFingerprint{
		ModelID:         definition.Model().Name,
		SystemPromptRev: hexSHA256(definition.EffectiveSystem()),
		ToolPolicyRev:   toolPolicyRev(definition.Tools()),
	}
}

func fingerprintWith(definition loop.BoundDefinition, fields ConfigFingerprintFields) event.ConfigFingerprint {
	fingerprint := FingerprintFrom(definition)
	fingerprint.AgentKind = fields.AgentKind
	fingerprint.RuntimeSkills = fields.RuntimeSkills
	fingerprint.WorkspaceRoot = fields.WorkspaceRoot
	fingerprint.AgentAdapter = fields.AdapterID
	fingerprint.PermissionPosture = fields.Posture
	fingerprint.NativePermissionPolicyRev = fields.NativePermissionPolicyRev
	fingerprint.ExternalCapabilityRev = fields.ExternalCapabilityRev
	return fingerprint
}

func fingerprintWithTopology(definition loop.BoundDefinition, fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string) event.ConfigFingerprint {
	fingerprint := fingerprintWith(definition, fields)
	fingerprint.TopologyRev = topologyRevision(definitions, primers, active)
	return fingerprint
}

func fingerprintWithTopologyAndHustles(definition loop.BoundDefinition, fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) event.ConfigFingerprint {
	fingerprint := fingerprintWith(definition, fields)
	fingerprint.TopologyRev = topologyRevisionWithHustles(definitions, primers, active, hustles, limits)
	return fingerprint
}

// frozenFingerprint is the rig-time compatibility projection. It depends only on
// immutable definitions and scalar rig fields, so restore can compare it immediately
// after replay without constructing workspace or loop collaborators.
func frozenFingerprint(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string) event.ConfigFingerprint {
	initial := frozenInitial(definitions, active)
	toolNames := frozenToolNames(definitions, active)
	return event.ConfigFingerprint{
		TopologyRev:               topologyRevision(definitions, primers, active),
		AgentKind:                 fields.AgentKind,
		ModelID:                   initial.Model.Name,
		SystemPromptRev:           hexSHA256(initial.EffectiveSystem),
		ToolPolicyRev:             hexSHA256(strings.Join(toolNames, "\n")),
		RuntimeSkills:             fields.RuntimeSkills,
		WorkspaceRoot:             fields.WorkspaceRoot,
		AgentAdapter:              fields.AdapterID,
		PermissionPosture:         fields.Posture,
		NativePermissionPolicyRev: fields.NativePermissionPolicyRev,
		ExternalCapabilityRev:     fields.ExternalCapabilityRev,
	}
}

// frozenInitial resolves the active loop's restore-time InitialFingerprint from the
// immutable definitions alone. The zero value stands in when no definition matches.
func frozenInitial(definitions []loop.Definition, active string) loop.InitialFingerprint {
	for _, definition := range definitions {
		if string(definition.Name()) == active {
			return definition.FingerprintInitial()
		}
	}
	return loop.InitialFingerprint{}
}

// frozenToolNames is the SINGLE source of the restore-time tool-name list shared by
// frozenFingerprint (its ToolPolicyRev) and frozenManifest (its name-only Tools), so
// the two can never drift apart. It reads the active loop's produced tool names,
// appends the literal "Subagent" when the active loop is delegate-capable (that
// built-in is injected structurally at Bind, without constructing the tool or its
// collaborators), and returns them sorted. The returned slice is a fresh copy.
func frozenToolNames(definitions []loop.Definition, active string) []string {
	initial := frozenInitial(definitions, active)
	toolNames := append([]string(nil), initial.ToolNames...)
	for _, definition := range definitions {
		if string(definition.Name()) == active && len(definition.Delegates()) > 0 {
			toolNames = append(toolNames, "Subagent")
			break
		}
	}
	sort.Strings(toolNames)
	return toolNames
}

// frozenManifest is the rig-time compatibility projection of the richer
// event.ConfigManifest, the manifest counterpart to frozenFingerprint. It depends
// only on immutable definitions and scalar rig fields, so restore can assemble it
// before constructing workspace or loop collaborators, and it draws every shared
// field from the SAME source frozenFingerprint uses — so a manifest and the legacy
// fingerprint of the same session always agree (see TestManifestMatchesFingerprint).
func frozenManifest(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string) event.ConfigManifest {
	initial := frozenInitial(definitions, active)
	toolNames := frozenToolNames(definitions, active)
	tools := make([]event.ToolManifestEntry, len(toolNames))
	for index, name := range toolNames {
		// TODO(phase-1 follow-up): tool schema digests require exposing schemas on
		// both the live and restore paths; empty for now (names-only parity).
		tools[index] = event.ToolManifestEntry{Name: name}
	}
	// Own the AppFields map: the manifest is embedded in SessionStarted and read during
	// restore, so it must not alias the caller's map (a later mutation would change stored
	// fingerprints and can data-race the restore reader). Preserve nil-ness — a nil input
	// stays nil, never an empty map: reflect.DeepEqual(nil, empty) is false and the
	// `omitzero` JSON tag serializes nil as absent but empty as `{}`, so allocating an empty
	// map where nil was expected would break round-trip/DeepEqual equality.
	var appFields map[string]string
	if fields.AppFields != nil {
		appFields = make(map[string]string, len(fields.AppFields))
		for k, v := range fields.AppFields {
			appFields[k] = v
		}
	}
	return event.ConfigManifest{
		SchemaVersion:             event.ManifestSchemaVersion,
		AgentKind:                 fields.AgentKind,
		TopologyRev:               topologyRevision(definitions, primers, active),
		ModelID:                   initial.Model.Name,
		SystemPromptRev:           hexSHA256(initial.EffectiveSystem),
		Tools:                     tools,
		RuntimeSkills:             fields.RuntimeSkills,
		WorkspaceRoot:             fields.WorkspaceRoot,
		WorkspaceTrust:            fields.WorkspaceTrust,
		AgentAdapter:              fields.AdapterID,
		PermissionPosture:         fields.Posture,
		NativePermissionPolicyRev: fields.NativePermissionPolicyRev,
		PermissionStrictness:      fields.PermissionStrictness,
		ConfinementRev:            fields.ConfinementRev,
		ConfinementStrictness:     fields.ConfinementStrictness,
		ExternalCapabilityRev:     fields.ExternalCapabilityRev,
		AppFields:                 appFields,
	}
}

func frozenFingerprintWithHustles(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) event.ConfigFingerprint {
	fingerprint := frozenFingerprint(fields, definitions, primers, active)
	if len(hustles) > 0 {
		fingerprint.TopologyRev = topologyRevisionWithHustles(definitions, primers, active, hustles, limits)
	}
	return fingerprint
}

// frozenManifestWithHustles is the manifest counterpart to
// frozenFingerprintWithHustles: it assembles the plain frozenManifest and, when
// hustles are present, overrides TopologyRev with the SAME hustle-aware revision the
// fingerprint uses. Stamping this manifest therefore keeps Manifest.TopologyRev
// byte-equal to the fingerprint's, so drift assessment never sees phantom topology
// drift on restore (see TestHustleBoundAndFrozenTopologyFingerprintEquivalent).
func frozenManifestWithHustles(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) event.ConfigManifest {
	manifest := frozenManifest(fields, definitions, primers, active)
	if len(hustles) > 0 {
		manifest.TopologyRev = topologyRevisionWithHustles(definitions, primers, active, hustles, limits)
	}
	return manifest
}

func topologyRevisionWithHustles(definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) string {
	copyOfLimits := limits
	return canonicalTopologyRevision(topologyRevisionInput{
		definitions: definitions,
		primers:     primers,
		active:      active,
		hustles:     hustles,
		limits:      &copyOfLimits,
	})
}

type topologyRevisionInput struct {
	definitions []loop.Definition
	primers     []string
	active      string
	hustles     []hustle.Definition
	limits      *HustleLimits
}

func canonicalTopologyRevision(input topologyRevisionInput) string {
	var material strings.Builder
	writeLoopTopology(&material, input.definitions, input.primers, input.active)
	legacyRevision := hexSHA256(material.String())
	if input.limits == nil {
		return legacyRevision
	}
	rows := make([]hustleTopologyRow, len(input.hustles))
	for index, definition := range input.hustles {
		rows[index] = hustleTopologyRow{Name: definition.Name(), PolicyRevision: definition.PolicyRevision()}
	}
	return hexSHA256Bytes(canonicalHustleTopologyMaterial(legacyRevision, rows, *input.limits))
}

type hustleTopologyRow struct {
	Name           hustle.Name
	PolicyRevision string
}

func canonicalHustleTopologyMaterial(legacyRevision string, rows []hustleTopologyRow, limits HustleLimits) []byte {
	const encodingDomain string = "looprig/rig/hustle-topology/v1"
	ordered := append([]hustleTopologyRow(nil), rows...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Name == ordered[j].Name {
			return ordered[i].PolicyRevision < ordered[j].PolicyRevision
		}
		return ordered[i].Name < ordered[j].Name
	})
	material := appendCanonicalString(nil, encodingDomain)
	material = appendCanonicalString(material, legacyRevision)
	material = binary.BigEndian.AppendUint64(material, uint64(len(ordered)))
	for _, row := range ordered {
		material = appendCanonicalString(material, string(row.Name))
		material = appendCanonicalString(material, row.PolicyRevision)
	}
	material = appendCanonicalInt64(material, int64(limits.BlockingConcurrent))
	material = appendCanonicalInt64(material, int64(limits.BlockingQueued))
	material = appendCanonicalInt64(material, int64(limits.BackgroundConcurrent))
	material = appendCanonicalInt64(material, int64(limits.BackgroundQueued))
	material = appendCanonicalInt64(material, int64(limits.AuditTimeout))
	material = appendCanonicalInt64(material, int64(limits.FinalizationTimeout))
	return appendCanonicalInt64(material, int64(limits.WorkerDrainTimeout))
}

func appendCanonicalString(material []byte, value string) []byte {
	material = binary.BigEndian.AppendUint64(material, uint64(len(value)))
	return append(material, value...)
}

func appendCanonicalInt64(material []byte, value int64) []byte {
	// #nosec G115 -- canonical encoding preserves the signed value's two's-complement bit pattern.
	return binary.BigEndian.AppendUint64(material, uint64(value))
}

func topologyRevision(definitions []loop.Definition, primers []string, active string) string {
	return canonicalTopologyRevision(topologyRevisionInput{
		definitions: definitions,
		primers:     primers,
		active:      active,
	})
}

func writeLoopTopology(material *strings.Builder, definitions []loop.Definition, primers []string, active string) {
	orderedDefinitions := append([]loop.Definition(nil), definitions...)
	sort.Slice(orderedDefinitions, func(i, j int) bool { return orderedDefinitions[i].Name() < orderedDefinitions[j].Name() })
	for _, candidate := range orderedDefinitions {
		material.WriteString("loop:")
		material.WriteString(string(candidate.Name()))
		material.WriteByte('\n')
		material.WriteString("policy:")
		material.WriteString(candidate.PolicyRevision())
		material.WriteByte('\n')
		delegates := candidate.Delegates()
		sort.Slice(delegates, func(i, j int) bool { return delegates[i] < delegates[j] })
		for _, delegate := range delegates {
			material.WriteString("delegate:")
			material.WriteString(string(delegate))
			material.WriteByte('\n')
		}
	}
	for _, primer := range primers {
		material.WriteString("primer:")
		material.WriteString(primer)
		material.WriteByte('\n')
	}
	material.WriteString("active:")
	material.WriteString(active)
}

func hexSHA256(value string) string {
	return hexSHA256Bytes([]byte(value))
}

func hexSHA256Bytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func toolPolicyRev(tools []tool.InvokableTool) string {
	names := make([]string, 0, len(tools))
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return hexSHA256(strings.Join(names, "\n"))
}
