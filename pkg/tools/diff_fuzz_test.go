package tools

import "testing"

// FuzzDiffPreview asserts diffPreview is TOTAL over its untrusted before/after
// (and path) inputs: for any combination it returns a string WITHOUT PANICKING
// and WITHOUT HANGING. The diff math walks bounded common prefix/suffix indices,
// so the contract under fuzz is the no-panic / always-returns-a-string property
// (mirrors the package's other fuzzers). The result value is otherwise
// unconstrained — it is a human-readable preview, not a machine patch.
func FuzzDiffPreview(f *testing.F) {
	seeds := []struct {
		path   string
		before string
		after  string
	}{
		{"f.txt", "one\ntwo\nthree\n", "one\nTWO\nthree\n"}, // single-line change
		{"f.txt", "a\nb\nc\n", "a\nb\nc\n"},                 // identical (no changes)
		{"f.txt", "", "added\n"},                            // empty before
		{"f.txt", "removed\n", ""},                          // empty after
		{"f.txt", "", ""},                                   // both empty
		{"a/b/c.go", "x\ny\nz\nw\nv\n", "x\nY\nZ\nw\nv\n"},  // multi-line change w/ context
		{"f", "no trailing newline", "still none"},          // no trailing newline
		{"f", "\n\n\n", "\n"},                               // only newlines
		{"f", "líne\nüber\n", "line\nuber\n"},               // multibyte
		{"", "\x00\x01\n", "\x02\n"},                        // empty path + control bytes
		{"f", "same\ndiff-old\nsame\n", "same\ndiff-new\nsame\n"},
	}
	for _, s := range seeds {
		f.Add(s.path, s.before, s.after)
	}

	f.Fuzz(func(t *testing.T, path, before, after string) {
		// Must always return a string without panicking. A zero-length result is
		// permissible; the property is no-panic + termination. _ keeps the value
		// referenced so the call is not optimized away.
		_ = diffPreview(path, before, after)
	})
}
