package session

import (
	"context"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
)

// captureCommand runs call (which sends exactly one command to the fake loop) in a
// goroutine and returns the command read off the fake loop's Commands channel. The
// call may park afterward (e.g. Invoke/Stream block on the per-turn reply that the
// fake loop never sends) — the test only needs the command shape, so the parked
// goroutine is harmless and reaped at test exit. A send that never arrives fails the
// test rather than hanging.
func captureCommand(t *testing.T, s *Session, cmds chan command.Command, call func(s *Session)) command.Command {
	t.Helper()
	go call(s)
	select {
	case cmd := <-cmds:
		return cmd
	case <-time.After(2 * time.Second):
		t.Fatal("method never sent a command")
		return nil
	}
}

// TestSessionStampsAgency asserts each command-sending session method stamps the
// correct Header.Agency on the command it sends: AgencyUser at the discrete
// human-origination points (the interactive Submit, the gate replies, the manual
// Interrupt), and the AgencyMachine zero default everywhere else (the programmatic
// Invoke/Stream submit and the SubagentResult hand-back). Agency is determined by
// WHICH session method was called — each already encodes a distinct origination
// semantics — so a machine path cannot accidentally claim user agency.
//
// The fake-loop seam captures the exact command sent on the unbuffered Commands
// channel — the only observable effect of these fire-and-route / fire-and-forget
// methods.
func TestSessionStampsAgency(t *testing.T) {
	t.Parallel()
	callID := mustUUID()
	blocks := []content.Block{&content.TextBlock{Text: "hi"}}

	tests := []struct {
		name       string
		call       func(s *Session)
		wantAgency identity.Agency
	}{
		{
			name:       "Submit (interactive, human-typed) -> AgencyUser",
			call:       func(s *Session) { _, _ = s.Submit(context.Background(), blocks) },
			wantAgency: identity.AgencyUser,
		},
		{
			name:       "Approve (human gate reply) -> AgencyUser",
			call:       func(s *Session) { _ = s.Approve(context.Background(), s.primaryLoopID, callID, tool.ScopeSession) },
			wantAgency: identity.AgencyUser,
		},
		{
			name:       "Deny (human gate reply) -> AgencyUser",
			call:       func(s *Session) { _ = s.Deny(context.Background(), s.primaryLoopID, callID) },
			wantAgency: identity.AgencyUser,
		},
		{
			name:       "ProvideUserInput (human answer) -> AgencyUser",
			call:       func(s *Session) { _ = s.ProvideUserInput(context.Background(), s.primaryLoopID, callID, "ans") },
			wantAgency: identity.AgencyUser,
		},
		{
			name:       "Interrupt (manual) -> AgencyUser",
			call:       func(s *Session) { _, _ = s.Interrupt(context.Background()) },
			wantAgency: identity.AgencyUser,
		},
		{
			name:       "Invoke (programmatic single-shot) -> AgencyMachine",
			call:       func(s *Session) { _, _ = s.Invoke(context.Background(), blocks) },
			wantAgency: identity.AgencyMachine,
		},
		{
			name: "Stream (programmatic single-shot) -> AgencyMachine",
			call: func(s *Session) {
				if sr, err := s.Stream(context.Background(), blocks); err == nil {
					_ = sr.Close()
				}
			},
			wantAgency: identity.AgencyMachine,
		},
		{
			name: "SubagentResult hand-back -> AgencyMachine",
			call: func(s *Session) {
				_ = s.deliverSubagentResult(context.Background(), s.primaryLoopID, mustUUID(), blocks)
			},
			wantAgency: identity.AgencyMachine,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, cmds, _ := sessionWithFakeLoop()
			cmd := captureCommand(t, s, cmds, tt.call)
			if got := cmd.CommandHeader().Agency; got != tt.wantAgency {
				t.Errorf("%T Header.Agency = %v, want %v", cmd, got, tt.wantAgency)
			}
		})
	}
}

// waitTurnStartedAgency polls the recorded events until a TurnStarted has been
// drained, returning its Cause.Agency. It bridges the async drain like
// waitTurnCausationID does.
func waitTurnStartedAgency(r *recordingSub, d time.Duration) (identity.Agency, bool) {
	deadline := time.Now().Add(d)
	for {
		r.mu.Lock()
		for _, ev := range r.events {
			if ts, ok := ev.(event.TurnStarted); ok {
				ag := ts.Cause.Agency
				r.mu.Unlock()
				return ag, true
			}
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			return identity.AgencyMachine, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSubmitAgencyReachesTurnStarted is the end-to-end proof against a REAL loop that
// the agency a session method stamps on its submit command surfaces on the resulting
// event.TurnStarted's Cause.Agency. A user Submit yields Cause.Agency==AgencyUser ("a
// human started this turn"); a programmatic Invoke yields AgencyMachine. This closes
// the loop on the design contract: TurnStarted.Cause.Agency equals its submit
// command's Header.Agency, for both the user and the machine case.
func TestSubmitAgencyReachesTurnStarted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		call       func(t *testing.T, s *Session)
		wantAgency identity.Agency
	}{
		{
			name: "Submit -> TurnStarted.Cause.Agency AgencyUser",
			call: func(t *testing.T, s *Session) {
				if _, err := s.Submit(context.Background(), nil); err != nil {
					t.Fatalf("Submit: %v", err)
				}
			},
			wantAgency: identity.AgencyUser,
		},
		{
			name: "Invoke -> TurnStarted.Cause.Agency AgencyMachine",
			call: func(t *testing.T, s *Session) {
				if _, err := s.Invoke(context.Background(), nil); err != nil {
					t.Fatalf("Invoke: %v", err)
				}
			},
			wantAgency: identity.AgencyMachine,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			rec, sub := observe(t, s)
			t.Cleanup(func() { _ = sub.Close() })

			tt.call(t, s)

			ag, ok := waitTurnStartedAgency(rec, 2*time.Second)
			if !ok {
				t.Fatal("no TurnStarted observed via the subscription")
			}
			if ag != tt.wantAgency {
				t.Errorf("TurnStarted.Cause.Agency = %v, want %v", ag, tt.wantAgency)
			}
		})
	}
}

// TestInterruptLoopStaysMachine guards the boundary-cancel translation: the internal
// interruptLoop (fired when an Invoke/Stream caller's ctx is cancelled, translating
// the boundary cancel into a turn interrupt) is a MACHINE action — it must NOT inherit
// the human-stamped AgencyUser of the public Interrupt. Only a human pressing
// interrupt (Session.Interrupt) is AgencyUser; the programmatic boundary cancel is
// machine, so we never falsely attribute it to a user (fail-secure attribution).
func TestInterruptLoopStaysMachine(t *testing.T) {
	t.Parallel()
	s, cmds, _ := sessionWithFakeLoop()
	cmd := captureCommand(t, s, cmds, func(s *Session) {
		l, _ := s.loopFor(s.primaryLoopID)
		s.interruptLoop(l)
	})
	ic, ok := cmd.(command.Interrupt)
	if !ok {
		t.Fatalf("interruptLoop sent %T, want command.Interrupt", cmd)
	}
	if ic.Header.Agency != identity.AgencyMachine {
		t.Errorf("interruptLoop Header.Agency = %v, want AgencyMachine (boundary cancel is machine)", ic.Header.Agency)
	}
}
