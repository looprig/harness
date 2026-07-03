package html

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/transcript"
)

func TestMessageText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *transcript.Message
		want string
	}{
		{
			name: "nil message",
			msg:  nil,
			want: "",
		},
		{
			name: "no blocks",
			msg:  &transcript.Message{Role: content.RoleUser},
			want: "",
		},
		{
			name: "single text block",
			msg:  &transcript.Message{Blocks: []content.Block{&content.TextBlock{Text: "hello"}}},
			want: "hello",
		},
		{
			name: "multiple text blocks newline-joined",
			msg: &transcript.Message{Blocks: []content.Block{
				&content.TextBlock{Text: "first"},
				&content.TextBlock{Text: "second"},
			}},
			want: "first\nsecond",
		},
		{
			name: "skips thinking and tool-use blocks",
			msg: &transcript.Message{Blocks: []content.Block{
				&content.ThinkingBlock{Thinking: "secret reasoning"},
				&content.TextBlock{Text: "visible"},
				&content.ToolUseBlock{ID: "tu1", Name: "Bash"},
			}},
			want: "visible",
		},
		{
			name: "only non-text blocks yields empty",
			msg: &transcript.Message{Blocks: []content.Block{
				&content.ThinkingBlock{Thinking: "secret"},
				&content.ToolUseBlock{ID: "tu1", Name: "Bash"},
			}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := messageText(tt.msg); got != tt.want {
				t.Errorf("messageText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMessageTime(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC)
	fallback := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		msg      *transcript.Message
		fallback time.Time
		want     time.Time
	}{
		{
			name:     "nil message falls back",
			msg:      nil,
			fallback: fallback,
			want:     fallback,
		},
		{
			name:     "zero message time falls back",
			msg:      &transcript.Message{},
			fallback: fallback,
			want:     fallback,
		},
		{
			name:     "non-zero message time wins",
			msg:      &transcript.Message{At: at},
			fallback: fallback,
			want:     at,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := messageTime(tt.msg, tt.fallback); !got.Equal(tt.want) {
				t.Errorf("messageTime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatClock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{name: "zero time empty", in: time.Time{}, want: ""},
		{name: "fixed time", in: time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC), want: "10:00:02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatClock(tt.in); got != tt.want {
				t.Errorf("formatClock() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{name: "zero time em dash", in: time.Time{}, want: "—"},
		{name: "fixed time", in: time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC), want: "2026-06-28 10:00:00 UTC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatTimestamp(tt.in); got != tt.want {
				t.Errorf("formatTimestamp() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNewTurnView asserts the multi-step, empty, and nil-User branches directly,
// rather than only through the single happy-path golden.
func TestNewTurnView(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC)
	aiStep := func(text string) *transcript.Step {
		return &transcript.Step{AI: &transcript.Message{
			Role:   content.RoleAssistant,
			At:     at,
			Blocks: []content.Block{&content.TextBlock{Text: text}},
		}}
	}

	tests := []struct {
		name      string
		turn      *transcript.Turn
		wantSteps int
	}{
		{
			name: "nil user, no steps",
			turn: &transcript.Turn{Index: 1, StartedAt: at},
		},
		{
			name: "user but empty (no steps)",
			turn: &transcript.Turn{
				Index:     2,
				StartedAt: at,
				User:      &transcript.Message{Blocks: []content.Block{&content.TextBlock{Text: "hi"}}},
			},
		},
		{
			name: "two steps",
			turn: &transcript.Turn{
				Index:     3,
				StartedAt: at,
				User:      &transcript.Message{Blocks: []content.Block{&content.TextBlock{Text: "go"}}},
				Steps:     []*transcript.Step{aiStep("one"), aiStep("two")},
			},
			wantSteps: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tv, err := newTurnView("operator", tt.turn, 0)
			if err != nil {
				t.Fatalf("newTurnView() error = %v", err)
			}
			if len(tv.Steps) != tt.wantSteps {
				t.Errorf("len(Steps) = %d, want %d", len(tv.Steps), tt.wantSteps)
			}
			// nil User yields empty rendered HTML; a present User yields non-empty.
			gotEmptyUser := tv.User == ""
			wantEmptyUser := tt.turn.User == nil
			if gotEmptyUser != wantEmptyUser {
				t.Errorf("User empty = %v, want %v (User=%q)", gotEmptyUser, wantEmptyUser, tv.User)
			}
			for i, sv := range tv.Steps {
				if sv.AgentName != "operator" {
					t.Errorf("step %d AgentName = %q, want %q", i, sv.AgentName, "operator")
				}
				if sv.AI == "" {
					t.Errorf("step %d AI HTML is empty", i)
				}
			}
		})
	}
}

// TestTruncateForDisplay exercises the byte cap directly, including the headline
// gap the golden never hits: a multibyte rune straddling resultByteCap, so the
// !utf8.RuneStart back-off branch runs and the output stays valid UTF-8. It also
// covers under-cap, exactly-at-cap, one-over (ASCII), and an all-continuation-byte
// input (invalid UTF-8) that must terminate at cut == 0 rather than loop forever.
func TestTruncateForDisplay(t *testing.T) {
	t.Parallel()

	const cap = resultByteCap
	const euro = "€" // 3 bytes: 0xE2 0x82 0xAC

	tests := []struct {
		name       string
		in         string
		wantShown  int // len(shown)
		wantElided int
	}{
		{name: "under cap unchanged", in: "hello", wantShown: 5, wantElided: 0},
		{name: "exactly at cap unchanged", in: strings.Repeat("a", cap), wantShown: cap, wantElided: 0},
		{name: "one over cap ascii", in: strings.Repeat("a", cap+1), wantShown: cap, wantElided: 1},
		{
			name: "multibyte rune straddles cap backs off",
			// the euro's lead byte sits at cap-1, so the cap falls mid-rune.
			in:         strings.Repeat("a", cap-1) + euro,
			wantShown:  cap - 1,
			wantElided: 3,
		},
		{
			name:       "all continuation bytes terminates at zero",
			in:         strings.Repeat("\x80", cap+10),
			wantShown:  0,
			wantElided: cap + 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shown, elided := truncateForDisplay(tt.in)
			if len(shown) != tt.wantShown {
				t.Errorf("len(shown) = %d, want %d", len(shown), tt.wantShown)
			}
			if elided != tt.wantElided {
				t.Errorf("elided = %d, want %d", elided, tt.wantElided)
			}
			if !utf8.ValidString(shown) {
				t.Errorf("shown is not valid UTF-8: %q", shown)
			}
		})
	}
}

// TestInlineText covers whitespace collapsing and the over-length truncation path
// (ellipsis + multibyte rune back-off, leaving valid UTF-8).
func TestInlineText(t *testing.T) {
	t.Parallel()

	const euro = "€" // 3 bytes

	tests := []struct {
		name        string
		in          string
		want        string // when exact match is expected
		wantSuffix  bool   // otherwise assert the truncation shape
		maxRuneLess bool
	}{
		{name: "empty", in: "", want: ""},
		{name: "collapses whitespace", in: "a  b\n\tc", want: "a b c"},
		{name: "under limit unchanged", in: strings.Repeat("x", inlineLimit), want: strings.Repeat("x", inlineLimit)},
		{
			name:       "over limit truncates with ellipsis and rune back-off",
			in:         strings.Repeat("x", inlineLimit-1) + euro + strings.Repeat("y", 40),
			wantSuffix: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := inlineText(tt.in)
			if !utf8.ValidString(got) {
				t.Errorf("inlineText() not valid UTF-8: %q", got)
			}
			if tt.wantSuffix {
				if !strings.HasSuffix(got, "…") {
					t.Errorf("inlineText() = %q, want trailing ellipsis", got)
				}
				// the euro straddling the limit is dropped, so only ASCII 'x' remains.
				if strings.ContainsRune(got, '€') {
					t.Errorf("inlineText() kept a half-truncated multibyte rune: %q", got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("inlineText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPrettyJSON covers the empty and invalid-JSON degradation branches (no panic;
// sensible fallback) plus the happy indent path.
func TestPrettyJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		in       json.RawMessage
		want     string
		contains string
	}{
		{name: "nil raw message", in: nil, want: ""},
		{name: "empty raw message", in: json.RawMessage(""), want: ""},
		{name: "invalid json returned as-is", in: json.RawMessage("{bad"), want: "{bad"},
		{name: "valid json indented", in: json.RawMessage(`{"a":1}`), contains: "\n  \"a\": 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := prettyJSON(tt.in)
			if tt.contains != "" {
				if !strings.Contains(got, tt.contains) {
					t.Errorf("prettyJSON() = %q, want substring %q", got, tt.contains)
				}
				return
			}
			if got != tt.want {
				t.Errorf("prettyJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestScopeName is a thin table over the full ApprovalScope enum plus the unknown
// fallback.
func TestScopeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		scope tool.ApprovalScope
		want  string
	}{
		{name: "once", scope: tool.ScopeOnce, want: "once"},
		{name: "session", scope: tool.ScopeSession, want: "session"},
		{name: "workspace", scope: tool.ScopeWorkspace, want: "workspace"},
		{name: "unknown fallback", scope: tool.ApprovalScope(0xff), want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := scopeName(tt.scope); got != tt.want {
				t.Errorf("scopeName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecisionVerb covers every Decision plus the pending (zero-value) default.
func TestDecisionVerb(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		decision  transcript.Decision
		wantVerb  string
		wantClass string
	}{
		{name: "approved", decision: transcript.DecisionApproved, wantVerb: "Approved ✓", wantClass: "approved"},
		{name: "denied", decision: transcript.DecisionDenied, wantVerb: "Denied ✗", wantClass: "denied"},
		{name: "answered", decision: transcript.DecisionAnswered, wantVerb: "Answered ✓", wantClass: "answered"},
		{name: "pending default", decision: transcript.DecisionPending, wantVerb: "Pending …", wantClass: "pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			verb, class := decisionVerb(&transcript.GateAction{Decision: tt.decision})
			if verb != tt.wantVerb {
				t.Errorf("verb = %q, want %q", verb, tt.wantVerb)
			}
			if class != tt.wantClass {
				t.Errorf("class = %q, want %q", class, tt.wantClass)
			}
		})
	}
}

// TestGateChip covers every chip variant, asserting each carries its DecidedAt time
// and that an answered AskUser chip surfaces the question and answer.
func TestGateChip(t *testing.T) {
	t.Parallel()

	decided := time.Date(2026, 6, 28, 10, 0, 9, 0, time.UTC)
	opened := time.Date(2026, 6, 28, 10, 0, 8, 0, time.UTC)

	tests := []struct {
		name      string
		gate      *transcript.GateAction
		wantClass string
		contains  []string
	}{
		{
			name:      "approved with scope and time",
			gate:      &transcript.GateAction{Decision: transcript.DecisionApproved, Scope: tool.ScopeSession, DecidedAt: decided},
			wantClass: "approved",
			contains:  []string{"You approved", "session", "10:00:09"},
		},
		{
			name:      "denied with time",
			gate:      &transcript.GateAction{Decision: transcript.DecisionDenied, DecidedAt: decided},
			wantClass: "denied",
			contains:  []string{"You denied", "10:00:09"},
		},
		{
			name:      "answered surfaces question answer and time",
			gate:      &transcript.GateAction{Decision: transcript.DecisionAnswered, Question: "Which env?", Answer: "staging", DecidedAt: decided},
			wantClass: "answered",
			contains:  []string{"You answered", "10:00:09", "Which env?", "staging"},
		},
		{
			name:      "answered without question falls back to colon form",
			gate:      &transcript.GateAction{Decision: transcript.DecisionAnswered, Answer: "yes", DecidedAt: decided},
			wantClass: "answered",
			contains:  []string{"You answered · 10:00:09: yes"},
		},
		{
			name:      "pending uses opened time",
			gate:      &transcript.GateAction{Decision: transcript.DecisionPending, OpenedAt: opened},
			wantClass: "pending",
			contains:  []string{"Awaiting your response", "10:00:08"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gv := gateChip(tt.gate)
			if gv.Class != tt.wantClass {
				t.Errorf("Class = %q, want %q", gv.Class, tt.wantClass)
			}
			for _, sub := range tt.contains {
				if !strings.Contains(gv.Text, sub) {
					t.Errorf("gateChip() text = %q, want substring %q", gv.Text, sub)
				}
			}
		})
	}
}

// TestNoticeMeta covers every NoticeKind plus the default fallback.
func TestNoticeMeta(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		kind      transcript.NoticeKind
		wantLabel string
		wantClass string
	}{
		{name: "active", kind: transcript.NoticeSessionActive, wantLabel: "active", wantClass: "active"},
		{name: "idle", kind: transcript.NoticeSessionIdle, wantLabel: "idle", wantClass: "idle"},
		{name: "stopped", kind: transcript.NoticeSessionStopped, wantLabel: "stopped", wantClass: "stopped"},
		{name: "restore started", kind: transcript.NoticeRestoreStarted, wantLabel: "restore", wantClass: "restore"},
		{name: "restore done", kind: transcript.NoticeRestoreDone, wantLabel: "restore", wantClass: "restore"},
		{name: "restore errored", kind: transcript.NoticeRestoreErrored, wantLabel: "restore error", wantClass: "error"},
		{name: "unknown default", kind: transcript.NoticeKind(0xff), wantLabel: "notice", wantClass: "notice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			label, class := noticeMeta(tt.kind)
			if label != tt.wantLabel {
				t.Errorf("label = %q, want %q", label, tt.wantLabel)
			}
			if class != tt.wantClass {
				t.Errorf("class = %q, want %q", class, tt.wantClass)
			}
		})
	}
}
