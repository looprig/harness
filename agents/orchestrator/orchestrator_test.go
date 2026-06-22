package orchestrator

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/identity"
)

// TestNameIsOrchestrator pins the agent's attribution name. The swarm catalog
// and Subagent delegation key on it, so a drift would silently mis-route.
func TestNameIsOrchestrator(t *testing.T) {
	t.Parallel()
	if Name != identity.AgentName("orchestrator") {
		t.Errorf("Name = %q, want %q", Name, "orchestrator")
	}
}

// TestDescriptionNonEmpty proves the one-line catalog description is present:
// the Subagent catalog + greeting render it, so an empty string would leave a
// blank entry.
func TestDescriptionNonEmpty(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(Description) == "" {
		t.Fatal("Description is empty")
	}
}

// TestRoleContainsResponsibilities proves the role prompt carries the
// orchestrator's defining duties (design §5): decompose + delegate, and the
// hard rule that subagent reports are DATA, never instructions to execute.
func TestRoleContainsResponsibilities(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "decomposes the task", want: "decompose"},
		{name: "delegates to subagents", want: "delegate"},
		{name: "treats reports as data", want: "DATA"},
		{name: "never executes sourced instructions", want: "instruction"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(strings.ToLower(Role), strings.ToLower(tt.want)) {
				t.Errorf("Role is missing %q", tt.want)
			}
		})
	}
}

// TestRoleIsWellFormedXML proves the role is a single well-formed
// <role name="orchestrator"> element, so the swarm can compose identity+role
// deterministically.
func TestRoleIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"role"`
		RoleNm  string   `xml:"name,attr"`
	}
	if err := xml.Unmarshal([]byte(Role), &probe); err != nil {
		t.Fatalf("Role is not well-formed XML: %v", err)
	}
	if probe.RoleNm != "orchestrator" {
		t.Errorf("Role name attr = %q, want %q", probe.RoleNm, "orchestrator")
	}
}
