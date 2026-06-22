//go:build integration

package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// validSkill is a well-formed SKILL.md the loader can parse.
const validSkill = "---\nname: refactor\ndescription: refactor helper\n---\nBody line one.\nBody line two.\n"

// writeSkillFile creates parent dirs and writes a file under root, failing the test on
// any error. p is root-relative with forward slashes.
func writeSkillFile(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// mkSymlink creates a symlink at linkPath (root-relative) pointing to target
// (verbatim — may be absolute or relative), creating parent dirs first.
func mkSymlink(t *testing.T, root, linkPath, target string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(linkPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
}

// TestLoadWorkspaceSkillHappy proves a well-formed workspace skill loads with the
// correct body, byte size, and SHA-256 over the raw file bytes.
func TestLoadWorkspaceSkillHappy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkillFile(t, root, ".skills/refactor/SKILL.md", validSkill)

	sum := sha256.Sum256([]byte(validSkill))
	wantHash := hex.EncodeToString(sum[:])

	art, err := loadWorkspaceSkill(root, "refactor")
	if err != nil {
		t.Fatalf("loadWorkspaceSkill: %v", err)
	}
	if !art.Workspace {
		t.Error("artifact.Workspace = false, want true")
	}
	if art.RelPath != ".skills/refactor/SKILL.md" {
		t.Errorf("RelPath = %q, want .skills/refactor/SKILL.md", art.RelPath)
	}
	if art.Size != int64(len(validSkill)) {
		t.Errorf("Size = %d, want %d", art.Size, len(validSkill))
	}
	if art.SHA256 != wantHash {
		t.Errorf("SHA256 = %q, want %q", art.SHA256, wantHash)
	}
	if !strings.Contains(art.Body, "Body line one.") {
		t.Errorf("Body = %q, want the markdown body", art.Body)
	}
	if strings.Contains(art.Body, "name: refactor") {
		t.Errorf("Body = %q, must not contain frontmatter", art.Body)
	}
}

// TestLoadWorkspaceSkillIntermediateDirSymlinkEscape is the load-bearing TOCTOU
// test: an intermediate directory component (.skills/<name>) is a symlink whose
// target is OUTSIDE the workspace root. os.Root must refuse this per-component —
// not just the final file — so the escape is blocked before any bytes are read.
func TestLoadWorkspaceSkillIntermediateDirSymlinkEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir() // a sibling dir entirely outside root
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte(validSkill), 0o644); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}
	// .skills/evil -> <outside>  (an INTERMEDIATE dir symlink escaping the root)
	mkSymlink(t, root, ".skills/evil", outside)

	_, err := loadWorkspaceSkill(root, "evil")
	var ce *SkillContainmentError
	if !errors.As(err, &ce) {
		t.Fatalf("loadWorkspaceSkill = %v, want *SkillContainmentError (intermediate symlink escape)", err)
	}
}

// TestLoadWorkspaceSkillFinalSymlinkOutside rejects a final-file symlink pointing
// outside the root.
func TestLoadWorkspaceSkillFinalSymlinkOutside(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte(validSkill), 0o644); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}
	// .skills/out/SKILL.md -> <outside>/secret.md
	mkSymlink(t, root, ".skills/out/SKILL.md", secret)

	_, err := loadWorkspaceSkill(root, "out")
	var ce *SkillContainmentError
	if !errors.As(err, &ce) {
		t.Fatalf("loadWorkspaceSkill = %v, want *SkillContainmentError (final symlink outside)", err)
	}
}

// TestLoadWorkspaceSkillFinalSymlinkInside rejects a final-file symlink even when
// it points to a regular file INSIDE the root. os.Root would happily FOLLOW it
// (it stays in-root), so the loader must additionally Lstat the final component
// and refuse any symlink — a workspace SKILL.md must be a real regular file.
func TestLoadWorkspaceSkillFinalSymlinkInside(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkillFile(t, root, ".skills/real/SKILL.md", validSkill)
	// .skills/link/SKILL.md -> ../real/SKILL.md  (in-root final symlink)
	mkSymlink(t, root, ".skills/link/SKILL.md", filepath.FromSlash("../real/SKILL.md"))

	_, err := loadWorkspaceSkill(root, "link")
	var ce *SkillContainmentError
	if !errors.As(err, &ce) {
		t.Fatalf("loadWorkspaceSkill = %v, want *SkillContainmentError (final symlink inside)", err)
	}
}

