package html

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/transcript"
	"github.com/looprig/harness/pkg/uuid"
)

// update regenerates the golden files when set: go test -run TestRenderMinimal -update.
var update = flag.Bool("update", false, "update golden files")

// fixedSessionID is a stable UUID so the golden is byte-deterministic.
var fixedSessionID = uuid.UUID{
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
}

// childLoopID is a stable UUID for the nested subagent loop, distinct from
// fixedSessionID, so the full golden is byte-deterministic.
var childLoopID = uuid.UUID{
	0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89,
	0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89,
}

// minimalSession builds the tiny model Task 7 renders: one user turn with one AI
// step, no tools/gates/subagents. Every timestamp is a fixed time.Date so the
// rendered golden is byte-stable without normalization (the renderer never reads
// a clock).
func minimalSession() *transcript.Session {
	started := time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)
	ended := time.Date(2026, 6, 28, 10, 5, 0, 0, time.UTC)
	exported := time.Date(2026, 6, 28, 10, 6, 0, 0, time.UTC)
	userAt := time.Date(2026, 6, 28, 10, 0, 1, 0, time.UTC)
	aiAt := time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC)

	return &transcript.Session{
		SessionID: fixedSessionID,
		Title:     "Minimal transcript",
		Config: transcript.Config{
			ModelID:           "claude-opus-4-8",
			AgentKind:         "operator",
			PermissionPosture: "ask",
			SystemPromptRev:   "rev-1",
		},
		StartedAt:  started,
		EndedAt:    ended,
		ExportedAt: exported,
		Root: &transcript.Loop{
			LoopID:    fixedSessionID,
			AgentName: "operator",
			StartedAt: started,
			Turns: []*transcript.Turn{
				{
					Index:     1,
					StartedAt: userAt,
					EndedAt:   aiAt,
					Outcome:   transcript.OutcomeDone,
					User: &transcript.Message{
						Role:   content.RoleUser,
						At:     userAt,
						Blocks: []content.Block{&content.TextBlock{Text: "Run the tests please."}},
					},
					Steps: []*transcript.Step{
						{
							AI: &transcript.Message{
								Role: content.RoleAssistant,
								At:   aiAt,
								Blocks: []content.Block{
									&content.TextBlock{Text: "## Done\n\nRan with `go test ./...` and all passed."},
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestRenderMinimal(t *testing.T) {
	t.Parallel()

	const golden = "testdata/minimal.golden.html"

	var buf bytes.Buffer
	if err := Render(&buf, minimalSession()); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	got := buf.Bytes()

	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", golden, err)
		}
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden %s: %v", golden, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Render() output != golden %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}

// TestRenderDeterministic proves the renderer reads no clock and iterates no maps:
// rendering the same model twice yields byte-identical output.
func TestRenderDeterministic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    *transcript.Session
	}{
		{name: "minimal session", s: minimalSession()},
		{name: "full session", s: fullSession()},
		{name: "empty session (nil root)", s: &transcript.Session{SessionID: fixedSessionID, Title: "Empty"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var a, b bytes.Buffer
			if err := Render(&a, tt.s); err != nil {
				t.Fatalf("Render() first pass error = %v", err)
			}
			if err := Render(&b, tt.s); err != nil {
				t.Fatalf("Render() second pass error = %v", err)
			}
			if !bytes.Equal(a.Bytes(), b.Bytes()) {
				t.Errorf("Render() not deterministic across two passes")
			}
		})
	}
}

func TestRenderTitleFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		s        *transcript.Session
		required []string
	}{
		{
			name: "empty title falls back to short session id",
			s:    &transcript.Session{SessionID: fixedSessionID},
			required: []string{
				"<title>Session 01234567</title>",
				`<h1 class="session-title">Session 01234567</h1>`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := Render(&buf, tt.s); err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			got := buf.String()
			for _, sub := range tt.required {
				if !strings.Contains(got, sub) {
					t.Errorf("Render() output missing %q\n%s", sub, got)
				}
			}
		})
	}
}

// failWriter fails every Write to exercise the RenderError path.
type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

// TestRenderError covers the typed-error contract: a write failure surfaces as a
// *RenderError whose Unwrap returns the cause.
func TestRenderError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("disk full")
	tests := []struct {
		name      string
		w         failWriter
		wantCause error
	}{
		{name: "write failure wraps cause", w: failWriter{err: sentinel}, wantCause: sentinel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Render(tt.w, minimalSession())
			if err == nil {
				t.Fatalf("Render() error = nil, want *RenderError")
			}
			var re *RenderError
			if !errors.As(err, &re) {
				t.Fatalf("Render() error = %T, want *RenderError", err)
			}
			if !errors.Is(err, tt.wantCause) {
				t.Errorf("errors.Is(err, cause) = false, want true (err=%v)", err)
			}
		})
	}
}

// TestRenderMarkdown proves the goldmark seam renders CommonMark structure and
// that the GFM extension is enabled: headings, lists and inline code render, and
// GFM-specific strikethrough (~~…~~ → <del>) and bare-URL autolinking work. Plain
// goldmark (no GFM) would leave ~~strike~~ and the bare URL as literal text, so
// these rows fail until extension.GFM is wired in.
func TestRenderMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		contains  []string
		wantEmpty bool
	}{
		{
			name:      "empty input early return",
			in:        "",
			wantEmpty: true,
		},
		{
			name:     "heading list and inline code",
			in:       "# Title\n\n- a\n- b\n\n`code`",
			contains: []string{"<h1", "<ul>", "<li>a</li>", "<li>b</li>", "<code>code</code>"},
		},
		{
			name:     "gfm strikethrough",
			in:       "~~strike~~",
			contains: []string{"<del>strike</del>"},
		},
		{
			name:     "gfm autolink of bare url",
			in:       "see https://example.com now",
			contains: []string{`<a href="https://example.com">https://example.com</a>`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := renderMarkdown(tt.in)
			if err != nil {
				t.Fatalf("renderMarkdown(%q) error = %v", tt.in, err)
			}
			out := string(got)
			if tt.wantEmpty && out != "" {
				t.Errorf("renderMarkdown(%q) = %q, want empty (early-return path)", tt.in, out)
			}
			for _, sub := range tt.contains {
				if !strings.Contains(out, sub) {
					t.Errorf("renderMarkdown(%q) = %q, want substring %q", tt.in, out, sub)
				}
			}
		})
	}
}

