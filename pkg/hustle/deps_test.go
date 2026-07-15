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

func TestDependencyBoundaries(t *testing.T) {
	t.Parallel()
	forbidden := []string{
		"github.com/looprig/harness/internal/",
		"github.com/looprig/harness/pkg/event",
		"github.com/looprig/harness/pkg/loop",
		"github.com/looprig/harness/pkg/rig",
		"github.com/looprig/harness/pkg/session",
		"github.com/looprig/harness/pkg/tool",
		"github.com/looprig/harness/pkg/tools",
		"github.com/looprig/tools",
		"github.com/looprig/llm",
	}
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