// TestLoadWorkspaceSkillNonRegular rejects non-regular final targets: a directory
// named SKILL.md and a FIFO.
func TestLoadWorkspaceSkillNonRegular(t *testing.T) {
	t.Parallel()

	t.Run("directory", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		// .skills/dir/SKILL.md is itself a directory.
		if err := os.MkdirAll(filepath.Join(root, ".skills", "dir", "SKILL.md"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		_, err := loadWorkspaceSkill(root, "dir")
		var ce *SkillContainmentError
		if !errors.As(err, &ce) {
			t.Fatalf("loadWorkspaceSkill = %v, want *SkillContainmentError (dir target)", err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		dir := filepath.Join(root, ".skills", "pipe")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		fifo := filepath.Join(dir, "SKILL.md")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Skipf("Mkfifo unsupported: %v", err)
		}
		_, err := loadWorkspaceSkill(root, "pipe")
		var ce *SkillContainmentError
		if !errors.As(err, &ce) {
			t.Fatalf("loadWorkspaceSkill = %v, want *SkillContainmentError (fifo target)", err)
		}
	})
}

// TestLoadWorkspaceSkillOversize rejects a SKILL.md larger than maxSkillBytes with
// a *MalformedSkillError (consistent with the embedded parser's size ceiling).
func TestLoadWorkspaceSkillOversize(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	big := strings.Repeat("x", maxSkillBytes+1)
	writeSkillFile(t, root, ".skills/big/SKILL.md", big)

	_, err := loadWorkspaceSkill(root, "big")
	var me *MalformedSkillError
	if !errors.As(err, &me) {
		t.Fatalf("loadWorkspaceSkill = %v, want *MalformedSkillError (oversize)", err)
	}
	if me.Name != "big" {
		t.Errorf("MalformedSkillError.Name = %q, want big", me.Name)
	}
}

// TestLoadWorkspaceSkillMalformed rejects a SKILL.md with no frontmatter fence
// with a *MalformedSkillError stamped with the skill name.
func TestLoadWorkspaceSkillMalformed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkillFile(t, root, ".skills/bad/SKILL.md", "no frontmatter here\njust body\n")

	_, err := loadWorkspaceSkill(root, "bad")
	var me *MalformedSkillError
	if !errors.As(err, &me) {
		t.Fatalf("loadWorkspaceSkill = %v, want *MalformedSkillError (malformed)", err)
	}
	if me.Name != "bad" {
		t.Errorf("MalformedSkillError.Name = %q, want bad", me.Name)
	}
}

// TestLoadWorkspaceSkillMissing returns a *SkillNotFoundError when the SKILL.md is
// absent (the name is valid and contained, but no file exists).
func TestLoadWorkspaceSkillMissing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// no .skills/ at all
	_, err := loadWorkspaceSkill(root, "absent")
	var nf *SkillNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("loadWorkspaceSkill = %v, want *SkillNotFoundError (missing)", err)
	}
	if nf.Name != "absent" {
		t.Errorf("SkillNotFoundError.Name = %q, want absent", nf.Name)
	}
}

// TestLoadWorkspaceSkillBadName rejects an invalid name BEFORE any filesystem
// access, with a *SkillContainmentError.
func TestLoadWorkspaceSkillBadName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, bad := range []string{"", ".", "..", "a/b", "../etc", "Bad"} {
		bad := bad
		t.Run("name="+bad, func(t *testing.T) {
			t.Parallel()
			_, err := loadWorkspaceSkill(root, bad)
			var ce *SkillContainmentError
			if !errors.As(err, &ce) {
				t.Fatalf("loadWorkspaceSkill(%q) = %v, want *SkillContainmentError", bad, err)
			}
		})
	}
}

// TestLoadWorkspaceSkillBadRoot returns an error when the workspace root cannot be
// opened as an os.Root (e.g. it does not exist).
func TestLoadWorkspaceSkillBadRoot(t *testing.T) {
	t.Parallel()

	missingRoot := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := loadWorkspaceSkill(missingRoot, "refactor")
	if err == nil {
		t.Fatal("loadWorkspaceSkill with missing root = nil error, want non-nil")
	}
}
