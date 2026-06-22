package tools

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/tool"
)

// skillToolBody is the markdown body of the well-formed fixture skill, returned
// verbatim by Load (the loader strips the frontmatter and returns only the body).
const skillToolBody = "# Body\n\nApply the checklist.\n"

// newSkillToolFS builds an in-memory skill tree shaped like the embedded
// production tree (skills/<name>/SKILL.md). It mirrors newTestSkillFS but is
// local to this file so the Skill-tool tests are self-contained.
func newSkillToolFS() fstest.MapFS {
	wellFormed := "---\n" +
		"name: code-style\n" +
		"description: A short checklist.\n" +
		"---\n" +
		skillToolBody
	return fstest.MapFS{
		"skills/code-style/SKILL.md": {Data: []byte(wellFormed)},
	}
}

// skillToolAllow scopes the operator agent to the code-style skill (and nothing
// else), so an unauthorized name fails the loader's per-agent gate.
func skillToolAllow() map[identity.AgentName]map[string]struct{} {
	return map[identity.AgentName]map[string]struct{}{
		identity.AgentName("operator"): {"code-style": {}},
	}
}

// TestSkillInfo pins the tool name (it MUST be "Skill" — the name the wiring lists
// in HardApprove so the tool auto-approves) and a non-empty description + schema.
func TestSkillInfo(t *testing.T) {
	t.Parallel()

	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	s := NewSkill(loader, identity.AgentName("operator"))

	info, err := s.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "Skill" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "Skill")
	}
	if strings.TrimSpace(info.Desc) == "" {
		t.Error("Info().Desc is empty")
	}
	if len(info.Schema) == 0 {
		t.Error("Info().Schema is empty")
	}
}

// TestSkillInvokableRun proves the tool decodes {name}, calls the loader scoped to
// its bound agent, and renders every outcome as a tool-result string (never a Go
// error). Happy path returns the body verbatim; an unknown/unauthorized name and a
// malformed-args document return an error string and never the body.
func TestSkillInvokableRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		argsJSON    string
		wantText    string   // exact result when set
		wantContain []string // substrings the result must contain
		wantNoBody  bool     // result must NOT contain the skill body
	}{
		{
			name:     "authorized name returns the body verbatim",
			argsJSON: `{"name":"code-style"}`,
			wantText: skillToolBody,
		},
		{
			name:        "unauthorized name returns an error string",
			argsJSON:    `{"name":"secret-skill"}`,
			wantContain: []string{"error:", "secret-skill"},
			wantNoBody:  true,
		},
		{
			name:        "empty name returns an error string",
			argsJSON:    `{"name":""}`,
			wantContain: []string{"error:"},
			wantNoBody:  true,
		},
		{
			name:        "non-object args returns an error string",
			argsJSON:    `not json`,
			wantContain: []string{"error:"},
			wantNoBody:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
			s := NewSkill(loader, identity.AgentName("operator"))

			res, err := s.InvokableRun(context.Background(), tt.argsJSON)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v, want nil (failures are tool-result strings)", err)
			}
			got := textOf(t, res)

			if tt.wantText != "" && got != tt.wantText {
				t.Errorf("result = %q, want %q", got, tt.wantText)
			}
			for _, sub := range tt.wantContain {
				if !strings.Contains(got, sub) {
					t.Errorf("result = %q, want it to contain %q", got, sub)
				}
			}
			if tt.wantNoBody && strings.Contains(got, skillToolBody) {
				t.Errorf("result = %q leaks the skill body on an error path", got)
			}
		})
	}
}

// TestSkillAuditSummary proves the audit summary names the skill ONLY (never the
// body), and degrades to a generic, body-free summary on unparsable args.
func TestSkillAuditSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		argsJSON string
		want     string
	}{
		{name: "named skill", argsJSON: `{"name":"code-style"}`, want: "Skill code-style"},
		{name: "empty name", argsJSON: `{"name":""}`, want: "Skill (unparsable args)"},
		{name: "non-object args", argsJSON: `not json`, want: "Skill (unparsable args)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
			s := NewSkill(loader, identity.AgentName("operator"))

			got := s.AuditSummary(tt.argsJSON)
			if got != tt.want {
				t.Errorf("AuditSummary(%q) = %q, want %q", tt.argsJSON, got, tt.want)
			}
			// The body must never reach the audit event.
			if strings.Contains(got, skillToolBody) {
				t.Errorf("AuditSummary leaked the skill body: %q", got)
			}
		})
	}
}

// TestSkillCapabilities is a compile-time + behavioral assertion that Skill is an
// InvokableTool and Auditable, and is NOT a PermissionPrompter (it auto-approves
// via being named in HardApprove, the same class as ReadFile/Subagent).
func TestSkillCapabilities(t *testing.T) {
	t.Parallel()
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	s := NewSkill(loader, identity.AgentName("operator"))
	var _ tool.InvokableTool = s
	var _ tool.Auditable = s
	if _, isPrompter := interface{}(s).(tool.PermissionPrompter); isPrompter {
		t.Error("Skill implements PermissionPrompter; it must auto-approve via HardApprove, not prompt")
	}
}
