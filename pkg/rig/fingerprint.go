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
	initial := loop.InitialFingerprint{}
	for _, definition := range definitions {
		if string(definition.Name()) == active {
			initial = definition.FingerprintInitial()
			break
		}
	}
	toolNames := append([]string(nil), initial.ToolNames...)
	for _, definition := range definitions {
		if string(definition.Name()) == active && len(definition.Delegates()) > 0 {
			// Delegate-capable loops receive this built-in structurally at Bind.
			// Include its stable model-facing name without constructing the tool or
			// its permission/controller collaborators.
			toolNames = append(toolNames, "Subagent")
			break
		}
	}
	sort.Strings(toolNames)
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
	}
}

func frozenFingerprintWithHustles(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) event.ConfigFingerprint {
	fingerprint := frozenFingerprint(fields, definitions, primers, active)
	if len(hustles) > 0 {
		fingerprint.TopologyRev = topologyRevisionWithHustles(definitions, primers, active, hustles, limits)
	}
	return fingerprint
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
