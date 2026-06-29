//go:build integration

package journal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/transcript/html"
	"github.com/ciram-co/looprig/pkg/transcript/journalsource"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// primaryPromptResolver is the export-time SystemPromptResolver swe wires: it resolves
// ONE loop (the primary) to a known prompt text and degrades every other loop to
// ("", false). Replaying a real session this is how a subagent loop — whose transient
// spawn-time prompt was never retained — degrades to the auditable Decision-4 warning.
type primaryPromptResolver struct {
	primary uuid.UUID
	text    string
}

func (r primaryPromptResolver) SystemPrompt(loopID uuid.UUID) (text string, ok bool) {
	if loopID == r.primary {
		return r.text, true
	}
	return "", false
}

// TestExportPipelineEndToEnd is the capstone integration: a realistic session is
// appended to a REAL journal (embedded JetStream) — primary + a nested reviewer
// subagent, a human-approved Bash gate — then driven through the WHOLE export chain
// exactly as the TUI's /export does it:
//
//	journal.RecordReplayer -> journalsource.Open -> transcript.Reconstruct ->
//	html.Render -> a file on disk.
//
// The fold and the renderer are unit-tested exhaustively elsewhere; this test's value
// is the REAL persisted-data chain. It asserts the audit-load-bearing features survive
// the round-trip: the approved Bash card, the "You approved" gate chip (scope + time),
// the inline-nested reviewer subagent at data-depth="1", the resolved primary system
// prompt, the child-loop degradation warning, a self-contained (offline) file, and that
// markup in a message is rendered inert (no live <script> from message text).
//
// Append ORDER mirrors the real runtime ordering the reconstruction was built for: the
// child loop's ENTIRE lifecycle and the gate+approval BOTH land BEFORE the primary
// StepDone that carries their Subagent/Bash tool-uses (the Subagent runs the child to
// completion and the gate resolves before the parent step commits).
func TestExportPipelineEndToEnd(t *testing.T) {
	// Coordinates (0xE*) and per-record EventIDs/CommandID (0xF*) are disjoint so every
	// journaled record carries a unique idempotency id (a shared id would de-dup on append).
	sid := seedUUID(0xE0)
	primaryLid := seedUUID(0xE1)
	primaryTid := seedUUID(0xE2)
	primaryStep := seedUUID(0xE3)
	childLid := seedUUID(0xE4)
	childTid := seedUUID(0xE5)
	childStep := seedUUID(0xE6)
	bashExecID := seedUUID(0xE7)

	const (
		modelID         = "claude-opus-4-8"
		agentKind       = "swe:operator"
		systemPromptRev = "sha256:deadbeefcafef00d"
		primaryPrompt   = "You are the primary operator agent. Follow SOLID and security rules."
		userText        = "Please run the tests and have someone audit the diff."
		// AI prose carries inline markup: goldmark (unsafe OFF) must render it inert.
		aiText      = "Running the test suite and delegating a structured code check.\n\n<script>alert('xss')</script>"
		bashCommand = "go test ./..."
		// Load-bearing wiring keys, hoisted to consts so a typo that silently breaks
		// nesting or result-pairing is a single compile-visible edit:
		//   subToolUseID  links the child LoopStarted.ParentToolUseID ↔ the Subagent ToolUseBlock.ID.
		//   bashToolUseID links the Bash ToolUseBlock.ID ↔ its paired ToolResultMessage.ToolUseID.
		subToolUseID  = "sub1"
		bashToolUseID = "tu-bash"
	)

	// Build the Bash tool-use input FROM bashCommand (single source of truth) so the gate's
	// PermissionRequest and the tool-use input cannot silently diverge — their agreement is
	// part of what this test proves.
	bashInput, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: bashCommand})
	if err != nil {
		t.Fatalf("marshal bash input: %v", err)
	}

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return base.Add(time.Duration(n) * time.Second) }

	// 1. SessionStarted: session-scoped (LoopID/TurnID/StepID zero), config fingerprint.
	sessionStarted := event.SessionStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid},
			EventID:     seedUUID(0xF0),
			CreatedAt:   at(0),
		},
		Config: event.ConfigFingerprint{
			ModelID:         modelID,
			AgentKind:       agentKind,
			SystemPromptRev: systemPromptRev,
		},
	}
	// 2. primary LoopStarted (the root: ParentToolUseID "").
	primaryLoop := event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: primaryLid},
			AgentName:   identity.AgentName("operator"),
			EventID:     seedUUID(0xF1),
			CreatedAt:   at(1),
		},
	}
	// 3. primary TurnStarted (the user's prompt).
	primaryTurnStarted := event.TurnStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: primaryLid, TurnID: primaryTid},
			EventID:     seedUUID(0xF2),
			CreatedAt:   at(2),
		},
		TurnIndex: 1,
		Message: &content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: userText}},
		}},
	}
	// 4. CHILD LoopStarted — the reviewer subagent, spawned by the (future) "sub1"
	//    tool-use; Cause.LoopID is the parent (primary) loop, the key the builder nests by.
	childLoop := event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: childLid},
			AgentName:   identity.AgentName("reviewer"),
			EventID:     seedUUID(0xF3),
			CreatedAt:   at(3),
			Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: primaryLid}},
		},
		ParentToolUseID: subToolUseID,
	}
	// 5-7. the child's full lifecycle, BEFORE the parent StepDone.
	childTurnStarted := event.TurnStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: childLid, TurnID: childTid},
			EventID:     seedUUID(0xF4),
			CreatedAt:   at(4),
		},
		TurnIndex: 1,
		Message: &content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: "Audit the staged diff."}},
		}},
	}
	childStepDone := event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: childLid, TurnID: childTid, StepID: childStep},
			EventID:     seedUUID(0xF5),
			CreatedAt:   at(5),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: "The diff looks correct."}},
			}},
		},
	}
	childTurnDone := event.TurnDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: childLid, TurnID: childTid},
			EventID:     seedUUID(0xF6),
			CreatedAt:   at(6),
		},
		TurnIndex: 1,
	}
	// 8. the Bash permission gate (full quartet + ToolExecutionID), BEFORE the StepDone.
	//    Request rides the durable wire (tool.MarshalPermissionRequest), so ToolName()
	//    survives replay and the gate binds to the Bash card by name.
	permRequested := event.PermissionRequested{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: primaryLid, TurnID: primaryTid, StepID: primaryStep},
			EventID:     seedUUID(0xF7),
			CreatedAt:   at(7),
		},
		ToolExecutionID: bashExecID,
		Request:         tool.BashRequest{Command: bashCommand},
	}
	// 9. the user's resolving command (a gate decision — the record the EventReplayer
	//    would drop). Approved at session scope, by a human (AgencyUser).
	approve := command.ApproveToolCall{
		Header: command.Header{
			CommandID: seedUUID(0xF8),
			Agency:    identity.AgencyUser,
			CreatedAt: at(8),
		},
		GateRoute: command.GateRoute{
			Coordinates:     identity.Coordinates{SessionID: sid, LoopID: primaryLid},
			ToolExecutionID: bashExecID,
		},
		Scope: tool.ScopeSession,
	}
	// 10. the primary StepDone: the AIMessage carries the Subagent (sub1) AND Bash
	//     (tu-bash) tool-uses + their paired results — reconciling the buffered child
	//     loop and flushing the buffered gate.
	primaryStepDone := event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: primaryLid, TurnID: primaryTid, StepID: primaryStep},
			EventID:     seedUUID(0xF9),
			CreatedAt:   at(9),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{
					&content.TextBlock{Text: aiText},
					&content.ToolUseBlock{ID: subToolUseID, Name: "Subagent", Input: json.RawMessage(`{"agent":"reviewer","prompt":"audit the diff"}`)},
					&content.ToolUseBlock{ID: bashToolUseID, Name: "Bash", Input: json.RawMessage(bashInput)},
				},
			}},
			&content.ToolResultMessage{
				Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: "reviewer: the diff looks correct"}}},
				ToolUseID: subToolUseID,
			},
			&content.ToolResultMessage{
				Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: "ok\nPASS"}}},
				ToolUseID: bashToolUseID,
			},
		},
	}
	// 11. primary TurnDone.
	primaryTurnDone := event.TurnDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: primaryLid, TurnID: primaryTid},
			EventID:     seedUUID(0xFA),
			CreatedAt:   at(10),
		},
		TurnIndex: 1,
	}

	// Append in real runtime order: child lifecycle + gate/approval all precede the
	// primary StepDone that carries their tool-uses.
	appendEvent(t, j, ctx, sessionStarted)
	appendEvent(t, j, ctx, primaryLoop)
	appendEvent(t, j, ctx, primaryTurnStarted)
	appendEvent(t, j, ctx, childLoop)
	appendEvent(t, j, ctx, childTurnStarted)
	appendEvent(t, j, ctx, childStepDone)
	appendEvent(t, j, ctx, childTurnDone)
	appendEvent(t, j, ctx, permRequested)
	appendCommand(t, j, ctx, sid, primaryLid, approve)
	appendEvent(t, j, ctx, primaryStepDone)
	appendEvent(t, j, ctx, primaryTurnDone)

	// Run the export pipeline exactly as the TUI action does.
	store := mustObjectStore(t, js, sid)
	rr := journal.NewRecordReplayer(js, store)
	src := journalsource.Open(rr, journal.ReplayRequest{
		SessionID: sid,
		LoopID:    uuid.UUID{}, // ALL loops — primary AND the nested reviewer
		From:      journal.Beginning(),
		Follow:    false,
	})
	resolver := primaryPromptResolver{primary: primaryLid, text: primaryPrompt}

	sess, warnings, err := transcript.Reconstruct(ctx, src, resolver)
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if sess == nil || sess.Root == nil {
		t.Fatalf("Reconstruct: nil session/root: %+v", sess)
	}

	var buf bytes.Buffer
	if err := html.Render(&buf, sess); err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Write the rendered transcript to a file on disk (t.TempDir), then read it back so
	// every assertion below is against the persisted artifact, not just the in-memory buf.
	outPath := filepath.Join(t.TempDir(), sid.String()+".html")
	if err := os.WriteFile(outPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", outPath, err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", outPath, err)
	}
	if len(got) == 0 {
		t.Fatal("written transcript file is empty")
	}
	if !utf8.Valid(got) {
		t.Fatal("written transcript file is not valid UTF-8")
	}
	page := string(got)

	// The child-loop degradation is the deterministic, audit-load-bearing warning: the
	// reviewer's transient prompt is not retained, so the primary-only resolver degrades
	// it to a per-loop, AgentName-identified warning. It is the ONLY warning this clean
	// session produces (no orphan child, no leftover gate).
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want exactly 1 (the child degradation); got %+v", len(warnings), warnings)
	}
	if w := warnings[0].Text; !strings.Contains(w, "system prompt unavailable for loop") ||
		!strings.Contains(w, "(reviewer)") || !strings.Contains(w, childLid.String()) {
		t.Errorf("warning[0] = %q, want it to name the child loop id %s and (reviewer)", w, childLid)
	}

	// Substring assertions over the rendered, on-disk bytes. present=false asserts ABSENCE
	// (the XSS / self-contained guards).
	checks := []struct {
		name    string
		substr  string
		present bool
	}{
		{"user message text", userText, true},
		{"AI message prose", "delegating a structured code check", true},
		{"Bash card Approved verb", "Approved ✓", true},
		{"gate chip — you approved at session scope", "You approved · session", true},
		{"nested subagent at depth 1", `data-depth="1"`, true},
		{"reviewer agent name rendered as text", "reviewer", true},
		{"resolved primary system prompt", primaryPrompt, true},
		{"child degradation warning surfaced", "system prompt unavailable for loop", true},
		{"reconstruction notes section", "Reconstruction notes", true},
		{"counts include 2 tools", "2 tools", true},
		{"counts include 1 gate", "1 gates", true},

		// Self-contained: no external assets (opens offline).
		{"no external link tag", "<link ", false},
		{`no external src="http`, `src="http`, false},
		{`no external href="http`, `href="http`, false},

		// XSS: message markup is inert — no live <script> survived from message text.
		{"no live script-alert from message", "<script>alert", false},
	}
	for _, c := range checks {
		got := strings.Contains(page, c.substr)
		if got != c.present {
			t.Errorf("%s: Contains(%q) = %v, want %v", c.name, c.substr, got, c.present)
		}
	}

	// Structurally confirm the nested reviewer subagent reconstructed (not just rendered):
	// the primary's Bash tool call carries the approved gate, and the Subagent call carries
	// the reviewer child loop.
	if len(sess.Root.Turns) != 1 || len(sess.Root.Turns[0].Steps) != 1 {
		t.Fatalf("root shape = %d turns; want 1 turn / 1 step", len(sess.Root.Turns))
	}
	step := sess.Root.Turns[0].Steps[0]
	var bash, sub *transcript.ToolCall
	for _, tc := range step.Tools {
		switch tc.Name {
		case "Bash":
			bash = tc
		case "Subagent":
			sub = tc
		}
	}
	if bash == nil || bash.Gate == nil || bash.Gate.Decision != transcript.DecisionApproved {
		t.Errorf("Bash tool call gate = %+v, want an Approved gate bound by name", bash)
	}
	if bash != nil && bash.Gate != nil && bash.Gate.Scope != tool.ScopeSession {
		t.Errorf("Bash gate scope = %v, want ScopeSession", bash.Gate.Scope)
	}
	if sub == nil || sub.Child == nil || sub.Child.AgentName != "reviewer" {
		t.Errorf("Subagent tool call child = %+v, want a reviewer child loop", sub)
	}
}
