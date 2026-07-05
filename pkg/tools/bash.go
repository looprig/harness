package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// bash.go implements the Bash tool: it runs a single shell command via `sh -c`
// inside a workspace-contained working directory, with a bounded timeout and a
// capped combined-output capture (design §4b, "Bash security model").
//
// DELIBERATE, DOCUMENTED EXCEPTION to CLAUDE.md's "never pass user input to
// exec.Command as a shell string" rule: a coding agent genuinely needs shell
// features (pipes, globs, &&, redirects) that an argv list cannot express, so the
// command is handed to `sh -c`. The boundary is NOT this argv shape and NOT the
// advisory DeniedBashPrefixes (trivially bypassable) — it is the PERMISSION GATE:
// Bash defaults to Ask, so a human approves each command before it runs. OS-level
// sandboxing (the real boundary) is out of scope and is the prerequisite for ever
// auto-approving Bash broadly. This exception is recorded in CLAUDE.md.
//
// Failure model: a non-zero EXIT CODE is a NORMAL tool result (the model reads
// stderr + the code), NOT a Go error. A timeout is a tool-result "error: command
// timed out". Only a structural surprise (impossible to start sh) is reported as a
// tool-result error; the tool never returns a Go error.

// bashToolName is the EXACT tool name classifyTool keys on for the bash class —
// it MUST equal "Bash" (check.go's toolBash).
const bashToolName = toolBash

// maxBashTimeout is the hard ceiling on a Bash command's wall-clock runtime. A
// caller-supplied timeout is clamped to this; it bounds resource use so a runaway
// command cannot block the agent indefinitely (CLAUDE.md: no unbounded I/O).
const maxBashTimeout = 120 * time.Second

// defaultBashTimeout is used when the caller omits (or supplies a non-positive)
// timeout. It is generous enough for typical build/test commands but bounded.
const defaultBashTimeout = 30 * time.Second

// maxBashOutputBytes caps the COMBINED stdout+stderr capture so a chatty command
// cannot exhaust memory or flood the model context. Output beyond this is dropped
// and a truncation notice is appended.
const maxBashOutputBytes = 32 * 1024 // 32 KiB

// bashShell and bashShellFlag are the interpreter and flag for the documented
// `sh -c <command>` exception. `sh` is the POSIX shell present on the host.
const (
	bashShell     = "sh"
	bashShellFlag = "-c"
)

// bashSchema is the JSON Schema for Bash's argument object. The field names
// (command/workdir/timeout) are the boundary-extraction contract shared with
// check.go (which parses "command" and "workdir").
const bashSchema = `{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The shell command to run via 'sh -c'. May use pipes, globs, redirects, and '&&'."},
    "workdir": {"type": "string", "description": "Workspace-relative working directory for the command (optional; defaults to the workspace root)."},
    "timeout": {"type": "integer", "minimum": 1, "maximum": 120, "description": "Maximum runtime in seconds (optional; default 30, hard cap 120)."}
  },
  "required": ["command"]
}`

const bashDesc = "Run a single shell command via 'sh -c' inside the workspace. Supports pipes, globs, redirects, and '&&'. Combined stdout+stderr is captured (capped at 32 KiB) and the exit code is reported. The working directory is confined to the workspace; runtime is bounded (default 30s, max 120s). Requires approval before each command."

// bashArgs is the typed decode of Bash's untrusted argsJSON.
type bashArgs struct {
	Command string `json:"command"`
	Workdir string `json:"workdir"`
	Timeout int    `json:"timeout"`
}

// Bash runs a single shell command in a workspace-contained directory. It depends
// only on the workspace root (least privilege); the advisory DeniedBashPrefixes
// gate is the PermissionChecker's concern, not the tool's. runner is the OPTIONAL
// confined-execution seam (§10.1): nil means direct `sh -c` execution (the
// bare-harness default), non-nil routes the command through the injected runner
// (e.g. the sandbox Executor) instead.
type Bash struct {
	root   string
	runner tool.CommandRunner
}