// TestRenderMarkdownXSS is the security core (Decision 11): raw HTML in message
// text must never survive as live markup. goldmark runs with raw-HTML passthrough
// OFF, so a raw tag is omitted (replaced by an HTML comment) and a tag inside a
// code span/fence is escaped to &lt;…&gt;. Each row scans the rendered bytes for
// live attack vectors and confirms they are inert. Note: ` onload=` is forbidden
// only for the raw-HTML rows (where the whole tag is dropped); in the code-fence
// row it legitimately appears as escaped, inert text, so only the live `<svg`
// opener is forbidden there.
func TestRenderMarkdownXSS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		forbidden []string // must NOT appear (case-insensitive): live vectors
		required  []string // must appear (exact case): proof of neutralization
	}{
		{
			name:      "user script tag omitted",
			in:        "<script>alert(1)</script>",
			forbidden: []string{"<script", "alert(1)", "<svg", "<img", " onerror=", " onload="},
			required:  []string{"<!-- raw HTML omitted -->"},
		},
		{
			name:      "ai closing-script then img onerror omitted",
			in:        "</script><img onerror=alert(1) src=x>",
			forbidden: []string{"<script", "<img", " onerror=", "alert(1)"},
			required:  []string{"<!-- raw HTML omitted -->"},
		},
		{
			name:      "raw svg onload omitted",
			in:        "<svg onload=alert(1)>",
			forbidden: []string{"<svg", " onload=", "alert(1)"},
			required:  []string{"<!-- raw HTML omitted -->"},
		},
		{
			name:      "svg in code fence is escaped not live",
			in:        "```\n<svg onload=alert(1)>\n```",
			forbidden: []string{"<svg", "<script"},
			required:  []string{"&lt;svg onload=alert(1)&gt;"},
		},
		{
			name:      "javascript url scheme filtered",
			in:        "[click](javascript:alert(1))",
			forbidden: []string{"javascript:", "alert(1)"},
			// Lock the neutralized shape: the dangerous scheme is stripped to an
			// empty href, the link text survives. A future goldmark upgrade that
			// changed dangerous-URL handling could not silently pass this row.
			required: []string{`href=""`, `>click</a>`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := renderMarkdown(tt.in)
			if err != nil {
				t.Fatalf("renderMarkdown(%q) error = %v", tt.in, err)
			}
			out := string(got)
			low := strings.ToLower(out)
			for _, bad := range tt.forbidden {
				if strings.Contains(low, strings.ToLower(bad)) {
					t.Errorf("renderMarkdown(%q) = %q\nmust NOT contain live vector %q", tt.in, out, bad)
				}
			}
			for _, want := range tt.required {
				if !strings.Contains(out, want) {
					t.Errorf("renderMarkdown(%q) = %q\nwant neutralized marker %q", tt.in, out, want)
				}
			}
		})
	}
}

