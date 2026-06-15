package tools

import "testing"

// FuzzMatchFetch asserts MatchFetch is TOTAL: for any (record, callMethod,
// callURL) triple — including homographs, userinfo URLs, host:port, leading-dot
// records, path-prefix records, malformed schemes/grammars, empty strings, and
// arbitrary garbage — it returns a bool WITHOUT PANICKING and WITHOUT HANGING.
// The matcher is pure and fail-secure, so the result value is irrelevant under
// fuzz; the contract is "never panics, always terminates" (mirrors
// FuzzGlobMatch / FuzzContainedPath).
func FuzzMatchFetch(f *testing.F) {
	seeds := []struct {
		record     string
		callMethod string
		callURL    string
	}{
		// §5e happy path.
		{"GET https://example.com", "GET", "https://example.com"},
		// Homograph host on both record and call sides.
		{"GET https://münchen.de", "GET", "https://xn--mnchen-3ya.de/"},
		{"GET https://раура1.com", "GET", "https://paypa1.com/"}, // mixed-script homograph
		// user@host (userinfo) URLs.
		{"GET https://user@host", "GET", "https://user@host/"},
		{"GET https://example.com", "GET", "https://user:pass@example.com/loot"},
		// host:port.
		{"GET https://example.com:8443", "GET", "https://example.com:443/"},
		// Leading-dot suffix record.
		{"GET https://.github.com", "GET", "https://api.github.com/x"},
		// Path-prefix record.
		{"GET https://example.com/api", "GET", "https://example.com/api/v1"},
		// Malformed schemes / grammars.
		{"GET https://", "GET", "https://example.com/"},       // record missing host
		{"https://example.com", "GET", "https://example.com"}, // too few fields
		{"GET HTTP ://bad", "GET", "::::"},                    // malformed scheme + url
		{"GET ht!tp://x", "GET", "ht!tp://x"},                 // illegal scheme rune
		// Empty strings.
		{"", "", ""},
		{"GET https://example.com", "", ""},
		{"", "GET", "https://example.com/"},
		// Garbage.
		{"\x00\x01\x02", "\x00", "\x00"},
		{"GET GET GET GET", "GET", "not a url at all"},
		{"   ", "   ", "   "},
	}
	for _, s := range seeds {
		f.Add(s.record, s.callMethod, s.callURL)
	}

	f.Fuzz(func(t *testing.T, record, callMethod, callURL string) {
		// Must return a bool for any input without panicking. The result value
		// is irrelevant; the contract under fuzz is total + non-panicking.
		_ = MatchFetch(record, callMethod, callURL)
	})
}