// BashOption configures a Bash at construction (functional-options pattern).
type BashOption func(*Bash)

// WithRunner injects a confined command runner. When set, InvokableRun routes the
// command through r.RunCommand instead of the direct `sh -c` path. A nil runner
// (the default) preserves the exact bare-harness direct-execution behavior.
func WithRunner(r tool.CommandRunner) BashOption {
	return func(b *Bash) { b.runner = r }
}

// NewBash constructs a Bash tool bound to the workspace root. With no options the
// runner is nil (direct execution); WithRunner injects a confined runner.
func NewBash(root string, opts ...BashOption) *Bash {
	b := &Bash{root: root}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Info returns Bash's self-description. Name MUST equal "Bash".
func (b *Bash) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   bashToolName,
		Desc:   bashDesc,
		Schema: json.RawMessage(bashSchema),
	}, nil
}

// AuditSummary returns the command itself — it is exactly what the user approves
// at the gate, so it is the right (and only) redacted summary. No secrets are
// added beyond the command the user already sees. An unparseable args document
// yields a generic summary.
func (b *Bash) AuditSummary(argsJSON string) string {
	var a bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Command == "" {
		return "Bash (unparsable args)"
	}
	return "Bash: " + a.Command
}

// BuildRequest derives the approval prompt: the command string (which doubles as
// the persisted exact-command Match). An unparseable args document or an empty
// command is a typed error so the runner treats the call as invalid.
func (b *Bash) BuildRequest(argsJSON string, _ tool.PreparedArtifact) (tool.PermissionRequest, error) {
	var a bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return nil, &bashError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if a.Command == "" {
		return nil, &bashError{reason: "a non-empty 'command' is required"}
	}
	return tool.BashRequest{Command: a.Command}, nil
}

// InvokableRun runs the command and returns its combined output + exit code as a
// tool result. A non-zero exit is a normal result; a timeout, an escaping
// workdir, or an unparseable args document is a tool-result error string. It
// never returns a Go error.
func (b *Bash) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var a bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	if a.Command == "" {
		return tool.TextResult("error: a non-empty 'command' is required"), nil
	}

	// Resolve the working directory under the workspace root (default: the root).
	// An escape is rejected (defense in depth; the gate also contains the workdir).
	dir := b.root
	if a.Workdir != "" {
		resolved, err := containedPath(b.root, a.Workdir)
		if err != nil {
			return tool.TextResult("error: workdir is outside the workspace: " + a.Workdir), nil
		}
		dir = resolved
	}

	// Bound the command's runtime: clamp the caller timeout into (0, 120s].
	timeout := clampBashTimeout(a.Timeout)
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		out      string
		exitCode int
		timedOut bool
		runErr   error
	)
	if b.runner != nil {
		// Confined path: the injected runner (e.g. the sandbox Executor) folds a
		// timeout/cancel into err (it returns ctx.Err()); adapt its byte output +
		// error into the (output, exitCode, timedOut, startErr) shape below.
		outBytes, ec, err := b.runner.RunCommand(ctx2, dir, a.Command)
		out, exitCode = string(outBytes), ec
		switch {
		case err == nil:
			// success: timedOut/runErr stay zero.
		case errors.Is(err, context.DeadlineExceeded) || ctx2.Err() == context.DeadlineExceeded:
			timedOut = true
		default:
			runErr = err
		}
	} else {
		out, exitCode, timedOut, runErr = runShellCommand(ctx2, dir, a.Command)
	}
	if timedOut {
		return tool.TextResult("error: command timed out after " + timeout.String()), nil
	}
	if runErr != nil {
		// sh could not be started (not an exit-code situation). Surface as a
		// tool-result error, not a Go error.
		return tool.TextResult("error: could not run command: " + runErr.Error()), nil
	}
	return tool.TextResult(formatBashResult(out, exitCode)), nil
}

