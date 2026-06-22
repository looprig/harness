package tools

import "testing"

func TestMatchGlob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pattern string
		relPath string
		want    bool
	}{
		// ** semantics.
		{name: "double star matches nested file", pattern: "src/**", relPath: "src/a/b.go", want: true},
		{name: "double star matches direct child", pattern: "src/**", relPath: "src/x.go", want: true},
		{name: "double star matches the base itself (zero segments)", pattern: "src/**", relPath: "src", want: true},
		{name: "double star does not match sibling of base", pattern: "src/**", relPath: "lib/x.go", want: false},
		{name: "bare double star matches anything deep", pattern: "**", relPath: "a/b/c/d.go", want: true},
		{name: "bare double star matches single", pattern: "**", relPath: "main.go", want: true},
		{name: "bare double star matches empty path", pattern: "**", relPath: "", want: true},
		{name: "bare double star matches dot path", pattern: "**", relPath: ".", want: true},
		{name: "double star in middle", pattern: "src/**/test.go", relPath: "src/a/b/test.go", want: true},
		{name: "double star in middle zero segments", pattern: "src/**/test.go", relPath: "src/test.go", want: true},
		{name: "double star in middle no match wrong tail", pattern: "src/**/test.go", relPath: "src/a/main.go", want: false},

		// Single-segment wildcards do not cross "/".
		{name: "star matches file in root", pattern: "*.go", relPath: "main.go", want: true},
		{name: "star does not cross slash", pattern: "*.go", relPath: "a/main.go", want: false},
		{name: "star within segment", pattern: "src/*.go", relPath: "src/main.go", want: true},
		{name: "question mark single char", pattern: "?.go", relPath: "a.go", want: true},
		{name: "question mark not two chars", pattern: "?.go", relPath: "ab.go", want: false},

		// Character classes.
		{name: "class matches a", pattern: "[ab].go", relPath: "a.go", want: true},
		{name: "class matches b", pattern: "[ab].go", relPath: "b.go", want: true},
		{name: "class rejects c", pattern: "[ab].go", relPath: "c.go", want: false},

		// Literals / exact.
		{name: "exact literal match", pattern: "go.mod", relPath: "go.mod", want: true},
		{name: "exact literal mismatch", pattern: "go.mod", relPath: "go.sum", want: false},
		{name: "nested literal match", pattern: "a/b/c.go", relPath: "a/b/c.go", want: true},

		// Cleaning / trailing slash / empty.
		{name: "trailing slash on path is cleaned", pattern: "src/*", relPath: "src/a/", want: true},
		{name: "trailing slash on pattern is cleaned", pattern: "src/", relPath: "src", want: true},
		{name: "double slash in path cleaned", pattern: "a/b", relPath: "a//b", want: true},
		{name: "dot segment in path cleaned", pattern: "a/b", relPath: "a/./b", want: true},
		{name: "empty pattern matches empty path", pattern: "", relPath: "", want: true},
		{name: "empty pattern does not match nonempty", pattern: "", relPath: "a.go", want: false},
		{name: "nonempty pattern does not match empty path", pattern: "*.go", relPath: "", want: false},

		// Literal ".." is handled sanely: it is just a segment. A pattern
		// without ".." does not match a path that (after clean) contains "..".
		{name: "dotdot path not matched by star pattern", pattern: "*", relPath: "../etc", want: false},
		{name: "dotdot path needs explicit dotdot pattern", pattern: "../etc", relPath: "../etc", want: true},

		// Bad pattern -> no panic, no match (verified by no-panic + want false).
		{name: "bad class pattern no match", pattern: "[", relPath: "a", want: false},
		{name: "bad class pattern in nested segment", pattern: "src/[", relPath: "src/a", want: false},
		{name: "bad pattern does not block trailing double star", pattern: "[/**", relPath: "x/y", want: false},

		// Mismatched length without ** must fail.
		{name: "pattern longer than path", pattern: "a/b/c", relPath: "a/b", want: false},
		{name: "path longer than pattern", pattern: "a/b", relPath: "a/b/c", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := matchGlob(tt.pattern, tt.relPath); got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.relPath, got, tt.want)
			}
		})
	}
}
