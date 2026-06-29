package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/transcript/journalsource"
	"github.com/ciram-co/looprig/pkg/tui/components"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// the canned-source content markers asserted in the rendered HTML.
const (
	exportUserMarker   = "ping-from-the-user"
	exportAIMarker     = "pong-from-the-assistant"
	exportPromptMarker = "SYSTEM-PROMPT-MARKER"
)

// recordSlice is a slice-backed transcript.RecordSource fake: it yields its records
// in order and returns io.EOF once exhausted (the contract Reconstruct drains).
type recordSlice struct {
	recs []transcript.Record
	i    int
}

func (r *recordSlice) Next(context.Context) (transcript.Record, error) {
	if r.i >= len(r.recs) {
		return nil, io.EOF
	}
	rec := r.recs[r.i]
	r.i++
	return rec, nil
}

// loopPrompts is a map-backed SystemPromptResolver keyed by loop id (absent loop →
// ok=false, the degradation path).
type loopPrompts map[uuid.UUID]string

func (p loopPrompts) SystemPrompt(id uuid.UUID) (string, bool) {
	text, ok := p[id]
	return text, ok
}

// cannedExportSource builds a one-turn/one-step/one-tool session record stream stamped
// onto loop lid within session sid: a user message, an AI message with a Bash tool-use,
// and its tool result. Reconstruct folds it into a Session with exactly 1 turn and 1 tool.
func cannedExportSource(sid, lid uuid.UUID) transcript.RecordSource {
	hdr := func() event.Header {
		return event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}}
	}
	evs := []event.Event{
		event.SessionStarted{Header: hdr(), Config: event.ConfigFingerprint{ModelID: "claude-opus-4-8", AgentKind: "operator"}},
		event.LoopStarted{Header: hdr()},
		event.TurnStarted{Header: hdr(), TurnIndex: 1, Message: &content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: exportUserMarker}},
		}}},
		event.StepDone{Header: hdr(), Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{
					&content.TextBlock{Text: exportAIMarker},
					&content.ToolUseBlock{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
				},
			}},
			&content.ToolResultMessage{
				Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: "ok"}}},
				ToolUseID: "tu1",
			},
		}},
		event.TurnDone{Header: hdr(), TurnIndex: 1},
	}
	recs := make([]transcript.Record, len(evs))
	for i, e := range evs {
		recs[i] = transcript.EventRecord{Event: e}
	}
	return &recordSlice{recs: recs}
}

// TestExportSlashCommandRegistered pins the command-table wiring: /export is a canonical
// slash command (so the completer offers it), isSlashCommand recognizes it, and helpText
// lists it.
func TestExportSlashCommandRegistered(t *testing.T) {
	t.Parallel()

	var found bool
	for _, c := range components.SlashCommands {
		if c.Name == "/export" {
			found = true
		}
	}
	if !found {
		t.Error("/export not in components.SlashCommands")
	}
	if !isSlashCommand("/export") {
		t.Error(`isSlashCommand("/export") = false, want true`)
	}
	if !strings.Contains(helpText(), "/export") {
		t.Errorf("helpText() missing /export; got %q", helpText())
	}
}

// TestExportCmdWritesFile drives the async export command over a fake Agent serving a
// canned record stream + resolver, under a temp HOME, and asserts the written file lands
// at ~/Downloads/<session-id>.html, is valid self-contained UTF-8 HTML carrying the
// user/AI/system-prompt markers, and that the result message reports the path + counts.
func TestExportCmdWritesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sid, lid := callID(0xA1), callID(0xB2)
	agent := &fakeAgent{
		exportSrc:     cannedExportSource(sid, lid),
		exportPrompts: loopPrompts{lid: exportPromptMarker},
	}

	msg, ok := exportCmd(context.Background(), agent)().(exportResultMsg)
	if !ok {
		t.Fatalf("exportCmd produced %T, want exportResultMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("export err = %v, want nil", msg.err)
	}
	if !agent.exportCalled {
		t.Error("ExportSource not called")
	}

	wantPath := filepath.Join(home, "Downloads", sid.String()+".html")
	if msg.path != wantPath {
		t.Errorf("path = %q, want %q", msg.path, wantPath)
	}
	if msg.turns != 1 || msg.tools != 1 {
		t.Errorf("counts = (%d turns, %d tools), want (1, 1)", msg.turns, msg.tools)
	}

	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("written file unreadable: %v", err)
	}
	if !utf8.Valid(data) {
		t.Error("written file is not valid UTF-8")
	}
	out := string(data)
	for _, marker := range []string{"<!DOCTYPE html>", "<html", exportUserMarker, exportAIMarker, exportPromptMarker} {
		if !strings.Contains(out, marker) {
			t.Errorf("HTML missing marker %q", marker)
		}
	}
	// Self-contained: no external assets (inline <style>/<script> only).
	for _, external := range []string{"<link ", `src="http`, `href="http`} {
		if strings.Contains(out, external) {
			t.Errorf("HTML is not self-contained; contains %q", external)
		}
	}
}

