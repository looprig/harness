package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ciram-co/looprig/pkg/identity"
)

// newTestSkillFS builds an in-memory skill tree shaped exactly like the embedded
// production tree (skills/<name>/SKILL.md) using stdlib testing/fstest. It does
// NOT import swarms/swe — keeping the tools test free of any reverse dependency
// on the embed package (swarms/swe imports tools, so importing it here would be a
// cycle). The well-formed entries carry a `---`-fenced frontmatter block plus a
// markdown body so the parser exercises its real split path.
func newTestSkillFS() fstest.MapFS {
	wellFormed := "---\n" +
		"name: code-style\n" +
		"description: A short checklist.\n" +
		"---\n" +
		"# Body\n\nApply the checklist.\n"

	// malformed: no opening fence at all -> parser rejects fail-secure.
	malformed := "no frontmatter here\njust a body\n"

	// oversize: a single body larger than maxSkillBytes -> rejected before parse.
	oversize := "---\nname: huge\n---\n" + strings.Repeat("x", maxSkillBytes+1)

	return fstest.MapFS{
		"skills/code-style/SKILL.md": {Data: []byte(wellFormed)},
		"skills/broken/SKILL.md":     {Data: []byte(malformed)},
		"skills/huge/SKILL.md":       {Data: []byte(oversize)},
		// Note: "ghost" is allowed for an agent below but intentionally has NO
		// file on disk, to exercise the missing-file path.
	}
}

const (
	agentReviewer identity.AgentName = "reviewer"
	agentCoder    identity.AgentName = "coder"
	agentEmpty    identity.AgentName = "empty" // present in the map with an empty set
)

// testAllow is the per-agent allow-map fixture: reviewer may load code-style,
// broken (malformed), huge (oversize), and ghost (allowed but file-missing);
// empty is present but authorized for nothing; coder is absent from the map
// entirely (all denied by absence).
func testAllow() map[identity.AgentName]map[string]struct{} {
	return map[identity.AgentName]map[string]struct{}{
		agentReviewer: {
			"code-style": {},
			"broken":     {},
			"huge":       {},
			"ghost":      {},
		},
		agentEmpty: {},
	}
}

func TestEmbeddedSkillLoaderLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		agent       identity.AgentName
		skill       string
		wantBody    string // exact body when no error expected
		wantUnknown bool   // expect *UnknownSkillError
		wantMalform bool   // expect *MalformedSkillError
		wantMissing bool   // expect *SkillNotFoundError
	}{
		{
			name:     "authorized agent and skill returns body",
			agent:    agentReviewer,
			skill:    "code-style",
			wantBody: "# Body\n\nApply the checklist.\n",
		},
		{
			name:        "skill not in agent set is unauthorized",
			agent:       agentReviewer,
			skill:       "secret-skill",
			wantUnknown: true,
		},
		{
			name:        "agent absent from allow-map denies all",
			agent:       agentCoder,
			skill:       "code-style",
			wantUnknown: true,
		},
		{
			name:        "agent present with empty set denies all",
			agent:       agentEmpty,
			skill:       "code-style",
			wantUnknown: true,
		},
		{
			name:        "empty name is unauthorized",
			agent:       agentReviewer,
			skill:       "",
			wantUnknown: true,
		},
		{
			name:        "path traversal name is unauthorized before any path build",
			agent:       agentReviewer,
			skill:       "../../etc/passwd",
			wantUnknown: true,
		},
		{
			name:        "authorized but file missing is not-found",
			agent:       agentReviewer,
			skill:       "ghost",
			wantMissing: true,
		},
		{
			name:        "authorized but malformed file propagates malformed",
			agent:       agentReviewer,
			skill:       "broken",
			wantMalform: true,
		},
		{
			name:        "authorized but oversize file propagates malformed",
			agent:       agentReviewer,
			skill:       "huge",
			wantMalform: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			loader := NewEmbeddedSkillLoader(newTestSkillFS(), testAllow())
			body, err := loader.Load(context.Background(), tt.agent, tt.skill)

			switch {
			case tt.wantUnknown:
				var ue *UnknownSkillError
				if !errors.As(err, &ue) {
					t.Fatalf("Load() error = %v, want *UnknownSkillError", err)
				}
				if ue.Agent != tt.agent || ue.Name != tt.skill {
					t.Errorf("UnknownSkillError = {Agent:%q Name:%q}, want {Agent:%q Name:%q}",
						ue.Agent, ue.Name, tt.agent, tt.skill)
				}
				if body != "" {
					t.Errorf("Load() body = %q, want empty on error", body)
				}
			case tt.wantMissing:
				var nf *SkillNotFoundError
				if !errors.As(err, &nf) {
					t.Fatalf("Load() error = %v, want *SkillNotFoundError", err)
				}
				if nf.Name != tt.skill {
					t.Errorf("SkillNotFoundError.Name = %q, want %q", nf.Name, tt.skill)
				}
				if body != "" {
					t.Errorf("Load() body = %q, want empty on error", body)
				}
			case tt.wantMalform:
				var me *MalformedSkillError
				if !errors.As(err, &me) {
					t.Fatalf("Load() error = %v, want *MalformedSkillError", err)
				}
				if me.Name != tt.skill {
					t.Errorf("MalformedSkillError.Name = %q, want %q (loader must stamp it)", me.Name, tt.skill)
				}
				if body != "" {
					t.Errorf("Load() body = %q, want empty on error", body)
				}
			default:
				if err != nil {
					t.Fatalf("Load() unexpected error = %v", err)
				}
				if body != tt.wantBody {
					t.Errorf("Load() body = %q, want %q", body, tt.wantBody)
				}
			}
		})
	}
}

