package rig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/looprig/harness/pkg/event"
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

// frozenFingerprint is the rig-time compatibility projection. It depends only on
// immutable definitions and scalar rig fields, so restore can compare it immediately
// after replay without constructing workspace or loop collaborators.
func frozenFingerprint(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string) event.ConfigFingerprint {
	return event.ConfigFingerprint{
		TopologyRev:               topologyRevision(definitions, primers, active),
		AgentKind:                 fields.AgentKind,
		RuntimeSkills:             fields.RuntimeSkills,
		WorkspaceRoot:             fields.WorkspaceRoot,
		AgentAdapter:              fields.AdapterID,
		PermissionPosture:         fields.Posture,
		NativePermissionPolicyRev: fields.NativePermissionPolicyRev,
	}
}

func topologyRevision(definitions []loop.Definition, primers []string, active string) string {
	var material strings.Builder
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
	return hexSHA256(material.String())
}

func hexSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
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
