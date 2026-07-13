package tools

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
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

// TestSkillCapabilities is a compile-time + behavioral assertion of the Skill
// capability surface under the enforced-gate model. Skill is ALWAYS an
// InvokableTool + Auditable, and ALSO a Preparer + EffectChecker +
// PermissionPrompter (so a workspace-enabled instance is gated). The capability
// methods are present on the type unconditionally, but for an EMBEDDED-ONLY
// instance they must BEHAVE as auto-approve, side-effect-free: CheckEffect for an
// embedded name yields handled=false (falls through to HardApprove) and Prepare
// yields a nil artifact (no snapshot, no gate). This pins that the embedded P2
// behavior is unchanged despite the wider type surface.
func TestSkillCapabilities(t *testing.T) {
	t.Parallel()
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	s := NewSkill(loader, identity.AgentName("operator"))

	var _ tool.InvokableTool = s
	var _ tool.Auditable = s
	var _ tool.Preparer = s
	var _ tool.PermissionPrompter = s
	var _ EffectChecker = s

	// Embedded-only behavior must be auto-approve + no artifact.
	if eff, handled := s.CheckEffect(`{"name":"code-style"}`); handled {
		t.Errorf("CheckEffect(embedded) handled = true (eff=%v); want false so it falls through to HardApprove (auto-approve)", eff)
	}
	prepared, err := s.Prepare(context.Background(), uuid.UUID{}, `{"name":"code-style"}`)
	if err != nil {
		t.Errorf("Prepare(embedded) error = %v, want nil", err)
	}
	if prepared != nil {
		t.Errorf("Prepare(embedded) = %v, want nil artifact (embedded handled by loader at exec)", prepared)
	}
}

// TestSkillCheckEffectEmbeddedOnly proves CheckEffect's decision WITHOUT a
// workspace source: an embedded name and an unknown name BOTH yield handled=false
// (auto-approve via HardApprove), and unparseable args also yield handled=false
// (the call auto-approves, then InvokableRun renders the error). No row may pin
// EffectAsk: an embedded-only Skill never gates.
func TestSkillCheckEffectEmbeddedOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		argsJSON string
	}{
		{name: "embedded name falls through to HardApprove", argsJSON: `{"name":"code-style"}`},
		{name: "unknown name falls through (fails secure at result)", argsJSON: `{"name":"secret-skill"}`},
		{name: "empty name falls through", argsJSON: `{"name":""}`},
		{name: "unparseable args falls through", argsJSON: `not json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
			s := NewSkill(loader, identity.AgentName("operator"))

			if eff, handled := s.CheckEffect(tt.argsJSON); handled {
				t.Errorf("CheckEffect(%q) handled = true (eff=%v); embedded-only must never gate", tt.argsJSON, eff)
			}
		})
	}
}

// TestSkillEmbeddedOnlyUnknownAutoApproves proves the embedded-only unknown-name
// path is fail-secure at the RESULT, not the gate: CheckEffect/Prepare do not gate
// (handled=false, nil artifact), and InvokableRun (no artifact in ctx) returns the
// UnknownSkillError string and never a body.
func TestSkillEmbeddedOnlyUnknownAutoApproves(t *testing.T) {
	t.Parallel()
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	s := NewSkill(loader, identity.AgentName("operator"))

	args := `{"name":"not-a-skill"}`

	if eff, handled := s.CheckEffect(args); handled {
		t.Errorf("CheckEffect = handled (eff=%v); want auto-approve fall-through", eff)
	}
	prepared, err := s.Prepare(context.Background(), uuid.UUID{}, args)
	if err != nil {
		t.Fatalf("Prepare error = %v, want nil (no workspace to consult)", err)
	}
	if prepared != nil {
		t.Fatalf("Prepare = %v, want nil artifact", prepared)
	}
	// No artifact in ctx → embedded path → loader miss → UnknownSkillError string.
	res, err := s.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("InvokableRun Go error = %v, want nil", err)
	}
	got := textOf(t, res)
	if !strings.Contains(got, "error:") || !strings.Contains(got, "not-a-skill") {
		t.Errorf("result = %q, want an error string naming the unknown skill", got)
	}
	if strings.Contains(got, skillToolBody) {
		t.Errorf("result = %q leaks a body on the unknown path", got)
	}
}

// TestSkillBuildRequestFailSecure proves BuildRequest is fail-secure when it is
// reached without the workspace snapshot it expects (embedded never gates, so this
// path should not occur in practice — but it must not fabricate a request): a nil,
// wrong-typed, or non-workspace artifact yields an error so the runner falls back
// to a redacted UnknownRequest rather than guessing a SkillLoadRequest.
func TestSkillBuildRequestFailSecure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		prepared tool.PreparedArtifact
	}{
		{name: "nil artifact", prepared: nil},
		{name: "wrong artifact type", prepared: tool.TokenArtifact{Token: "x"}},
		{name: "non-workspace skill artifact", prepared: &tool.SkillArtifact{Workspace: false, Body: "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
			s := NewSkill(loader, identity.AgentName("operator"))

			req, err := s.BuildRequest(`{"name":"x"}`, tt.prepared)
			if err == nil {
				t.Errorf("BuildRequest = (%v, nil), want a fail-secure error", req)
			}
			if req != nil {
				t.Errorf("BuildRequest req = %v, want nil on the fail-secure path", req)
			}
		})
	}
}
