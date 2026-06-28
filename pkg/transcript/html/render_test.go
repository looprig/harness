package html

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// update regenerates the golden files when set: go test -run TestRenderMinimal -update.
var update = flag.Bool("update", false, "update golden files")

// fixedSessionID is a stable UUID so the golden is byte-deterministic.
var fixedSessionID = uuid.UUID{
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
}

// minimalSession builds the tiny model Task 7 renders: one user turn with one AI
// step, no tools/gates/subagents. Every timestamp is a fixed time.Date so the
// rendered golden is byte-stable without normalization (the renderer never reads
// a clock).
func minimalSession() *transcript.Session {
	started := time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)
	ended := time.Date(2026, 6, 28, 10, 5, 0, 0, time.UTC)
	exported := time.Date(2026, 6, 28, 10, 6, 0, 0, time.UTC)
	userAt := time.Date(2026, 6, 28, 10, 0, 1, 0, time.UTC)
	aiAt := time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC)

	return &transcript.Session{
		SessionID: fixedSessionID,
		Title:     "Minimal transcript",
		Config: transcript.Config{
			ModelID:           "claude-opus-4-8",
			AgentKind:         "operator",
			PermissionPosture: "ask",
			SystemPromptRev:   "rev-1",
		},
		StartedAt:  started,
		EndedAt:    ended,
		ExportedAt: exported,
		Root: &transcript.Loop{
			LoopID:    fixedSessionID,
			AgentName: "operator",
			StartedAt: started,
			Turns: []*transcript.Turn{
				{
					Index:     1,
					StartedAt: userAt,
					EndedAt:   aiAt,
					Outcome:   transcript.OutcomeDone,
					User: &transcript.Message{
						Role:   content.RoleUser,
						At:     userAt,
						Blocks: []content.Block{&content.TextBlock{Text: "Run the tests please."}},
					},
					Steps: []*transcript.Step{
						{
							AI: &transcript.Message{
								Role: content.RoleAssistant,
								At:   aiAt,
								Blocks: []content.Block{
									&content.TextBlock{Text: "## Done\n\nRan with `go test ./...` and all passed."},
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestRenderMinimal(t *testing.T) {
	t.Parallel()

	const golden = "testdata/minimal.golden.html"

	var buf bytes.Buffer
	if err := Render(&buf, minimalSession()); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	got := buf.Bytes()

	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", golden, err)
		}
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden %s: %v", golden, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Render() output != golden %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}

// TestRenderDeterministic proves the renderer reads no clock and iterates no maps:
// rendering the same model twice yields byte-identical output.
func TestRenderDeterministic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    *transcript.Session
	}{
		{name: "minimal session", s: minimalSession()},
		{name: "empty session (nil root)", s: &transcript.Session{SessionID: fixedSessionID, Title: "Empty"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var a, b bytes.Buffer
			if err := Render(&a, tt.s); err != nil {
				t.Fatalf("Render() first pass error = %v", err)
			}
			if err := Render(&b, tt.s); err != nil {
				t.Fatalf("Render() second pass error = %v", err)
			}
			if !bytes.Equal(a.Bytes(), b.Bytes()) {
				t.Errorf("Render() not deterministic across two passes")
			}
		})
	}
}

// failWriter fails every Write to exercise the RenderError path.
type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

// TestRenderError covers the typed-error contract: a write failure surfaces as a
// *RenderError whose Unwrap returns the cause.
func TestRenderError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("disk full")
	tests := []struct {
		name      string
		w         failWriter
		wantCause error
	}{
		{name: "write failure wraps cause", w: failWriter{err: sentinel}, wantCause: sentinel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Render(tt.w, minimalSession())
			if err == nil {
				t.Fatalf("Render() error = nil, want *RenderError")
			}
			var re *RenderError
			if !errors.As(err, &re) {
				t.Fatalf("Render() error = %T, want *RenderError", err)
			}
			if !errors.Is(err, tt.wantCause) {
				t.Errorf("errors.Is(err, cause) = false, want true (err=%v)", err)
			}
		})
	}
}

// TestRenderMarkdown proves the goldmark seam renders CommonMark structure and
// that the GFM extension is enabled: headings, lists and inline code render, and
// GFM-specific strikethrough (~~…~~ → <del>) and bare-URL autolinking work. Plain
// goldmark (no GFM) would leave ~~strike~~ and the bare URL as literal text, so
// these rows fail until extension.GFM is wired in.
func TestRenderMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		contains  []string
		wantEmpty bool
	}{
		{
			name:      "empty input early return",
			in:        "",
			wantEmpty: true,
		},
		{
			name:     "heading list and inline code",
			in:       "# Title\n\n- a\n- b\n\n`code`",
			contains: []string{"<h1", "<ul>", "<li>a</li>", "<li>b</li>", "<code>code</code>"},
		},
		{
			name:     "gfm strikethrough",
			in:       "~~strike~~",
			contains: []string{"<del>strike</del>"},
		},
		{
			name:     "gfm autolink of bare url",
			in:       "see https://example.com now",
			contains: []string{`<a href="https://example.com">https://example.com</a>`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := renderMarkdown(tt.in)
			if err != nil {
				t.Fatalf("renderMarkdown(%q) error = %v", tt.in, err)
			}
			out := string(got)
			if tt.wantEmpty && out != "" {
				t.Errorf("renderMarkdown(%q) = %q, want empty (early-return path)", tt.in, out)
			}
			for _, sub := range tt.contains {
				if !strings.Contains(out, sub) {
					t.Errorf("renderMarkdown(%q) = %q, want substring %q", tt.in, out, sub)
				}
			}
		})
	}
}

// TestRenderMarkdownXSS is the security core (Decision 11): raw HTML in message
// text must never survive as live markup. goldmark runs with raw-HTML passthrough
// OFF, so a raw tag is omitted (replaced by an HTML comment) and a tag inside a
// code span/fence is escaped to &lt;…&gt;. Each row scans the rendered bytes for
// live attack vectors and confirms they are inert. Note: ` onload=` is forbidden
// only for the raw-HTML rows (where the whole tag is dropped); in the code-fence
// row it legitimately appears as escaped, inert text, so only the live `<svg`
// opener is forbidden there.
func TestRenderMarkdownXSS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		forbidden []string // must NOT appear (case-insensitive): live vectors
		required  []string // must appear (exact case): proof of neutralization
	}{
		{
			name:      "user script tag omitted",
			in:        "<script>alert(1)</script>",
			forbidden: []string{"<script", "alert(1)", "<svg", "<img", " onerror=", " onload="},
			required:  []string{"<!-- raw HTML omitted -->"},
		},
		{
			name:      "ai closing-script then img onerror omitted",
			in:        "</script><img onerror=alert(1) src=x>",
			forbidden: []string{"<script", "<img", " onerror=", "alert(1)"},
			required:  []string{"<!-- raw HTML omitted -->"},
		},
		{
			name:      "raw svg onload omitted",
			in:        "<svg onload=alert(1)>",
			forbidden: []string{"<svg", " onload=", "alert(1)"},
			required:  []string{"<!-- raw HTML omitted -->"},
		},
		{
			name:      "svg in code fence is escaped not live",
			in:        "```\n<svg onload=alert(1)>\n```",
			forbidden: []string{"<svg", "<script"},
			required:  []string{"&lt;svg onload=alert(1)&gt;"},
		},
		{
			name:      "javascript url scheme filtered",
			in:        "[click](javascript:alert(1))",
			forbidden: []string{"javascript:", "alert(1)"},
			// Lock the neutralized shape: the dangerous scheme is stripped to an
			// empty href, the link text survives. A future goldmark upgrade that
			// changed dangerous-URL handling could not silently pass this row.
			required: []string{`href=""`, `>click</a>`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := renderMarkdown(tt.in)
			if err != nil {
				t.Fatalf("renderMarkdown(%q) error = %v", tt.in, err)
			}
			out := string(got)
			low := strings.ToLower(out)
			for _, bad := range tt.forbidden {
				if strings.Contains(low, strings.ToLower(bad)) {
					t.Errorf("renderMarkdown(%q) = %q\nmust NOT contain live vector %q", tt.in, out, bad)
				}
			}
			for _, want := range tt.required {
				if !strings.Contains(out, want) {
					t.Errorf("renderMarkdown(%q) = %q\nwant neutralized marker %q", tt.in, out, want)
				}
			}
		})
	}
}

// TestRenderXSSEndToEnd renders a full page whose user and AI messages carry XSS
// payloads, proving the payload's signature never reaches the output as live
// markup. The assertions key on tokens unique to the payload (alert(1), onerror=,
// <img>, <svg>) that the renderer's own <style>/<script>/template chrome never
// contains, so they isolate injected markup from the page's legitimate scripting.
func TestRenderXSSEndToEnd(t *testing.T) {
	t.Parallel()

	s := minimalSession()
	s.Root.Turns[0].User.Blocks = []content.Block{&content.TextBlock{Text: "<script>alert(1)</script>"}}
	s.Root.Turns[0].Steps[0].AI.Blocks = []content.Block{&content.TextBlock{Text: "</script><img onerror=alert(1) src=x>"}}

	var buf bytes.Buffer
	if err := Render(&buf, s); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	low := strings.ToLower(buf.String())
	for _, bad := range []string{"alert(1)", " onerror=", " onload=", "<img", "<svg"} {
		if strings.Contains(low, strings.ToLower(bad)) {
			t.Errorf("full page contains live XSS vector %q\n%s", bad, buf.String())
		}
	}
}
