package tools

import (
	"strings"
	"testing"
)

// FuzzApplyReplacement asserts applyReplacement is TOTAL over its untrusted
// inputs (original/old/replacement + replace_all): for any combination it returns
// WITHOUT PANICKING and WITHOUT HANGING. The primary contract under fuzz is the
// no-panic / termination property (mirrors FuzzGlobMatch / FuzzContainedPath /
// FuzzMatchFetch). The function is a thin wrapper over strings.Count/Replace, so
// the only structural invariant asserted beyond no-panic is: a rule violation
// (non-empty errMsg) ALWAYS returns an empty result (fail-clean). The "no `old`
// remains after replace_all" property is deliberately NOT asserted — non-overlap-
// ping replacement can re-create `old` across the seam (e.g. ReplaceAll("aaaa",
// "aa","a") == "aa"), so it does not hold in general and over-constrains.
func FuzzApplyReplacement(f *testing.F) {
	seeds := []struct {
		original    string
		old         string
		replacement string
		replaceAll  bool
	}{
		{"alpha\nbravo\ncharlie\n", "bravo", "BRAVO", false}, // single unique match
		{"x\nx\nother\n", "x", "y", true},                    // replace all
		{"only-one\n", "only-one", "two", true},              // replace_all, single match
		{"hello", "zulu", "X", false},                        // zero matches
		{"x\nx\n", "x", "y", false},                          // ambiguous (>=2, !all)
		{"", "", "", false},                                  // all empty
		{"abc", "", "Z", false},                              // empty old (Count==len+1)
		{"aaaa", "a", "aa", true},                            // replacement contains old
		{"aaaa", "aa", "a", true},                            // overlapping-ish all
		{"line", "line", "", false},                          // replace with empty
		{"\x00\x01", "\x00", "\x02", true},                   // control bytes
		{strings.Repeat("ab", 256), "ab", "cd", true},        // many matches
		{"über", "ü", "u", true},                             // multibyte
	}
	for _, s := range seeds {
		f.Add(s.original, s.old, s.replacement, s.replaceAll)
	}

	f.Fuzz(func(t *testing.T, original, old, replacement string, replaceAll bool) {
		// Must terminate and never panic for any input.
		updated, errMsg := applyReplacement(original, old, replacement, replaceAll)

		if errMsg != "" {
			// A rule violation returns an empty result and a non-secret message
			// (fail-clean): the only structural invariant safe to assert here.
			if updated != "" {
				t.Fatalf("rule violation returned a non-empty result %q with errMsg %q", updated, errMsg)
			}
		}
	})
}
