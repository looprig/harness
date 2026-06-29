package tui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/transcript/html"
	"github.com/ciram-co/looprig/pkg/transcript/journalsource"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// export.go implements the /export command: a DIRECT USER ACTION that snapshots the
// session's journal, reconstructs it, renders a self-contained HTML transcript, and
// writes ~/Downloads/<session-id>.html. Unlike an agent tool call it deliberately does
// NOT pass through the permission gate and writes OUTSIDE the workspace — that is the
// design's documented Decision 9 exception (a human typed /export; there is no untrusted
// model deciding the path). Only the resolved path is ever logged/surfaced, never the
// transcript content (Decision 9). The cmd runs off the update loop and reports an
// exportResultMsg; Screen maps it to an info/warn/error notice.

// exportTimeout bounds the whole reconstruct → render → write pipeline so /export can
// never wedge the update loop on a slow journal replay.
const exportTimeout = 30 * time.Second

// exportFilePerm is the mode of the written transcript file: owner read/write,
// group/world read. The export is a user-facing artifact dropped in ~/Downloads (a
// deliberate out-of-workspace D9 exception), so world-readable 0644 is appropriate —
// unlike the workspace WriteFile tool's owner-only 0600.
const exportFilePerm os.FileMode = 0o644

// exportDirPerm is the mode used when MkdirAll must create the ~/Downloads directory.
const exportDirPerm os.FileMode = 0o755

// downloadsDir is the fixed subdirectory of the user's home the export lands in.
const downloadsDir = "Downloads"

// exportUnavailableNotice is the friendly message shown when ExportSource reports the
// session is not journal-backed (a *journalsource.ExportUnavailableError) — a benign
// advisory, not an error crash.
const exportUnavailableNotice = "export needs a persisted session"

// exportResultMsg reports the outcome of an /export run back to the update loop. On
// success path/turns/tools are set and err is nil; on any failure (unavailable,
// reconstruct, render, or write) err carries the typed cause and the rest are zero. It
// is a tea.Msg.
type exportResultMsg struct {
	path  string
	turns int
	tools int
	err   error
}

// ExportWriteError reports that the rendered transcript could not be written to disk
// (the ~/Downloads target was unwritable, a temp/rename step failed, or the home dir
// could not be resolved). Path is the intended destination; Cause wraps the underlying
// os error. It is errors.As-able and never includes the transcript content (D9).
type ExportWriteError struct {
	Path  string
	Cause error
}

func (e *ExportWriteError) Error() string {
	if e.Cause != nil {
		return "tui: could not write transcript export to " + e.Path + ": " + e.Cause.Error()
	}
	return "tui: could not write transcript export to " + e.Path
}

func (e *ExportWriteError) Unwrap() error { return e.Cause }

// exportCmd returns an async tea.Cmd that snapshots the agent's journal and writes the
// HTML transcript, reporting an exportResultMsg. It mirrors the other bounded commands
// (submitCmd, interruptTurn): the work runs off the update loop under a bounded context
// so /export never blocks the UI thread.
func exportCmd(ctx context.Context, agent Agent) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, exportTimeout)
		defer cancel()
		return runExport(c, agent)
	}
}

// runExport performs the reconstruct → render → atomic-write pipeline and returns the
// typed result. Each stage's failure is surfaced as a typed error on the result (the
// ExportUnavailableError from ExportSource, *transcript.ReconstructError, *html.RenderError,
// or *ExportWriteError). Only the path is ever placed on the result — never content.
func runExport(ctx context.Context, agent Agent) exportResultMsg {
	src, prompts, err := agent.ExportSource(ctx)
	if err != nil {
		// Includes *journalsource.ExportUnavailableError; the handler maps it to a
		// friendly notice rather than an error crash.
		return exportResultMsg{err: err}
	}

	session, _, err := transcript.Reconstruct(ctx, src, prompts)
	if err != nil {
		return exportResultMsg{err: err}
	}
	// The TUI MAY read a clock (only html.Render must not); stamping ExportedAt here keeps
	// the renderer clock-free while dating the document. .UTC() drops the local TZ and
	// monotonic reading — this timestamp is rendered into a portable, shareable HTML file.
	session.ExportedAt = time.Now().UTC()

	var buf bytes.Buffer
	if err := html.Render(&buf, session); err != nil {
		return exportResultMsg{err: err}
	}

	path, err := exportPath(session.SessionID)
	if err != nil {
		return exportResultMsg{err: err}
	}
	if err := writeExportFile(path, buf.Bytes()); err != nil {
		return exportResultMsg{err: err}
	}

	turns, tools := countTurnsTools(session)
	return exportResultMsg{path: path, turns: turns, tools: tools}
}

