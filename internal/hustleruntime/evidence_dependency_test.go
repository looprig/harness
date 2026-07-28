package hustleruntime

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestEvidenceRuntimeDoesNotImportGeneralAuthorityOwners(t *testing.T) {
	t.Parallel()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	directory := filepath.Dir(currentFile)
	for _, name := range []string{"contracts.go", "evidence_runner.go"} {
		file, err := parser.ParseFile(
			token.NewFileSet(), filepath.Join(directory, name), nil, parser.ImportsOnly,
		)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{
				"github.com/looprig/harness/internal/loopruntime",
				"github.com/looprig/harness/internal/sessionruntime",
				"github.com/looprig/harness/pkg/session",
				"github.com/looprig/classifiers",
				"github.com/looprig/sandbox",
			} {
				if importPath == forbidden || strings.HasPrefix(importPath, forbidden+"/") {
					t.Fatalf("%s imports authority owner %q", name, importPath)
				}
			}
		}
	}
}
