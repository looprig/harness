package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// errorWithFields is the common interface satisfied by every typed TUI error.
type errorWithFields interface {
	Error() string
}

func TestErrorMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      errorWithFields
		contains []string
	}{
		{
			name:     "EmptyInputError",
			err:      &EmptyInputError{},
			contains: []string{"input"},
		},
		{
			name:     "UnsupportedAttachmentError carries ext",
			err:      &UnsupportedAttachmentError{Ext: ".xyz"},
			contains: []string{".xyz"},
		},
		{
			name:     "ImageUnsupportedError carries ext",
			err:      &ImageUnsupportedError{Ext: ".png"},
			contains: []string{".png"},
		},
		{
			name:     "DeniedAttachmentError carries path and reason",
			err:      &DeniedAttachmentError{Path: "/etc/secret", Reason: "denylist match"},
			contains: []string{"/etc/secret", "denylist match"},
		},
		{
			name:     "AttachmentTooLargeError carries path size and max",
			err:      &AttachmentTooLargeError{Path: "p", Size: 9, Max: 5},
			contains: []string{"p", "9", "5"},
		},
		{
			name:     "AttachmentNotFoundError carries path",
			err:      &AttachmentNotFoundError{Path: "/no/such", Cause: errors.New("nope")},
			contains: []string{"/no/such"},
		},
		{
			name:     "AttachmentReadError carries path",
			err:      &AttachmentReadError{Path: "/bad/read", Cause: errors.New("nope")},
			contains: []string{"/bad/read"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.err.Error()
			if got == "" {
				t.Fatalf("Error() returned empty string")
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestAttachmentNotFoundErrorUnwrap(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("os: file does not exist")
	wrapped := fmt.Errorf("attach: %w", &AttachmentNotFoundError{Path: "p", Cause: sentinel})

	var target *AttachmentNotFoundError
	if !errors.As(wrapped, &target) {
		t.Fatalf("errors.As failed to recover *AttachmentNotFoundError")
	}
	if target.Path != "p" {
		t.Errorf("recovered Path = %q, want %q", target.Path, "p")
	}
	if !errors.Is(wrapped, sentinel) {
		t.Errorf("errors.Is failed to reach sentinel through Unwrap chain")
	}
}

func TestAttachmentReadErrorUnwrap(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("os: permission denied")
	wrapped := fmt.Errorf("attach: %w", &AttachmentReadError{Path: "p", Cause: sentinel})

	var target *AttachmentReadError
	if !errors.As(wrapped, &target) {
		t.Fatalf("errors.As failed to recover *AttachmentReadError")
	}
	if target.Path != "p" {
		t.Errorf("recovered Path = %q, want %q", target.Path, "p")
	}
	if !errors.Is(wrapped, sentinel) {
		t.Errorf("errors.Is failed to reach sentinel through Unwrap chain")
	}
}
