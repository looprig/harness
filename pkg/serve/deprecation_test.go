package serve_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Keep the migration notice discoverable through go doc, where consumers of the
// retained import path look for its support status.
func TestPackageDocumentsCompatibilityDeprecation(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(packageDir(t), "serve.go"), nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if f.Doc == nil || !strings.Contains(f.Doc.Text(), "Deprecated: Use Factory") {
		t.Fatal("package documentation must deprecate the public BFF role in favor of Factory")
	}
}
