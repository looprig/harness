package session

import (
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

func TestConfigMismatchErrorNamesChangedFingerprintFields(t *testing.T) {
	err := (&ConfigMismatchError{
		Persisted: event.ConfigFingerprint{
			AgentKind:       "swe:operator",
			ModelID:         "model-a",
			SystemPromptRev: "old-prompt",
			WorkspaceRoot:   "/work/swe",
		},
		Live: event.ConfigFingerprint{
			TopologyRev:     "new-topology",
			AgentKind:       "coderig:operator",
			ModelID:         "model-a",
			SystemPromptRev: "new-prompt",
			WorkspaceRoot:   "exclusive:/work/coderig",
		},
	}).Error()

	for _, want := range []string{
		"topology",
		`agent kind ("swe:operator" -> "coderig:operator")`,
		"system prompt",
		`workspace root ("/work/swe" -> "exclusive:/work/coderig")`,
	} {
		if !strings.Contains(err, want) {
			t.Errorf("ConfigMismatchError.Error() = %q, want changed field %q", err, want)
		}
	}
	if strings.Contains(err, "model") {
		t.Errorf("ConfigMismatchError.Error() = %q, must not report unchanged model", err)
	}
}

// TestConfigMismatchErrorNamesExternalCapabilityDrift covers the field an
// application's MCP configuration lands in.
//
// It is worth its own test because MCP drift is the one case that arrives ALONE:
// every other Rev here moves when a developer edits the rig, and those edits tend
// to move several at once, whereas a third-party server changing its toolset
// moves this and nothing else. Before it was named, that restore reported
// "changed fields: " — a refusal that told an operator a configuration they did
// not touch had changed, and then declined to say which.
func TestConfigMismatchErrorNamesExternalCapabilityDrift(t *testing.T) {
	err := (&ConfigMismatchError{
		Persisted: event.ConfigFingerprint{ExternalCapabilityRev: "digest-of-yesterdays-servers"},
		Live:      event.ConfigFingerprint{ExternalCapabilityRev: "digest-of-todays-servers"},
	}).Error()

	if !strings.Contains(err, "external capability") {
		t.Errorf("ConfigMismatchError.Error() = %q, want it to name the changed external capability", err)
	}
	// The digests themselves are not an operator's business, and are not
	// something they could act on if they were.
	if strings.Contains(err, "digest-of-todays-servers") {
		t.Errorf("ConfigMismatchError.Error() = %q, must not print the digest itself", err)
	}
}
