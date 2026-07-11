//go:build integration

package claude

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/foreignloop"
)

// integrationModel is a small, cheap model alias understood by the installed claude
// CLI's --model flag. This test PINS that alias (and --verbose / --permission-mode
// default) against the real binary.
const integrationModel = "haiku"

// turnTimeout bounds one foreign turn so a hung CLI fails the test rather than
// blocking until the go-test deadline. Spawn does not bind ctx (teardown is via
// Close), so we enforce the bound with a watchdog that Closes the stream.
const turnTimeout = 120 * time.Second

// TestAgentSpawnIntegration drives the real claude CLI: a fresh session (--session-id)
// then a resume (--resume) on the same sid+cwd. It pins the argv (--verbose, the
// permission-mode value) and the transcript-path encoding against the installed binary.
// It SKIPS when the binary or the API credential is absent.
func TestAgentSpawnIntegration(t *testing.T) {
	execPath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude binary not on PATH; skipping integration test")
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping integration test")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	agent := &Agent{
		ExecPath: execPath,
		Home:     home,
		Model:    integrationModel,
		Env: whitelistEnv(os.Environ(),
			[]string{"PATH", "HOME", "TERM", "LANG"},
			map[string]string{"ANTHROPIC_API_KEY": key}),
	}
	sid, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	cwd := t.TempDir()

	phases := []struct {
		name     string
		startNew bool
	}{
		{name: "start new session", startNew: true},
		{name: "resume session", startNew: false},
	}
	for _, ph := range phases {
		t.Run(ph.name, func(t *testing.T) {
			turn := foreignloop.ForeignTurn{
				SystemPrompt: "You are a terse test agent.",
				ForeignSID:   sid.String(),
				StartNew:     ph.startNew,
				Input:        []content.Block{&content.TextBlock{Text: "say OK"}},
				Cwd:          cwd,
				Posture:      foreignloop.PostureDefault,
			}
			tp, sawError := spawnDrain(t, agent, turn)
			if sawError {
				t.Fatalf("phase %q: foreign terminal reported an error", ph.name)
			}
			if tp == "" {
				t.Fatalf("phase %q: empty transcript path", ph.name)
			}
			if _, err := os.Stat(tp); err != nil {
				t.Fatalf("phase %q: transcript %q not found: %v", ph.name, tp, err)
			}
		})
	}
}

// spawnDrain spawns one turn, drains the live stream to its terminal under a watchdog
// deadline, and returns the transcript path plus whether a terminal-error event was
// seen. The deferred Close reaps the process group.
func spawnDrain(t *testing.T, agent *Agent, turn foreignloop.ForeignTurn) (string, bool) {
	t.Helper()
	stream, err := agent.Spawn(context.Background(), turn)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = stream.Close() }()
	watchdog := time.AfterFunc(turnTimeout, func() { _ = stream.Close() })
	defer watchdog.Stop()

	var sawTerminal, sawError bool
	for ev := range stream.Events() {
		switch ev.Kind {
		case foreignloop.ForeignTerminalOK:
			sawTerminal = true
		case foreignloop.ForeignTerminalError:
			sawTerminal, sawError = true, true
		}
	}
	if !sawTerminal {
		t.Fatalf("stream closed without a terminal event")
	}
	return stream.TranscriptPath(), sawError
}
