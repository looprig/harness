package tools

import (
	"strings"
	"testing"
)

// FuzzParseDuckDuckGoHTML asserts the DuckDuckGo HTML parser is TOTAL: for any
// byte sequence — valid HTML, truncated tags, deeply nested garbage, NUL bytes,
// unterminated attributes, or arbitrary noise — parseDuckDuckGoHTML returns a
// (possibly empty) []SearchResult WITHOUT PANICKING and WITHOUT exceeding the max
// cap. DuckDuckGo's HTML is untrusted scraped input, so the parser's contract
// under fuzz is "never panics, always terminates, never returns more than max"
// (mirrors FuzzMatchFetch / FuzzGlobMatch).
func FuzzParseDuckDuckGoHTML(f *testing.F) {
	seeds := []string{
		ddgFixture,
		"",
		"<a class=\"result__a\" href=\"https://x\">t</a>",
		`<div class="result"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fx">t</a><<<broken`,
		`<a class="result__a" href="`,                  // unterminated attribute
		`<a class="result__a result__snippet" href=x>`, // both classes on one tag
		"\x00\x01\x02<a>",
		strings.Repeat("<a class=\"result__a\" href=\"https://x\">", 1000),
	}
	for _, s := range seeds {
		f.Add(s, 5)
	}

	f.Fuzz(func(t *testing.T, body string, max int) {
		// Clamp max into the tool's legitimate range so the fuzzer doesn't just
		// explore negative/huge ints (the tool always clamps before calling).
		max = clampWebSearchResults(max)
		got := parseDuckDuckGoHTML(strings.NewReader(body), max)
		if len(got) > max {
			t.Fatalf("parser returned %d results, exceeds max %d", len(got), max)
		}
	})
}
