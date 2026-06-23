package styles

import (
	"regexp"
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

// markdownSGR matches CSI ... m (SGR) escape sequences in glamour output.
var markdownSGR = regexp.MustCompile("\x1b\\[([0-9;]*)m")

// sgrParamIsRed reports whether a single SGR param list sets a RED foreground.
// It parses tokens (split on ';') so 256-color/truecolor introducers are matched as
// whole color tokens — NOT naive substrings (which would falsely match "38;5;1"
// inside "38;5;187"). Red = basic 31/91, the reddish 256-color palette glamour's
// code-block chroma theme emits (Operator #EF8080 → "210", GenericDeleted #FD5B5B →
// "203", plus other true reds), or a truecolor with a high-R, low-G/B foreground.
func sgrParamIsRed(params string) bool {
	red256 := map[int]bool{
		1: true, 9: true, 196: true, 203: true, 210: true,
		160: true, 161: true, 167: true, 197: true, 204: true,
	}
	atoi := func(s string) int {
		n := 0
		for _, c := range s {
			if c < '0' || c > '9' {
				return -1
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	toks := strings.Split(params, ";")
	for i := 0; i < len(toks); i++ {
		if toks[i] == "31" || toks[i] == "91" {
			return true
		}
		if toks[i] == "38" && i+2 < len(toks) && toks[i+1] == "5" {
			if red256[atoi(toks[i+2])] {
				return true
			}
			i += 2
			continue
		}
		if toks[i] == "38" && i+4 < len(toks) && toks[i+1] == "2" {
			r, g, b := atoi(toks[i+2]), atoi(toks[i+3]), atoi(toks[i+4])
			if r >= 180 && g >= 0 && g <= 150 && b >= 0 && b <= 150 {
				return true
			}
			i += 4
			continue
		}
	}
	return false
}

// renderHasRedRun reports whether any non-whitespace text run in the rendered output
// carries a red SGR foreground (the leading SGR escape before the run). Walks the SGR
// escapes and inspects the text between each escape and the next.
func renderHasRedRun(out string) (string, bool) {
	idxs := markdownSGR.FindAllStringSubmatchIndex(out, -1)
	for i, m := range idxs {
		params := out[m[2]:m[3]]
		if !sgrParamIsRed(params) {
			continue
		}
		runStart := m[1]
		runEnd := len(out)
		if i+1 < len(idxs) {
			runEnd = idxs[i+1][0]
		}
		run := out[runStart:runEnd]
		if strings.TrimSpace(run) != "" {
			return run, true
		}
	}
	return "", false
}

// TestNewMarkdownRendererCodeBlockNoRedSymbols is the regression guard for the TUI
// bug where structural symbols in CODE BLOCKS rendered RED. Root cause: glamour's
// DarkStyleConfig code-block chroma theme colors the Operator token ("/", "+", "-",
// "=", "->", …) salmon-red (#EF8080 → ANSI 256 "210") and the GenericDeleted token
// (diff "-" lines) red (#FD5B5B → "203"). NewMarkdownRenderer now retones both to a
// neutral gray. This asserts NO red SGR run survives for representative code blocks
// exercising those symbols; before the fix the Operator/GenericDeleted runs were red.
func TestNewMarkdownRendererCodeBlockNoRedSymbols(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		md   string
	}{
		{
			name: "go operators (slash, plus, minus, arrow)",
			md:   "```go\nx := a / b + c - d\ny <- ch\n```\n",
		},
		{
			name: "diff add/remove markers",
			md:   "```diff\n- removed cmd/swe/ line\n+ added line\n```\n",
		},
		{
			name: "sql arithmetic operators",
			md:   "```sql\nSELECT a/b, c+d FROM t WHERE x = 1;\n```\n",
		},
		{
			name: "python operators",
			md:   "```python\nx = a / b + c - d\n```\n",
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
			if run, red := renderHasRedRun(out); red {
				t.Errorf("Render(%q): a code-block run rendered RED: %q\nfull=%q", tt.md, run, out)
			}
		})
	}
}
