// Command nexus is the CLI TUI entry point: it parses the agent-name argument,
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
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/inventivepotter/urvi/agents/coding"
	personalassistant "github.com/inventivepotter/urvi/agents/personal-assistant"
	"github.com/inventivepotter/urvi/internal/registry"
	"github.com/inventivepotter/urvi/tui"
)

// defaultAgent is the agent opened when no agent name is given on the CLI.
const defaultAgent = "personal-assistant"

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
	// Register returns an error only on duplicate names; with static, distinct
	// registrations that cannot happen, but handle it defensively. *Assistant and
	// *Coding satisfy tui.Agent, so each constructor type-checks as
	// (tui.Agent, error).
	_ = reg.Register(defaultAgent, func(c context.Context) (tui.Agent, error) {
		return personalassistant.New(c)
	})
	_ = reg.Register("coding", func(c context.Context) (tui.Agent, error) {
		return coding.New(c)
	})
	return reg
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name := agentName(os.Args[1:])
	reg := buildRegistry()

	// open is the OpenAgent thunk: the TUI uses it to (re)open the agent on /clear.
	open := func(c context.Context) (tui.Agent, error) { return reg.Open(c, name) }

	agent, err := open(ctx)
	if err != nil {
		var unknown *registry.UnknownNameError
		if errors.As(err, &unknown) {
			fmt.Fprintf(os.Stderr, "unknown agent %q; available: %v\n", name, reg.Names())
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	screen := tui.New(ctx, agent, open)
	prog := tea.NewProgram(screen, tea.WithAltScreen())

	// SIGINT/SIGTERM (non-keyboard) cancels ctx → quit the TUI for a clean
	// teardown; no-op if already quit. defer stop() cancels ctx on return, so
	// this goroutine is reaped at exit and never leaks.
	go func() {
		<-ctx.Done()
		prog.Quit()
	}()

	final, runErr := prog.Run()

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
		fmt.Fprintln(os.Stderr, "tui error:", runErr)
		os.Exit(1)
	}
}
