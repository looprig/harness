package session

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

// ConfigFingerprintFields are the swarm-level fingerprint inputs that do NOT live on
// loop.Definition and so cannot be derived by FingerprintFrom alone. The composition root
// (the swarm) injects them via WithConfigFingerprintFields; both the New and Restore
// construction paths merge them onto the loop-derived fingerprint, so a session cannot
// silently resume under a different agent identity, skill-trust mode, or workspace.
//
//   - AgentKind: the swarm + primary agent identity (e.g. "swe:orchestrator").
//   - RuntimeSkills: whether the untrusted, human-gated workspace skill source was on.
//   - WorkspaceRoot: the canonical absolute workspace-root id (filepath.Clean of abs).
//   - AdapterID: the foreign-agent adapter identity (e.g. "claude"); empty for native.
//   - Posture: the foreign agent's non-interactive permission posture (e.g. "default");
//     empty for native.
//
// A zero-valued ConfigFingerprintFields (the default, no option) leaves all fields
// empty, so a non-swarm/native/legacy caller is unaffected and the fingerprint is
// purely the loop-derived one (additive evolution).
type ConfigFingerprintFields struct {
	AgentKind     string
	RuntimeSkills bool
	WorkspaceRoot string
	// AdapterID is the foreign-agent adapter identity the composition root injects
	// (e.g. "claude"). Empty for a native session.
	AdapterID string
	// Posture is the foreign agent's non-interactive permission posture string the
	// composition root injects (e.g. "default", "acceptEdits"). Empty for a native session.
	Posture string
	// NativePermissionPolicyRev is the content digest of the NATIVE permission
	// configuration (allowlist + hard-deny lists + MaxReadBytes + headless mode bits),
	// computed by tools.PolicyFingerprint at the composition root and injected. Empty
	// for a foreign session (which uses Posture) or a caller that does not inject it.
	NativePermissionPolicyRev string
}

// FingerprintFrom derives the stable config fingerprint a session stamps onto its
// SessionStarted from the loop.Definition it ran under. It lives in the session package
// because the session is the layer that both owns the loop.Definition and constructs
// SessionStarted (the event package defines only the value + its equality, and must
// not import loop). The derivation is deterministic — identical config yields an
// Equal fingerprint — and changes when the model, system prompt, or tool set
// changes:
//
//   - ModelID is the Model.Name (the model id) verbatim.
//   - SystemPromptRev is a hex sha256 of the system-prompt text, so a prompt change
//     is detectable without persisting the prompt.
//   - ToolPolicyRev is a hex sha256 over the tool set's stable identity (its sorted,
//     newline-joined tool names), so reordering the registry does not perturb it but
//     adding/removing/renaming a tool does.
//
// The swarm-level fields (AgentKind, RuntimeSkills, WorkspaceRoot) are NOT on
// loop.Definition and are left zero here; the composition root injects them via
// WithConfigFingerprintFields, and the construction paths merge them with
// fingerprintWith. A bare FingerprintFrom (no injection) therefore leaves them empty.
func FingerprintFrom(cfg loop.BoundDefinition) event.ConfigFingerprint {
	return event.ConfigFingerprint{
		ModelID:         cfg.Model().Name,
		SystemPromptRev: hexSHA256(cfg.System()),
		ToolPolicyRev:   toolPolicyRev(cfg.Tools()),
	}
}

// fingerprintWith returns the loop-derived fingerprint with the injected swarm-level
// fields applied. It is the single merge point both New and Restore use to compute the
// LIVE fingerprint, so the stamped (New) and compared-against (Restore) fingerprints
// are derived identically — the restore comparison would spuriously mismatch otherwise.
// Zero-valued fields leave the corresponding loop-derived/empty value unchanged.
func fingerprintWith(cfg loop.BoundDefinition, fields ConfigFingerprintFields) event.ConfigFingerprint {
	fpr := FingerprintFrom(cfg)
	fpr.AgentKind = fields.AgentKind
	fpr.RuntimeSkills = fields.RuntimeSkills
	fpr.WorkspaceRoot = fields.WorkspaceRoot
	fpr.AgentAdapter = fields.AdapterID
	fpr.PermissionPosture = fields.Posture
	fpr.NativePermissionPolicyRev = fields.NativePermissionPolicyRev
	return fpr
}

// hexSHA256 returns the lowercase-hex sha256 digest of s. An empty input yields the
// well-defined digest of the empty string (stable across calls), so the fingerprint
// is always populated and comparable.
func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// toolPolicyRev digests the tool set's stable identity: the sorted, newline-joined
// names of the registered tools. Names are sorted so registry order never changes
// the digest; a tool's Info error excludes only that tool (best-effort — the
// fingerprint is an identity hash for change detection, not a security boundary).
// The digest covers the empty string when no tool reports a name.
func toolPolicyRev(tools []tool.InvokableTool) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return hexSHA256(strings.Join(names, "\n"))
}
