// Command swe is the SWE-Swarm TUI entry point and composition root. It parses the CLI
// invocation (--list / --resume), starts the embedded JetStream engine on the default
// StoreDir, and either prints the resumable-session catalog (--list) or hands the shared
// CLI runtime (internal/cli.Run) a thunk that opens/resumes the PERSISTED swarm session.
// It is wiring only: all runtime behavior (logging, signal teardown, the TUI) lives in
// internal/cli, and all session/persistence behavior lives in swarms/swe.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/inventivepotter/urvi/internal/cli"
	"github.com/inventivepotter/urvi/internal/persistence"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/inventivepotter/urvi/swarms/swe"
	"github.com/inventivepotter/urvi/tui"
)

// bannerName is the SWE-Swarm's user-facing banner name shown in the TUI session-ready
// notice (passed through internal/cli.Banner).
const bannerName = "SWE"

// listTimeout bounds the --list catalog read.
const listTimeout = 10 * time.Second

// Process exit codes main returns via os.Exit. exitOK / exitRuntime mirror the runtime's
// codes; exitUsage is the boundary-failure code for a malformed invocation or a
// persistence/list failure (distinct from a TUI run error, which internal/cli.Run owns).
const (
	exitOK     = 0
	exitUsage  = 2
	exitFailed = 1
)

// cliFlags is the parsed CLI invocation: whether to list sessions and exit (--list), and
// which session to resume (--resume <uuid>; zero = new session). There is
// no positional agent name — swe is a single swarm.
type cliFlags struct {
	list   bool
	resume uuid.UUID
}

// FlagParseError reports a malformed CLI invocation (an unknown flag, a non-UUID --resume
// value, the mutually-exclusive --list + --resume combination, or an unexpected positional
// arg). It is a typed boundary error: untrusted CLI input is validated here, before any
// wiring runs, and is errors.As-recoverable.
type FlagParseError struct {
	Reason string
	Cause  error
}

func (e *FlagParseError) Error() string {
	if e.Cause != nil {
		return "swe: " + e.Reason + ": " + e.Cause.Error()
	}
	return "swe: " + e.Reason
}
func (e *FlagParseError) Unwrap() error { return e.Cause }

// parseFlags parses args (os.Args[1:]) into a cliFlags, validating every value at this
// boundary: --resume must be a canonical UUID (parsed via uuid.UnmarshalText, fail-closed),
// --list and --resume are mutually exclusive (a list-and-resume request is ambiguous), and
// no positional args are accepted (swe is a single swarm — there is no agent to name). It
// uses an isolated FlagSet (ContinueOnError, discarded output) so a bad flag returns a
// typed error rather than calling os.Exit, keeping main the single exit point and making
// the parser unit-testable.
func parseFlags(args []string) (cliFlags, error) {
	fs := flag.NewFlagSet("swe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		list   = fs.Bool("list", false, "list resumable sessions and exit")
		resume = fs.String("resume", "", "resume the session with this id")
	)
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, &FlagParseError{Reason: "invalid flags", Cause: err}
	}

	// swe takes no positional args: reject any so a typo'd flag (e.g. a bare "list"
	// instead of "-list") fails loud at the boundary rather than being silently ignored.
	if fs.NArg() > 0 {
		return cliFlags{}, &FlagParseError{Reason: "unexpected argument " + strconv.Quote(fs.Arg(0))}
	}

	out := cliFlags{list: *list}

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

// startPersistence builds the embedded engine on the default StoreDir and the per-process
// swe.Persistence context (lease manager + catalog) over it. On any failure it returns a
// typed error and no engine (so the caller fails loud). The engine and Persistence are
// owned by main for the whole process; the caller closes the engine at exit.
func startPersistence() (*persistence.Engine, *swe.Persistence, error) {
	opts, err := persistence.DefaultEngineOptions()
	if err != nil {
		return nil, nil, err
	}
	engine, err := persistence.Open(opts)
	if err != nil {
		return nil, nil, err
	}
	p, err := swe.NewPersistence(engine.JetStream())
	if err != nil {
		_ = engine.Close()
		return nil, nil, err
	}
	return engine, p, nil
}

// listSessions prints the replay-free session catalog (id, status, last-active, title) to
// w, newest-active first. It is the --list path: it reads the KV index only (no replay, no
// stream consumer). An empty catalog prints a friendly note rather than nothing.
func listSessions(ctx context.Context, p *swe.Persistence, w io.Writer) error {
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	metas, err := p.List(lctx)
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

// openThunk builds the tui.OpenAgent the runtime drives. It returns a closure that opens a
// PERSISTED swarm session: the FIRST call honors resume (a non-zero id restores that
// session); every later call (a /clear reopen) starts a fresh NEW session, so /clear never
// re-restores the same id. The first-call latch is guarded so a reopen is deterministically
// a new session. The returned thunk yields a
// tui.Agent (the persisted *sessionAgent satisfies it).
func openThunk(p *swe.Persistence, resume uuid.UUID) tui.OpenAgent {
	var opened bool
	return func(c context.Context) (tui.Agent, error) {
		sel := swe.SessionSelector{}
		if !opened {
			sel.Resume = resume // only the first open resumes; /clear reopens start fresh
		}
		opened = true
		return p.Open(c, sel)
	}
}

// run is the testable composition root: it parses flags, starts the embedded engine,
// handles --list (print + exit) or builds the persisted openThunk and delegates to
// internal/cli.Run. It returns a process exit code and never calls os.Exit, so main stays
// the single exit point. ctx is the process root (signal-aware); out/errOut are the list
// + error sinks.
func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	flags, ferr := parseFlags(args)
	if ferr != nil {
		fmt.Fprintln(errOut, ferr)
		return exitUsage
	}

	// Start the embedded JetStream engine over the persistent StoreDir under the user data
	// dir. It outlives every agent (a /clear reopens a fresh session against the same
	// engine) and is shut down cleanly at exit. A failure to start fails loud — persistence
	// is the point.
	engine, persist, perr := startPersistence()
	if perr != nil {
		fmt.Fprintln(errOut, "persistence:", perr)
		return exitFailed
	}
	defer func() { _ = engine.Close() }()

	// --list: print the replay-free session catalog and exit (no TUI, no replay). It reads
	// only the KV index, so it is cheap even with many sessions and runs no turn.
	if flags.list {
		if err := listSessions(ctx, persist, out); err != nil {
			fmt.Fprintln(errOut, "list:", err)
			return exitFailed
		}
		return exitOK
	}

	// The initial open honors --resume; every /clear reopen starts a FRESH persisted
	// session. internal/cli.Run owns logging, signal teardown, the TUI, and bounded Close.
	open := openThunk(persist, flags.resume)
	return cli.Run(ctx, open, cli.Banner{Name: bannerName})
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
