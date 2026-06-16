package prompts

import (
	"strings"
	"testing"
)

func TestSystemPrompt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "identifies as Togo", want: "Togo"},
		{name: "names an auto-approved tool", want: "ReadFile"},
		{name: "names an approval-gated tool", want: "Bash"},
		{name: "mentions approval", want: "approv"},
		{name: "mentions secrets", want: "secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(SystemPrompt, tt.want) {
				t.Errorf("SystemPrompt is missing %q", tt.want)
			}
		})
	}
}

func TestSystemPromptNonEmpty(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(SystemPrompt) == "" {
		t.Fatal("SystemPrompt is empty")
	}
}