// exportPath resolves the absolute ~/Downloads/<session-id>.html target. The filename is
// a UUID (no path separators), so the join cannot escape the home dir; filepath.Clean
// canonicalizes the result. A failure to resolve the home dir is a typed ExportWriteError
// (the write can never proceed without a destination).
func exportPath(sessionID uuid.UUID) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &ExportWriteError{Cause: err}
	}
	return filepath.Clean(filepath.Join(home, downloadsDir, sessionID.String()+".html")), nil
}

// writeExportFile writes data to path atomically: it creates ~/Downloads if needed, writes
// to a uniquely-named temp file in the SAME directory (O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW
// @0644, refusing to clobber or follow a pre-planted symlink at the temp name), fsyncs,
// closes, and os.Renames it over the target. The temp file is removed on any post-create
// failure. Every failure is a typed *ExportWriteError carrying the destination path. This
// mirrors pkg/tools/writefile.go's atomicWriteFile; the tui cannot import package tools (a
// higher layer), so the pattern is reproduced here for ~/Downloads at 0644.
func writeExportFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, exportDirPerm); err != nil {
		return &ExportWriteError{Path: path, Cause: err}
	}

	tmp, err := uniqueExportTempPath(dir)
	if err != nil {
		return &ExportWriteError{Path: path, Cause: err}
	}

	// #nosec G304 -- tmp = the destination's parent dir + a crypto/rand suffix.
	// O_EXCL|O_NOFOLLOW refuse to clobber an existing name or follow a pre-planted
	// symlink at the temp name.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, exportFilePerm)
	if err != nil {
		return &ExportWriteError{Path: path, Cause: err}
	}
	if err := writeSyncCloseExport(f, data); err != nil {
		_ = os.Remove(tmp)
		return &ExportWriteError{Path: path, Cause: err}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return &ExportWriteError{Path: path, Cause: err}
	}
	return nil
}

// writeSyncCloseExport writes data to f, fsyncs it durable, and closes it. On a write or
// sync error it closes f best-effort (the temp file is removed by the caller) and returns
// the original error.
func writeSyncCloseExport(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// uniqueExportTempPath returns a never-before-used temp file path in dir using a
// crypto/rand suffix (collision-resistant; the O_EXCL create still guards it). It does
// NOT create the file — the caller opens it O_EXCL|O_NOFOLLOW.
func uniqueExportTempPath(dir string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, ".looprig-export-"+hex.EncodeToString(b[:])+".tmp"), nil
}

// countTurnsTools counts every turn and tool call in the reconstructed session, recursing
// through subagent child loops (a Subagent tool call's ToolCall.Child) so nested loops are
// included in the success-notice tally.
func countTurnsTools(s *transcript.Session) (turns, tools int) {
	if s == nil {
		return 0, 0
	}
	return countLoop(s.Root)
}

// countLoop tallies the turns and tool calls of one loop and, recursively, of any subagent
// loops its tool calls spawned.
func countLoop(loop *transcript.Loop) (turns, tools int) {
	if loop == nil {
		return 0, 0
	}
	for _, turn := range loop.Turns {
		turns++
		for _, step := range turn.Steps {
			for _, tc := range step.Tools {
				tools++
				if tc.Child != nil {
					ct, ctools := countLoop(tc.Child)
					turns += ct
					tools += ctools
				}
			}
		}
	}
	return turns, tools
}

// handleExportResult commits the export outcome as a transcript notice and flushes it to
// scrollback. A *journalsource.ExportUnavailableError is a benign advisory (warn notice,
// no crash); any other error is an out-of-band error notice; success is an info notice
// naming the written path and the turn/tool counts. Only the path is surfaced — never the
// transcript content (D9).
func (m *Screen) handleExportResult(msg exportResultMsg) tea.Cmd {
	if msg.err != nil {
		var unavailable *journalsource.ExportUnavailableError
		if errors.As(msg.err, &unavailable) {
			m.transcript = m.transcript.CommitNotice(noticeWarn, exportUnavailableNotice)
			return m.flush()
		}
		m.transcript = m.transcript.CommitError(msg.err)
		return m.flush()
	}
	m.transcript = m.transcript.CommitNotice(noticeInfo,
		fmt.Sprintf("Exported → %s (%d turns · %d tools)", msg.path, msg.turns, msg.tools))
	return m.flush()
}
