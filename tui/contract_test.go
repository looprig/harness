package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

// contract_test.go is the Task 11 regression-LOCK for the TUI side of the AskUser
// answer contract (design §2/§4 finding): with a with-choices prompt, the
// interactionModel must NEVER emit a uiAnswer whose Text is arbitrary typed text —
// it is ALWAYS a listed choice or the literal "other". If it ever did, that answer
// would fail tools.validateAnswer and surface as a tool-result ERROR string.
//
// validateAnswer is UNEXPORTED in package tools, so this tui-package test asserts
// the PUBLIC invariant directly — the produced uiAction.Text — rather than calling
// validateAnswer across the package boundary. The tools side of the same contract
// (the tool ACCEPTS those exact outputs) is locked in tools/askuser_test.go.

// choiceMembers reports whether text is a member of choices ∪ {otherChoice} — the
// exact set tools.validateAnswer accepts when choices is non-empty.
func choiceMembers(text string, choices []string) bool {
	if text == otherChoice {
		return true
	}
	for _, c := range choices {
		if text == c {
			return true
		}
	}
	return false
}

// TestContractChoiceAnswerAlwaysAMember is Guard A: for a with-choices prompt, NO
// single key route through interactionModel.Update produces a uiAnswer whose Text
// is outside Choices ∪ {"other"}. Equivalently, every key either (a) does not emit
// uiAnswer at all, or (b) emits a uiAnswer.Text that validateAnswer would accept.
//
// The table drives one fresh choice prompt per key — letters/printables, digits in
// and out of range, the 'o' escape hatch, up/down navigation, and esc — and the
// loop asserts the invariant uniformly. Printable LETTERS in choice mode must NOT
// emit a uiAnswer (there is no free-text capture in choice mode); the test pins
// that explicitly via wantEmitsAnswer=false.
func TestContractChoiceAnswerAlwaysAMember(t *testing.T) {
	t.Parallel()

	// A choice set that deliberately does NOT contain any of the printable letters
	// we drive (a/b/q/z) nor a digit-shaped string, so a stray free-text capture or
	// an off-by-one accelerator would escape the member set and fail the guard.
	choices := []string{"alpha", "beta", "gamma"}

	tests := []struct {
		name            string
		preDowns        int             // down-presses to move selection before the key
		key             tea.KeyPressMsg // the key under test
		wantEmitsAnswer bool            // true → this key must produce a uiAnswer
	}{
		// Selection submit at various selected positions: Text is always Choices[sel].
		{name: "enter at head selects a member", key: tea.KeyPressMsg{Code: tea.KeyEnter}, wantEmitsAnswer: true},
		{name: "enter after one down selects a member", preDowns: 1, key: tea.KeyPressMsg{Code: tea.KeyEnter}, wantEmitsAnswer: true},
		{name: "enter after two downs selects a member", preDowns: 2, key: tea.KeyPressMsg{Code: tea.KeyEnter}, wantEmitsAnswer: true},
		{name: "keypad enter selects a member", key: tea.KeyPressMsg{Code: tea.KeyKpEnter}, wantEmitsAnswer: true},

		// Digit accelerators in range select a member; out of range emit nothing.
		{name: "digit 1 selects a member", key: runeKey('1'), wantEmitsAnswer: true},
		{name: "digit 3 selects a member", key: runeKey('3'), wantEmitsAnswer: true},
		{name: "digit 4 (out of range) emits no answer", key: runeKey('4'), wantEmitsAnswer: false},
		{name: "digit 9 (out of range) emits no answer", key: runeKey('9'), wantEmitsAnswer: false},

		// The 'o' escape hatch emits the literal otherChoice — itself a member.
		{name: "o emits the other escape hatch", key: runeKey('o'), wantEmitsAnswer: true},

		// Printable LETTERS in choice mode must NOT emit a uiAnswer (no free text).
		{name: "letter a emits no answer", key: runeKey('a'), wantEmitsAnswer: false},
		{name: "letter b emits no answer", key: runeKey('b'), wantEmitsAnswer: false},
		{name: "letter q emits no answer", key: runeKey('q'), wantEmitsAnswer: false},
		{name: "letter z emits no answer", key: runeKey('z'), wantEmitsAnswer: false},

		// Navigation and interrupt never emit a uiAnswer.
		{name: "up emits no answer", key: tea.KeyPressMsg{Code: tea.KeyUp}, wantEmitsAnswer: false},
		{name: "down emits no answer", key: tea.KeyPressMsg{Code: tea.KeyDown}, wantEmitsAnswer: false},
		{name: "esc emits no answer (interrupt)", key: tea.KeyPressMsg{Code: tea.KeyEsc}, wantEmitsAnswer: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := choiceModel(choices)
			for i := 0; i < tt.preDowns; i++ {
				m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			}

			_, action := m.Update(tt.key)

			gotAnswer := action.Kind == uiAnswer
			if gotAnswer != tt.wantEmitsAnswer {
				t.Fatalf("emits uiAnswer = %v, want %v (action = %+v)", gotAnswer, tt.wantEmitsAnswer, action)
			}
			// THE INVARIANT: whenever a uiAnswer IS emitted in choice mode, its Text
			// must be accepted by the contract (a listed choice or "other"). A failure
			// here is a real contract bug — the TUI would send text validateAnswer
			// rejects, surfacing as a tool-result error.
			if gotAnswer && !choiceMembers(action.Text, choices) {
				t.Errorf("choice-mode uiAnswer.Text = %q is NOT a member of %v ∪ {%q} — would fail validateAnswer", action.Text, choices, otherChoice)
			}
		})
	}
}

