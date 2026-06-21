// Command urvi is the CLI TUI entry point: it parses the agent-name argument,
// builds the agent registry, opens the requested agent, and runs the TUI. This
// file is the composition root — wiring only; all behavior lives in tui,
// registry, and the agent packages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/agents/coding"
	"github.com/inventivepotter/urvi/internal/logging"
	"github.com/inventivepotter/urvi/internal/persistence"
	"github.com/inventivepotter/urvi/internal/registry"
	"github.com/inventivepotter/urvi/internal/ttylog"
	"github.com/inventivepotter/urvi/internal/uuid"
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

// listTimeout bounds the --list catalog read.
const listTimeout = 10 * time.Second

// startPersistence builds the embedded engine on the default StoreDir and the per-process
// Persistence context (lease manager + catalog) over it. On any failure it returns a typed
// error and no engine (so the caller fails loud). The engine and Persistence are owned by
// main for the whole process; the caller closes the engine at exit.
func startPersistence() (*persistence.Engine, *coding.Persistence, error) {
	opts, err := persistence.DefaultEngineOptions()
	if err != nil {
		return nil, nil, err
	}
	engine, err := persistence.Open(opts)
	if err != nil {
		return nil, nil, err
	}
	p, err := coding.NewPersistence(engine.JetStream())
	if err != nil {
		_ = engine.Close()
		return nil, nil, err
	}
	return engine, p, nil
}

// listSessions prints the replay-free session catalog (id, status, last-active, title) to
// w, newest-active first. It is the --list path: it reads the KV index only (no replay, no
// stream consumer). An empty catalog prints a friendly note rather than nothing.
func listSessions(ctx context.Context, p *coding.Persistence, w io.Writer) error {
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	metas, err := p.ListSessions(lctx)
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		fmt.Fprintln(w, "no sessions yet")
		return nil
	}
	for _, m := range metas {
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "%s  %-7s  %s  %s\n",
			m.SessionID, m.Status, m.LastActiveAt.Format(time.RFC3339), title)
	}
	return nil
}

// openThunk builds the tui.OpenAgent thunk. For the coding agent it returns a closure that
// opens a PERSISTED session: the FIRST call honors resume (a non-zero id restores that
// session); every later call (a /clear reopen) starts a fresh new session, so /clear never
// re-restores the same id. For any other agent it falls back to the registry (unpersisted),
// preserving the unknown-agent error path. The first-call latch is guarded so a reopen is
// deterministically a new session.
func openThunk(name string, reg *registry.Registry[tui.Agent], p *coding.Persistence, resume uuid.UUID) tui.OpenAgent {
	if name != defaultAgent {
		return func(c context.Context) (tui.Agent, error) { return reg.Open(c, name) }
	}
	var opened bool
	return func(c context.Context) (tui.Agent, error) {
		sel := coding.SessionSelector{}
		if !opened {
			sel.Resume = resume // only the first open resumes; /clear reopens start fresh
		}
		opened = true
		return coding.NewPersistent(c, p, sel)
	}
}

// agentName returns the first non-flag CLI arg, or defaultAgent if none.
func agentName(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return defaultAgent
}

// cliFlags is the parsed CLI invocation: which agent to open, whether to list sessions
// and exit (--list), and which session to resume (--resume <uuid>; zero = new session).
type cliFlags struct {
	agent  string
	list   bool
	resume uuid.UUID
}

// FlagParseError reports a malformed CLI invocation (an unknown flag, a non-UUID
// --resume value, or the mutually-exclusive --list + --resume combination). It is a
// typed boundary error: untrusted CLI input is validated here, before any wiring runs.
type FlagParseError struct {
	Reason string
	Cause  error
}

func (e *FlagParseError) Error() string {
	if e.Cause != nil {
		return "cli: " + e.Reason + ": " + e.Cause.Error()
	}
	return "cli: " + e.Reason
}
func (e *FlagParseError) Unwrap() error { return e.Cause }

// parseFlags parses args (os.Args[1:]) into a cliFlags, validating every value at this
// boundary: --resume must be a canonical UUID (parsed via uuid.UnmarshalText, fail-closed),
// and --list and --resume are mutually exclusive (a list-and-resume request is ambiguous).
// The agent name is the first positional arg after the flags, defaulting to defaultAgent.
// It uses an isolated FlagSet (ContinueOnError, discarded output) so a bad flag returns a
// typed error rather than calling os.Exit — making it unit-testable and keeping main the
// single exit point.
func parseFlags(args []string) (cliFlags, error) {
	fs := flag.NewFlagSet("urvi", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		list   = fs.Bool("list", false, "list resumable sessions and exit")
		resume = fs.String("resume", "", "resume the session with this id")
	)
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, &FlagParseError{Reason: "invalid flags", Cause: err}
	}

	out := cliFlags{agent: defaultAgent, list: *list}
	if name := fs.Arg(0); name != "" {
		out.agent = name
	}

	// Detect whether --resume was explicitly given (vs left at its empty default): an
	// explicit --resume with an empty/whitespace value is a malformed invocation, rejected
	// at the boundary rather than silently treated as "no resume".
	var resumeGiven bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "resume" {
			resumeGiven = true
		}
	})
	if resumeGiven {
		v := strings.TrimSpace(*resume)
		if v == "" {
			return cliFlags{}, &FlagParseError{Reason: "--resume requires a session id"}
		}
		var id uuid.UUID
		if err := id.UnmarshalText([]byte(v)); err != nil {
			return cliFlags{}, &FlagParseError{Reason: "invalid --resume session id", Cause: err}
		}
		out.resume = id
	}

	if out.list && !out.resume.IsZero() {
		return cliFlags{}, &FlagParseError{Reason: "--list and --resume are mutually exclusive"}
	}
	return out, nil
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

	flags, ferr := parseFlags(os.Args[1:])
	if ferr != nil {
		fmt.Fprintln(os.Stderr, ferr)
		os.Exit(2)
	}
	name := flags.agent

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

	// Start the embedded JetStream engine (in-process, no TCP) over the persistent
	// StoreDir under the user data dir. This is what turns persistence on in the real CLI:
	// the session journal writes through this engine's JetStreamContext. The engine
	// outlives every agent (a /clear reopens a fresh session against the same engine) and
	// is shut down cleanly at process exit. A failure to start fails loud (persistence is
	// the point) — but only the coding agent is journaled; other agents run unpersisted.
	engine, persist, perr := startPersistence()
	if perr != nil {
		logger.Error("start persistence engine failed", "err", perr.Error())
		fmt.Fprintln(os.Stderr, "persistence:", perr)
		os.Exit(1)
	}
	defer func() {
		if engine != nil {
			if cerr := engine.Close(); cerr != nil {
				logger.Warn("embedded engine close error", "err", cerr.Error())
			}
		}
	}()

	// --list: print the replay-free session catalog and exit (no TUI, no replay). It reads
	// only the KV index, so it is cheap even with many sessions.
	if flags.list {
		if err := listSessions(ctx, persist, os.Stdout); err != nil {
			logger.Error("list sessions failed", "err", err.Error())
			fmt.Fprintln(os.Stderr, "list:", err)
			os.Exit(1)
		}
		return
	}

	// open is the OpenAgent thunk: the initial open honors --resume (resume that session),
	// and every /clear reopen starts a FRESH persisted session. Only the coding agent is
	// journaled; any other agent falls back to the registry (unpersisted).
	open := openThunk(name, reg, persist, flags.resume)

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
