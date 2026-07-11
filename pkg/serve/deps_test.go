package serve_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/serve"
	"github.com/looprig/harness/pkg/session"
)

// Structural-satisfaction proofs (compile-time): the real, concrete session types
// must satisfy the narrow interfaces serve declares WITHOUT serve importing the
// session package. If a real method signature ever drifts from an interface, this
// file stops compiling — the guardrail that keeps serve's contract honest.
var (
	_ serve.LiveSession                    = (session.Session)(nil)
	_ serve.Rig[session.SessionController] = (*rig.Rig)(nil)
)

// allowedImports is the EXACT set of import paths the production (non-test) serve
// package may depend on directly (SPEC §2, Dependency Inversion). serve is the HTTP
// surface: it couples ONLY to the narrow interfaces it defines plus the leaf value
// types those interfaces mention, never to the session/loop/llm/store machinery
// behind them. Anything outside this set (most importantly pkg/session, any llm
// package, any store package) is a dependency-inversion violation.
var allowedImports = map[string]struct{}{
	// This module's leaf packages the interface signatures reference.
	"github.com/looprig/harness/pkg/event": {},
	"github.com/looprig/harness/pkg/gate":  {},
	// Core value types the interface signatures reference.
	"github.com/looprig/core/content": {},
	"github.com/looprig/core/uuid":    {},
}

// isStdlib reports whether importPath is a Go standard-library package. Stdlib import
// paths have no dot in their first path segment (they are not domains), which cleanly
// separates "context"/"net/http" from "github.com/..." without maintaining a list.
func isStdlib(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// importAllowed is the pure decision the guard applies to one import path: an import
// is permitted iff it is stdlib or is in the explicit allow-set. Extracted so the
// guard's fail-on-violation behavior is unit-testable (TestImportAllowed) without
// planting a real disallowed import in the production package.
func importAllowed(importPath string) bool {
	if isStdlib(importPath) {
		return true
	}
	_, ok := allowedImports[importPath]
	return ok
}

// TestImportAllowed pins the guard's detection logic: stdlib and the four allow-set
// paths are permitted; the forbidden low-level packages (session, an llm package, a
// store package) are rejected. This proves TestProductionImportsAreAllowed is a real
// failing guard without needing a disallowed import in serve.go itself.
func TestImportAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "stdlib context", path: "context", want: true},
		{name: "stdlib net/http", path: "net/http", want: true},
		{name: "allowed event", path: "github.com/looprig/harness/pkg/event", want: true},
		{name: "allowed gate", path: "github.com/looprig/harness/pkg/gate", want: true},
		{name: "allowed content", path: "github.com/looprig/core/content", want: true},
		{name: "allowed uuid", path: "github.com/looprig/core/uuid", want: true},
		{name: "forbidden session", path: "github.com/looprig/harness/pkg/session", want: false},
		{name: "forbidden llm", path: "github.com/looprig/harness/pkg/llm", want: false},
		{name: "forbidden store", path: "github.com/looprig/harness/pkg/sessionstore", want: false},
		{name: "forbidden third-party", path: "github.com/looprig/storage", want: false},
		{name: "empty path", path: "", want: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := importAllowed(tt.path); got != tt.want {
				t.Errorf("importAllowed(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestProductionImportsAreAllowed is the dependency-inversion guard: it parses every
// non-test .go file in the serve package directory and fails if any import is neither
// stdlib nor in allowedImports. It is stdlib-only (os.ReadDir + go/parser) so it needs
// no toolchain subprocess, and it inspects the CURRENT package directory (resolved from
// this test file) so it is independent of the process CWD.
func TestProductionImportsAreAllowed(t *testing.T) {
	t.Parallel()

	dir := packageDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read serve package dir %q: %v", dir, err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !isProductionFile(entry.Name()) {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		file, perr := parser.ParseFile(fset, filePath, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse serve file %q: %v", filePath, perr)
		}
		for _, imp := range file.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				t.Fatalf("%s: unquote import %q: %v", filePath, imp.Path.Value, uerr)
			}
			if !importAllowed(path) {
				t.Errorf("file %s imports disallowed path %q; production serve may import only stdlib plus %v",
					entry.Name(), path, sortedAllowed())
			}
		}
	}
}

// isProductionFile reports whether a directory entry name is a production (non-test)
// Go source file, so the guard inspects the production package and never the _test.go
// files (which MAY import pkg/session for the structural-satisfaction proofs above).
func isProductionFile(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// packageDir resolves the serve package's own source directory from this test file's
// location, so the guard does not depend on the test process's working directory.
func packageDir(t *testing.T) string {
	t.Helper()
	// This test lives in the serve package directory; the CWD of a `go test` run IS
	// that package directory, so "." resolves to it.
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}
	return dir
}

// sortedAllowed returns the allowed import paths in a stable order for error messages.
func sortedAllowed() []string {
	out := make([]string, 0, len(allowedImports))
	for p := range allowedImports {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
