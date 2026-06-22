// Command urvi is the CLI TUI entry point: it parses the agent-name argument,
// builds the agent registry, opens the requested agent, and runs the TUI. This
// file is the composition root — wiring only; all behavior lives in tui,
// registry, and the agent packages.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/inventivepotter/urvi/agents/coding"
	"github.com/inventivepotter/urvi/internal/cli"
	"github.com/inventivepotter/urvi/internal/persistence"
	"github.com/inventivepotter/urvi/internal/registry"
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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	flags, ferr := parseFlags(os.Args[1:])
	if ferr != nil {
		fmt.Fprintln(os.Stderr, ferr)
		os.Exit(2)
	}
	name := flags.agent

	reg := buildRegistry()

	// Reject an unknown agent at this boundary (where the registry lives) so the
	// shared runtime never sees it: the persisted coding thunk handles defaultAgent,
	// and every other name must be a registered agent. This preserves the dedicated
	// exit code 2 + "available:" hint that the lazy first-open used to surface.
	if name != defaultAgent && !knownAgent(reg, name) {
		fmt.Fprintf(os.Stderr, "unknown agent %q; available: %v\n", name, reg.Names())
		os.Exit(2)
	}

	// Start the embedded JetStream engine (in-process, no TCP) over the persistent
	// StoreDir under the user data dir. This is what turns persistence on in the real CLI:
	// the session journal writes through this engine's JetStreamContext. The engine
	// outlives every agent (a /clear reopens a fresh session against the same engine) and
	// is shut down cleanly at process exit. A failure to start fails loud (persistence is
	// the point) — but only the coding agent is journaled; other agents run unpersisted.
	engine, persist, perr := startPersistence()
	if perr != nil {
		fmt.Fprintln(os.Stderr, "persistence:", perr)
		os.Exit(1)
	}
	defer func() {
		if engine != nil {
			_ = engine.Close()
		}
	}()

	// --list: print the replay-free session catalog and exit (no TUI, no replay). It reads
	// only the KV index, so it is cheap even with many sessions.
	if flags.list {
		if err := listSessions(ctx, persist, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "list:", err)
			os.Exit(1)
		}
		return
	}

	// open is the OpenAgent thunk: the initial open honors --resume (resume that session),
	// and every /clear reopen starts a FRESH persisted session. Only the coding agent is
	// journaled; any other agent falls back to the registry (unpersisted). The shared
	// runtime owns logging, signal-driven shutdown, stdio capture, the TUI program, and
	// bounded teardown — it takes this thunk as both the initial constructor and the
	// /clear reopen thunk.
	open := openThunk(name, reg, persist, flags.resume)
	os.Exit(cli.Run(ctx, open, cli.Banner{Name: agentDisplayName(name), Description: agentDescription(name)}))
}

// knownAgent reports whether name is a registered agent in reg.
func knownAgent(reg *registry.Registry[tui.Agent], name string) bool {
	for _, n := range reg.Names() {
		if n == name {
			return true
		}
	}
	return false
}
