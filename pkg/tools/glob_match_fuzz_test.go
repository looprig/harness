package tools

import "testing"

// FuzzGlobMatch asserts matchGlob never panics (and, by virtue of its iterative
// bounded algorithm, never hangs) for any (pattern, path) pair — including
// malformed patterns where path.Match would return ErrBadPattern.
func FuzzGlobMatch(f *testing.F) {
	seeds := []struct {
		pattern string
		relPath string
	}{
		{"src/**", "src/a/b.go"},
		{"**", "anything/at/all"},
		{"*.go", "main.go"},
		{"[ab].go", "a.go"},
		{"src/**/test.go", "src/x/test.go"},
		{"", ""},
		{"[", "a"},   // bad pattern
		{"[a-", "z"}, // bad pattern
		{"a/**/b/**/c", "a/b/c"},
		{"../etc", "../etc"},
		{"a//b/./c", "a/b/c"},
		{"\\", "x"},         // lone escape -> ErrBadPattern in path.Match
		{"**/**/**", "a/b"}, // many doublestars
	}
	for _, s := range seeds {
		f.Add(s.pattern, s.relPath)
	}
	f.Fuzz(func(t *testing.T, pattern, relPath string) {
		// Must return a bool for any input without panicking. The result value
		// is irrelevant; the contract under fuzz is total + non-panicking.
		_ = matchGlob(pattern, relPath)
	})
}
