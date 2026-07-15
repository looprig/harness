//go:build integration

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// wsSkillBody is the markdown body of the workspace fixture skill (the bytes after
// the frontmatter that loadWorkspaceSkill returns as Body).
const wsSkillBody = "Workspace body line.\n"

// wsSkill is a well-formed workspace SKILL.md whose name does NOT collide with the
// embedded code-style fixture, so it exercises the pure workspace path.
const wsSkill = "---\nname: ws-refactor\ndescription: a workspace skill\n---\n" + wsSkillBody

// newWorkspaceSkillTool wires an embedded loader (the operator may load code-style)
// PLUS a workspace root, returning a workspace-enabled Skill bound to "operator".
func newWorkspaceSkillTool(t *testing.T, root string) *Skill {
	t.Helper()
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	return NewSkill(loader, identity.AgentName("operator"), WithWorkspaceRoot(root))
}

// TestSkillWorkspaceLoadHappy proves the full workspace gate path for a
// non-embedded name on a workspace-enabled Skill: CheckEffect → EffectAsk; Prepare
// → a *tool.SkillArtifact with the right size + SHA-256; BuildRequest(_, artifact)
// → a SkillLoadRequest carrying that metadata, AllowedScopes()=={ScopeOnce};
// InvokableRun (artifact in ctx) → the snapshot Body verbatim.
func TestSkillWorkspaceLoadHappy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkillFile(t, root, ".skills/ws-refactor/SKILL.md", wsSkill)

	sum := sha256.Sum256([]byte(wsSkill))
	wantHash := hex.EncodeToString(sum[:])
	wantRel := ".skills/ws-refactor/SKILL.md"

	s := newWorkspaceSkillTool(t, root)
	args := `{"name":"ws-refactor"}`

	// CheckEffect: an untrusted workspace load must be gated.
	eff, handled := s.CheckEffect(args)
	if !handled {
		t.Fatalf("CheckEffect handled = false, want handled=true for a workspace load")
	}
	if eff != loop.EffectAsk {
		t.Fatalf("CheckEffect effect = %v, want EffectAsk", eff)
	}

	// Prepare: a TOCTOU-safe snapshot artifact with the correct metadata.
	prepared, err := s.Prepare(context.Background(), uuid.UUID{}, args)
	if err != nil {
		t.Fatalf("Prepare error = %v, want nil", err)
	}
	art, ok := prepared.(*tool.SkillArtifact)
	if !ok || art == nil {
		t.Fatalf("Prepare artifact type = %T, want *tool.SkillArtifact", prepared)
	}
	if !art.Workspace {
		t.Error("artifact.Workspace = false, want true")
	}
	if art.RelPath != wantRel {
		t.Errorf("RelPath = %q, want %q", art.RelPath, wantRel)
	}
	if art.Size != int64(len(wsSkill)) {
		t.Errorf("Size = %d, want %d", art.Size, len(wsSkill))
	}
	if art.SHA256 != wantHash {
		t.Errorf("SHA256 = %q, want %q", art.SHA256, wantHash)
	}
	if art.Body != wsSkillBody {
		t.Errorf("artifact.Body = %q, want %q", art.Body, wsSkillBody)
	}

	// BuildRequest: a SkillLoadRequest carrying the snapshot metadata + ScopeOnce.
	req, err := s.BuildRequest(args, prepared)
	if err != nil {
		t.Fatalf("BuildRequest error = %v, want nil", err)
	}
	slr, ok := req.(tool.SkillLoadRequest)
	if !ok {
		t.Fatalf("BuildRequest type = %T, want tool.SkillLoadRequest", req)
	}
	if slr.RelPath != wantRel {
		t.Errorf("request RelPath = %q, want %q", slr.RelPath, wantRel)
	}
	if slr.Agent != identity.AgentName("operator") {
		t.Errorf("request Agent = %q, want operator", slr.Agent)
	}
	if slr.Size != int64(len(wsSkill)) {
		t.Errorf("request Size = %d, want %d", slr.Size, len(wsSkill))
	}
	if slr.SHA256 != wantHash {
		t.Errorf("request SHA256 = %q, want %q", slr.SHA256, wantHash)
	}
	if scopes := req.AllowedScopes(); len(scopes) != 1 || scopes[0] != tool.ScopeOnce {
		t.Errorf("AllowedScopes = %v, want exactly [ScopeOnce] (untrusted load is never persisted)", scopes)
	}
	// The prompt body must never carry the skill body.
	if strings.Contains(req.Description(), wsSkillBody) {
		t.Errorf("Description() = %q leaks the skill body into the prompt", req.Description())
	}

	// InvokableRun: with the artifact in ctx, return the approved snapshot Body.
	ctx := loop.WithPrepared(context.Background(), prepared)
	res, err := s.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("InvokableRun Go error = %v, want nil", err)
	}
	if got := textOf(t, res); got != wsSkillBody {
		t.Errorf("InvokableRun result = %q, want the snapshot body %q", got, wsSkillBody)
	}
}

