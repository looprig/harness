package tools

import (
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/idna"
)

// match.go holds the per-tool predicates that decide whether a saved
// ApprovalRecord.Match string matches a live tool call. These are
// SECURITY-CRITICAL: a false positive auto-approves a call the user never
// granted. Every predicate is FAIL-SECURE — any parse, normalization, or
// ambiguity error yields NO match (the caller then falls through to
// EffectAsk/EffectDeny, never EffectAutoApprove).
//
// The predicates are pure (no I/O) and return a plain bool: a matcher that
// "errors" is, by contract, a no-match. There is no (bool, error) signature
// because callers must treat every failure identically as "deny side" — a
// separate error channel would invite a caller to accidentally treat an error
// as anything other than no-match.

// schemeDefault is the scheme an Fetch approval record is interpreted under when
// it omits one: an "http://" grant must be explicit, so a bare host defaults to
// https (the safer scheme).
const schemeDefault = "https"

// fetchFieldCount is the number of space-separated fields in the Fetch match
// grammar "<METHOD> <target>": exactly two.
const fetchFieldCount = 2

// MatchFileGlob reports whether pattern matches the workspace-relative path
// relPath. relPath MUST be the relativised containedPath output (cleaned,
// symlink-resolved, workspace-relative) produced by the caller; as a fail-secure
// safety net this rejects any relPath that is absolute or contains a ".."
// segment, since such a path could never be a legitimate contained relPath and
// must never be matched. Matching itself delegates to the shared matchGlob
// (supports "**").
func MatchFileGlob(pattern, relPath string) bool {
	if isUnsafeRelPath(relPath) {
		return false
	}
	return matchGlob(pattern, relPath)
}

