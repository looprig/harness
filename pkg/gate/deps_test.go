package gate

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

const harnessPackagePrefix = "github.com/looprig/harness/"

func TestGatePackageDependencies(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		harnessPackagePrefix + "pkg/loop",
		harnessPackagePrefix + "pkg/rig",
		harnessPackagePrefix + "pkg/session",
		harnessPackagePrefix + "internal/loopruntime",
		harnessPackagePrefix + "internal/sessionruntime",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		filename := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, filename, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse production file %s: %v", filename, err)
			continue
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Errorf("%s import path: %v", filename, err)
				continue
			}
			for _, prefix := range forbidden {
				if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
					t.Errorf("%s imports forbidden runtime/session dependency %q", filename, importPath)
				}
			}
		}
	}
}
