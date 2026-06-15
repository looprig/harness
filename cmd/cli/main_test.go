package main

import (
	"slices"
	"testing"
)

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
		{name: "flag before default-able name", args: []string{"--debug", "personal-assistant"}, want: "personal-assistant"},
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