// TestRenderXSSEndToEnd renders a full page whose user and AI messages carry XSS
// payloads, proving the payload's signature never reaches the output as live
// markup. The assertions key on tokens unique to the payload (alert(1), onerror=,
// <img>, <svg>) that the renderer's own <style>/<script>/template chrome never
// contains, so they isolate injected markup from the page's legitimate scripting.
func TestRenderXSSEndToEnd(t *testing.T) {
	t.Parallel()

	s := minimalSession()
	s.Root.Turns[0].User.Blocks = []content.Block{&content.TextBlock{Text: "<script>alert(1)</script>"}}
	s.Root.Turns[0].Steps[0].AI.Blocks = []content.Block{&content.TextBlock{Text: "</script><img onerror=alert(1) src=x>"}}

	var buf bytes.Buffer
	if err := Render(&buf, s); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	low := strings.ToLower(buf.String())
	for _, bad := range []string{"alert(1)", " onerror=", " onload=", "<img", "<svg"} {
		if strings.Contains(low, strings.ToLower(bad)) {
			t.Errorf("full page contains live XSS vector %q\n%s", bad, buf.String())
		}
	}
}

// fullSession builds the model Task 9 renders: it exercises every layout feature —
// a collapsed system prompt, a user message, an AI message with a thinking block,
// three tool cards (an approved Bash, a denied EditFile, and a Subagent whose
// Child loop nests inline), user-action gate chips (approved/denied/answered), and
// session Notices plus reconstruction Warnings. Every timestamp is a fixed
// time.Date so the golden is byte-stable without normalization.
func fullSession() *transcript.Session {
	started := time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)
	ended := time.Date(2026, 6, 28, 10, 5, 0, 0, time.UTC)
	exported := time.Date(2026, 6, 28, 10, 6, 0, 0, time.UTC)
	userAt := time.Date(2026, 6, 28, 10, 0, 1, 0, time.UTC)
	aiAt := time.Date(2026, 6, 28, 10, 0, 5, 0, time.UTC)

	bashGate := &transcript.GateAction{
		Kind:      transcript.GateKindPermission,
		Decision:  transcript.DecisionApproved,
		Scope:     tool.ScopeSession,
		ToolName:  "Bash",
		ToolUseID: "tu-bash",
		OpenedAt:  time.Date(2026, 6, 28, 10, 0, 2, 0, time.UTC),
		DecidedAt: time.Date(2026, 6, 28, 10, 0, 3, 0, time.UTC),
	}
	editGate := &transcript.GateAction{
		Kind:      transcript.GateKindPermission,
		Decision:  transcript.DecisionDenied,
		ToolName:  "EditFile",
		ToolUseID: "tu-edit",
		OpenedAt:  time.Date(2026, 6, 28, 10, 0, 6, 0, time.UTC),
		DecidedAt: time.Date(2026, 6, 28, 10, 0, 7, 0, time.UTC),
	}
	askGate := &transcript.GateAction{
		Kind:      transcript.GateKindAskUser,
		Decision:  transcript.DecisionAnswered,
		Question:  "Which environment?",
		Choices:   []string{"prod", "staging"},
		Answer:    "staging",
		OpenedAt:  time.Date(2026, 6, 28, 10, 0, 8, 0, time.UTC),
		DecidedAt: time.Date(2026, 6, 28, 10, 0, 9, 0, time.UTC),
	}

	child := &transcript.Loop{
		LoopID:          childLoopID,
		AgentName:       "reviewer",
		ParentToolUseID: "tu-sub",
		StartedAt:       time.Date(2026, 6, 28, 10, 0, 10, 0, time.UTC),
		Turns: []*transcript.Turn{
			{
				Index:     1,
				StartedAt: time.Date(2026, 6, 28, 10, 0, 10, 0, time.UTC),
				EndedAt:   time.Date(2026, 6, 28, 10, 0, 12, 0, time.UTC),
				Outcome:   transcript.OutcomeDone,
				User: &transcript.Message{
					Role:   content.RoleUser,
					At:     time.Date(2026, 6, 28, 10, 0, 10, 0, time.UTC),
					Blocks: []content.Block{&content.TextBlock{Text: "Review the test output."}},
				},
				Steps: []*transcript.Step{
					{
						AI: &transcript.Message{
							Role:   content.RoleAssistant,
							At:     time.Date(2026, 6, 28, 10, 0, 11, 0, time.UTC),
							Blocks: []content.Block{&content.TextBlock{Text: "Looks good. No issues found."}},
						},
					},
				},
			},
		},
	}

	bashTool := &transcript.ToolCall{
		ToolUseID: "tu-bash",
		Name:      "Bash",
		Input:     json.RawMessage(`{"command":"go test ./..."}`),
		Result:    []content.Block{&content.TextBlock{Text: "ok\nall tests passed"}},
		At:        aiAt,
		Gate:      bashGate,
	}
	editTool := &transcript.ToolCall{
		ToolUseID: "tu-edit",
		Name:      "EditFile",
		Input:     json.RawMessage(`{"path":"main.go","old":"a","new":"b"}`),
		Result:    []content.Block{&content.TextBlock{Text: "permission denied by user"}},
		IsError:   true,
		At:        aiAt,
		Gate:      editGate,
	}
	subTool := &transcript.ToolCall{
		ToolUseID: "tu-sub",
		Name:      "Subagent",
		Input:     json.RawMessage(`{"agent":"reviewer","task":"review diff"}`),
		Result:    []content.Block{&content.TextBlock{Text: "review complete: no issues"}},
		At:        aiAt,
		Child:     child,
	}

	return &transcript.Session{
		SessionID: fixedSessionID,
		Title:     "Full transcript",
		Config: transcript.Config{
			ModelID:           "claude-opus-4-8",
			AgentKind:         "operator",
			PermissionPosture: "ask",
			SystemPromptRev:   "rev-1",
		},
		StartedAt:  started,
		EndedAt:    ended,
		ExportedAt: exported,
		Root: &transcript.Loop{
			LoopID:       fixedSessionID,
			AgentName:    "operator",
			SystemPrompt: "You are a helpful operator. Run tests and review the diff.",
			StartedAt:    started,
			Turns: []*transcript.Turn{
				{
					Index:     1,
					StartedAt: userAt,
					EndedAt:   aiAt,
					Outcome:   transcript.OutcomeDone,
					User: &transcript.Message{
						Role:   content.RoleUser,
						At:     userAt,
						Blocks: []content.Block{&content.TextBlock{Text: "Please run the tests, then have a reviewer check the diff."}},
					},
					Steps: []*transcript.Step{
						{
							AI: &transcript.Message{
								Role: content.RoleAssistant,
								At:   aiAt,
								Blocks: []content.Block{
									&content.TextBlock{Text: "I'll run the tests and spin up a **reviewer**."},
									&content.ThinkingBlock{Thinking: "First confirm the build, then run the full suite."},
								},
							},
							Tools: []*transcript.ToolCall{bashTool, editTool, subTool},
							Gates: []*transcript.GateAction{bashGate, editGate, askGate},
						},
					},
				},
			},
		},
		Notices: []transcript.Notice{
			{Kind: transcript.NoticeRestoreStarted, Text: "restoring session from journal", At: time.Date(2026, 6, 28, 9, 59, 58, 0, time.UTC)},
			{Kind: transcript.NoticeRestoreDone, Text: "session restored", At: time.Date(2026, 6, 28, 9, 59, 59, 0, time.UTC)},
			{Kind: transcript.NoticeSessionIdle, Text: "session went idle", At: time.Date(2026, 6, 28, 10, 4, 0, 0, time.UTC)},
			{Kind: transcript.NoticeSessionStopped, Text: "session stopped", At: ended},
		},
		Warnings: []transcript.Warning{
			{Text: "system prompt unavailable for loop " + childLoopID.String() + " (reviewer) (rev rev-1)", At: started},
		},
	}
}

