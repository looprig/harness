package html

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// update regenerates the golden files when set: go test -run TestRenderMinimal -update.
var update = flag.Bool("update", false, "update golden files")

// fixedSessionID is a stable UUID so the golden is byte-deterministic.
var fixedSessionID = uuid.UUID{
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
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
