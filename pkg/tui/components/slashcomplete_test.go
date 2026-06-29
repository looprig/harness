package components

import (
	"strings"
	"testing"
)

func TestNewSlashComplete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		prefix    string
		wantNil   bool
		wantCount int
		wantNames []string
	}{
		{
			name:      "prefix /c matches clear",
			prefix:    "/c",
			wantCount: 1,
			wantNames: []string{"/clear"},
		},
		{
			name:      "prefix slash matches all",
			prefix:    "/",
			wantCount: 3,
			wantNames: []string{"/clear", "/help", "/export"},
		},
		{
			name:      "prefix /h matches help",
			prefix:    "/h",
			wantCount: 1,
			wantNames: []string{"/help"},
		},
		{
			name:      "prefix /e matches export",
			prefix:    "/e",
			wantCount: 1,
			wantNames: []string{"/export"},
		},
		{
			name:    "prefix /zzz matches nothing",
			prefix:  "/zzz",
			wantNil: true,
		},
		{
			name:    "prefix /x matches nothing",
			prefix:  "/x",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := NewSlashComplete(tt.prefix)

			if tt.wantNil {
				if got != nil {
					t.Fatalf("NewSlashComplete(%q) = %+v, want nil", tt.prefix, got)
				}
				return
			}

			if got == nil {
				t.Fatalf("NewSlashComplete(%q) = nil, want %d items", tt.prefix, tt.wantCount)
			}
			if len(got.items) != tt.wantCount {
				t.Fatalf("NewSlashComplete(%q) item count = %d, want %d", tt.prefix, len(got.items), tt.wantCount)
			}
			for i, name := range tt.wantNames {
				if got.items[i].Name != name {
					t.Errorf("NewSlashComplete(%q) item[%d].Name = %q, want %q", tt.prefix, i, got.items[i].Name, name)
				}
			}
		})
	}
}

func TestSlashCompleteSelected(t *testing.T) {
	t.Parallel()

	s := NewSlashComplete("/")
	if s == nil {
		t.Fatal("NewSlashComplete(\"/\") = nil, want non-nil")
	}

	if got := s.Selected(); got.Name != "/clear" {
		t.Errorf("Selected() = %q, want first match %q", got.Name, "/clear")
	}
}

func TestSlashCompleteCursorWrap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		moves    []func(*SlashComplete)
		wantName string
	}{
		{
			name:     "no move stays on first",
			moves:    nil,
			wantName: "/clear",
		},
		{
			name:     "down moves to second",
			moves:    []func(*SlashComplete){(*SlashComplete).Down},
			wantName: "/help",
		},
		{
			name:     "down twice moves to third",
			moves:    []func(*SlashComplete){(*SlashComplete).Down, (*SlashComplete).Down},
			wantName: "/export",
		},
		{
			name:     "down thrice wraps to first",
			moves:    []func(*SlashComplete){(*SlashComplete).Down, (*SlashComplete).Down, (*SlashComplete).Down},
			wantName: "/clear",
		},
		{
			name:     "up wraps to last",
			moves:    []func(*SlashComplete){(*SlashComplete).Up},
			wantName: "/export",
		},
		{
			name:     "up twice from first lands on second",
			moves:    []func(*SlashComplete){(*SlashComplete).Up, (*SlashComplete).Up},
			wantName: "/help",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := NewSlashComplete("/")
			if s == nil {
				t.Fatal("NewSlashComplete(\"/\") = nil, want non-nil")
			}
			for _, move := range tt.moves {
				move(s)
			}
			if got := s.Selected(); got.Name != tt.wantName {
				t.Errorf("Selected().Name = %q, want %q", got.Name, tt.wantName)
			}
		})
	}
}

func TestSlashCompleteView(t *testing.T) {
	t.Parallel()

	s := NewSlashComplete("/")
	if s == nil {
		t.Fatal("NewSlashComplete(\"/\") = nil, want non-nil")
	}

	view := s.View()
	if view == "" {
		t.Fatal("View() = empty, want non-empty")
	}
	for _, want := range []string{"/clear", "/help", "/export"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() = %q, want substring %q", view, want)
		}
	}
}
