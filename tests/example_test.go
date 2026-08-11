package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const offlineExamplesCommand = "GOWORK=off GOCACHE=/tmp/looprig-harness-docs-gocache go test -race ./examples/..."

type examplesManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Repository    string `json:"repository"`
	ProofSources  []struct {
		ID, Type, Path, Symbol string
	} `json:"proofSources"`
	Examples []struct {
		ID, Ecosystem, Owner, SourcePath, Availability string
		Versions                                       map[string]string
		OfflineCommand, Assertion, WorkflowPath, JobID string
		Cleanup                                        string
		LiveGate                                       any
		ProofIDs                                       []string
	} `json:"examples"`
}

func TestDocsExamplesArtifacts(t *testing.T) {
	t.Parallel()
	wantProofs := map[string]struct{ typeName, path, symbol string }{
		"example-harness-lifecycle-fixture":      {"executable-fixture", "examples/lifecycle/example_test.go", "Example_toolLoopRigSessionLifecycle"},
		"example-harness-persistence-fixture":    {"executable-fixture", "examples/persistence/example_test.go", "Example_sessionJournalAndWorkspaceStores"},
		"example-harness-policy-fixture":         {"executable-fixture", "examples/policy/example_test.go", "Example_hooksAndHeadlessGateFallback"},
		"example-harness-serving-fixture":        {"executable-fixture", "examples/serving/example_test.go", "Example_readOnlyHTTPAdapter"},
		"example-harness-composition-fixture":    {"executable-fixture", "examples/composition/example_test.go", "Example_compactionAndDelegationComposition"},
		"example-harness-manifest-contract-test": {"test", "tests/example_test.go", "TestDocsExamplesArtifacts"},
	}
	for _, proof := range wantProofs {
		path := proof.path
		if _, err := os.Stat(filepath.Join("..", path)); err != nil {
			t.Errorf("runnable example %q: %v", path, err)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "testdata/docs/examples.json"))
	if err != nil {
		t.Fatalf("read examples manifest: %v", err)
	}
	var manifest examplesManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode examples manifest: %v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.Repository != "harness" {
		t.Fatalf("manifest identity = schema %d repository %q", manifest.SchemaVersion, manifest.Repository)
	}
	proofs := make(map[string]bool)
	for _, proof := range manifest.ProofSources {
		if !strings.HasPrefix(proof.ID, "example-harness-") {
			t.Errorf("non-canonical proof ID %q", proof.ID)
		}
		if proof.Type != "executable-fixture" && proof.Type != "test" {
			t.Errorf("proof %q type = %q", proof.ID, proof.Type)
		}
		if strings.Contains(proof.Path, "#") {
			t.Errorf("proof %q path contains symbol fragment", proof.ID)
		}
		if _, err := os.Stat(filepath.Join("..", proof.Path)); err != nil {
			t.Errorf("proof %q path does not resolve: %v", proof.ID, err)
		}
		want, ok := wantProofs[proof.ID]
		if !ok {
			t.Errorf("unexpected proof source %q", proof.ID)
		} else if proof.Type != want.typeName || proof.Path != want.path || proof.Symbol != want.symbol {
			t.Errorf("proof %q = type %q path %q symbol %q, want type %q path %q symbol %q", proof.ID, proof.Type, proof.Path, proof.Symbol, want.typeName, want.path, want.symbol)
		}
		proofs[proof.ID] = true
	}
	if len(manifest.ProofSources) != len(wantProofs) {
		t.Errorf("manifest proof sources = %d, want %d", len(manifest.ProofSources), len(wantProofs))
	}
	if len(manifest.Examples) != 5 {
		t.Fatalf("manifest examples = %d, want 5", len(manifest.Examples))
	}
	seen := make(map[string]bool)
	for _, example := range manifest.Examples {
		if !strings.HasPrefix(example.ID, "example-harness-") || seen[example.ID] {
			t.Errorf("invalid or duplicate example ID %q", example.ID)
		}
		seen[example.ID] = true
		if example.Ecosystem != "go" || example.Owner != "harness" || example.Availability != "source-workspace" {
			t.Errorf("example %q classification is incorrect", example.ID)
		}
		if len(example.Versions) != 1 || example.Versions["github.com/looprig/harness"] != "source-workspace" {
			t.Errorf("example %q versions = %#v", example.ID, example.Versions)
		}
		if example.OfflineCommand != offlineExamplesCommand || example.WorkflowPath != ".github/workflows/docs-examples.yml" || example.JobID != "docs-examples" {
			t.Errorf("example %q execution metadata is incorrect", example.ID)
		}
		if example.SourcePath == "" || example.Assertion == "" || example.Cleanup == "" || example.LiveGate != nil {
			t.Errorf("example %q execution metadata is incomplete", example.ID)
		}
		if len(example.ProofIDs) < 2 {
			t.Errorf("example %q needs source and contract proofs", example.ID)
		}
		for _, proofID := range example.ProofIDs {
			if !proofs[proofID] {
				t.Errorf("example %q references unknown proof %q", example.ID, proofID)
			}
		}
	}

	workflow, err := os.ReadFile(filepath.Join("..", ".github/workflows/docs-examples.yml"))
	if err != nil {
		t.Fatalf("read docs examples workflow: %v", err)
	}
	for _, literal := range []string{
		"docs-examples:",
		offlineExamplesCommand,
		"GOWORK=off GOCACHE=/tmp/looprig-harness-docs-gocache make test",
		"GOWORK=off GOCACHE=/tmp/looprig-harness-docs-gocache go test -race ./...",
	} {
		if !strings.Contains(string(workflow), literal) {
			t.Errorf("workflow does not contain %q", literal)
		}
	}
}