// TestRenderFull renders the every-feature model and asserts both the structural
// markers for each Decision-7 feature and byte-equality against the golden file
// (regenerated with -update). Timestamps are fixed in the model, so no
// normalization is needed; a drift in any feature's markup fails the golden.
func TestRenderFull(t *testing.T) {
	t.Parallel()

	const golden = "testdata/full.golden.html"

	var buf bytes.Buffer
	if err := Render(&buf, fullSession()); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	got := buf.Bytes()
	out := string(got)

	wantContains := []string{
		// header: session id, model, agent kind, counts
		fixedSessionID.String(),
		"claude-opus-4-8",
		"operator",
		"2 turns",
		"3 tools",
		"3 gates",
		// system prompt collapsed block
		`class="system-prompt"`,
		"You are a helpful operator.",
		// user message: accent bar + timestamp
		`class="accent-bar"`,
		"▌",
		"10:00:01",
		// AI message: collapsible, expanded by default, lime bullet + agent name
		`<details class="message ai-message" open>`,
		`class="ai-dot"`,
		"●",
		// thinking: collapsible, collapsed by default
		`<details class="thinking">`,
		"First confirm the build",
		// tool cards: name + decision verbs + pretty input + result
		`class="tool-card`,
		"Bash",
		"Approved ✓",
		"EditFile",
		"Denied ✗",
		"Subagent",
		"command",
		"all tests passed",
		// user-action chips — every chip carries its DecidedAt time; the answered
		// AskUser chip surfaces the captured question + answer (quotes escape to
		// &#34;, so we assert the unquoted tokens).
		`class="gate-chip`,
		"You approved · session · 10:00:03",
		"You denied · 10:00:07",
		"You answered · 10:00:09",
		"Which environment?",
		"staging",
		// nested subagent loop, indented, data-depth=1
		`data-depth="1"`,
		"reviewer",
		`class="subagent"`,
		// notices section in timeline order
		`class="notices"`,
		"session restored",
		"session stopped",
		// reconstruction warnings section
		`class="warnings"`,
		"system prompt unavailable",
		// toolbar controls
		`id="collapse-all"`,
		`id="expand-all"`,
		`id="jump-to-top"`,
	}
	for _, sub := range wantContains {
		if !strings.Contains(out, sub) {
			t.Errorf("Render() output missing marker %q", sub)
		}
	}

	// Notices render in timeline order: restore-done before session-stopped.
	if i, j := strings.Index(out, "session restored"), strings.Index(out, "session stopped"); i < 0 || j < 0 || i > j {
		t.Errorf("notices out of order: restored at %d, stopped at %d", i, j)
	}

	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", golden, err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden %s: %v", golden, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Render() output != golden %s\n--- got ---\n%s", golden, out)
	}
}

