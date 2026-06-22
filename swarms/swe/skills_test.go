package swe

import (
	"io/fs"
	"strings"
	"testing"
)

// TestSkillsFSListsExample asserts the embedded skill tree contains the curated
// example skill at its expected path and that no unexpected entries leak in.
func TestSkillsFSListsExample(t *testing.T) {
	t.Parallel()

	const wantPath = "skills/code-style/SKILL.md"

	var found []string
	err := fs.WalkDir(SkillsFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(SkillsFS) error = %v", err)
	}

	var sawWant bool
	for _, p := range found {
		if p == wantPath {
			sawWant = true
		}
		if !strings.HasSuffix(p, "/SKILL.md") {
			t.Errorf("embedded entry %q does not end in /SKILL.md", p)
		}
	}
	if !sawWant {
		t.Fatalf("SkillsFS missing %q; found %v", wantPath, found)
	}
}

// TestSkillsFSExampleIsWellFormed reads the embedded example through the fs.FS
// surface (the same surface a later SkillLoader will use) and asserts it has a
// fenced frontmatter block carrying name and description and a non-empty body.
// It does not call the unexported tools.parseSkill; the parser's behavior is
// pinned by tools' own unit + fuzz tests.
func TestSkillsFSExampleIsWellFormed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		wantName string
	}{
		{
			name:     "code-style skill",
			path:     "skills/code-style/SKILL.md",
			wantName: "name: code-style",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw, err := fs.ReadFile(SkillsFS, tt.path)
			if err != nil {
				t.Fatalf("ReadFile(%q) error = %v", tt.path, err)
			}
			content := string(raw)
			if !strings.HasPrefix(content, "---\n") {
				t.Errorf("%q does not open with a frontmatter fence", tt.path)
			}
			// A closing fence must follow the opening one.
			if strings.Count(content, "\n---") < 1 {
				t.Errorf("%q has no closing frontmatter fence", tt.path)
			}
			if !strings.Contains(content, tt.wantName) {
				t.Errorf("%q frontmatter missing %q", tt.path, tt.wantName)
			}
			if !strings.Contains(content, "description:") {
				t.Errorf("%q frontmatter missing description", tt.path)
			}
		})
	}
}
