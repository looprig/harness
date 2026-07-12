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
    "timeout": {"type": "integer", "minimum": 1, "maximum": 120, "description": "Maximum runtime in seconds (optional; default 30, hard cap 120)."},
    "grants": {"type": "array", "items": {"type": "string"}, "description": "Escalation grant tokens received in a PRIOR denial result for this exact command. Attach them verbatim to retry; NEVER invent, guess, or modify a token."}
  },
  "required": ["command"]
}`

const bashDesc = "Run a single shell command via 'sh -c' inside the workspace. Supports pipes, globs, redirects, and '&&'. Combined stdout+stderr is captured (capped at 32 KiB) and the exit code is reported. The working directory is confined to the workspace; runtime is bounded (default 30s, max 120s). Requires approval before each command."

// bashArgs is the typed decode of Bash's untrusted argsJSON.
type bashArgs struct {
	Command string `json:"command"`
	Workdir string `json:"workdir"`
	Timeout int    `json:"timeout"`
	// Grants are opaque escalation tokens the model re-attaches from a prior
	// denial result for THIS command. They are OPAQUE to harness — it never
	// mints or verifies them (the sandbox does); it only carries them to a
	// GrantedRunner. Absent means no grants.
	Grants []string `json:"grants,omitempty"`
}

// BashTool runs a single shell command in a workspace-contained directory. It
// depends on the workspace root and an optional confined-execution runner;
// advisory command policy remains the PermissionChecker's concern. A nil runner
// means direct `sh -c` execution (the bare-harness default), while an invalid
// option or typed-nil runner fails closed through a model-safe error.
type BashTool struct {
	root    string
	runner  tool.CommandRunner
	coord   tool.WorkspaceCoordinator
	obs     tool.WorkspaceObservations
	initErr error
}

// BashOption configures a BashTool at construction (functional-options pattern).
type BashOption func(*BashTool)

// WithRunner injects a confined command runner. When set, InvokableRun routes the
// command through r.RunCommand instead of the direct `sh -c` path. A nil runner
// (the default) preserves the exact bare-harness direct-execution behavior.
func WithRunner(r tool.CommandRunner) BashOption {
	return func(b *BashTool) { b.runner = r }
}

// WithWorkspaceCoordinator binds the session workspace coordinator so a command run
// holds the EXCLUSIVE whole-workspace mutation permit (design §"File-tool optimistic
// concurrency and binding"). A nil or typed-nil coordinator is ignored (the tool runs
// coordinator-free — the standalone/bare path).
func WithWorkspaceCoordinator(coord tool.WorkspaceCoordinator) BashOption {
	return func(b *BashTool) {
		if !nilInterface(coord) {
			b.coord = coord
		}
	}
}

// WithObservations binds the loop's shared file-observation set so a command run
// invalidates it wholesale afterward (the changed paths are unknowable). A nil or
// typed-nil set is ignored (no invalidation).
func WithObservations(obs tool.WorkspaceObservations) BashOption {
	return func(b *BashTool) {
		if !nilInterface(obs) {
			b.obs = obs
		}
	}
}

// NewBash constructs a BashTool bound to the workspace root. With no options, or
// WithRunner(nil), the tool uses direct execution. Invalid options and typed-nil
// runners are retained as initialization errors and fail closed when invoked.
func NewBash(root string, opts ...BashOption) *BashTool {
	config, initErr := resolveBashOptions(opts)
	b := newBash(root, config)
	b.initErr = initErr
	return b
}

type bashConfig struct {
	runner tool.CommandRunner
	coord  tool.WorkspaceCoordinator
	obs    tool.WorkspaceObservations
}

func resolveBashOptions(opts []BashOption) (bashConfig, error) {
	resolved := &BashTool{}
	for _, opt := range opts {
		if opt == nil {
			return bashConfig{}, &DefinitionBuildError{Definition: bashToolName, Dependency: "option"}
		}
		opt(resolved)
	}
	if resolved.runner != nil && nilInterface(resolved.runner) {
		return bashConfig{}, &DefinitionBuildError{Definition: bashToolName, Dependency: "runner"}
	}
	return bashConfig{runner: resolved.runner, coord: resolved.coord, obs: resolved.obs}, nil
}

func newBash(root string, config bashConfig) *BashTool {
	return &BashTool{root: root, runner: config.runner, coord: config.coord, obs: config.obs}
}

// Info returns Bash's self-description. Name MUST equal "Bash".
func (b *BashTool) Info(context.Context) (*tool.ToolInfo, error) {
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
func (b *BashTool) AuditSummary(argsJSON string) string {
	var a bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Command == "" {
		return "Bash (unparsable args)"
	}
	return "Bash: " + a.Command
}

// BuildRequest derives the approval prompt: the command string (which doubles as
// the persisted exact-command Match) plus any MAC-verified escalation grants the
// call would apply (SPEC §9.3). An unparseable args document or an empty command is
// a typed error so the runner treats the call as invalid; a planned grant token that
// fails MAC verification also fails the build so it never reaches a prompt.
//
// Two distinct notions of "grants" meet here and MUST NOT be conflated: the display
// grants attached below come from planGrants (executor-AUTHORIZED tokens the operator
// approves) — NOT from a.Grants, which are the model's OPAQUE retry tokens carried
// verbatim and consumed separately at InvokableRun. BuildRequest never puts a.Grants
// on the prompt (a model-supplied token is not executor-verified, so it must not be
// shown as an authorized grant).
func (b *BashTool) BuildRequest(argsJSON string, _ tool.PreparedArtifact) (tool.PermissionRequest, error) {
	if b.initErr != nil {
		return nil, &bashError{reason: "Bash is unavailable", cause: b.initErr}
	}
	var a bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return nil, &bashError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if a.Command == "" {
		return nil, &bashError{reason: "a non-empty 'command' is required"}
	}
	grants, err := b.planGrants(a.Workdir, a.Command)
	if err != nil {
		return nil, err
	}
	return tool.BashRequest{Command: a.Command, Grants: grants}, nil
}

// planGrants asks the injected runner — when it is a sandbox escalation planner —
// for the MAC-verified grant descriptions to show in the permission prompt. It
// probes b.runner STRUCTURALLY (harness never imports sandbox, §10.1): a runner that
// implements BOTH PlanGrants (mint candidate escalation tokens for dir+command) and
// DescribeGrant (MAC-verify a token → its bound description) is a planner; anything
// else (nil, or a plain CommandRunner) plans no grants and the prompt is unchanged.
//
// For each candidate token DescribeGrant must return ok==true (the token is
// executor-minted and MAC-intact); a token that fails verification is fabricated or
// tampered and MUST NOT reach the prompt, so the whole build fails (SPEC §10.7). The
// dir is derived via the shared resolveDir (same as InvokableRun), so grants are
// planned for the call's real working directory; an escaping workdir has no confined
// dir to plan for (the run would reject it anyway), so it plans no grants rather than
// failing.
//
// It deliberately does NOT read bashArgs.Grants: those are the model's opaque retry
// tokens (unverified; consumed at InvokableRun), whereas these display grants are the
// executor's own PlanGrants→DescribeGrant output. Wiring a.Grants in here would put an
// unverified model-supplied token on the prompt as if it were authorized — exactly the
// fail-secure violation this seam exists to prevent.
func (b *BashTool) planGrants(workdir, command string) ([]tool.GrantDisplay, error) {
	pg, okPlan := b.runner.(interface {
		PlanGrants(dir, command string) []string
	})
	dg, okDescribe := b.runner.(interface {
		DescribeGrant(token string) (string, bool)
	})
	if !okPlan || !okDescribe {
		return nil, nil
	}

	dir, err := b.resolveDir(workdir)
	if err != nil {
		return nil, nil
	}

	tokens := pg.PlanGrants(dir, command)
	if len(tokens) == 0 {
		return nil, nil
	}
	grants := make([]tool.GrantDisplay, 0, len(tokens))
	for _, tok := range tokens {
		desc, ok := dg.DescribeGrant(tok)
		if !ok {
			return nil, &bashError{reason: "an escalation grant token failed MAC verification"}
		}
		grants = append(grants, tool.GrantDisplay{Token: tok, Description: desc})
	}
	return grants, nil
}

// resolveDir resolves a workspace-relative workdir to the confined absolute directory
// a command runs in. It delegates to the package-level resolveSpawnDir so InvokableRun
// (which maps its error to a tool-result string), planGrants (which plans no grants on
// error), AND the PermissionChecker's grant re-mint seam all derive the spawn dir
// identically — "grants are planned/minted for the same dir InvokableRun runs in" is a
// structural guarantee, not a copy-paste coincidence.
func (b *BashTool) resolveDir(workdir string) (string, error) {
	return resolveSpawnDir(b.root, workdir)
}

// resolveSpawnDir maps a workspace-relative workdir to the confined absolute directory
// a Bash command runs in: the root VERBATIM when workdir is empty, else
// containedPath(root, workdir) (which rejects any escape). It is the SINGLE definition
// of "the spawn dir", shared by Bash.resolveDir (the run/plan dir) and the
// PermissionChecker's grant re-mint seam (grant_remint.go), which MUST plan/mint grants
// for the SAME dir the spawn uses — the executor binds each token to
// hashCommand(dir, command) and re-verifies it against the actual spawn dir. Because
// both callers pass the same workspace root string — a composition-root invariant: the
// checker's WorkspaceRoot IS the Bash tool's root (the same invariant workspaceRelPath
// already relies on) — dir-consistency is structural, not a coincidence.
func resolveSpawnDir(root, workdir string) (string, error) {
	if workdir == "" {
		return root, nil
	}
	return containedPath(root, workdir)
}

// InvokableRun runs the command and returns its combined output + exit code as a
// tool result. A non-zero exit is a normal result; a timeout, an escaping
// workdir, or an unparseable args document is a tool-result error string. It
// never returns a Go error.
func (b *BashTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	if b.initErr != nil {
		return tool.TextResult("error: Bash is unavailable: " + b.initErr.Error()), nil
	}
	var a bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	if a.Command == "" {
		return tool.TextResult("error: a non-empty 'command' is required"), nil
	}

	// Resolve the working directory under the workspace root (default: the root).
	// An escape is rejected (defense in depth; the gate also contains the workdir).
	dir, err := b.resolveDir(a.Workdir)
	if err != nil {
		return tool.TextResult("error: workdir is outside the workspace: " + a.Workdir), nil
	}

	// Take the EXCLUSIVE whole-workspace mutation permit for the run: Bash may change
	// unknowable paths, so while it runs it excludes ALL structured path mutations and
	// every other whole/checkpoint permit session-wide. Acquire on the OUTER ctx (not
	// the command-timeout ctx below) so a slow command's timeout can't cancel an
	// already-held permit; a ctx-canceled acquire returns WITHOUT running. A nil
	// coordinator (bare path) yields a no-op permit.
	permit, err := b.acquireWhole(ctx)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	defer permit.Release()
	// Whichever way the run ends (success, non-zero exit, timeout, or start error) the
	// loop's ENTIRE file-observation set is invalidated, because the changed paths are
	// unknowable — Bash gains no file-level compare-and-swap. This defer fires only
	// once the command has been attempted (after a successful acquire).
	defer b.invalidateObservations()

	// Gather escalation grants from BOTH sources: the tool's own args and any the
	// runner placed on ctx after a pre-ask approval (union, dedup, order-stable).
	// The tokens stay OPAQUE — harness only carries them to a GrantedRunner.
	merged := mergeGrants(a.Grants, tool.GrantsFromContext(ctx))

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
	// Grant-aware dispatch: when grants are present AND the injected runner supports
	// them, run via RunCommandWithGrants; otherwise the exact Task-15 behavior
	// (RunCommand if a runner is set, else direct sh -c). Grants present but no
	// GrantedRunner (nil runner, or a RunCommand-only runner) falls through — the
	// tokens are ignored at the exec layer (the gate already saw them).
	if gr, ok := b.runner.(tool.GrantedRunner); ok && len(merged) > 0 {
		// Confined + escalated path: the injected runner (e.g. the sandbox Executor)
		// folds a timeout/cancel into err (it returns ctx.Err()); adapt its byte
		// output + error into the (output, exitCode, timedOut, startErr) shape.
		outBytes, ec, err := gr.RunCommandWithGrants(ctx2, dir, a.Command, merged)
		out, exitCode, timedOut, runErr = adaptRunnerResult(ctx2, outBytes, ec, err)
	} else if b.runner != nil {
		// Confined path: same adaptation as the grants path, without the tokens.
		outBytes, ec, err := b.runner.RunCommand(ctx2, dir, a.Command)
		out, exitCode, timedOut, runErr = adaptRunnerResult(ctx2, outBytes, ec, err)
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

// acquireWhole takes the exclusive whole-workspace mutation permit for a command run.
// A nil coordinator (the bare/standalone path) yields a no-op permit so InvokableRun
// runs the command coordinator-free. ctx is the OUTER per-call ctx; a canceled acquire
// returns the coordinator's typed error and no permit.
func (b *BashTool) acquireWhole(ctx context.Context) (tool.WorkspacePermit, error) {
	if nilInterface(b.coord) {
		return noPermit{}, nil
	}
	return b.coord.Acquire(ctx, tool.WorkspaceOperationWholeMutation, "")
}

// invalidateObservations drops the loop's entire file-observation set after a Bash
// run (a no-op when no observation set is bound).
func (b *BashTool) invalidateObservations() {
	if !nilInterface(b.obs) {
		b.obs.InvalidateAll()
	}
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

// adaptRunnerResult maps an injected runner's (output, exitCode, err) return into
// the (out, exitCode, timedOut, startErr) shape InvokableRun consumes, applying
// the DeadlineExceeded→timedOut rule: the sandbox executor folds a timeout/cancel
// into its returned err (ctx.Err()), so a deadline-exceeded err (directly or via
// the expired ctx) is a timeout, not a start error. It is shared by the plain and
// grant-carrying runner dispatch paths so both adapt identically.
func adaptRunnerResult(ctx context.Context, outBytes []byte, exitCode int, err error) (out string, code int, timedOut bool, startErr error) {
	out, code = string(outBytes), exitCode
	switch {
	case err == nil:
		// success: timedOut/startErr stay zero.
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded:
		timedOut = true
	default:
		startErr = err
	}
	return out, code, timedOut, startErr
}

// mergeGrants unions the tool's own grant args with any grants the runner placed
// on the ctx (a pre-ask approval): de-duplicated and order-stable — args first (in
// order, first occurrence wins), then ctx-only tokens not already present. Grant
// tokens are OPAQUE — harness never inspects or mints them; this only carries and
// de-dupes the strings. No grants from either source → nil (so len==0 selects the
// non-escalated path).
func mergeGrants(argsGrants, ctxGrants []string) []string {
	if len(argsGrants) == 0 && len(ctxGrants) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(argsGrants)+len(ctxGrants))
	merged := make([]string, 0, len(argsGrants)+len(ctxGrants))
	add := func(tokens []string) {
		for _, g := range tokens {
			if _, dup := seen[g]; dup {
				continue
			}
			seen[g] = struct{}{}
			merged = append(merged, g)
		}
	}
	add(argsGrants)
	add(ctxGrants)
	if len(merged) == 0 {
		return nil
	}
	return merged
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

// compile-time assertions: BashTool is an InvokableTool, a PermissionPrompter (Ask),
// and Auditable. It is NOT a WriteTarget (it is not a path-write tool).
var (
	_ tool.InvokableTool      = (*BashTool)(nil)
	_ tool.PermissionPrompter = (*BashTool)(nil)
	_ tool.Auditable          = (*BashTool)(nil)
)
