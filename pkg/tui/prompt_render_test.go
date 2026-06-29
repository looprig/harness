package tui

import (
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/tool"
)

// TestRenderPermissionBox covers the permission-control rendering: a bordered box
// with an "Approve <ToolName>?" header that shows ONLY the scope keys the request
// offers — [y] once iff ScopeOnce, [s] session iff ScopeSession, [w] workspace iff
// ScopeWorkspace — and [n] deny ALWAYS. (+N more pending) appears when the queue is
// deeper than one.
func TestRenderPermissionBox(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		req         tool.PermissionRequest
		pending     int
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "bash offers all scopes plus deny",
			req:         tool.BashRequest{Command: "go build"},
			pending:     1,
			wantContain: []string{"Approve Bash?", "go build", "[y] once", "[s] session", "[w] workspace", "[n] deny"},
			wantAbsent:  []string{"more pending"},
		},
		{
			name:        "fetch shows method and url",
			req:         tool.FetchRequest{Method: "GET", URL: "https://google.com"},
			pending:     1,
			wantContain: []string{"Approve Fetch?", "GET https://google.com", "[y] once", "[s] session", "[w] workspace", "[n] deny"},
			wantAbsent:  []string{"more pending"},
		},
		{
			name:        "unknown offers only once and deny",
			req:         tool.UnknownRequest{Tool: "Mystery", Summary: "does a thing"},
			pending:     1,
			wantContain: []string{"Approve Mystery?", "does a thing", "[y] once", "[n] deny"},
			wantAbsent:  []string{"[s] session", "[w] workspace"},
		},
		{
			name:        "more pending hint when queue deeper than one",
			req:         tool.BashRequest{Command: "rm -rf /"},
			pending:     3,
			wantContain: []string{"Approve Bash?", "(+2 more pending)"},
			wantAbsent:  []string{},
		},
		{
			name:        "single pending shows no hint",
			req:         tool.FileWriteRequest{Path: "/tmp/x"},
			pending:     1,
			wantContain: []string{"Approve WriteFile?", "[y] once"},
			wantAbsent:  []string{"more pending"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := promptFromPermission(callID(1), tt.req)
			got := stripANSI(renderPermissionBox(p, 80, tt.pending))

			for _, sub := range tt.wantContain {
				if !strings.Contains(got, sub) {
					t.Errorf("renderPermissionBox missing %q in:\n%s", sub, got)
				}
			}
			for _, sub := range tt.wantAbsent {
				if sub != "" && strings.Contains(got, sub) {
					t.Errorf("renderPermissionBox unexpectedly contains %q in:\n%s", sub, got)
				}
			}
		})
	}
}

// TestRenderAskUserBoxChoices covers the choice-list AskUser box: numbered choices
// [1].., an [o] other escape hatch, the ▸ cursor on prompt.selected, and a window
// that scrolls with selected so a highlighted row past the height budget stays
// visible.
func TestRenderAskUserBoxChoices(t *testing.T) {
	t.Parallel()

	twelve := []string{
		"internal/version.Version()", "git describe", "VERSION file",
		"build-time ldflags", "hardcoded constant", "env var", "config key",
		"latest git tag", "CHANGELOG top", "date-based", "manual prompt", "ask each build",
	}

	tests := []struct {
		name        string
		question    string
		choices     []string
		selected    int
		height      int
		pending     int
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "short list shows all numbered with other and cursor",
			question:    "pick one",
			choices:     []string{"alpha", "beta", "gamma"},
			selected:    0,
			height:      10,
			pending:     1,
			wantContain: []string{"1.", "alpha", "2.", "beta", "3.", "gamma", "[o] other", "▸"},
		},
		{
			name:        "cursor marks the selected row",
			question:    "pick one",
			choices:     []string{"alpha", "beta", "gamma"},
			selected:    2,
			height:      10,
			pending:     1,
			wantContain: []string{"▸ 3. gamma"},
			wantAbsent:  []string{"▸ 1.", "▸ 2."},
		},
		{
			name:        "window scrolls so high selection stays visible",
			question:    "Which source?",
			choices:     twelve,
			selected:    9, // the 10th choice, well past a small window
			height:      5,
			pending:     1,
			wantContain: []string{"▸ 10. date-based"},
			wantAbsent:  []string{"1. internal/version.Version()"}, // scrolled out of the window
		},
		{
			name:        "more pending hint with many choices",
			question:    "Which source?",
			choices:     twelve,
			selected:    0,
			height:      8,
			pending:     4,
			wantContain: []string{"(+3 more pending)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := promptFromUserInput(callID(2), tt.question, tt.choices)
			p.selected = tt.selected
			got := stripANSI(renderAskUserBox(p, 80, tt.height, tt.pending))

			for _, sub := range tt.wantContain {
				if !strings.Contains(got, sub) {
					t.Errorf("renderAskUserBox missing %q in:\n%s", sub, got)
				}
			}
			for _, sub := range tt.wantAbsent {
				if sub != "" && strings.Contains(got, sub) {
					t.Errorf("renderAskUserBox unexpectedly contains %q in:\n%s", sub, got)
				}
			}
		})
	}
}

// TestRenderAskUserBoxFreeText covers the free-text variant: the question renders
// above the reused input box, with NO choice list and NO [o] other hint.
func TestRenderAskUserBoxFreeText(t *testing.T) {
	t.Parallel()

	p := promptFromUserInput(callID(3), "What should the version look like?", nil)
	got := stripANSI(renderAskUserBox(p, 80, 6, 1))

	for _, sub := range []string{"answer", "What should the version look like?"} {
		if !strings.Contains(got, sub) {
			t.Errorf("free-text box missing %q in:\n%s", sub, got)
		}
	}
	for _, sub := range []string{"[o] other", "▸", "1."} {
		if strings.Contains(got, sub) {
			t.Errorf("free-text box unexpectedly contains choice affordance %q in:\n%s", sub, got)
		}
	}
}

// TestChoiceWindow covers the pure window calculator that selects the visible slice
// of choices and the cursor offset within it, scrolling so selected stays inside.
func TestChoiceWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		total         int
		selected      int
		cap           int
		wantStart     int
		wantEnd       int
		wantHasCursor bool
	}{
		{name: "all fit", total: 3, selected: 1, cap: 5, wantStart: 0, wantEnd: 3, wantHasCursor: true},
		{name: "cap zero shows none", total: 5, selected: 2, cap: 0, wantStart: 0, wantEnd: 0, wantHasCursor: false},
		{name: "selection at start", total: 12, selected: 0, cap: 3, wantStart: 0, wantEnd: 3, wantHasCursor: true},
		{name: "selection scrolls window", total: 12, selected: 9, cap: 3, wantStart: 8, wantEnd: 11, wantHasCursor: true},
		{name: "selection at end clamps", total: 12, selected: 11, cap: 3, wantStart: 9, wantEnd: 12, wantHasCursor: true},
		{name: "empty list", total: 0, selected: 0, cap: 5, wantStart: 0, wantEnd: 0, wantHasCursor: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			start, end := choiceWindow(tt.total, tt.selected, tt.cap)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("choiceWindow(%d,%d,%d) = (%d,%d), want (%d,%d)",
					tt.total, tt.selected, tt.cap, start, end, tt.wantStart, tt.wantEnd)
			}
			hasCursor := tt.selected >= start && tt.selected < end
			if hasCursor != tt.wantHasCursor {
				t.Errorf("cursor visible = %v, want %v (start=%d end=%d sel=%d)",
					hasCursor, tt.wantHasCursor, start, end, tt.selected)
			}
		})
	}
}
