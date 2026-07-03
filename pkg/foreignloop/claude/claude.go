package claude

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/foreignloop"
)

// closeGrace is how long the process group has to exit on SIGINT before SIGKILL.
const closeGrace = 2 * time.Second

// SpawnConfigError is the fail-closed result of an Agent misconfiguration detected
// before the child is ever started (e.g. an empty ExecPath).
type SpawnConfigError struct{ Field, Reason string }

func (e *SpawnConfigError) Error() string {
	return "claude: spawn config: " + e.Field + ": " + e.Reason
}

// Agent is the real `claude` subprocess adapter; it satisfies
// foreignloop.ForeignAgent. Env is the FULL, already-whitelisted child environment:
// the composition root builds it via whitelistEnv, and the adapter uses it verbatim
// as cmd.Env — it NEVER calls os.Environ() itself (that gate is the caller's).
type Agent struct {
	ExecPath string
	Home     string
	Model    string
	Env      []string
}

// Spawn starts the claude CLI for one foreign turn in its own process group and
// returns the live decoded stream. It builds the argv (no shell), pins the cwd and
// the pre-whitelisted env, and derives the deterministic transcript path
// (best-effort: a derivation failure soft-degrades to no transcript).
func (a *Agent) Spawn(_ context.Context, t foreignloop.ForeignTurn) (foreignloop.ForeignStream, error) {
	if a.ExecPath == "" {
		return nil, &SpawnConfigError{Field: "ExecPath", Reason: "empty"}
	}
	cmd, stdout, err := a.start(t)
	if err != nil {
		return nil, &foreignloop.SpawnError{Cause: err}
	}
	events, decErr := foreignloop.DecodeStream(stdout)
	tp, perr := transcriptPath(a.Home, t.Cwd, t.ForeignSID)
	if perr != nil {
		slog.Warn("claude: transcript path derivation failed; degrading to none", "error", perr)
		tp = ""
	}
	return &stream{events: events, tp: tp, cmd: cmd, decErr: decErr, pgid: cmd.Process.Pid}, nil
}

// start builds and starts the child process in its own process group, returning the
// running command and its stdout pipe. stderr is drained to io.Discard so a full
// stderr pipe can never block the child.
func (a *Agent) start(t foreignloop.ForeignTurn) (*exec.Cmd, io.Reader, error) {
	args := buildArgs(t, a.Model)
	// #nosec G204 -- ExecPath is operator-configured (composition root), and args is a
	// fixed argv list passed positionally; there is no shell and no string splitting.
	cmd := exec.Command(a.ExecPath, args...)
	cmd.Dir = t.Cwd
	cmd.Env = a.Env
	// NOTE: the argv (buildArgs) carries no prompt — claude -p reads the turn's prompt
	// from stdin. We feed the flattened text blocks and let exec close stdin at EOF so
	// claude sees a complete, single prompt.
	cmd.Stdin = strings.NewReader(promptText(t.Input))
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, stdout, nil
}

// stream is the live foreign stream for one spawned claude process. Close is
// idempotent (sync.Once) and tears down the whole process group.
type stream struct {
	events   <-chan foreignloop.ForeignEvent
	tp       string
	cmd      *exec.Cmd
	decErr   func() error
	pgid     int
	once     sync.Once
	closeErr error
}

func (s *stream) Events() <-chan foreignloop.ForeignEvent { return s.events }
func (s *stream) TranscriptPath() string                  { return s.tp }

// Close signals the child's process GROUP (SIGINT, then SIGKILL after a grace
// period) and reaps it. It is safe to call exactly once via the actor's deferred
// Close; repeat calls return the first result.
func (s *stream) Close() error {
	s.once.Do(func() { s.closeErr = s.shutdown() })
	return s.closeErr
}

// shutdown SIGINTs the process group, arms a SIGKILL after closeGrace, then reaps the
// child. A non-zero exit code becomes a *foreignloop.ForeignExitError. A pending
// stream decode error is surfaced as a warning (terminal semantics ride the events).
func (s *stream) shutdown() error {
	_ = syscall.Kill(-s.pgid, syscall.SIGINT)
	kill := time.AfterFunc(closeGrace, func() { _ = syscall.Kill(-s.pgid, syscall.SIGKILL) })
	defer kill.Stop()
	waitErr := s.cmd.Wait()
	if derr := s.decErr(); derr != nil {
		slog.Warn("claude: foreign stream decode error", "error", derr)
	}
	return exitError(waitErr)
}

// promptText flattens a turn's input blocks into the plain-text prompt fed to claude
// over stdin. Only text blocks contribute (the print-mode CLI takes a text prompt);
// non-text blocks are ignored here.
func promptText(blocks []content.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// exitError maps a cmd.Wait result to the typed foreign exit error: a non-zero exit
// code becomes *foreignloop.ForeignExitError; a clean exit (or a non-ExitError
// teardown error) is nil.
func exitError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code != 0 {
			return &foreignloop.ForeignExitError{Code: code}
		}
	}
	return nil
}
