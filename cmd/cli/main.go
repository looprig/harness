// Command urvi is the CLI TUI entry point: it parses the agent-name argument,
// builds the agent registry, opens the requested agent, and runs the TUI. This
// file is the composition root — wiring only; all behavior lives in tui,
// registry, and the agent packages.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/agents/coding"
	"github.com/inventivepotter/urvi/internal/logging"
	"github.com/inventivepotter/urvi/internal/registry"
	"github.com/inventivepotter/urvi/internal/ttylog"
	"github.com/inventivepotter/urvi/tui"
)

// defaultAgent is the agent opened when no agent name is given on the CLI.
const defaultAgent = "coding"

// agentDescriptions maps each registrable agent name to a one-line description shown
// in the TUI startup banner. It lives at the composition root (not on the agent
// packages) so the narrow tui.Agent interface need not expose a Description method —
// the metadata is wired here alongside the registry registration. A name with no
// entry falls back to an empty description (the banner then shows the bare name).
var agentDescriptions = map[string]string{
	"coding": "a careful software engineer that works through tools",
}

// agentDescription returns the banner description for name, or "" if none is
// registered (the banner degrades to the bare name).
func agentDescription(name string) string { return agentDescriptions[name] }

// agentDisplayNames maps an agent's registry name to its user-facing display
// name (shown in the banner). Unmapped agents fall back to the registry name.
var agentDisplayNames = map[string]string{
	"coding": "Togo",
}

// agentDisplayName returns the banner display name for name, falling back to
// name itself when there is no override.
func agentDisplayName(name string) string {
	if d, ok := agentDisplayNames[name]; ok {
		return d
	}
	return name
}

// closeTimeout bounds the best-effort teardown Close of the current agent.
const closeTimeout = 5 * time.Second

// agentName returns the first non-flag CLI arg, or defaultAgent if none.
func agentName(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return defaultAgent
}

// buildRegistry returns the agent registry with the built-in agents registered.
func buildRegistry() *registry.Registry[tui.Agent] {
	reg := registry.New[tui.Agent]()
	// Register returns an error only on duplicate names; with a single static
	// registration that cannot happen, but handle it defensively. *Coding
	// satisfies tui.Agent, so the constructor type-checks as (tui.Agent, error).
	_ = reg.Register(defaultAgent, func(c context.Context) (tui.Agent, error) {
		return coding.New(c)
	})
	return reg
}

// logDirName / logFileName locate urvi's log file under the user's home directory
// (~/.urvi/urvi.log). Both the structured app logger (slog) and the captured
// third-party stderr write here. envLogLevel overrides the minimum slog level.
const (
	logDirName  = ".urvi"
	logFileName = "urvi.log"
	envLogLevel = "URVI_LOG_LEVEL"
)

// openLogFile opens (creating as needed) the append-mode ~/.urvi/urvi.log file.
func openLogFile() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, logDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// #nosec G304 -- fixed filename under the user's own home directory, not input.
	return os.OpenFile(filepath.Join(dir, logFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name := agentName(os.Args[1:])

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
	logger.Info("urvi starting", "agent", name)

	reg := buildRegistry()

	// open is the OpenAgent thunk: the TUI uses it to (re)open the agent on /clear.
	open := func(c context.Context) (tui.Agent, error) { return reg.Open(c, name) }

	agent, err := open(ctx)
	if err != nil {
		logger.Error("open agent failed", "agent", name, "err", err.Error())
		var unknown *registry.UnknownNameError
		if errors.As(err, &unknown) {
			fmt.Fprintf(os.Stderr, "unknown agent %q; available: %v\n", name, reg.Names())
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
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

	screen := tui.New(ctx, agent, open, tui.AgentBanner{Name: agentDisplayName(name), Description: agentDescription(name)})
	prog := tea.NewProgram(screen, progOpts...)

	// SIGINT/SIGTERM (non-keyboard) cancels ctx → quit the TUI for a clean
	// teardown; no-op if already quit. defer stop() cancels ctx on return, so
	// this goroutine is reaped at exit and never leaks.
	go func() {
		<-ctx.Done()
		prog.Quit()
	}()

	final, runErr := prog.Run()
	_ = restoreStdio()

	// Backstop bounded Close of the *current* agent (which /clear may have
	// swapped), even on a Run error: prefer the live agent off the final model,
	// else fall back to the initial one. Close is idempotent, so the double call
	// with the TUI's own Ctrl+C teardown is safe. Best-effort on teardown.
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
		os.Exit(1)
	}
}
