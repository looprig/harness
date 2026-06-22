package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateSkillName covers the strict ASCII-slug name rule (design §7a): the
// ONLY accepted names are a bounded slug `^[a-z0-9][a-z0-9_-]*$`; everything else
// — empty, `.`, `..`, separators, control chars, uppercase, over-length — is a
// containment violation. This is pure validation (no filesystem), so it is
// untagged.
func TestValidateSkillName(t *testing.T) {
	t.Parallel()

	overLong := strings.Repeat("a", maxSkillNameLen+1)
	atLimit := strings.Repeat("a", maxSkillNameLen)

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		// happy / valid slugs
		{name: "simple lowercase", input: "refactor", wantErr: false},
		{name: "with digits", input: "skill123", wantErr: false},
		{name: "starts with digit", input: "1skill", wantErr: false},
		{name: "with hyphen", input: "code-review", wantErr: false},
		{name: "with underscore", input: "code_review", wantErr: false},
		{name: "single char", input: "a", wantErr: false},
		{name: "single digit", input: "0", wantErr: false},
		{name: "at length limit", input: atLimit, wantErr: false},

		// boundary / error cases
		{name: "empty", input: "", wantErr: true},
		{name: "dot", input: ".", wantErr: true},
		{name: "dotdot", input: "..", wantErr: true},
		{name: "leading hyphen", input: "-skill", wantErr: true},
		{name: "leading underscore", input: "_skill", wantErr: true},
		{name: "forward slash", input: "a/b", wantErr: true},
		{name: "back slash", input: "a\\b", wantErr: true},
		{name: "traversal", input: "../etc", wantErr: true},
		{name: "uppercase", input: "Refactor", wantErr: true},
		{name: "all uppercase", input: "SKILL", wantErr: true},
		{name: "space", input: "code review", wantErr: true},
		{name: "dot in name", input: "a.b", wantErr: true},
		{name: "null byte", input: "a\x00b", wantErr: true},
		{name: "newline", input: "a\nb", wantErr: true},
		{name: "tab", input: "a\tb", wantErr: true},
		{name: "delete control", input: "a\x7fb", wantErr: true},
		{name: "non-ascii", input: "café", wantErr: true},
		{name: "over length", input: overLong, wantErr: true},
		{name: "tilde", input: "a~b", wantErr: true},
		{name: "colon", input: "a:b", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateSkillName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSkillName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				var ce *SkillContainmentError
				if !errors.As(err, &ce) {
					t.Fatalf("validateSkillName(%q) error = %v, want *SkillContainmentError", tt.input, err)
				}
				if ce.Name != tt.input {
					t.Errorf("SkillContainmentError.Name = %q, want %q", ce.Name, tt.input)
				}
				if ce.Reason == "" {
					t.Error("SkillContainmentError.Reason is empty, want a non-secret reason")
				}
			}
		})
	}
}

// TestLoadWorkspaceSkillSmoke drives loadWorkspaceSkill through its non-symlink
// paths with a real temp workspace: a happy load (correct body, size, SHA-256),
// a rejected bad name (before any FS touch), and a missing file. The exhaustive
// symlink-escape / non-regular / oversize cases that need real symlinks and FIFOs
// live in skill_workspace_integration_test.go (build tag `integration`); this
// untagged smoke test keeps the loader's core path covered in the default run.
func TestLoadWorkspaceSkillSmoke(t *testing.T) {
	t.Parallel()

	const doc = "---\nname: refactor\ndescription: helper\n---\nThe body text.\n"

	writeAt := func(t *testing.T, root, rel, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	t.Run("happy load", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeAt(t, root, ".skills/refactor/SKILL.md", doc)

		art, err := loadWorkspaceSkill(root, "refactor")
		if err != nil {
			t.Fatalf("loadWorkspaceSkill: %v", err)
		}
		if !art.Workspace {
			t.Error("Workspace = false, want true")
		}
		if art.RelPath != ".skills/refactor/SKILL.md" {
			t.Errorf("RelPath = %q, want .skills/refactor/SKILL.md", art.RelPath)
		}
		if art.Size != int64(len(doc)) {
			t.Errorf("Size = %d, want %d", art.Size, len(doc))
		}
		sum := sha256.Sum256([]byte(doc))
		if want := hex.EncodeToString(sum[:]); art.SHA256 != want {
			t.Errorf("SHA256 = %q, want %q", art.SHA256, want)
		}
		if !strings.Contains(art.Body, "The body text.") {
			t.Errorf("Body = %q, want the markdown body", art.Body)
		}
	})

	t.Run("bad name rejected before fs", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		_, err := loadWorkspaceSkill(root, "../escape")
		var ce *SkillContainmentError
		if !errors.As(err, &ce) {
			t.Fatalf("loadWorkspaceSkill = %v, want *SkillContainmentError", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		_, err := loadWorkspaceSkill(root, "absent")
		var nf *SkillNotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("loadWorkspaceSkill = %v, want *SkillNotFoundError", err)
		}
	})
}
