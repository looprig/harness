package sessionruntime

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/hustle"
)

func lifecycleTestHustle(t *testing.T, name hustle.Name) hustle.Definition {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName(name),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 512}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("prompt", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	return definition
}

func TestLifecycleHustlesCaptureDefensiveCopies(t *testing.T) {
	t.Parallel()
	first := lifecycleTestHustle(t, "first")
	second := lifecycleTestHustle(t, "second")
	limits := HustleLimits{
		BlockingConcurrent: 1, BackgroundConcurrent: 2,
		AuditTimeout: time.Second, FinalizationTimeout: 2 * time.Second,
		WorkerDrainTimeout: 3 * time.Second,
	}
	definitions := []hustle.Definition{first}
	option := WithLifecycleHustles(definitions, limits)
	definitions[0] = second
	limits.BlockingConcurrent = 99

	tests := []struct {
		name        string
		mutateFirst bool
	}{
		{name: "caller mutation after option creation is excluded"},
		{name: "separate lifecycle applications do not share slices", mutateFirst: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			firstLifecycle := &Lifecycle{}
			option(firstLifecycle)
			if tt.mutateFirst {
				firstLifecycle.hustles[0] = second
			}
			secondLifecycle := &Lifecycle{}
			option(secondLifecycle)
			if got := secondLifecycle.hustles[0].Name(); got != "first" {
				t.Fatalf("captured hustle name = %q, want first", got)
			}
			if got := secondLifecycle.hustleLimits.BlockingConcurrent; got != 1 {
				t.Fatalf("captured blocking concurrent = %d, want 1", got)
			}
		})
	}
}

func TestLifecycleHustleDependencyBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file string
	}{
		{name: "lifecycle imports leaf hustle without rig", file: "lifecycle.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), tt.file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			var sawHustle bool
			for _, spec := range file.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				switch path {
				case "github.com/looprig/harness/pkg/hustle":
					sawHustle = true
				case "github.com/looprig/harness/pkg/rig":
					t.Fatal("sessionruntime lifecycle imports pkg/rig")
				}
			}
			if !sawHustle {
				t.Fatal("sessionruntime lifecycle does not import leaf pkg/hustle")
			}
		})
	}
}