// isUnsafeRelPath reports whether relPath is absolute or escapes its root via a
// ".." segment. The caller is supposed to pass an already-contained relative
// path; anything else is a programming error or an attempted escape, and is
// treated as no-match.
func isUnsafeRelPath(relPath string) bool {
	if path.IsAbs(relPath) || strings.HasPrefix(relPath, "/") {
		return true
	}
	cleaned := path.Clean(relPath)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return true
	}
	// Inspect the pre-clean segments too: "a/../b" cleans to "b" (which is safe
	// to match as "b"), but we deliberately reject any input that contained a
	// ".." segment so a caller can never smuggle traversal through normalization.
	for _, seg := range strings.Split(relPath, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// MatchBash reports whether a live shell command matches a recorded Bash
// approval. record is the recorded Match; prefix is the record's Prefix flag
// (the hand-edited, risky opt-in); call is the live command.
//
// Both sides are normalized (trim + collapse internal whitespace runs to a
// single space). Default (prefix=false) is EXACT normalized equality: a grant of
// "go test ./..." does NOT approve "go test ./...; echo x". With prefix=true the
// match succeeds when the normalized call has the normalized record as a leading
// substring — used only when a human explicitly opts in.
func MatchBash(record string, prefix bool, call string) bool {
	nr := normalizeWhitespace(record)
	nc := normalizeWhitespace(call)
	if prefix {
		return strings.HasPrefix(nc, nr)
	}
	return nc == nr
}

// normalizeWhitespace trims leading/trailing whitespace and collapses every
// internal run of whitespace (spaces, tabs, newlines) to a single space. It is
// the canonical Bash command normalization shared by Grant (which records the
// normalized command) and MatchBash (which compares against it).
func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// fetchTarget is the parsed scheme+host+path of one side of a Fetch match: the
// canonical, IDNA-normalized comparison form. host is always lower-cased and
// punycode (ASCII); path is the cleaned URL path ("" means "any path").
type fetchTarget struct {
	method   string // upper-cased HTTP method
	scheme   string // lower-cased scheme (default https on the record side)
	host     string // lower-cased, idna.Lookup.ToASCII-normalized hostname (no port)
	hostDot  bool   // true if the record host began with a leading "." (suffix match)
	pathPref string // path-prefix; "" means "any path" (record side only)
}

// MatchFetch reports whether a live Fetch call (callMethod + callURL) matches a
// recorded Fetch approval whose Match grammar is "<METHOD> <scheme>://<host>
// [<path-prefix>]". Matching is exact on method and scheme, exact-or-leading-dot
// -suffix on host (NEVER substring), and an opt-in segment-aware path prefix.
// Every parse or IDNA-normalization failure on either side returns false
// (fail-secure).
func MatchFetch(record, callMethod, callURL string) bool {
	rec, ok := parseFetchRecord(record)
	if !ok {
		return false
	}
	call, ok := parseFetchCall(callMethod, callURL)
	if !ok {
		return false
	}
	if rec.method != call.method {
		return false
	}
	if rec.scheme != call.scheme {
		return false
	}
	if !hostMatches(rec, call.host) {
		return false
	}
	return pathMatches(rec.pathPref, call.pathPref)
}

// parseFetchRecord parses the record Match grammar into a canonical fetchTarget.
// It returns ok=false on any malformed input or non-normalizable host
// (fail-secure). A record host beginning with "." is a leading-dot suffix
// pattern; the dot is stripped for normalization and hostDot is set.
func parseFetchRecord(record string) (fetchTarget, bool) {
	fields := strings.Fields(record)
	if len(fields) != fetchFieldCount {
		return fetchTarget{}, false
	}
	method := strings.ToUpper(fields[0])
	target := fields[1]

	// Default the scheme to https when the record omits "scheme://".
	if !strings.Contains(target, "://") {
		target = schemeDefault + "://" + target
	}
	u, err := url.Parse(target)
	if err != nil {
		return fetchTarget{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return fetchTarget{}, false
	}

	rawHost := strings.ToLower(u.Hostname()) // Hostname() => port excluded
	dot := strings.HasPrefix(rawHost, ".")
	if dot {
		rawHost = strings.TrimPrefix(rawHost, ".")
	}
	host, ok := normalizeHost(rawHost)
	if !ok {
		return fetchTarget{}, false
	}

	return fetchTarget{
		method:   method,
		scheme:   scheme,
		host:     host,
		hostDot:  dot,
		pathPref: cleanPathPrefix(u.Path),
	}, true
}

// parseFetchCall parses the live call's method + URL into a canonical
// fetchTarget. The call side never carries a leading-dot host or a path-prefix
// pattern (its path is the concrete request path). It returns ok=false on a
// malformed URL, a missing host, or a non-normalizable host (fail-secure).
func parseFetchCall(callMethod, callURL string) (fetchTarget, bool) {
	u, err := url.Parse(callURL)
	if err != nil {
		return fetchTarget{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return fetchTarget{}, false
	}
	rawHost := strings.ToLower(u.Hostname())
	if rawHost == "" {
		return fetchTarget{}, false
	}
	host, ok := normalizeHost(rawHost)
	if !ok {
		return fetchTarget{}, false
	}
	return fetchTarget{
		method:   strings.ToUpper(strings.TrimSpace(callMethod)),
		scheme:   scheme,
		host:     host,
		pathPref: cleanPathPrefix(u.Path),
	}, true
}

// normalizeHost converts a hostname to its canonical ASCII (punycode) form via
// idna.Lookup.ToASCII (the strict lookup profile). A non-normalizable host
// (homograph with disallowed runes, invalid ACE label, etc.) returns ok=false
// so the caller fails secure. An empty host is not normalizable.
func normalizeHost(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" {
		return "", false
	}
	return ascii, true
}

// hostMatches reports whether callHost satisfies the record's host. With no
// leading dot it is EXACT equality. With a leading dot ("/.github.com") it
// matches the apex (callHost == record host) OR any subdomain (callHost ends in
// ".<recordHost>"). It is NEVER a substring/prefix match, so "example.com" can
// never match "example.com.evil.com".
func hostMatches(rec fetchTarget, callHost string) bool {
	if callHost == rec.host {
		return true
	}
	if rec.hostDot {
		return strings.HasSuffix(callHost, "."+rec.host)
	}
	return false
}

// cleanPathPrefix returns the cleaned URL path used for prefix comparison. A
// root ("/") or empty path normalizes to "" meaning "any path". Otherwise the
// path is path.Clean-ed and any trailing slash dropped so comparison is
// segment-aligned.
func cleanPathPrefix(p string) string {
	if p == "" || p == "/" {
		return ""
	}
	cleaned := path.Clean(p)
	if cleaned == "/" || cleaned == "." {
		return ""
	}
	return cleaned
}

// pathMatches reports whether callPath satisfies a record path-prefix. An empty
// recordPref means the record opted out of path matching → any path matches.
// Otherwise matching is SEGMENT-AWARE: callPath must equal recordPref or have it
// as a path-segment prefix ("/api" matches "/api" and "/api/v1" but NOT
// "/apixyz").
func pathMatches(recordPref, callPath string) bool {
	if recordPref == "" {
		return true
	}
	if callPath == recordPref {
		return true
	}
	return strings.HasPrefix(callPath, recordPref+"/")
}
