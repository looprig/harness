package rig

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// --- helpers ------------------------------------------------------------------------

func wsStoreT(t *testing.T, opts ...workspacestore.Option) *workspacestore.Store {
	t.Helper()
	ws, err := workspacestore.Open(memstore.New().Blobs, opts...)
	if err != nil {
		t.Fatalf("workspacestore.Open: %v", err)
	}
	return ws
}

func sessionStoreT(t *testing.T) *sessionstore.Store {
	t.Helper()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	return store
}

func planLoopT(t *testing.T, tools ...tool.Definition) loop.Definition {
	t.Helper()
	d, err := loop.Define(
		loop.WithName(identity.AgentName("planner")),
		loop.WithInference(&stubLLM{}, validModel("planner")),
		loop.WithTools(tools...),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	return d
}

// defineWith builds a minimal valid rig plus the extra options under test.
func defineWith(t *testing.T, store *sessionstore.Store, extra ...Option) (*Rig, error) {
	t.Helper()
	opts := append([]Option{
		WithLoops(planLoopT(t)),
		WithPrimers("planner"),
		WithSessionStore(store),
	}, extra...)
	return Define(opts...)
}

func wsRequiringTool() tool.Definition {
	return tool.NewDefinition("needs-workspace", tool.RequiresWorkspace, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return nil, nil
	})
}

// --- placement matrix ---------------------------------------------------------------

func TestDefinePlacementMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		options  func(t *testing.T, root string) []Option
		wantKind WorkspacePlacementErrorKind
		wantErr  bool
	}{
		{
			name: "zero placement is valid",
			options: func(t *testing.T, root string) []Option {
				return nil
			},
		},
		{
			name: "single exclusive placement is valid",
			options: func(t *testing.T, root string) []Option {
				return []Option{WithExclusiveWorkspace(wsStoreT(t), root, memstore.New()), WithSnapshots(SnapshotPolicy{})}
			},
		},
		{
			name: "single per-session placement is valid",
			options: func(t *testing.T, root string) []Option {
				return []Option{WithSessionWorkspaces(wsStoreT(t), root), WithSnapshots(SnapshotPolicy{})}
			},
		},
		{
			name: "single shared placement is valid",
			options: func(t *testing.T, root string) []Option {
				return []Option{WithSharedWorkspace(wsStoreT(t), root), WithSnapshots(SnapshotPolicy{})}
			},
		},
		{
			name: "multiple placements rejected",
			options: func(t *testing.T, root string) []Option {
				return []Option{
					WithExclusiveWorkspace(wsStoreT(t), root, memstore.New()),
					WithSharedWorkspace(wsStoreT(t), root),
				}
			},
			wantErr:  true,
			wantKind: WorkspaceMultiplePlacements,
		},
		{
			name: "nil workspace store rejected",
			options: func(t *testing.T, root string) []Option {
				return []Option{WithSharedWorkspace(nil, root)}
			},
			wantErr:  true,
			wantKind: WorkspaceNilStore,
		},
		{
			name: "exclusive with nil leaser rejected",
			options: func(t *testing.T, root string) []Option {
				return []Option{WithExclusiveWorkspace(wsStoreT(t), root, nil)}
			},
			wantErr:  true,
			wantKind: WorkspaceNilLeaser,
		},
		{
			name: "empty root rejected",
			options: func(t *testing.T, root string) []Option {
				return []Option{WithSharedWorkspace(wsStoreT(t), "   ")}
			},
			wantErr:  true,
			wantKind: WorkspaceEmptyRoot,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			_, err := defineWith(t, sessionStoreT(t), tt.options(t, root)...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Define() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var target *WorkspacePlacementError
			if !errors.As(err, &target) || target.Kind != tt.wantKind {
				t.Fatalf("Define() err = %T %v, want kind %q", err, err, tt.wantKind)
			}
		})
	}
}

// TestDefineWorkspaceToolWithoutPlacement proves a workspace-requiring tool makes a
// no-placement rig invalid, while a placement satisfies it.
func TestDefineWorkspaceToolWithoutPlacement(t *testing.T) {
	t.Parallel()
	store := sessionStoreT(t)
	d, err := loop.Define(
		loop.WithName(identity.AgentName("planner")),
		loop.WithInference(&stubLLM{}, validModel("planner")),
		loop.WithTools(wsRequiringTool()),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	_, err = Define(WithLoops(d), WithPrimers("planner"), WithSessionStore(store))
	var target *WorkspacePlacementError
	if !errors.As(err, &target) || target.Kind != WorkspaceToolWithoutPlacement {
		t.Fatalf("Define() err = %T %v, want workspace-tool-without-placement", err, err)
	}

	// With a placement it is valid.
	store2 := sessionStoreT(t)
	d2, _ := loop.Define(
		loop.WithName(identity.AgentName("planner")),
		loop.WithInference(&stubLLM{}, validModel("planner")),
		loop.WithTools(wsRequiringTool()),
	)
	if _, err := Define(WithLoops(d2), WithPrimers("planner"), WithSessionStore(store2), WithSharedWorkspace(wsStoreT(t), t.TempDir()), WithSnapshots(SnapshotPolicy{})); err != nil {
		t.Fatalf("Define() with placement err = %v, want nil", err)
	}
}

// TestDefinePersistenceOverlap proves persistence paths equal to or beneath the workspace
// fail, while disjoint and ancestor paths are accepted.
func TestDefinePersistenceOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		spool   func(root string) string
		wantErr bool
	}{
		{name: "spool beneath root overlaps", spool: func(root string) string { return filepath.Join(root, "spool") }, wantErr: true},
		{name: "spool equal to root overlaps", spool: func(root string) string { return root }, wantErr: true},
		{name: "spool ancestor of root accepted", spool: func(root string) string { return filepath.Dir(root) }},
		{name: "spool disjoint accepted", spool: func(root string) string { return filepath.Join(filepath.Dir(root), "elsewhere") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			root := filepath.Join(parent, "project")
			ws := wsStoreT(t, workspacestore.WithSpoolDir(tt.spool(root)))
			_, err := defineWith(t, sessionStoreT(t), WithSharedWorkspace(ws, root), WithSnapshots(SnapshotPolicy{}))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Define() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var target *PersistenceOverlapError
				if !errors.As(err, &target) {
					t.Fatalf("Define() err = %T %v, want *PersistenceOverlapError", err, err)
				}
			}
		})
	}
}