// TestEmbeddedSkillLoaderImplementsInterface is a compile-time assertion that the
// concrete type satisfies the narrow SkillLoader and SkillDescriber interfaces.
func TestEmbeddedSkillLoaderImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ SkillLoader = NewEmbeddedSkillLoader(newTestSkillFS(), testAllow())
	var _ SkillDescriber = NewEmbeddedSkillLoader(newTestSkillFS(), testAllow())
}

// TestEmbeddedSkillLoaderDescribe proves Describe authorizes (agent, name) against
// the same closed allow-set as Load, then returns the parsed frontmatter
// (name+description) WITHOUT the body — the metadata the swarm renders into the
// <available_skills> catalog. It is fail-secure: an unauthorized/unknown name,
// a missing file, and a malformed file all error with the same typed errors as
// Load.
func TestEmbeddedSkillLoaderDescribe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		agent       identity.AgentName
		skill       string
		wantName    string
		wantDesc    string
		wantUnknown bool
		wantMalform bool
		wantMissing bool
	}{
		{
			name:     "authorized agent and skill returns metadata",
			agent:    agentReviewer,
			skill:    "code-style",
			wantName: "code-style",
			wantDesc: "A short checklist.",
		},
		{
			name:        "skill not in agent set is unauthorized",
			agent:       agentReviewer,
			skill:       "secret-skill",
			wantUnknown: true,
		},
		{
			name:        "agent absent from allow-map denies all",
			agent:       agentCoder,
			skill:       "code-style",
			wantUnknown: true,
		},
		{
			name:        "authorized but file missing is not-found",
			agent:       agentReviewer,
			skill:       "ghost",
			wantMissing: true,
		},
		{
			name:        "authorized but malformed file propagates malformed",
			agent:       agentReviewer,
			skill:       "broken",
			wantMalform: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			loader := NewEmbeddedSkillLoader(newTestSkillFS(), testAllow())
			meta, err := loader.Describe(context.Background(), tt.agent, tt.skill)

			switch {
			case tt.wantUnknown:
				var ue *UnknownSkillError
				if !errors.As(err, &ue) {
					t.Fatalf("Describe() error = %v, want *UnknownSkillError", err)
				}
			case tt.wantMissing:
				var nf *SkillNotFoundError
				if !errors.As(err, &nf) {
					t.Fatalf("Describe() error = %v, want *SkillNotFoundError", err)
				}
			case tt.wantMalform:
				var me *MalformedSkillError
				if !errors.As(err, &me) {
					t.Fatalf("Describe() error = %v, want *MalformedSkillError", err)
				}
			default:
				if err != nil {
					t.Fatalf("Describe() unexpected error = %v", err)
				}
				if meta.Name != tt.wantName {
					t.Errorf("Describe() Name = %q, want %q", meta.Name, tt.wantName)
				}
				if meta.Description != tt.wantDesc {
					t.Errorf("Describe() Description = %q, want %q", meta.Description, tt.wantDesc)
				}
			}
		})
	}
}
