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
