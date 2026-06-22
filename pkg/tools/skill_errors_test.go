package tools

import (
	"errors"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/identity"
)

func TestUnknownSkillError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		agent    identity.AgentName
		skill    string
		wantSubs []string
	}{
		{
			name:     "happy path names agent and skill",
			agent:    identity.AgentName("coder"),
			skill:    "code-style",
			wantSubs: []string{"coder", "code-style", "unknown or unauthorized"},
		},
		{
			name:     "empty agent and skill still formats",
			agent:    identity.AgentName(""),
			skill:    "",
			wantSubs: []string{"tools:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var err error = &UnknownSkillError{Agent: tt.agent, Name: tt.skill}

			var target *UnknownSkillError
			if !errors.As(err, &target) {
				t.Fatalf("errors.As failed to recover *UnknownSkillError from %T", err)
			}
			if target.Agent != tt.agent {
				t.Errorf("Agent = %q, want %q", target.Agent, tt.agent)
			}
			if target.Name != tt.skill {
				t.Errorf("Name = %q, want %q", target.Name, tt.skill)
			}
			msg := err.Error()
			for _, sub := range tt.wantSubs {
				if !strings.Contains(msg, sub) {
					t.Errorf("Error() = %q, want substring %q", msg, sub)
				}
			}
		})
	}
}

func TestMalformedSkillError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		skill    string
		reason   string
		wantSubs []string
		notSubs  []string
	}{
		{
			name:     "named skill includes name and reason",
			skill:    "code-style",
			reason:   "duplicate key name",
			wantSubs: []string{"code-style", "duplicate key name", "malformed skill"},
		},
		{
			name:     "empty name omits name segment",
			skill:    "",
			reason:   "no opening frontmatter fence",
			wantSubs: []string{"malformed skill:", "no opening frontmatter fence"},
			notSubs:  []string{"malformed skill :"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var err error = &MalformedSkillError{Name: tt.skill, Reason: tt.reason}

			var target *MalformedSkillError
			if !errors.As(err, &target) {
				t.Fatalf("errors.As failed to recover *MalformedSkillError from %T", err)
			}
			if target.Name != tt.skill {
				t.Errorf("Name = %q, want %q", target.Name, tt.skill)
			}
			if target.Reason != tt.reason {
				t.Errorf("Reason = %q, want %q", target.Reason, tt.reason)
			}
			msg := err.Error()
			for _, sub := range tt.wantSubs {
				if !strings.Contains(msg, sub) {
					t.Errorf("Error() = %q, want substring %q", msg, sub)
				}
			}
			for _, sub := range tt.notSubs {
				if strings.Contains(msg, sub) {
					t.Errorf("Error() = %q, must not contain %q", msg, sub)
				}
			}
		})
	}
}
