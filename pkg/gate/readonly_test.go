package gate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// canonicalDir returns a fresh directory with every symlink resolved, the form
// a prepared tool puts in a requirement's Scope (t.TempDir sits under a
// symlinked /var on macOS).
func canonicalDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadOnlyAccessAllowsReadsOnlyWithinRoots(t *testing.T) {
	t.Parallel()
	root := canonicalDir(t)
	other := canonicalDir(t)
	access, err := gate.NewReadOnlyAccess(root)
	if err != nil {
		t.Fatalf("NewReadOnlyAccess() error = %v", err)
	}
	if got := access.AccessVersion(); got != gate.CurrentAccessVersion {
		t.Fatalf("AccessVersion() = %d, want %d", got, gate.CurrentAccessVersion)
	}
	tests := []struct {
		name  string
		kind  string
		scope string
		want  uint8
	}{
		{name: "root itself", kind: "filesystem.read", scope: root, want: gate.AccessAllow},
		{name: "file under root", kind: "filesystem.read", scope: filepath.Join(root, "src", "main.go"), want: gate.AccessAllow},
		{name: "tree scope at root", kind: "filesystem.read", scope: "tree:" + root, want: gate.AccessAllow},
		{name: "tree scope under root", kind: "filesystem.read", scope: "tree:" + filepath.Join(root, "src"), want: gate.AccessAllow},
		{name: "sibling with shared prefix", kind: "filesystem.read", scope: root + "-evil", want: gate.AccessDeny},
		{name: "parent of root", kind: "filesystem.read", scope: filepath.Dir(root), want: gate.AccessDeny},
		{name: "other directory", kind: "filesystem.read", scope: filepath.Join(other, "secret"), want: gate.AccessDeny},
		{name: "tree scope outside", kind: "filesystem.read", scope: "tree:" + other, want: gate.AccessDeny},
		{name: "whole host", kind: "filesystem.read", scope: "host:*", want: gate.AccessDeny},
		{name: "write under root", kind: "filesystem.write", scope: filepath.Join(root, "main.go"), want: gate.AccessDeny},
		{name: "write host", kind: "filesystem.write", scope: "host:*", want: gate.AccessDeny},
		{name: "command", kind: "command.execute", scope: "", want: gate.AccessDeny},
		{name: "network", kind: "network", scope: "", want: gate.AccessDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := access.AccessFor(tt.kind, tt.scope)
			if err != nil {
				t.Fatalf("AccessFor(%q, %q) error = %v", tt.kind, tt.scope, err)
			}
			if got != tt.want {
				t.Fatalf("AccessFor(%q, %q) = %d, want %d", tt.kind, tt.scope, got, tt.want)
			}
		})
	}
}

func TestReadOnlyAccessFailsClosedOnMalformedInput(t *testing.T) {
	t.Parallel()
	root := canonicalDir(t)
	access, err := gate.NewReadOnlyAccess(root)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, kind, scope string }{
		{name: "unknown kind", kind: "mcp.call", scope: ""},
		{name: "relative read scope", kind: "filesystem.read", scope: "src/main.go"},
		{name: "unclean read scope", kind: "filesystem.read", scope: root + "/../etc"},
		{name: "empty read scope", kind: "filesystem.read", scope: ""},
		{name: "empty tree scope", kind: "filesystem.read", scope: "tree:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := access.AccessFor(tt.kind, tt.scope)
			if err == nil {
				t.Fatalf("AccessFor(%q, %q) = %d, nil; want an error", tt.kind, tt.scope, got)
			}
			if got != gate.AccessDeny {
				t.Fatalf("AccessFor(%q, %q) = %d on error, want AccessDeny", tt.kind, tt.scope, got)
			}
		})
	}

	var zero *gate.ReadOnlyAccess
	if zero.AccessVersion() != 0 {
		t.Fatal("nil ReadOnlyAccess must report an unsupported access version")
	}
	if _, err := zero.AccessFor("filesystem.read", root); err == nil {
		t.Fatal("nil ReadOnlyAccess must fail closed")
	}
	if (&gate.ReadOnlyAccess{}).AccessVersion() != 0 {
		t.Fatal("unconstructed ReadOnlyAccess must report an unsupported access version")
	}
}

func TestNewReadOnlyAccessRejectsUnsafeRoots(t *testing.T) {
	t.Parallel()
	dir := canonicalDir(t)
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		roots []string
	}{
		{name: "no roots", roots: nil},
		{name: "empty root", roots: []string{""}},
		{name: "whitespace root", roots: []string{" " + dir}},
		{name: "missing root", roots: []string{filepath.Join(dir, "missing")}},
		{name: "file root", roots: []string{file}},
		{name: "filesystem root", roots: []string{string(filepath.Separator)}},
		{name: "one bad among good", roots: []string{dir, filepath.Join(dir, "missing")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			access, err := gate.NewReadOnlyAccess(tt.roots...)
			if err == nil {
				t.Fatalf("NewReadOnlyAccess(%q) = %v, nil; want an error", tt.roots, access.Roots())
			}
			var accessErr *gate.AccessError
			if !errors.As(err, &accessErr) || accessErr.Kind != gate.AccessRootInvalid {
				t.Fatalf("NewReadOnlyAccess(%q) error = %v, want AccessError{Kind: %q}", tt.roots, err, gate.AccessRootInvalid)
			}
		})
	}
}

