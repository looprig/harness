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