// TestSkillWorkspaceNoReReadAfterPrepare is the TOCTOU proof: after Prepare takes
// the snapshot, the on-disk file is MUTATED (and then DELETED). InvokableRun must
// still return the ORIGINAL snapshot Body — it reads the artifact bound to the
// call, never re-opening the file — so a workspace writer cannot swap the body
// between the human prompt and execution.
func TestSkillWorkspaceNoReReadAfterPrepare(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rel := ".skills/ws-refactor/SKILL.md"
	writeSkillFile(t, root, rel, wsSkill)

	s := newWorkspaceSkillTool(t, root)
	args := `{"name":"ws-refactor"}`

	prepared, err := s.Prepare(context.Background(), uuid.UUID{}, args)
	if err != nil {
		t.Fatalf("Prepare error = %v, want nil", err)
	}

	full := filepath.Join(root, filepath.FromSlash(rel))

	// 1. MUTATE the file to a different, malicious body after the snapshot.
	tampered := "---\nname: ws-refactor\ndescription: tampered\n---\nMALICIOUS injected body.\n"
	if err := os.WriteFile(full, []byte(tampered), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	ctx := loop.WithPrepared(context.Background(), prepared)
	res, err := s.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("InvokableRun (after mutate) Go error = %v, want nil", err)
	}
	got := textOf(t, res)
	if got != wsSkillBody {
		t.Errorf("after mutate, result = %q, want the ORIGINAL snapshot body %q (no re-read)", got, wsSkillBody)
	}
	if strings.Contains(got, "MALICIOUS") {
		t.Errorf("result = %q served the tampered on-disk body — TOCTOU re-read bug", got)
	}

	// 2. DELETE the file entirely; InvokableRun must STILL serve the snapshot.
	if err := os.Remove(full); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	res2, err := s.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("InvokableRun (after delete) Go error = %v, want nil", err)
	}
	if got := textOf(t, res2); got != wsSkillBody {
		t.Errorf("after delete, result = %q, want the snapshot body %q (no re-read)", got, wsSkillBody)
	}
}

// TestSkillWorkspaceEmbeddedWins proves that a name present in BOTH the embedded
// allow-set AND the workspace resolves to the EMBEDDED skill with NO workspace
// consult: even with a same-named (different-bodied) file planted in the workspace,
// CheckEffect auto-approves (handled=false), Prepare returns a nil artifact, and
// InvokableRun returns the EMBEDDED body — never the planted workspace body.
func TestSkillWorkspaceEmbeddedWins(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Plant a same-named workspace skill with an attacker body under the EMBEDDED
	// name "code-style"; embedded-wins must ignore it entirely.
	planted := "---\nname: code-style\ndescription: planted\n---\nPLANTED workspace body.\n"
	writeSkillFile(t, root, ".skills/code-style/SKILL.md", planted)

	s := newWorkspaceSkillTool(t, root)
	args := `{"name":"code-style"}`

	// Auto-approve fall-through (embedded), not a workspace gate.
	if eff, handled := s.CheckEffect(args); handled {
		t.Errorf("CheckEffect handled = true (eff=%v); embedded-wins must auto-approve, not gate", eff)
	}
	// No workspace consult → nil artifact.
	prepared, err := s.Prepare(context.Background(), uuid.UUID{}, args)
	if err != nil {
		t.Fatalf("Prepare error = %v, want nil", err)
	}
	if prepared != nil {
		t.Fatalf("Prepare = %v, want nil artifact (embedded-wins never consults the workspace)", prepared)
	}
	// No artifact in ctx → embedded loader path → embedded body, never the planted one.
	res, err := s.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("InvokableRun Go error = %v, want nil", err)
	}
	got := textOf(t, res)
	if got != skillToolBody {
		t.Errorf("result = %q, want the EMBEDDED body %q", got, skillToolBody)
	}
	if strings.Contains(got, "PLANTED") {
		t.Errorf("result = %q served the planted workspace body — embedded-wins violated", got)
	}
}

// TestSkillWorkspaceUnknownMissingFile proves the fail-secure Prepare path: a
// workspace-enabled, non-embedded name whose file is ABSENT yields a typed Prepare
// error (no artifact), so the runner fails the call fail-secure (no gate, no body).
func TestSkillWorkspaceUnknownMissingFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir() // no .skills/ tree at all
	s := newWorkspaceSkillTool(t, root)
	args := `{"name":"does-not-exist"}`

	// It IS gated (workspace-enabled, non-embedded), so the gate-time decision Asks.
	if eff, handled := s.CheckEffect(args); !handled || eff != loop.EffectAsk {
		t.Errorf("CheckEffect = (%v, handled=%v), want (EffectAsk, true)", eff, handled)
	}

	prepared, err := s.Prepare(context.Background(), uuid.UUID{}, args)
	if prepared != nil {
		t.Errorf("Prepare artifact = %v, want nil on the missing-file path", prepared)
	}
	var nf *SkillNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("Prepare error = %v, want *SkillNotFoundError (fail-secure)", err)
	}
}

// TestSkillWorkspaceBadArgsFailSecure proves a workspace-enabled tool with
// unparseable/empty-name args returns a typed Prepare error (fail-secure) rather
// than silently loading nothing.
func TestSkillWorkspaceBadArgsFailSecure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		argsJSON string
	}{
		{name: "unparseable args", argsJSON: `not json`},
		{name: "empty name", argsJSON: `{"name":""}`},
		{name: "whitespace name", argsJSON: `{"name":"   "}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			s := newWorkspaceSkillTool(t, root)

			prepared, err := s.Prepare(context.Background(), uuid.UUID{}, tt.argsJSON)
			if prepared != nil {
				t.Errorf("Prepare artifact = %v, want nil on bad args", prepared)
			}
			var ce *SkillContainmentError
			if !errors.As(err, &ce) {
				t.Fatalf("Prepare error = %v, want *SkillContainmentError (fail-secure)", err)
			}
		})
	}
}
