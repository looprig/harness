package main

import (
	"context"
	"testing"

	"github.com/inventivepotter/urvi/swarms/swe"
	"github.com/inventivepotter/urvi/tui"
)

// TestSWEConstructorWiring is a compile-time wiring smoke test: it proves swe.New
// is the agent constructor the shared runtime expects — a
// func(context.Context) (tui.Agent, error). If swe.New's signature drifts (or the
// runtime's expected constructor shape changes), the entry point in main would stop
// compiling; this pins that contract in a test so the break is named, not silent.
//
// main packages are hard to unit-test (main() runs the real TUI), so the smoke test
// asserts the wiring contract rather than invoking main.
func TestSWEConstructorWiring(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ctor func(context.Context) (tui.Agent, error)
	}{
		{name: "swe.New is the wired constructor", ctor: swe.New},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.ctor == nil {
				t.Fatal("constructor is nil; main would have no agent to run")
			}
		})
	}
}
