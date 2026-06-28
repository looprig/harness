package transcript

import "testing"

func TestModelZeroValues(t *testing.T) {
	var s Session
	if s.Root != nil || len(s.Notices) != 0 || len(s.Warnings) != 0 {
		t.Fatalf("zero Session not empty: %+v", s)
	}
	// sum-type guard: EventRecord and CommandRecord both satisfy Record.
	var _ Record = EventRecord{}
	var _ Record = CommandRecord{}
	// meaningful-zero invariant: an unterminated turn is Running and an
	// unresolved gate is Pending, both by zero value. Guard against an iota
	// reorder silently breaking it.
	if OutcomeRunning != 0 || DecisionPending != 0 {
		t.Fatalf("meaningful zero broken: Running=%d Pending=%d", OutcomeRunning, DecisionPending)
	}
}
