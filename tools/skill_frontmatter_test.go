package tools

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSkill(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		wantName    string
		wantDesc    string
		wantBody    string
		wantErr     bool
		wantReaster string // substring expected in the MalformedSkillError reason (when wantErr)
	}{
		{
			name:     "happy path name description and body (LF)",
			raw:      "---\nname: code-style\ndescription: A coding style checklist\n---\nUse tabs, not spaces.\n",
			wantName: "code-style",
			wantDesc: "A coding style checklist",
			wantBody: "Use tabs, not spaces.\n",
		},
		{
			name:     "CRLF line endings parse identically",
			raw:      "---\r\nname: code-style\r\ndescription: A coding style checklist\r\n---\r\nUse tabs, not spaces.\r\n",
			wantName: "code-style",
			wantDesc: "A coding style checklist",
			wantBody: "Use tabs, not spaces.\n",
		},
		{
			name:     "empty body is allowed",
			raw:      "---\nname: code-style\ndescription: desc\n---\n",
			wantName: "code-style",
			wantDesc: "desc",
			wantBody: "",
		},
		{
			name:     "empty body with no trailing newline after fence",
			raw:      "---\nname: code-style\ndescription: desc\n---",
			wantName: "code-style",
			wantDesc: "desc",
			wantBody: "",
		},
		{
			name:     "leading blank lines before opening fence are tolerated",
			raw:      "\n\n---\nname: code-style\ndescription: desc\n---\nBody.\n",
			wantName: "code-style",
			wantDesc: "desc",
			wantBody: "Body.\n",
		},
		{
			name:     "values are trimmed of surrounding whitespace",
			raw:      "---\nname:   code-style   \ndescription:\tsome desc\t\n---\nBody.\n",
			wantName: "code-style",
			wantDesc: "some desc",
			wantBody: "Body.\n",
		},
		{
			name:     "unknown keys are ignored",
			raw:      "---\nname: code-style\nlicense: MIT\ndescription: desc\n---\nBody.\n",
			wantName: "code-style",
			wantDesc: "desc",
			wantBody: "Body.\n",
		},
		{
			name:     "blank lines and comments inside frontmatter are skipped",
			raw:      "---\nname: code-style\n\n# a comment\ndescription: desc\n---\nBody.\n",
			wantName: "code-style",
			wantDesc: "desc",
			wantBody: "Body.\n",
		},
		{
			name:        "no opening fence is malformed",
			raw:         "name: code-style\ndescription: desc\nBody.\n",
			wantErr:     true,
			wantReaster: "opening",
		},
		{
			name:        "empty input is malformed (no opening fence)",
			raw:         "",
			wantErr:     true,
			wantReaster: "opening",
		},
		{
			name:        "unterminated fence is malformed",
			raw:         "---\nname: code-style\ndescription: desc\nBody without closing fence.\n",
			wantErr:     true,
			wantReaster: "closing",
		},
		{
			name:        "duplicate name key is malformed",
			raw:         "---\nname: code-style\nname: other\ndescription: desc\n---\nBody.\n",
			wantErr:     true,
			wantReaster: "duplicate",
		},
		{
			name:        "duplicate description key is malformed",
			raw:         "---\nname: code-style\ndescription: a\ndescription: b\n---\nBody.\n",
			wantErr:     true,
			wantReaster: "duplicate",
		},
		{
			name:        "frontmatter line without colon is malformed",
			raw:         "---\nname code-style\ndescription: desc\n---\nBody.\n",
			wantErr:     true,
			wantReaster: "key: value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			meta, body, err := parseSkill([]byte(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSkill() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var mse *MalformedSkillError
				if !errors.As(err, &mse) {
					t.Fatalf("parseSkill() error = %T, want *MalformedSkillError", err)
				}
				if tt.wantReaster != "" && !strings.Contains(mse.Reason, tt.wantReaster) {
					t.Errorf("MalformedSkillError.Reason = %q, want substring %q", mse.Reason, tt.wantReaster)
				}
				// Fail-secure: no partial parse leaks out alongside the error.
				if meta.Name != "" || meta.Description != "" || body != "" {
					t.Errorf("on error want zero meta/body, got meta=%+v body=%q", meta, body)
				}
				return
			}
			if meta.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", meta.Name, tt.wantName)
			}
			if meta.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", meta.Description, tt.wantDesc)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestParseSkillOversize(t *testing.T) {
	t.Parallel()

	// One byte over the cap must be rejected before any parsing.
	raw := make([]byte, maxSkillBytes+1)
	for i := range raw {
		raw[i] = 'a'
	}
	meta, body, err := parseSkill(raw)
	if err == nil {
		t.Fatalf("parseSkill() on oversize input = nil error, want error")
	}
	var mse *MalformedSkillError
	if !errors.As(err, &mse) {
		t.Fatalf("parseSkill() oversize error = %T, want *MalformedSkillError", err)
	}
	if !strings.Contains(mse.Reason, "exceeds") {
		t.Errorf("oversize Reason = %q, want substring %q", mse.Reason, "exceeds")
	}
	if meta.Name != "" || meta.Description != "" || body != "" {
		t.Errorf("on oversize want zero meta/body, got meta=%+v body=%q", meta, body)
	}
}

func TestParseSkillAtCapBoundary(t *testing.T) {
	t.Parallel()

	// Exactly maxSkillBytes is allowed (boundary): a valid doc padded with body.
	head := "---\nname: code-style\ndescription: desc\n---\n"
	raw := make([]byte, maxSkillBytes)
	copy(raw, head)
	for i := len(head); i < len(raw); i++ {
		raw[i] = 'x'
	}
	meta, _, err := parseSkill(raw)
	if err != nil {
		t.Fatalf("parseSkill() at exactly maxSkillBytes = %v, want nil", err)
	}
	if meta.Name != "code-style" {
		t.Errorf("Name = %q, want %q", meta.Name, "code-style")
	}
}
