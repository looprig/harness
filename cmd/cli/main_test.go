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

func TestDefaultTUIOptions(t *testing.T) {
	t.Parallel()
	got := defaultTUIOptions()
	if got.AltScreen {
		t.Errorf("AltScreen = true, want false (scrollback-first requires normal screen for tea.Println)")
	}
	if got.Mouse {
		t.Errorf("Mouse = true, want false (no mouse capture in scrollback-first)")
	}
}

func TestTeaProgramOptions(t *testing.T) {
	t.Parallel()
	// In Bubble Tea v2 alt-screen and mouse are no longer ProgramOptions — they are
	// fields on the tea.View the model returns (applied in screen.View()). So
	// teaProgramOptions derives no program options from the modes: for every input,
	// including alt-screen and/or mouse, it returns zero options. The scrollback-first
	// intent (both off) is asserted separately by TestDefaultTUIOptions and realized
	// by screen.View() returning a tea.NewView whose AltScreen/MouseMode are their
	// zero values.
	tests := []struct {
		name string
		in   tuiOptions
		want int
	}{
		{name: "no modes appends nothing", in: tuiOptions{}, want: 0},
		{name: "alt screen no longer a program option", in: tuiOptions{AltScreen: true}, want: 0},
		{name: "mouse no longer a program option", in: tuiOptions{Mouse: true}, want: 0},
		{name: "both no longer program options", in: tuiOptions{AltScreen: true, Mouse: true}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// tea.ProgramOption values are opaque func types and cannot be
			// compared for equality, so assert the count of options returned.
			if got := len(teaProgramOptions(tt.in)); got != tt.want {
				t.Errorf("len(teaProgramOptions(%+v)) = %d, want %d", tt.in, got, tt.want)
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
