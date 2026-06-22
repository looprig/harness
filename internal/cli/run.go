// Package cli is the shared CLI runtime for urvi's TUI entry points. It owns the
// process-level plumbing that every entry point repeats — structured logging to
// ~/.urvi/urvi.log, signal-driven shutdown, stdout/stderr capture so third-party
// libraries don't corrupt live scrollback, building and running the Bubble Tea
// program, and bounded teardown — parameterized by an agent constructor and a
// startup banner. Entry points (cmd/swe) stay thin: they select an agent
// and call Run; all runtime behavior lives here.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/logging"
	"github.com/inventivepotter/urvi/internal/ttylog"
	"github.com/inventivepotter/urvi/tui"
)

// Banner is the startup metadata shown in the TUI's session-ready notice: the
// agent's user-facing display Name and an optional one-line Description. It is the
// runtime's narrow view of the banner; entry points construct it and pass it to Run,
// which maps it onto the TUI's tui.AgentBanner.
type Banner struct {
	Name        string
	Description string
}

// agentBanner maps the runtime Banner onto the TUI's banner type verbatim.
func (b Banner) agentBanner() tui.AgentBanner {
	return tui.AgentBanner{Name: b.Name, Description: b.Description}
}

// Process exit codes Run returns to its caller (which passes them to os.Exit). They
// are the runtime's only side-effect surface: Run never calls os.Exit itself, so the
// caller stays the single exit point and Run is unit-testable.
const (
	// exitOK is a clean TUI run + teardown.
	exitOK = 0
	// exitAgentError is an agent construction failure or a TUI run error.
	exitAgentError = 1
)

// closeTimeout bounds the best-effort teardown Close of the current agent.
const closeTimeout = 5 * time.Second

// logDirName / logFileName locate urvi's log file under the user's home directory
// (~/.urvi/urvi.log). Both the structured app logger (slog) and the captured
// third-party stderr write here. envLogLevel overrides the minimum slog level.
const (
	logDirName  = ".urvi"
	logFileName = "urvi.log"
	envLogLevel = "URVI_LOG_LEVEL"
)

// logFilePath resolves the log directory and file path under home, joining with
// filepath.Join so it is correct on every platform. It is a pure helper (no I/O) so
// the path resolution is unit-testable independent of the filesystem.
func logFilePath(home string) (dir, file string) {
	dir = filepath.Join(home, logDirName)
	return dir, filepath.Join(dir, logFileName)
}

// openLogFile opens (creating as needed) the append-mode ~/.urvi/urvi.log file.
func openLogFile() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir, file := logFilePath(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// #nosec G304 -- fixed filename under the user's own home directory, not input.
	return os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// program is the narrow slice of *tea.Program that Run drives: run the TUI to
// completion (returning the final model) and force-quit it on signal. *tea.Program
// satisfies it; defining it here lets tests fake the program without a terminal.
type program interface {
	Run() (tea.Model, error)
	Quit()
}

// newProgram is the Bubble Tea program-construction seam: it builds a program for
// the model + options. It is a package-level var so tests can swap it for a fake (a
// real tea.Program needs a terminal). The default builds a real *tea.Program.
var newProgram = func(model tea.Model, opts ...tea.ProgramOption) program {
	return tea.NewProgram(model, opts...)
}

// Run is the shared CLI runtime. It opens the structured log, installs
// signal-driven shutdown, constructs the agent via newAgent, captures stdout/stderr
// so third-party libraries land in the log instead of the live scrollback, builds
// and runs the Bubble Tea TUI (with newAgent as the /clear reopen thunk), tears the
// agent down within a bounded timeout, and returns a process exit code. It never
// calls os.Exit — the caller does — so it stays the single exit point and Run is
// testable.
//
// ctx is the caller's root context; Run derives a signal-aware context from it so
// SIGINT/SIGTERM trigger a clean TUI teardown. newAgent constructs the agent (and is
// reused as the TUI's /clear thunk); banner is the startup notice metadata.
func Run(ctx context.Context, newAgent func(context.Context) (tui.Agent, error), banner Banner) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Open the shared log file and build the injected structured logger up front so
	// startup and early failures are captured. slog and the later stderr redirect
	// both write to ~/.urvi/urvi.log. Best-effort: if the file can't be opened the
	// logger falls back to discarding records (zero-value Config) and the TUI runs.
	logFile, logErr := openLogFile()
	logger := logging.New(logging.Config{})
	if logErr == nil {
		lvl, _ := logging.ParseLevel(os.Getenv(envLogLevel))
		logger = logging.New(logging.Config{Writer: logFile, Level: lvl})
		defer func() { _ = logFile.Close() }()
	}
	logger.Info("urvi starting", "agent", banner.Name)

	// open is the OpenAgent thunk: it constructs the initial agent and is reused as
	// the TUI's /clear reopen thunk, so a /clear builds a fresh agent the same way.
	open := tui.OpenAgent(newAgent)

	agent, err := open(ctx)
	if err != nil {
		logger.Error("open agent failed", "agent", banner.Name, "err", err.Error())
		fmt.Fprintln(os.Stderr, err)
		return exitAgentError
	}

	// Hand the TUI a dedicated handle to the real terminal, then point the process's
	// stdout+stderr at the log file. Libraries that log to stdout or stderr — e.g. the
	// TDX attestation verifier, which calls logger.Init(os.Stdout) at package init —
	// then land in the log instead of corrupting live scrollback. Best-effort: on
	// failure the TUI renders to the real stdout as usual. Restored right after Run so
	// the teardown/error reporting below still reaches the terminal.
	//
	// The only program option ever needed is the ttylog redirect (WithOutput). In v2,
	// scrollback-first = no alt-screen / no mouse is NOT a program option: it is
	// achieved by screen.View() leaving the returned tea.View's AltScreen false and
	// MouseMode at MouseModeNone (the v2 zero values), so the program stays on the
	// normal screen (tea.Println writes to native scrollback) and never grabs the mouse.
	var progOpts []tea.ProgramOption
	restoreStdio := func() error { return nil }
	if logErr == nil {
		if capture, cerr := ttylog.CaptureStdio(logFile); cerr == nil {
			progOpts = append(progOpts, tea.WithOutput(capture.TTY))
			restoreStdio = capture.Restore
		}
	}

	screen := tui.New(ctx, agent, open, banner.agentBanner())
	prog := newProgram(screen, progOpts...)

	// SIGINT/SIGTERM (non-keyboard) cancels ctx → quit the TUI for a clean teardown;
	// no-op if already quit. defer stop() cancels ctx on return, so this goroutine is
	// reaped at exit and never leaks. The done channel releases it when Run returns
	// even if no signal arrived, so it never blocks past the program's lifetime.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			prog.Quit()
		case <-done:
		}
	}()

	final, runErr := prog.Run()
	close(done)
	_ = restoreStdio()

	// Backstop bounded Close of the *current* agent (which /clear may have swapped),
	// even on a Run error: prefer the live agent off the final model, else fall back
	// to the initial one. Close is idempotent, so the double call with the TUI's own
	// Ctrl+C teardown is safe. Best-effort on teardown.
	toClose := agent
	if s, ok := final.(tui.Screen); ok {
		toClose = s.Agent()
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = toClose.Close(closeCtx)

	if runErr != nil {
		logger.Error("tui exited with error", "err", runErr.Error())
		fmt.Fprintln(os.Stderr, "tui error:", runErr)
		return exitAgentError
	}
	return exitOK
}
