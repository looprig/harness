package codex

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/foreignloop"
)

// closeGrace is how long the process group has to exit on SIGINT before SIGKILL.
const closeGrace = 2 * time.Second

// maxLineBytes is the scanner's per-line ceiling for Codex JSONL output.
const maxLineBytes = 1 << 20

// SpawnConfigError is the fail-closed result of an Agent misconfiguration detected
// before the child is ever started (e.g. an empty ExecPath).
type SpawnConfigError struct{ Field, Reason string }

func (e *SpawnConfigError) Error() string {
	return "codex: spawn config: " + e.Field + ": " + e.Reason
}

// Spawn starts the codex CLI for one foreign turn in its own process group and
// returns the live decoded stream. Codex JSONL is the committed source for v1, so
// TranscriptPath is intentionally empty.
func (a *Agent) Spawn(_ context.Context, t foreignloop.ForeignTurn) (foreignloop.ForeignStream, error) {
	if a.ExecPath == "" {
		return nil, &SpawnConfigError{Field: "ExecPath", Reason: "empty"}
	}
	cmd, stdout, err := a.start(t)
	if err != nil {
		return nil, &foreignloop.SpawnError{Cause: err}
	}
	events, decErr := decodeJSONL(stdout)
	return &stream{events: events, cmd: cmd, decErr: decErr, pgid: cmd.Process.Pid}, nil
}

// start builds and starts the child process without a shell. stderr is drained to
// io.Discard so a full stderr pipe can never block the child.
func (a *Agent) start(t foreignloop.ForeignTurn) (*exec.Cmd, io.Reader, error) {
	cfg := runConfig{
		cwd:              t.Cwd,
		model:            a.Model,
		profile:          a.Profile,
		additionalDirs:   append([]string(nil), a.AdditionalDirs...),
		sandbox:          a.Sandbox,
		approval:         a.Approval,
		ignoreUserConfig: a.IgnoreUserConfig,
		ignoreRules:      a.IgnoreRules,
		skipGitRepoCheck: a.SkipGitRepoCheck,
	}
	prompt := promptText(t.SystemPrompt, t.Input)
	var args []string
	if t.StartNew {
		args = buildStartArgs(t, cfg, prompt)
	} else {
		args = buildResumeArgs(t, cfg, prompt)
	}
	// #nosec G204 -- ExecPath is operator-configured, and args is a fixed argv list
	// passed positionally; there is no shell and no string splitting.
	cmd := exec.Command(a.ExecPath, args...)
	cmd.Dir = t.Cwd
	cmd.Env = a.Env
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

// stream is the live foreign stream for one spawned codex process. Close is
// idempotent (sync.Once) and tears down the whole process group.
type stream struct {
	events   <-chan foreignloop.ForeignEvent
	cmd      *exec.Cmd
	decErr   func() error
	pgid     int
	once     sync.Once
	closeErr error
}

func (s *stream) Events() <-chan foreignloop.ForeignEvent { return s.events }
func (s *stream) TranscriptPath() string                  { return "" }

// Close signals the child's process GROUP (SIGINT, then SIGKILL after a grace
// period) and reaps it. Repeat calls return the first result.
func (s *stream) Close() error {
	s.once.Do(func() { s.closeErr = s.shutdown() })
	return s.closeErr
}

func (s *stream) shutdown() error {
	_ = syscall.Kill(-s.pgid, syscall.SIGINT)
	kill := time.AfterFunc(closeGrace, func() { _ = syscall.Kill(-s.pgid, syscall.SIGKILL) })
	defer kill.Stop()
	waitErr := s.cmd.Wait()
	if derr := s.decErr(); derr != nil {
		slog.Warn("codex: foreign stream decode error", "error", derr)
	}
	return exitError(waitErr)
}

func decodeJSONL(r io.Reader) (<-chan foreignloop.ForeignEvent, func() error) {
	ch := make(chan foreignloop.ForeignEvent)
	var (
		mu       sync.Mutex
		firstErr error
	)
	setErr := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
		for sc.Scan() {
			evs, err := decodeLine(sc.Bytes())
			if err != nil {
				setErr(err)
				continue
			}
			for _, ev := range evs {
				ch <- ev
			}
		}
		if err := sc.Err(); err != nil {
			setErr(&foreignloop.DecodeError{Cause: err})
		}
	}()
	return ch, func() error {
		mu.Lock()
		defer mu.Unlock()
		return firstErr
	}
}

// promptText flattens text input blocks into the prompt passed as the final Codex
// argv element. Non-text blocks do not contribute.
func promptText(system string, blocks []content.Block) string {
	var task strings.Builder
	for _, blk := range blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			task.WriteString(tb.Text)
		}
	}
	return "<looprig-system>" + system + "</looprig-system>\n\n<user-task>" + task.String() + "</user-task>"
}

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
