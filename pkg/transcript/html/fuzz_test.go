package html

import (
	"regexp"
	"strings"
	"testing"
)

// bannedTagOpeners are tag openers that goldmark NEVER emits when raw-HTML
// passthrough is disabled. None is a markdown construct, and goldmark escapes any
// literal '<' in text to "&lt;", so a literal "<script"/"<svg"/… substring in the
// output can only mean raw HTML leaked through — the exact XSS regression Task 8
// guards against. (goldmark DOES legitimately emit <img>/<a>/<input> from markdown
// image/link/task-list syntax with escaped, quoted attributes, so those tags are
// NOT banned. A dangerous URL scheme like "javascript:" is intentionally absent
// here: as plain text it is harmless — goldmark renders it as inert prose — and a
// substring ban would false-positive on it; the scheme is filtered to "" inside
// real href/src values, which TestRenderMarkdownXSS verifies on controlled input.)
var bannedTagOpeners = []string{
	"<script", "<svg", "<iframe", "<object", "<embed",
}

var (
	// tagRe captures each literal HTML tag region. Escaped markup uses &lt;…&gt;,
	// so it never matches here — only real tags goldmark emitted do. The trailing
	// `(?:>|$)` also catches an UNTERMINATED tag at end-of-input (e.g. a leaked
	// `<img onerror=alert(1)` with no closing `>`), so a handler there is still
	// scanned rather than silently skipped.
	tagRe = regexp.MustCompile(`<[^>]*(?:>|$)`)
	// quotedRe matches a quoted attribute value; stripping these leaves only the
	// tag's bare attribute names, where a live handler would have to sit.
	quotedRe = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	// liveAttrRe matches an on…= event-handler attribute name. The separator class
	// is `[\s/]`, not just whitespace, because HTML5 also accepts `/` between
	// attributes, so `<img/onerror=…>` must not slip the oracle.
	liveAttrRe = regexp.MustCompile(`(?i)[\s/]on[a-z]+\s*=`)
)

// containsLiveEventHandler reports whether out carries an on…= event handler as a
// LIVE attribute on a real tag. It scans only literal <…> tag regions and strips
// quoted attribute values first, so an on…= that is merely escaped code text (e.g.
// `x onload=y` → <code>x onload=y</code>) or a quoted attribute value (e.g. an
// image alt, <img … alt=" onload=x">) is correctly NOT flagged — only a genuinely
// live handler is. This is why the fuzz property below does not naively scan the
// whole output for `(?i)[\s/]on\w+=`: goldmark legitimately escapes such bytes, and
// a blanket scan would report spurious crashes on benign input.
func containsLiveEventHandler(out string) bool {
	for _, tag := range tagRe.FindAllString(out, -1) {
		bare := quotedRe.ReplaceAllString(tag, "")
		if liveAttrRe.MatchString(bare) {
			return true
		}
	}
	return false
}

// FuzzRenderMarkdown asserts the single renderMarkdown chokepoint can never emit
// live raw HTML, for ANY input. The properties: renderMarkdown returns without
// error (the fuzz harness independently catches panics and hangs); the output
// carries none of the bannedTagOpeners (proving raw-HTML passthrough stays off);
// and no live event-handler attribute survives. Seeded with the XSS payloads and
// normal GFM markdown so the corpus explores both the attack surface and the
// happy path.
func FuzzRenderMarkdown(f *testing.F) {
	seeds := []string{
		"",
		"# Title\n\n- a\n- b\n\n`code`",
		"~~strike~~ and https://example.com",
		"<script>alert(1)</script>",
		"</script><img onerror=alert(1) src=x>",
		"<svg onload=alert(1)>",
		"`<svg onload=alert(1)>`",
		"```\n<svg onload=alert(1)>\n```",
		"![alt](javascript:alert(1))",
		"[link](javascript:alert(1))",
		"<a href=\"x\" onclick=\"y\">z</a>",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, err := renderMarkdown(in)
		if err != nil {
			t.Fatalf("renderMarkdown(%q) error = %v", in, err)
		}
		low := strings.ToLower(string(got))
		for _, banned := range bannedTagOpeners {
			if strings.Contains(low, banned) {
				t.Fatalf("raw HTML leaked: output for %q contains %q\n%s", in, banned, got)
			}
		}
		if containsLiveEventHandler(string(got)) {
			t.Fatalf("live event handler in output for input %q\n%s", in, got)
		}
	})
}
