package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

// FingerprintFrom derives the stable config fingerprint a session stamps onto its
// SessionStarted from the loop.Config it ran under. It lives in the session package
// because the session is the layer that both owns the loop.Config and constructs
// SessionStarted (the event package defines only the value + its equality, and must
// not import loop). The derivation is deterministic — identical config yields an
// Equal fingerprint — and changes when the model, system prompt, or tool set
// changes:
//
//   - ModelID is the model spec id verbatim.
//   - SystemPromptRev is a hex sha256 of the system-prompt text, so a prompt change
//     is detectable without persisting the prompt.
//   - ToolPolicyRev is a hex sha256 over the tool set's stable identity (its sorted,
//     newline-joined tool names), so reordering the registry does not perturb it but
//     adding/removing/renaming a tool does.
//
// AgentKind is left empty: loop.Config does not expose an agent kind today.
//
// TODO: set AgentKind once the agent passes it through config.
func FingerprintFrom(cfg loop.Config) event.ConfigFingerprint {
	return event.ConfigFingerprint{
		ModelID:         cfg.Model.Model,
		SystemPromptRev: hexSHA256(cfg.Model.System),
		ToolPolicyRev:   toolPolicyRev(cfg.Tools),
	}
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
func toolPolicyRev(ts loop.ToolSet) string {
	names := make([]string, 0, len(ts.Registry))
	for _, t := range ts.Registry {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return hexSHA256(strings.Join(names, "\n"))
}
