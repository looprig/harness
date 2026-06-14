package registry

import (
	"strings"
	"testing"
)

func TestDuplicateNameErrorError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		errName string
	}{
		{name: "simple name", errName: "alpha"},
		{name: "empty name", errName: ""},
		{name: "name with spaces", errName: "two words"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := &DuplicateNameError{Name: tt.errName}
			msg := err.Error()
			if msg == "" {
				t.Fatalf("Error() = empty string, want non-empty")
			}
			if !strings.Contains(msg, tt.errName) {
				t.Errorf("Error() = %q, want it to contain name %q", msg, tt.errName)
			}
		})
	}
}

func TestUnknownNameErrorError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		errName string
		known   []string
	}{
		{name: "with known names", errName: "delta", known: []string{"alpha", "beta"}},
		{name: "empty name no known", errName: "", known: nil},
		{name: "name with known single", errName: "x", known: []string{"y"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := &UnknownNameError{Name: tt.errName, Known: tt.known}
			msg := err.Error()
			if msg == "" {
				t.Fatalf("Error() = empty string, want non-empty")
			}
			if !strings.Contains(msg, tt.errName) {
				t.Errorf("Error() = %q, want it to contain name %q", msg, tt.errName)
			}
			for _, k := range tt.known {
				if !strings.Contains(msg, k) {
					t.Errorf("Error() = %q, want it to contain known name %q", msg, k)
				}
			}
		})
	}
}