// TestExportResultNotices covers the notice mapping in Screen.Update: a successful export
// commits an info notice carrying the path + counts; an *ExportWriteError commits an error
// notice; a *journalsource.ExportUnavailableError commits a friendly warn notice (never an
// error crash). It also drives the write-failure path end-to-end through exportCmd to prove
// the typed ExportWriteError surfaces.
func TestExportResultNotices(t *testing.T) {
	newScreen := func(agent *fakeAgent) Screen {
		return New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	}

	t.Run("success commits an info notice with path and counts", func(t *testing.T) {
		t.Parallel()
		m := newScreen(&fakeAgent{})
		m, _ = updateScreen(t, m, exportResultMsg{path: "/home/u/Downloads/s.html", turns: 2, tools: 3})

		rec := lastCommitted(t, m)
		if rec.Kind != kindNotice || rec.Level != noticeInfo {
			t.Fatalf("committed = (kind %d, level %d), want (kindNotice, noticeInfo)", rec.Kind, rec.Level)
		}
		text := committedText(rec)
		for _, want := range []string{"/home/u/Downloads/s.html", "2 turns", "3 tools"} {
			if !strings.Contains(text, want) {
				t.Errorf("success notice %q missing %q", text, want)
			}
		}
	})

	t.Run("ExportWriteError commits an error notice", func(t *testing.T) {
		t.Parallel()
		m := newScreen(&fakeAgent{})
		writeErr := &ExportWriteError{Path: "/ro/Downloads/s.html", Cause: errors.New("permission denied")}
		m, _ = updateScreen(t, m, exportResultMsg{err: writeErr})

		rec := lastCommitted(t, m)
		if rec.Kind != kindNotice || rec.Level != noticeError {
			t.Fatalf("committed = (kind %d, level %d), want (kindNotice, noticeError)", rec.Kind, rec.Level)
		}
	})

	t.Run("ExportUnavailableError commits a friendly warn notice", func(t *testing.T) {
		t.Parallel()
		m := newScreen(&fakeAgent{})
		m, _ = updateScreen(t, m, exportResultMsg{err: &journalsource.ExportUnavailableError{}})

		rec := lastCommitted(t, m)
		if rec.Kind != kindNotice || rec.Level != noticeWarn {
			t.Fatalf("committed = (kind %d, level %d), want (kindNotice, noticeWarn)", rec.Kind, rec.Level)
		}
	})

	t.Run("unwritable Downloads yields a typed ExportWriteError", func(t *testing.T) {
		// NOTE: no t.Parallel — this subtest uses t.Setenv (forbidden under t.Parallel).
		// Point HOME at a regular file so MkdirAll(home/Downloads) cannot create the dir.
		tmp := t.TempDir()
		badHome := filepath.Join(tmp, "home-is-a-file")
		if err := os.WriteFile(badHome, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed bad home: %v", err)
		}
		t.Setenv("HOME", badHome)

		sid, lid := callID(0xC3), callID(0xD4)
		agent := &fakeAgent{exportSrc: cannedExportSource(sid, lid), exportPrompts: loopPrompts{lid: exportPromptMarker}}

		msg := exportCmd(context.Background(), agent)().(exportResultMsg)
		var writeErr *ExportWriteError
		if !errors.As(msg.err, &writeErr) {
			t.Fatalf("export err = %v, want *ExportWriteError", msg.err)
		}
		if writeErr.Unwrap() == nil {
			t.Error("ExportWriteError should wrap a cause")
		}
	})
}

// TestExportUnavailableWritesNoFile proves a non-persisted session (ExportSource returns
// *journalsource.ExportUnavailableError) takes the friendly path: the cmd surfaces the
// typed error and no file is ever written under HOME.
func TestExportUnavailableWritesNoFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	agent := &fakeAgent{exportErr: &journalsource.ExportUnavailableError{}}
	msg := exportCmd(context.Background(), agent)().(exportResultMsg)

	var unavailable *journalsource.ExportUnavailableError
	if !errors.As(msg.err, &unavailable) {
		t.Fatalf("export err = %v, want *journalsource.ExportUnavailableError", msg.err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "Downloads")); len(entries) != 0 {
		t.Errorf("Downloads has %d entries, want 0 (no file written for an unavailable export)", len(entries))
	}
}