// TestRenderToolOutputXSS proves tool Input/Result flow through html/template
// auto-escaping — NOT goldmark — so injected markup renders as inert escaped text.
// The forbidden list keys on payload-unique LIVE tag openers (`<svg`, `<img`,
// `<iframe`) that the page's own <style>/<script> chrome never emits: with every
// `<` escaped to `&lt;`, none can appear. (We cannot forbid `<script` because the
// page legitimately carries its own inline app.js <script> element — Task 8's
// end-to-end XSS test keys on the same payload-unique tokens for the same reason.)
// The escaped-form `required` markers prove the neutralization, including that the
// injected <script> tag survives only as inert escaped text. The bare attribute
// strings (` onload=`, ` onerror=`) legitimately appear as inert text INSIDE the
// escaped tags, so they are not forbidden.
func TestRenderToolOutputXSS(t *testing.T) {
	t.Parallel()

	s := fullSession()
	tc := s.Root.Turns[0].Steps[0].Tools[0]
	tc.Result = []content.Block{
		&content.TextBlock{Text: "<script>alert(1)</script>"},
		&content.TextBlock{Text: "<svg onload=alert(1)>"},
	}
	tc.Input = json.RawMessage(`{"x":"<img onerror=alert(1) src=x>"}`)

	var buf bytes.Buffer
	if err := Render(&buf, s); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	out := buf.String()
	low := strings.ToLower(out)

	for _, bad := range []string{"<svg", "<img", "<iframe"} {
		if strings.Contains(low, bad) {
			t.Errorf("tool output produced LIVE tag opener %q\n%s", bad, out)
		}
	}
	for _, want := range []string{"&lt;script&gt;", "&lt;svg onload=", "&lt;img onerror="} {
		if !strings.Contains(out, want) {
			t.Errorf("tool output missing escaped (inert) marker %q\n%s", want, out)
		}
	}
}

// TestRenderToolResultElision proves an oversized tool Result is capped with a
// "… N bytes elided" note and that the elided tail never reaches the output.
func TestRenderToolResultElision(t *testing.T) {
	t.Parallel()

	const tail = "TAILMARKER_SHOULD_BE_ELIDED"
	big := strings.Repeat("A", 20_000) + tail

	s := fullSession()
	s.Root.Turns[0].Steps[0].Tools[0].Result = []content.Block{&content.TextBlock{Text: big}}

	var buf bytes.Buffer
	if err := Render(&buf, s); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "bytes elided") {
		t.Errorf("oversized result missing elision note")
	}
	if strings.Contains(out, tail) {
		t.Errorf("elided tail leaked into output")
	}
}