// TestRootLeaseNameDeterministicAndAliasConverges proves the exclusive root lease name is a
// stable sha256 of the canonical root, and that lexical aliases converge on it.
func TestRootLeaseNameDeterministicAndAliasConverges(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	canon, err := canonicalPath(root)
	if err != nil {
		t.Fatalf("canonicalPath: %v", err)
	}
	name := rootLeaseName(canon)
	if !strings.HasPrefix(name, leaseNamePrefix) {
		t.Fatalf("lease name %q lacks prefix %q", name, leaseNamePrefix)
	}
	if err := storage.ValidateName(name); err != nil {
		t.Fatalf("lease name %q invalid: %v", name, err)
	}
	// A lexical alias (trailing "/." and "//") canonicalizes to the same name.
	aliasRaw := root + string(filepath.Separator) + "." + string(filepath.Separator)
	aliasCanon, err := canonicalPath(aliasRaw)
	if err != nil {
		t.Fatalf("canonicalPath(alias): %v", err)
	}
	if rootLeaseName(aliasCanon) != name {
		t.Fatalf("alias lease name %q != %q", rootLeaseName(aliasCanon), name)
	}
}

// TestPlacementFingerprintDistinguishesModeAndRoot proves the fingerprint field folds in
// {mode, canonical root} so different placements produce different fingerprints.
func TestPlacementFingerprintDistinguishesModeAndRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exclusive, _, err := resolvePlacement([]pendingPlacement{{mode: sessionruntime.PlacementExclusive, store: wsStoreT(t), root: root, leaser: memstore.New()}})
	if err != nil {
		t.Fatalf("resolve exclusive: %v", err)
	}
	shared, _, err := resolvePlacement([]pendingPlacement{{mode: sessionruntime.PlacementShared, store: wsStoreT(t), root: root}})
	if err != nil {
		t.Fatalf("resolve shared: %v", err)
	}
	fpExclusive := placementFingerprint(exclusive, exclusive.Root)
	fpShared := placementFingerprint(shared, shared.Root)
	if fpExclusive == fpShared {
		t.Fatalf("exclusive and shared placements produced identical fingerprint %q", fpExclusive)
	}
	if !strings.HasPrefix(fpExclusive, "exclusive:") || !strings.HasPrefix(fpShared, "shared:") {
		t.Fatalf("fingerprints do not carry mode: %q %q", fpExclusive, fpShared)
	}
}

// TestPathOverlapsRoot is the pure predicate table.
func TestPathOverlapsRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		root      string
		candidate string
		want      bool
	}{
		{name: "equal overlaps", root: "/a/b", candidate: "/a/b", want: true},
		{name: "beneath overlaps", root: "/a/b", candidate: "/a/b/c", want: true},
		{name: "ancestor disjoint", root: "/a/b", candidate: "/a", want: false},
		{name: "sibling disjoint", root: "/a/b", candidate: "/a/c", want: false},
		{name: "prefix-not-path disjoint", root: "/a/b", candidate: "/a/bc", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pathOverlapsRoot(tt.root, tt.candidate); got != tt.want {
				t.Fatalf("pathOverlapsRoot(%q,%q) = %v, want %v", tt.root, tt.candidate, got, tt.want)
			}
		})
	}
}

// TestWithSeedSnapshotValidation proves the NewSession seed option validation.
func TestWithSeedSnapshotValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		opts    []SessionOption
		wantErr bool
		kind    SessionOptionErrorKind
	}{
		{name: "no options", opts: nil},
		{name: "valid seed", opts: []SessionOption{WithSeedSnapshot(workspacestore.Ref("abc"))}},
		{name: "empty seed rejected", opts: []SessionOption{WithSeedSnapshot(workspacestore.Ref(""))}, wantErr: true, kind: SessionOptionEmptySeed},
		{name: "duplicate seed rejected", opts: []SessionOption{WithSeedSnapshot("a"), WithSeedSnapshot("b")}, wantErr: true, kind: SessionOptionDuplicateSeed},
		{name: "nil option rejected", opts: []SessionOption{nil}, wantErr: true, kind: SessionOptionNil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveSessionOptions(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveSessionOptions err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var target *SessionOptionError
				if !errors.As(err, &target) || target.Kind != tt.kind {
					t.Fatalf("err = %T %v, want kind %q", err, err, tt.kind)
				}
			}
		})
	}
}