// clampBashTimeout maps a caller-supplied timeout (seconds) into a bounded
// time.Duration: ≤0 → defaultBashTimeout; otherwise min(timeout, maxBashTimeout).
func clampBashTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultBashTimeout
	}
	d := time.Duration(seconds) * time.Second
	if d > maxBashTimeout {
		return maxBashTimeout
	}
	return d
}

// runShellCommand runs `sh -c command` in dir, capturing COMBINED stdout+stderr
// capped at maxBashOutputBytes (the cappedBuffer drops bytes past the cap and
// records the overflow). It returns (output, exitCode, timedOut, startErr):
//   - timedOut is true when ctx's deadline fired (the process was killed);
//   - startErr is non-nil only when sh could not be started (structural);
//   - a non-zero exit code is returned WITHOUT a startErr (a normal result).
func runShellCommand(ctx context.Context, dir, command string) (output string, exitCode int, timedOut bool, startErr error) {
	// #nosec G204 -- DELIBERATE, documented exception (see file header & CLAUDE.md):
	// the Bash tool runs a single human-approved command via `sh -c`; the security
	// boundary is the permission gate, not this argv shape. exec.CommandContext
	// bounds the runtime so the process is killed on timeout.
	cmd := exec.CommandContext(ctx, bashShell, bashShellFlag, command)
	cmd.Dir = dir

	var buf cappedBuffer
	buf.limit = maxBashOutputBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := buf.cappedString()

	// A deadline-exceeded context means the process was killed by the timeout.
	if ctx.Err() == context.DeadlineExceeded {
		return out, 0, true, nil
	}
	if err == nil {
		return out, 0, false, nil
	}
	// A non-zero exit is a normal result, not a start error.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), false, nil
	}
	// Anything else (sh not found, permission denied to exec) is a start error.
	return out, 0, false, err
}

// formatBashResult renders the combined output and exit code into the tool-result
// text. The exit code line is always present so the model can branch on it.
func formatBashResult(output string, exitCode int) string {
	body := output
	if body != "" && body[len(body)-1] != '\n' {
		body += "\n"
	}
	return body + "[exit code: " + strconv.Itoa(exitCode) + "]"
}

// cappedBuffer is an io.Writer that accumulates up to limit bytes and then drops
// the rest, remembering whether anything was dropped. It is used as BOTH the
// stdout and stderr sink so the two streams are interleaved into one capped
// combined buffer (a write past the cap is silently truncated, never panics).
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

// Write appends as many bytes of p as fit under limit, marking truncated if any
// were dropped. It always reports len(p) written (the producer must not see a
// short write / error) so the command keeps running to completion.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		if len(p) > 0 {
			c.truncated = true
		}
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// cappedString returns the captured output, appending a single truncation notice
// line when bytes were dropped past the cap.
func (c *cappedBuffer) cappedString() string {
	s := c.buf.String()
	if c.truncated {
		if s != "" && s[len(s)-1] != '\n' {
			s += "\n"
		}
		s += "[output truncated at " + strconv.Itoa(c.limit) + " bytes]"
	}
	return s
}

// bashError is the typed failure for Bash arg parsing (used by BuildRequest). It
// carries a non-secret reason; InvokableRun maps every failure to a tool-result
// string rather than returning this.
type bashError struct {
	reason string
	cause  error
}

func (e *bashError) Error() string { return e.reason }

func (e *bashError) Unwrap() error { return e.cause }

// compile-time assertions: Bash is an InvokableTool, a PermissionPrompter (Ask),
// and Auditable. It is NOT a WriteTarget (it is not a path-write tool).
var (
	_ tool.InvokableTool      = (*Bash)(nil)
	_ tool.PermissionPrompter = (*Bash)(nil)
	_ tool.Auditable          = (*Bash)(nil)
)
