package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/inventivepotter/urvi/tui"
)

// fakeAgent is a no-op tui.Agent stand-in: construction-success path needs a live
// agent so Run can build the TUI model and bound a teardown Close. Every method is a
// benign no-op; Close records that teardown ran.
type fakeAgent struct {
	loopID uuid.UUID
	closed *bool
}

func (a *fakeAgent) Submit(context.Context, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (a *fakeAgent) PrimaryLoopID() uuid.UUID                { return a.loopID }
func (a *fakeAgent) Interrupt(context.Context) (bool, error) { return false, nil }
func (a *fakeAgent) AcceptsImages() bool                     { return false }
func (a *fakeAgent) ReplayBacklog(context.Context) ([]event.Event, error) {
	return nil, nil
}
func (a *fakeAgent) Subscribe(event.EventFilter) (tui.EventStream, error) { return nil, nil }
func (a *fakeAgent) Approve(context.Context, uuid.UUID, uuid.UUID, tool.ApprovalScope) error {
	return nil
}
func (a *fakeAgent) Deny(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (a *fakeAgent) ProvideAnswer(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (a *fakeAgent) Close(context.Context) error {
	if a.closed != nil {
		*a.closed = true
	}
	return nil
}

// fakeProgram is a program stand-in: Run reports a preset final model + error
// without touching a terminal; Quit is a no-op (no signal in these tests).
type fakeProgram struct {
	final tea.Model
	err   error
}

func (p *fakeProgram) Run() (tea.Model, error) { return p.final, p.err }
func (p *fakeProgram) Quit()                   {}

// TestBannerAgentBanner proves Banner maps verbatim onto tui.AgentBanner (no
// defaulting, no field swap) so the startup notice shows exactly what the caller set.
func TestBannerAgentBanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		banner Banner
		want   tui.AgentBanner
	}{
		{name: "name and description", banner: Banner{Name: "SWE", Description: "swarm"}, want: tui.AgentBanner{Name: "SWE", Description: "swarm"}},
		{name: "name only", banner: Banner{Name: "SWE"}, want: tui.AgentBanner{Name: "SWE"}},
		{name: "empty banner", banner: Banner{}, want: tui.AgentBanner{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.banner.agentBanner(); got != tt.want {
				t.Errorf("agentBanner() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestLogFilePath proves the log path resolves to <dir>/.urvi/urvi.log against a
// supplied home, joining with filepath.Join (no hardcoded separators).
func TestLogFilePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		home    string
		wantDir string
		wantLog string
	}{
		{
			name:    "typical home",
			home:    "/home/alice",
			wantDir: filepath.Join("/home/alice", logDirName),
			wantLog: filepath.Join("/home/alice", logDirName, logFileName),
		},
		{
			name:    "home with trailing slash",
			home:    "/root/",
			wantDir: filepath.Join("/root/", logDirName),
			wantLog: filepath.Join("/root/", logDirName, logFileName),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, log := logFilePath(tt.home)
			if dir != tt.wantDir {
				t.Errorf("dir = %q, want %q", dir, tt.wantDir)
			}
			if log != tt.wantLog {
				t.Errorf("log = %q, want %q", log, tt.wantLog)
			}
		})
	}
}

// TestRunConstructionError proves the newAgent-failure path returns the agent
// failure exit code and never panics (no model built, no program run).
//
// The Run* tests swap the package-level runProgram seam, so they share mutable
// global state and must NOT run in parallel with each other.
func TestRunConstructionError(t *testing.T) {
	var ran bool
	swapNewProgram(t, func(m tea.Model, _ ...tea.ProgramOption) program {
		ran = true
		return &fakeProgram{final: m}
	})

	boom := errors.New("construct failed")
	newAgent := func(context.Context) (tui.Agent, error) { return nil, boom }

	got := Run(context.Background(), newAgent, Banner{Name: "SWE"})
	if got != exitAgentError {
		t.Errorf("Run() exit = %d, want %d", got, exitAgentError)
	}
	if ran {
		t.Error("program ran despite construction failure")
	}
}

// TestRunHappyPath proves the success path: newAgent yields an agent, the program
// runs via the seam and returns no error, the agent is Closed at teardown, and Run
// returns exitOK.
func TestRunHappyPath(t *testing.T) {
	var progRan bool
	swapNewProgram(t, func(m tea.Model, _ ...tea.ProgramOption) program {
		progRan = true
		return &fakeProgram{final: m}
	})

	var closed bool
	newAgent := func(context.Context) (tui.Agent, error) {
		return &fakeAgent{loopID: newLoopID(t), closed: &closed}, nil
	}

	got := Run(context.Background(), newAgent, Banner{Name: "SWE"})
	if got != exitOK {
		t.Errorf("Run() exit = %d, want %d", got, exitOK)
	}
	if !progRan {
		t.Error("program seam was not invoked")
	}
	if !closed {
		t.Error("agent was not Closed at teardown")
	}
}

// TestRunProgramError proves a tea.Program run error maps to the agent-error exit
// code and still tears the agent down.
func TestRunProgramError(t *testing.T) {
	swapNewProgram(t, func(m tea.Model, _ ...tea.ProgramOption) program {
		return &fakeProgram{final: m, err: errors.New("run failed")}
	})

	var closed bool
	newAgent := func(context.Context) (tui.Agent, error) {
		return &fakeAgent{loopID: newLoopID(t), closed: &closed}, nil
	}

	got := Run(context.Background(), newAgent, Banner{Name: "SWE"})
	if got != exitAgentError {
		t.Errorf("Run() exit = %d, want %d", got, exitAgentError)
	}
	if !closed {
		t.Error("agent was not Closed after a run error")
	}
}

// swapNewProgram replaces the package-level program-construction seam for the
// duration of the test and restores it on cleanup, so Run is exercised without a
// real terminal.
func swapNewProgram(t *testing.T, fn func(tea.Model, ...tea.ProgramOption) program) {
	t.Helper()
	prev := newProgram
	newProgram = fn
	t.Cleanup(func() { newProgram = prev })
}

// newLoopID mints a non-zero loop id for a fake agent's PrimaryLoopID.
func newLoopID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}
