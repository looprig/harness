package claude

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/foreignloop"
)

// wrapTestTurn is a minimal fresh-session turn; the exec target in these tests is
// a harmless binary (echo), so the argv content is irrelevant to the assertions.
func wrapTestTurn() foreignloop.ForeignTurn {
	return foreignloop.ForeignTurn{
		ForeignSID: "11111111-2222-3333-4444-555555555555",
		StartNew:   true,
		Input:      []content.Block{&content.TextBlock{Text: "hi"}},
	}
}

// lookEcho resolves a harmless, always-present binary to stand in for the foreign
// agent; it lets start() reach cmd.Start() without a real claude CLI.
func lookEcho(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("echo")
	if err != nil {
		t.Skipf("echo not on PATH: %v", err)
	}
	return p
}

// hasEnv reports whether env contains the exact entry want.
func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// reap drains a started test child's stdout to EOF and tears down its process
// group so no orphan survives the test.
func reap(t *testing.T, cmd *exec.Cmd, stdout io.Reader) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if stdout != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
}

// TestStartWrapSeam asserts the injected Wrap seam is honored: a nil Wrap leaves
// today's behavior unchanged, and a non-nil Wrap is invoked and its returned cmd
// is the one that actually starts (proven by a marker the Wrap stamps onto env).
func TestStartWrapSeam(t *testing.T) {
	echo := lookEcho(t)
	const marker = "LOOPRIG_WRAP_TEST=1"

	tests := []struct {
		name       string
		wrap       func(*exec.Cmd) (*exec.Cmd, error)
		wantMarker bool // the started cmd should carry the marker iff Wrap stamped it
	}{
		{
			name:       "nil wrap leaves the command unchanged",
			wrap:       nil,
			wantMarker: false,
		},
		{
			name: "non-nil wrap is called and its result is used",
			wrap: func(c *exec.Cmd) (*exec.Cmd, error) {
				c.Env = append(c.Env, marker)
				return c, nil
			},
			wantMarker: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{ExecPath: echo, Model: "small", Wrap: tt.wrap}
			cmd, stdout, err := a.start(wrapTestTurn())
			if err != nil {
				t.Fatalf("start() err = %v, want nil", err)
			}
			defer reap(t, cmd, stdout)

			if cmd.Process == nil {
				t.Fatal("start() returned before starting the process")
			}
			if got := hasEnv(cmd.Env, marker); got != tt.wantMarker {
				t.Errorf("started cmd carries marker = %v, want %v (start must run the cmd Wrap returned)", got, tt.wantMarker)
			}
			// The path is unchanged here because these Wraps mutate in place; this
			// guards against start ignoring the returned cmd entirely.
			if cmd.Path != echo {
				t.Errorf("cmd.Path = %q, want %q", cmd.Path, echo)
			}
		})
	}
}

// TestStartWrapFailsClosed asserts a Wrap that cannot confine the process fails
// the spawn closed with a *WrapError and starts NO process — including the
// defensive nil-command case.
func TestStartWrapFailsClosed(t *testing.T) {
	echo := lookEcho(t)
	boom := errors.New("wrap boom")

	tests := []struct {
		name   string
		wrap   func(*exec.Cmd) (*exec.Cmd, error)
		wantIs error // errors.Is target in the chain, or nil
	}{
		{
			name:   "wrap returns an error",
			wrap:   func(*exec.Cmd) (*exec.Cmd, error) { return nil, boom },
			wantIs: boom,
		},
		{
			name:   "wrap returns a nil command with no error",
			wrap:   func(*exec.Cmd) (*exec.Cmd, error) { return nil, nil },
			wantIs: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{ExecPath: echo, Model: "small", Wrap: tt.wrap}
			cmd, _, err := a.start(wrapTestTurn())

			var we *WrapError
			if !errors.As(err, &we) {
				t.Fatalf("start() err = %v, want *WrapError", err)
			}
			if cmd != nil {
				t.Error("start() returned a non-nil cmd on wrap failure; must be nil (no process started)")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("start() err chain missing cause %v", tt.wantIs)
			}
		})
	}
}

// TestSpawnSurfacesWrapError asserts a wrap failure propagates through Spawn as a
// *foreignloop.SpawnError whose chain still exposes the *WrapError.
func TestSpawnSurfacesWrapError(t *testing.T) {
	echo := lookEcho(t)
	a := &Agent{
		ExecPath: echo,
		Model:    "small",
		Wrap:     func(*exec.Cmd) (*exec.Cmd, error) { return nil, errors.New("nope") },
	}
	s, err := a.Spawn(context.Background(), wrapTestTurn())
	if s != nil {
		_ = s.Close()
	}
	var se *foreignloop.SpawnError
	if !errors.As(err, &se) {
		t.Fatalf("Spawn() err = %v, want *foreignloop.SpawnError", err)
	}
	var we *WrapError
	if !errors.As(err, &we) {
		t.Fatalf("Spawn() err = %v, want a *WrapError in the chain", err)
	}
}
