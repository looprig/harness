package tui

import (
	"strconv"
	"strings"

	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tui/styles"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// promptKind tags a prompt as a permission gate or a user-input request. Each is
// rendered and routed differently, so the kind is carried explicitly rather than
// inferred from which fields are populated.
type promptKind uint8

const (
	// promptPermission is a tool-call approval gate: the user approves (at a
	// scope) or denies.
	promptPermission promptKind = iota
	// promptUserInput is an AskUser request: the user picks a choice or types a
	// free-text answer.
	promptUserInput
)

// prompt is the interaction layer's view-model for one pending request, keyed by
// the gate's ToolExecutionID. It carries everything the renderer needs and the selection
// state the modal key router (Task 8) mutates — but holds NO agent reference: the
// interactionModel only PRODUCES a uiAction; Screen drives the agent. A permission
// prompt uses ToolName/Description/Scopes; a user-input prompt uses
// Question/Choices/selected/freeText.
type prompt struct {
	ToolExecutionID uuid.UUID
	// LoopID is the producing loop's id, stamped from the enqueuing event's
	// Header (zero when that event carries no LoopID). It scopes terminal-event clearing
	// per loop (design §7): a TurnDone/TurnFailed/TurnInterrupted clears only the
	// prompts whose LoopID matches the finishing loop, so one loop ending never
	// abandons a sibling loop's pending gate.
	LoopID      uuid.UUID
	Kind        promptKind
	ToolName    string               // promptPermission: approval-prompt header
	Description string               // promptPermission: approval-prompt body (redacted)
	Scopes      []tool.ApprovalScope // promptPermission: scopes the request allows
	Question    string               // promptUserInput: the AskUser question
	Choices     []string             // promptUserInput: selectable choices (nil → free-text)
	selected    int                  // promptUserInput: cursor over Choices
	freeText    bool                 // promptUserInput: true when there are no Choices
}

// promptFromPermission builds a permission prompt view-model from a sealed
// PermissionRequest. ToolName/Description/Scopes are read off the request via its
// interface methods, so any concrete request type (Bash, FileWrite, Unknown, …)
// projects uniformly. freeText is false: a permission gate is never free-text.
func promptFromPermission(callID uuid.UUID, req tool.PermissionRequest) prompt {
	return prompt{
		ToolExecutionID: callID,
		Kind:            promptPermission,
		ToolName:        req.ToolName(),
		Description:     req.Description(),
		Scopes:          req.AllowedScopes(),
	}
}

// promptFromUserInput builds a user-input prompt view-model. freeText is true
// exactly when there are no choices (an empty or nil slice), in which case the
// user types an answer rather than picking one.
func promptFromUserInput(callID uuid.UUID, question string, choices []string) prompt {
	return prompt{
		ToolExecutionID: callID,
		Kind:            promptUserInput,
		Question:        question,
		Choices:         choices,
		freeText:        len(choices) == 0,
	}
}

// offersScope reports whether the permission prompt allows approving at scope.
// The modal router gates each scope key (y/s/w) on membership so a key for a
// scope the request never offers (e.g. session on an UnknownRequest) is a no-op
// rather than producing an approval the policy layer cannot honor.
func (p *prompt) offersScope(scope tool.ApprovalScope) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// moveSelection shifts the choice cursor by delta and clamps it to the valid
// range [0, len(Choices)-1]. An empty choice list pins the cursor at zero. It is
// the up/down handler for choice mode; the value-copy router calls it on the head
// of the RETURNED model's freshly-cloned slice (see interactionModel.choiceKey).
func (p *prompt) moveSelection(delta int) {
	n := len(p.Choices)
	if n == 0 {
		p.selected = 0
		return
	}
	next := p.selected + delta
	if next < 0 {
		next = 0
	}
	if next > n-1 {
		next = n - 1
	}
	p.selected = next
}

// scopeHint pairs an ApprovalScope with its key+label legend fragment. The keys
// match the modal router (interaction.go permissionKey): y/s/w approve at
// once/session/workspace.
type scopeHint struct {
	scope tool.ApprovalScope
	label string
}

// permissionScopeHints is the ordered legend; only scopes the request offers are
// rendered (offersScope), so an UnknownRequest (ScopeOnce only) shows just [y].
var permissionScopeHints = []scopeHint{
	{tool.ScopeOnce, "[y] once"},
	{tool.ScopeSession, "[s] session"},
	{tool.ScopeWorkspace, "[w] workspace"},
}

// renderPermissionBox renders the compact permission control: an emphasised box
// headed "Approve <ToolName>?" whose body lists ONLY the offered scope keys (y/s/w)
// plus the always-present [n] deny. When pending > 1 a faint "(+N more pending)"
// note trails the box. It consumes the view-model only — no agent, no mutation.
func renderPermissionBox(p prompt, width, pending int) string {
	keys := make([]string, 0, len(permissionScopeHints)+1)
	for _, h := range permissionScopeHints {
		if p.offersScope(h.scope) {
			keys = append(keys, h.label)
		}
	}
	keys = append(keys, "[n] deny") // deny is always offered (fail-secure)
	header := styles.PromptHeaderStyle.Render("Approve " + p.ToolName + "?")
	rows := make([]string, 0, 2)
	if p.Description != "" {
		rows = append(rows, strings.Join(wrapToWidth(p.Description, promptInnerWidth(width)), "\n"))
	}
	rows = append(rows, strings.Join(keys, "   "))
	body := strings.Join(rows, "\n")
	return promptBox(header, body, width, pending)
}

// renderAskUserBox renders an AskUser prompt control. With choices it shows the
// numbered list (a window scrolling with selected so a high row stays visible), the
// ▸ cursor, an [o] other escape hatch and a key legend; with no choices it renders
// the free-text variant (the question above the reused answer field, no list/[o]).
// height bounds the choice window; width wraps the body. Pure: view-model only.
func renderAskUserBox(p prompt, width, height, pending int) string {
	if p.freeText {
		return renderFreeTextBox(p, width, pending)
	}
	return renderChoiceBox(p, width, height, pending)
}

// renderFreeTextBox renders the free-text answer control: a box headed "answer"
// whose body is the (width-wrapped) question. The actual editor is the reused
// composer placed by the surface in modeAnswerPrompt — this control is the framing.
func renderFreeTextBox(p prompt, width, pending int) string {
	header := styles.PromptHeaderStyle.Render("answer")
	body := strings.Join(wrapToWidth(p.Question, promptInnerWidth(width)), "\n")
	return promptBox(header, body, width, pending)
}

// choiceLegend is the key hint shown beneath a choice list.
const choiceLegend = "↑/↓ select · enter answer · 1–9 quick · o other · esc"

// choicePrefixWidth is the 2-cell cursor/indent prefix on every choice row
// ("▸ " when selected, "  " otherwise); the choice text wraps in the remaining
// columns.
const choicePrefixWidth = 2

// choiceChromeRows is the number of non-choice rows the box always reserves
// inside its height budget: the "[o] other" hint and the key legend.
const choiceChromeRows = 2

// renderChoiceBox renders the numbered-choice control: the visible window of
// choices (scrolled to keep selected in view), the ▸ cursor on the selected row,
// the [o] other hint and the key legend, headed "<Question> · choice n/total".
func renderChoiceBox(p prompt, width, height, pending int) string {
	capacity := choiceWindowCap(height)
	start, end := choiceWindow(len(p.Choices), p.selected, capacity)
	rows := make([]string, 0, end-start+2)
	for i := start; i < end; i++ {
		rows = append(rows, choiceRow(i, p.Choices[i], i == p.selected, promptInnerWidth(width)))
	}
	rows = append(rows, styles.PromptHintStyle.Render("[o] other"))
	rows = append(rows, styles.PromptHintStyle.Render(choiceLegend))
	header := styles.PromptHeaderStyle.Render(choiceHeader(p, len(p.Choices)))
	return promptBox(header, strings.Join(rows, "\n"), width, pending)
}

// choiceHeader is the "<Question> · choice n/total" box title, omitting the choice
// counter when there are no choices (defensive; choice mode always has some).
func choiceHeader(p prompt, total int) string {
	if total == 0 {
		return p.Question
	}
	return p.Question + " · choice " + strconv.Itoa(p.selected+1) + "/" + strconv.Itoa(total)
}

// choiceRow renders one numbered choice line: "▸ N. text" for the selected row
// (the cursor + 1-based index), "  N. text" otherwise, width-wrapped under the box.
func choiceRow(index int, text string, selected bool, width int) string {
	label := strconv.Itoa(index+1) + ". " + text
	if selected {
		return styles.PromptCursorStyle.Render("▸ " + truncate(label, width-choicePrefixWidth))
	}
	return "  " + truncate(label, width-choicePrefixWidth)
}

// promptBox frames header+body in the emphasised PromptBoxStyle and trails the
// faint "(+N more pending)" note when the queue is deeper than one (pending > 1).
//
// The design mockups draw the header embedded in the top border (┌─ Approve
// Bash? ─┐), but Lipgloss v2 exposes no border-title API, so the prompt header is
// rendered as a bold first content row inside the box rather than embedded in the
// top border. The visual contract is otherwise unchanged.
func promptBox(header, body string, width, pending int) string {
	inner := promptInnerWidth(width)
	content := header + "\n" + body
	box := styles.PromptBoxStyle.Width(inner).Render(content)
	if pending > 1 {
		note := styles.PromptHintStyle.Render("(+" + strconv.Itoa(pending-1) + " more pending)")
		return box + "\n" + note
	}
	return box
}

// promptInnerWidth is the content width inside a prompt box: the box width less the
// border's horizontal frame, floored at 1.
func promptInnerWidth(width int) int {
	inner := width - styles.PromptBoxStyle.GetHorizontalFrameSize()
	if inner < 1 {
		inner = 1
	}
	return inner
}

// truncate clips s to at most width display runes, appending "…" when it overflows.
// A non-positive width returns "". It keeps a long choice on a single row so the
// window's row count stays predictable.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// choiceWindowCap is the number of choice rows the box can show given its height
// budget: height less the non-choice chrome rows ([o] other + the key legend),
// floored at 0.
func choiceWindowCap(height int) int {
	capacity := height - choiceChromeRows
	if capacity < 0 {
		return 0
	}
	return capacity
}

// choiceWindow returns the [start,end) half-open slice of choices the box shows,
// scrolled so selected stays inside the window of at most capacity rows. With a
// capacity of 0 or an empty list it returns an empty window. When the list fits,
// the whole range is returned; otherwise the window slides so it is roughly
// centred on selected, clamped to the list bounds.
func choiceWindow(total, selected, capacity int) (int, int) {
	if total <= 0 || capacity <= 0 {
		return 0, 0
	}
	if capacity >= total {
		return 0, total
	}
	start := selected - capacity/2
	if start < 0 {
		start = 0
	}
	if start > total-capacity {
		start = total - capacity
	}
	return start, start + capacity
}