func TestNewReadOnlyAccessCanonicalizesRoots(t *testing.T) {
	t.Parallel()
	real := canonicalDir(t)
	links := canonicalDir(t)
	link := filepath.Join(links, "workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := gate.NewReadOnlyAccess(link, real, real+string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := access.Roots(), []string{real}; !slices.Equal(got, want) {
		t.Fatalf("Roots() = %q, want %q (symlink resolved, deduplicated)", got, want)
	}
	// A prepared tool reports the resolved path, which is inside the root.
	if got, err := access.AccessFor("filesystem.read", filepath.Join(real, "a.txt")); err != nil || got != gate.AccessAllow {
		t.Fatalf("AccessFor(resolved) = %d, %v, want allow", got, err)
	}
	// The gate compares strings, not files: a spelling through a symlink that
	// lives outside every root is lexically outside, so it is denied.
	if got, _ := access.AccessFor("filesystem.read", filepath.Join(link, "a.txt")); got != gate.AccessDeny {
		t.Fatalf("AccessFor(via outside symlink) = %d, want deny", got)
	}
	// Roots returns a copy.
	roots := access.Roots()
	roots[0] = "/"
	if access.Roots()[0] != real {
		t.Fatal("Roots() must return a copy")
	}
}

func TestReadOnlyEvaluatorAuthorizesPreparedRequests(t *testing.T) {
	t.Parallel()
	root := canonicalDir(t)
	other := canonicalDir(t)
	evaluator, err := gate.NewReadOnlyEvaluator(root)
	if err != nil {
		t.Fatalf("NewReadOnlyEvaluator() error = %v", err)
	}
	if evaluator.Interactive() {
		t.Fatal("read-only evaluator must be headless")
	}
	read := func(path string) tool.Requirement {
		return tool.Requirement{Kind: "filesystem.read", Scope: path, Match: path, Description: "read " + path}
	}
	tests := []struct {
		name         string
		requirements []tool.Requirement
		wantApproved bool
		wantErrKind  gate.AccessErrorKind
	}{
		{name: "read inside", requirements: []tool.Requirement{read(filepath.Join(root, "a.go"))}, wantApproved: true},
		{name: "read outside", requirements: []tool.Requirement{read(filepath.Join(other, "a.go"))}},
		{name: "read inside and outside", requirements: []tool.Requirement{read(filepath.Join(root, "a.go")), read(other)}},
		{name: "write inside", requirements: []tool.Requirement{{
			Kind: "filesystem.write", Scope: filepath.Join(root, "a.go"), Match: filepath.Join(root, "a.go"), Description: "write",
		}}},
		{name: "network", requirements: []tool.Requirement{{Kind: "network", Match: "example.com", Description: "fetch"}}},
		{name: "unknown kind", requirements: []tool.Requirement{{Kind: "mcp.call", Match: "x", Description: "x"}}, wantErrKind: gate.AccessSourceMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolution, err := evaluator.Authorize(context.Background(), tool.Request{ToolName: "T", Requirements: tt.requirements})
			if tt.wantErrKind != "" {
				var accessErr *gate.AccessError
				if !errors.As(err, &accessErr) || accessErr.Kind != tt.wantErrKind {
					t.Fatalf("Authorize() = %+v, %v; want AccessError %q", resolution, err, tt.wantErrKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if resolution.Approved != tt.wantApproved {
				t.Fatalf("Authorize().Approved = %v, want %v", resolution.Approved, tt.wantApproved)
			}
			if !tt.wantApproved && resolution.Denial != gate.DenialStructural {
				t.Fatalf("Authorize().Denial = %q, want structural", resolution.Denial)
			}
			if len(resolution.Grants) != 0 {
				t.Fatalf("Authorize().Grants = %v, want none", resolution.Grants)
			}
		})
	}
}

func TestReadOnlyAccessBindingsCoverStandardKinds(t *testing.T) {
	t.Parallel()
	access, err := gate.NewReadOnlyAccess(canonicalDir(t))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, binding := range access.Bindings() {
		if binding.Source != access {
			t.Fatalf("binding %q routes to %v, want the read-only source", binding.Kind, binding.Source)
		}
		kinds = append(kinds, binding.Kind)
	}
	slices.Sort(kinds)
	if want := []string{"command.execute", "filesystem.read", "filesystem.write", "network"}; !slices.Equal(kinds, want) {
		t.Fatalf("Bindings() kinds = %q, want %q", kinds, want)
	}
}
