package styles

import (
	"strings"
	"testing"
)

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
