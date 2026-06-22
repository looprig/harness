package styles

import (
	"strings"
	"testing"
)

// TestToolStyles verifies the tool-call and tool-result styles render their input
// to non-empty output (mirrors the role-style expectations). Faint styling may add
// ANSI escapes; the text content must always survive.
func TestToolStyles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		style interface{ Render(...string) string }
		in    string
	}{
		{name: "tool call style renders text", style: ToolCallStyle, in: "└ ReadFile  config.yaml  ✓"},
		{name: "tool result style renders text", style: ToolResultStyle, in: "    port: 8080"},
		{name: "tool call style empty input", style: ToolCallStyle, in: ""},
		{name: "tool result style empty input", style: ToolResultStyle, in: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := tt.style.Render(tt.in)
			if tt.in == "" {
				return // empty input may render empty; just must not panic.
			}
			if !strings.Contains(out, tt.in) {
				t.Errorf("Render(%q) = %q, want to contain the input text", tt.in, out)
			}
		})
	}
}

func TestNewMarkdownRenderer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		width int
	}{
		{name: "standard width", width: 80},
		{name: "narrow width", width: 20},
		{name: "wide width", width: 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewMarkdownRenderer(tt.width)
			if err != nil {
				t.Fatalf("NewMarkdownRenderer(%d) error = %v, want nil", tt.width, err)
			}
			if r == nil {
				t.Fatalf("NewMarkdownRenderer(%d) returned nil renderer", tt.width)
			}

			out, err := r.Render("# hi")
			if err != nil {
				t.Fatalf("Render() error = %v, want nil", err)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("Render(\"# hi\") = empty, want non-empty output")
			}
		})
	}
}

// TestNewMarkdownRendererPalette verifies the Nexus color overrides applied over
// glamour's DarkStyleConfig: markdown headings render in MarkdownHeadingColor and
// inline `code` spans in MarkdownInlineCodeColor — both #A2D2FF — instead of
// glamour's heading blue (ANSI 256 "39") and inline-code red (ANSI 256 "203"). The
// inline `code` span also drops glamour's background fill (ANSI 256 "236") and its
// U+00A0 prefix/suffix padding. glamour emits color via x/ansi (no terminal
// color-profile downgrade), so the rendered output carries the literal truecolor SGR
// escape regardless of TTY.
func TestNewMarkdownRendererPalette(t *testing.T) {
	t.Parallel()

	const (
		brandBlueSGR = "38;2;162;210;255" // #A2D2FF foreground
		glamourBlue  = "38;5;39"          // old heading color
		glamourRed   = "38;5;203"         // old inline-code color
		codeBgSGR    = "48;5;236"         // old inline-code background fill
		nbsp         = "\u00a0"           // old inline-code prefix/suffix padding
	)

	tests := []struct {
		name    string
		md      string
		wantSGR string   // truecolor escape that MUST appear
		absent  []string // substrings that must NOT appear
	}{
		{name: "heading uses brand blue, no hash marker", md: "## Section", wantSGR: brandBlueSGR, absent: []string{glamourBlue, "#"}},
		{
			name:    "inline code: brand blue, no background, no nbsp padding",
			md:      "run `go test` now",
			wantSGR: brandBlueSGR,
			absent:  []string{glamourRed, codeBgSGR, nbsp},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewMarkdownRenderer(80)
			if err != nil {
				t.Fatalf("NewMarkdownRenderer error = %v, want nil", err)
			}
			out, err := r.Render(tt.md)
			if err != nil {
				t.Fatalf("Render(%q) error = %v, want nil", tt.md, err)
			}
			if !strings.Contains(out, tt.wantSGR) {
				t.Errorf("Render(%q) = %q, want it to contain SGR %q", tt.md, out, tt.wantSGR)
			}
			for _, a := range tt.absent {
				if strings.Contains(out, a) {
					t.Errorf("Render(%q) = %q, must NOT contain %q", tt.md, out, a)
				}
			}
		})
	}
}

// TestNewMarkdownRendererHeadingNoHashes verifies the H2–H6 prefix override: every
// heading level renders its text WITHOUT glamour's literal "#" markers, while the
// heading text itself survives. H1 is covered separately (it uses a background bar,
// never a "#" marker).
func TestNewMarkdownRendererHeadingNoHashes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		md   string
		text string // the heading text that MUST still appear
	}{
		{name: "h2", md: "## Alpha", text: "Alpha"},
		{name: "h3", md: "### Bravo", text: "Bravo"},
		{name: "h4", md: "#### Charlie", text: "Charlie"},
		{name: "h5", md: "##### Delta", text: "Delta"},
		{name: "h6", md: "###### Echo", text: "Echo"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewMarkdownRenderer(80)
			if err != nil {
				t.Fatalf("NewMarkdownRenderer error = %v, want nil", err)
			}
			out, err := r.Render(tt.md)
			if err != nil {
				t.Fatalf("Render(%q) error = %v, want nil", tt.md, err)
			}
			if !strings.Contains(out, tt.text) {
				t.Errorf("Render(%q) = %q, want it to contain the heading text %q", tt.md, out, tt.text)
			}
			if strings.Contains(out, "#") {
				t.Errorf("Render(%q) = %q, must NOT contain a '#' hash marker", tt.md, out)
			}
		})
	}
}
