package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ciram-co/looprig/pkg/tool"
)

// store_hardening_test.go exercises the §3c WRITE-side store hardening: EVERY
// store directory component under <home> (`.looprig`, `.looprig/workspaces`, and the
// `<hash>` leaf) is tightened to 0700 by a ScopeWorkspace Grant, even when a
// pre-existing ancestor was created world-writable. A chmod failure on a
// component (e.g. an ancestor owned by another user) makes Grant fail secure
// with a typed *PolicyStoreError and write nothing.

// TestGrantTightensAllAncestorDirs proves a ScopeWorkspace Grant forces 0700 on
// every store component under <home> — not just the <hash> leaf — closing the
// store-poisoning vector where a pre-existing loose ~/.looprig or
// ~/.looprig/workspaces lets a non-owner plant an attacker-owned approvals.json.
func TestGrantTightensAllAncestorDirs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// precreate maps a store-relative dir (under <home>) to the loose mode it
		// is pre-created with (via Mkdir + an explicit Chmod so the umask cannot
		// strip the loose bits). An empty map is the clean-install case.
		precreate map[string]os.FileMode
	}{
		{
			name:      "clean install (no pre-existing dirs)",
			precreate: nil,
		},
		{
			name: "pre-existing loose ~/.looprig and ~/.looprig/workspaces",
			precreate: map[string]os.FileMode{
				filepath.Join(urviDirName):                    0o777,
				filepath.Join(urviDirName, workspacesDirName): 0o777,
			},
		},
		{
			name: "pre-existing loose ~/.looprig only",
			precreate: map[string]os.FileMode{
				filepath.Join(urviDirName): 0o755,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := newWS(t)
			if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
				t.Fatalf("write main.go: %v", err)
			}
			home := t.TempDir()

			// Pre-create the requested ancestor dirs at their loose modes.
			for rel, mode := range tt.precreate {
				dir := filepath.Join(home, rel)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir %q: %v", dir, err)
				}
				// Chmod explicitly so the umask cannot strip the loose bits.
				if err := os.Chmod(dir, mode); err != nil {
					t.Fatalf("chmod %q to %o: %v", dir, mode, err)
				}
			}

			// Capture home's mode BEFORE Grant: Grant must leave the trust anchor
			// (home and anything above the store root) untouched.
			homeBefore, err := os.Stat(home)
			if err != nil {
				t.Fatalf("stat home before: %v", err)
			}

			pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
			pc.SetHomeDir(func() (string, error) { return home, nil })

			if err := pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeWorkspace); err != nil {
				t.Fatalf("Grant(ScopeWorkspace): %v", err)
			}

			wsFile := wsApprovalsPathFor(t, home, ws)
			// ALL THREE store components under <home> must be exactly 0700.
			for _, dir := range []string{
				filepath.Join(home, urviDirName),
				filepath.Join(home, urviDirName, workspacesDirName),
				filepath.Dir(wsFile), // the <hash> leaf.
			} {
				di, err := os.Stat(dir)
				if err != nil {
					t.Fatalf("stat dir %q: %v", dir, err)
				}
				if perm := di.Mode().Perm(); perm != storeDirPerm {
					t.Errorf("dir %q perm = %o, want %o", dir, perm, storeDirPerm)
				}
			}

			// <home> itself is the trust anchor and must NOT be touched by Grant:
			// its mode must be exactly what it was before the Grant.
			hi, err := os.Stat(home)
			if err != nil {
				t.Fatalf("stat home: %v", err)
			}
			if hi.Mode().Perm() != homeBefore.Mode().Perm() {
				t.Errorf("home perm changed by Grant: was %o, now %o (home is the trust anchor)", homeBefore.Mode().Perm(), hi.Mode().Perm())
			}
		})
	}
}

// TestMkdirStoreDirTightensComponents unit-tests the helper directly: it creates
// the missing components AND tightens every store-owned component under home to
// exactly 0700, regardless of any pre-existing loose mode, and never touches home.
func TestMkdirStoreDirTightensComponents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// looseRel→mode dirs to pre-create before mkdirStoreDir (nil = clean).
		precreate map[string]os.FileMode
	}{
		{name: "clean install", precreate: nil},
		{
			name: "loose ~/.looprig + workspaces",
			precreate: map[string]os.FileMode{
				urviDirName: 0o777,
				filepath.Join(urviDirName, workspacesDirName): 0o775,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			for rel, mode := range tt.precreate {
				dir := filepath.Join(home, rel)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir %q: %v", dir, err)
				}
				if err := os.Chmod(dir, mode); err != nil {
					t.Fatalf("chmod %q: %v", dir, mode)
				}
			}
			leaf := filepath.Join(home, urviDirName, workspacesDirName, "deadbeef")
			if err := mkdirStoreDir(home, leaf); err != nil {
				t.Fatalf("mkdirStoreDir: %v", err)
			}
			for _, dir := range []string{
				filepath.Join(home, urviDirName),
				filepath.Join(home, urviDirName, workspacesDirName),
				leaf,
			} {
				di, err := os.Stat(dir)
				if err != nil {
					t.Fatalf("stat %q: %v", dir, err)
				}
				if perm := di.Mode().Perm(); perm != storeDirPerm {
					t.Errorf("dir %q perm = %o, want %o", dir, perm, storeDirPerm)
				}
			}
		})
	}
}

// TestGrantTightenAncestorFailsSecure proves that when a store ancestor cannot be
// created/tightened — the locked-or-other-user-owned-ancestor attack signal (the
// real case being an EPERM chmod on a dir owned by another user) — Grant returns
// a typed *PolicyStoreError and writes NOTHING. It is simulated locally by making
// ~/.looprig mode 0000 (unsearchable/unwritable) so reaching/tightening the deeper
// store components is denied; the chmod-EPERM case is the same fail-secure exit.
func TestGrantTightenAncestorFailsSecure(t *testing.T) {
	// Not parallel: mutates directory modes; a cleanup restores them so t.TempDir
	// can remove the tree.
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod/traversal restrictions do not apply")
	}
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	home := t.TempDir()

	hash, err := workspaceHash(ws)
	if err != nil {
		t.Fatalf("workspaceHash: %v", err)
	}
	// Pre-create ~/.looprig, then lock it to 0000 so neither MkdirAll nor the chmod
	// walk can reach/alter the components beneath it — the attack signal.
	urvi := filepath.Join(home, urviDirName)
	if err := os.MkdirAll(urvi, 0o700); err != nil {
		t.Fatalf("mkdir ~/.looprig: %v", err)
	}
	if err := os.Chmod(urvi, 0o000); err != nil {
		t.Fatalf("chmod ~/.looprig 0000: %v", err)
	}
	t.Cleanup(func() {
		// Restore search/write so t.TempDir cleanup can recurse and remove the tree.
		_ = os.Chmod(urvi, 0o700)
	})

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return home, nil })

	grantErr := pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeWorkspace)
	if grantErr == nil {
		t.Fatal("Grant with a locked ancestor = nil, want a typed error")
	}
	var storeErr *PolicyStoreError
	if !errors.As(grantErr, &storeErr) {
		t.Errorf("error = %T, want *PolicyStoreError", grantErr)
	}

	// Restore perms to inspect: NO approvals file should have been written.
	if err := os.Chmod(urvi, 0o700); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	wsFile := workspaceApprovalsPath(home, hash)
	if _, statErr := os.Stat(wsFile); !os.IsNotExist(statErr) {
		t.Errorf("Grant must NOT write the approvals file on an ancestor failure; stat err = %v", statErr)
	}
}
