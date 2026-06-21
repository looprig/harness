package main

import (
	"slices"
	"testing"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestParseFlags covers the CLI flag parser: --list, --resume <uuid>, and the positional
// agent name, plus boundary validation (an invalid resume id fails at the boundary, not
// deep in the wiring; --list and --resume are mutually exclusive).
func TestParseFlags(t *testing.T) {
	t.Parallel()

	validID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	tests := []struct {
		name       string
		args       []string
		wantAgent  string
		wantList   bool
		wantResume uuid.UUID
		wantErr    bool
	}{
		{name: "no flags → default agent, new session", args: nil, wantAgent: defaultAgent},
		{name: "positional agent name", args: []string{"coding"}, wantAgent: "coding"},
		{name: "list flag", args: []string{"-list"}, wantAgent: defaultAgent, wantList: true},
		{name: "list flag double dash", args: []string{"--list"}, wantAgent: defaultAgent, wantList: true},
		{name: "resume a session", args: []string{"-resume", validID.String()}, wantAgent: defaultAgent, wantResume: validID},
		{name: "resume then agent name", args: []string{"-resume", validID.String(), "coding"}, wantAgent: "coding", wantResume: validID},
		{name: "invalid resume id rejected", args: []string{"-resume", "not-a-uuid"}, wantErr: true},
		{name: "empty resume id rejected", args: []string{"-resume", ""}, wantErr: true},
		{name: "list and resume are mutually exclusive", args: []string{"-list", "-resume", validID.String()}, wantErr: true},
		{name: "unknown flag rejected", args: []string{"-nope"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFlags(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", got.agent, tt.wantAgent)
			}
			if got.list != tt.wantList {
				t.Errorf("list = %v, want %v", got.list, tt.wantList)
			}
			if got.resume != tt.wantResume {
				t.Errorf("resume = %v, want %v", got.resume, tt.wantResume)
			}
		})
	}
}

func TestAgentName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no args returns default", args: nil, want: defaultAgent},
		{name: "empty slice returns default", args: []string{}, want: defaultAgent},
		{name: "single positional arg", args: []string{"foo"}, want: "foo"},
		{name: "flag then positional", args: []string{"-v", "bar"}, want: "bar"},
		{name: "only a flag returns default", args: []string{"-v"}, want: defaultAgent},
		{name: "flag before positional name", args: []string{"--debug", "coding"}, want: "coding"},
		{name: "coding selects coding agent", args: []string{"coding"}, want: "coding"},
		{name: "first positional wins", args: []string{"-x", "alpha", "beta"}, want: "alpha"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := agentName(tt.args); got != tt.want {
				t.Errorf("agentName(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestAgentDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		agentName string
		want      string
	}{
		{name: "known agent returns its description", agentName: "coding", want: "a careful software engineer that works through tools"},
		{name: "unknown agent returns empty (banner shows bare name)", agentName: "nope", want: ""},
		{name: "empty name returns empty", agentName: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := agentDescription(tt.agentName); got != tt.want {
				t.Errorf("agentDescription(%q) = %q, want %q", tt.agentName, got, tt.want)
			}
		})
	}
}

func TestAgentDisplayName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		agentName string
		want      string
	}{
		{name: "coding agent displays as Togo", agentName: "coding", want: "Togo"},
		{name: "unmapped agent falls back to its name", agentName: "other", want: "other"},
		{name: "empty name falls back to empty", agentName: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := agentDisplayName(tt.agentName); got != tt.want {
				t.Errorf("agentDisplayName(%q) = %q, want %q", tt.agentName, got, tt.want)
			}
		})
	}
}

func TestBuildRegistry(t *testing.T) {
	t.Parallel()
	reg := buildRegistry()
	if reg == nil {
		t.Fatal("buildRegistry() returned nil")
	}
	// Assert the built-in agents are registered by name. Names() is checked
	// rather than Open() because opening an agent reads the environment and the
	// working directory; the registry only promises the binding exists here.
	names := reg.Names()
	for _, want := range []string{defaultAgent, "coding"} {
		if !slices.Contains(names, want) {
			t.Errorf("buildRegistry().Names() = %v, want it to contain %q", names, want)
		}
	}
}