// TestContractChoiceAnswerMemberUnderKeySequences is the stronger form of Guard A:
// it drives a SEQUENCE of assorted keys (navigation interleaved with submit
// attempts and letters) against a single choice prompt and asserts the same
// invariant after every step — no ordering of routes can ever produce a uiAnswer
// whose Text escapes Choices ∪ {"other"}. It also confirms that once a submit pops
// the head the model returns to compose and stops emitting choice answers.
func TestContractChoiceAnswerMemberUnderKeySequences(t *testing.T) {
	t.Parallel()

	choices := []string{"red", "green", "blue", "cyan", "magenta"}

	// A mixed key stream: letters (no-op), digits (in/out of range), navigation,
	// the escape hatch, and submits. Driven against fresh prompts and as a running
	// sequence; either way every emitted choice answer must be a member.
	keys := []tea.KeyPressMsg{
		runeKey('x'), runeKey('y'), // letters: no answer
		{Code: tea.KeyDown}, {Code: tea.KeyDown}, // navigate
		runeKey('7'), // out-of-range digit: no answer
		runeKey('2'), // in-range digit: a member
		{Code: tea.KeyUp},
		runeKey('o'),               // escape hatch: "other"
		{Code: tea.KeyEnter},       // submit selected: a member
		{Code: tea.KeyKpEnter},     // keypad submit: a member
		runeKey('5'), runeKey('1'), // more in-range digits
	}

	t.Run("each key against a fresh choice prompt", func(t *testing.T) {
		t.Parallel()
		// Drive each key independently (fresh prompt, mid-list selection) so a key
		// that only ever emits at a particular selected index is still exercised.
		for _, k := range keys {
			m := choiceModel(choices)
			m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // selected = 1
			m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // selected = 2
			_, action := m.Update(k)
			if action.Kind == uiAnswer && !choiceMembers(action.Text, choices) {
				t.Errorf("key %+v produced uiAnswer.Text %q, not a member of %v ∪ {%q}", k, action.Text, choices, otherChoice)
			}
		}
	})

	t.Run("running sequence never escapes the member set", func(t *testing.T) {
		t.Parallel()
		m := choiceModel(choices)
		for _, k := range keys {
			var action uiAction
			m, action = m.Update(k)
			if action.Kind == uiAnswer && !choiceMembers(action.Text, choices) {
				t.Fatalf("key %+v in sequence produced uiAnswer.Text %q, not a member of %v ∪ {%q}", k, action.Text, choices, otherChoice)
			}
			// After a submit pops the head the queue drains to compose mode; from
			// there a printable key is composer text, never a choice answer. Re-enter
			// a fresh choice prompt so the remaining keys keep exercising choice mode.
			if m.mode == modeCompose {
				m = m.ApplyEvent(event.UserInputRequested{CallID: callID(99), Question: "again", Choices: choices})
			}
		}
	})
}
