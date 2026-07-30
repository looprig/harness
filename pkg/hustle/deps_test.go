package hustle

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenHustleImports is design §22.8's boundary: "pkg/hustle does not
// import gate, rig, session, or runtime internals." pkg/gate is included
// even though pkg/hustle importing it today would already be blocked by
// Go's own import-cycle detection (pkg/gate imports pkg/hustle) — this test
// must assert the design rule itself rather than silently rely on the
// compiler to enforce it, so the boundary stays caught even if the cycle
// relationship ever changes.
var forbiddenHustleImports = []string{
	"github.com/looprig/harness/internal/",
	"github.com/looprig/harness/pkg/event",
	"github.com/looprig/harness/pkg/gate",
	"github.com/looprig/harness/pkg/loop",
	"github.com/looprig/harness/pkg/rig",
	"github.com/looprig/harness/pkg/session",
	"github.com/looprig/harness/pkg/tools",
	"github.com/looprig/tools",
	"github.com/looprig/llm",
}

// TestForbiddenHustleImportsCoversDesignBoundary proves design §22.8's full
// forbidden list ("gate, rig, session, or runtime internals") is actually
// enforced by TestDependencyBoundaries below, not just the subset that
// happens to compile-error today.
func TestForbiddenHustleImportsCoversDesignBoundary(t *testing.T) {
	t.Parallel()
	want := []string{
		"github.com/looprig/harness/pkg/gate",
		"github.com/looprig/harness/pkg/rig",
		"github.com/looprig/harness/pkg/session",
		"github.com/looprig/harness/internal/",
	}
	for _, w := range want {
		found := false
		for _, f := range forbiddenHustleImports {
			if f == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("forbiddenHustleImports = %v, want it to contain %q", forbiddenHustleImports, w)
		}
	}
}

func TestDependencyBoundaries(t *testing.T) {
	t.Parallel()
	forbidden := forbiddenHustleImports
	tests := []struct {
		name string
		dir  string
	}{
		{name: "public hustle package is a leaf", dir: "."},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entries, err := os.ReadDir(tt.dir)
			if err != nil {
				t.Fatalf("os.ReadDir() error = %v", err)
			}
			fileSet := token.NewFileSet()
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
					continue
				}
				filename := filepath.Join(tt.dir, entry.Name())
				file, parseErr := parser.ParseFile(fileSet, filename, nil, parser.ImportsOnly)
				if parseErr != nil {
					t.Fatalf("parser.ParseFile(%s) error = %v", filename, parseErr)
				}
				for _, imported := range file.Imports {
					path, unquoteErr := strconv.Unquote(imported.Path.Value)
					if unquoteErr != nil {
						t.Fatalf("strconv.Unquote(%s) error = %v", imported.Path.Value, unquoteErr)
					}
					for _, prefix := range forbidden {
						if strings.HasPrefix(path, prefix) {
							t.Errorf("%s imports forbidden dependency %q", filename, path)
						}
					}
				}
			}
		})
	}
}

func TestEvidenceToolsAddOnlyTheNarrowToolContractDependency(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("definition.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `"github.com/looprig/harness/pkg/tool"`) {
		t.Fatal("definition.go must use the narrow pkg/tool definition contract")
	}
}
