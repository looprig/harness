//go:build integration && unix

package ttylog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureStdioRedirectsBothStreams verifies a real fd-level capture: while
// active, writes to os.Stdout and os.Stderr land in the target file and the returned
// TTY handle is usable; after Restore, writes no longer reach the file. It is
// intentionally NOT parallel — it mutates the process-wide standard streams.
func TestCaptureStdioRedirectsBothStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdio.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer func() { _ = f.Close() }()

	cap, err := CaptureStdio(f)
	if err != nil {
		t.Fatalf("CaptureStdio: %v", err)
	}
	if cap.TTY == nil {
		t.Fatal("CaptureStdio returned a nil TTY handle")
	}

	const outMarker, errMarker = "STDOUT_MARKER", "STDERR_MARKER"
	fmt.Fprintln(os.Stdout, outMarker)
	fmt.Fprintln(os.Stderr, errMarker)

	if err := cap.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// After restore, neither write should reach the file.
	fmt.Fprintln(os.Stdout, "AFTER_RESTORE_STDOUT")
	fmt.Fprintln(os.Stderr, "AFTER_RESTORE_STDERR")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	for _, want := range []string{outMarker, errMarker} {
		if !strings.Contains(string(got), want) {
			t.Errorf("log file = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(string(got), "AFTER_RESTORE") {
		t.Errorf("log file captured a post-restore write: %q", got)
	}
}
