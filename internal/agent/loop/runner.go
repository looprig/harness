package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// runner.go is the heart of the agentic loop: RunBatch resolves permissions
// (prompting via gates), batches execution (serial then bounded-parallel with
// same-WriteTarget serialization), wraps each call in the middleware chain,
// recovers panics, and emits exactly one ToolCallStarted + one ToolCallCompleted
// per requested call whatever its fate. It is concurrency-correct and
// fail-secure: any permission ambiguity → the call is not executed.

// Pre-execution / failure tool-result strings. Every tool failure becomes a
// model-visible tool-result STRING (the contract) — never an aborted turn. The
// prefixes are named constants so they read consistently in the stream and in
// tests.
const (
	errPrefixUnknownTool = "error: unknown tool: "
	errInvalidArgs       = "error: invalid tool arguments (not valid JSON)"
	errPermissionDenied  = "error: permission denied"
	errPanicPrefix       = "error: tool panicked: "
	errEmptyResult       = "error: empty result"
	errWriteTargetPrefix = "error: invalid tool arguments: "
)

// ResultPreview caps. ResultPreview is stream-only (the sink projection drops it)
// but it may still hold a slice of tool output, so it is capped by BOTH a byte
// budget and a line budget, with a visible truncation marker when exceeded.
const (
	previewMaxBytes  = 2 * 1024 // ~2 KiB
	previewMaxLines  = 20
	truncationMarker = "… [truncated]"
)

// result is the package-private outcome of one tool call. Results are returned in
// the SAME ORDER as the requested calls (the model pairs tool_use↔tool_result by
// position/ID), and each carries its originating ToolUseBlock.ID so runTurn can
// build the paired ToolMessage.
type result struct {
	CallID    uuid.UUID
	ToolUseID string
	Content   []content.Block
	IsError   bool
}

// resolved is the runner's per-call working state, threaded from resolution
// through execution. It is built once per requested call in call order.
type resolved struct {
	callID  uuid.UUID
	block   content.ToolUseBlock
	argsstr string

	t       tool.InvokableTool // nil for an unknown tool
	summary string             // ToolCallStarted.Summary (redacted)

	// failed marks a pre-execution failure (unknown tool, invalid args,
	// permission denied, WriteTarget error). Its result is fixed before any
	// execution begins; it never reaches runOne.
	failed    bool
	failedMsg string

	// writeKey groups calls that must serialize relative to each other (same
	// resolved write target). Empty when the call has no write target.
	writeKey string
	hasWrite bool

	sequential bool
}

// RunBatch executes a batch of tool calls. It assigns a CallID per call, resolves
// tools + permissions sequentially (so a session grant on call N is visible to
// call N+1's Check), emits ALL ToolCallStarted before executing any call, runs the
// executable calls (serial batch drained first, then bounded-parallel with
// same-WriteTarget serialization), and emits one ToolCallCompleted per call. The
// returned []result is in call order.
func RunBatch(
	ctx context.Context,
	calls []content.ToolUseBlock,
	ts ToolSet,
	gateReg chan<- gateRegistration,
	emit func(event.Event),
) []result {
	rs := make([]*resolved, len(calls))
	for i, c := range calls {
		rs[i] = newResolved(ctx, c, ts)
	}

	// Sequential permission resolution, in call order, BEFORE any execution, so a
	// session grant on call N is visible to call N+1's Check. A ctx cancel during a
	// gate wait tears the whole batch down: return what we have (runTurn's rollback
	// discards a cancelled batch's results).
	for _, r := range rs {
		if r.failed || r.t == nil {
			continue
		}
		if err := resolvePermission(ctx, r, ts, gateReg, emit); err != nil {
			if ctx.Err() != nil {
				return collectResults(rs)
			}
			// A non-ctx error means denied / interrupted gate; resolvePermission has
			// already marked r.failed.
		}
	}

	// Emit ALL ToolCallStarted for the batch BEFORE executing any call — every
	// requested call (including pre-execution failures) gets a Started, and every
	// Started precedes every Completed so the TUI groups the batch race-free.
	for _, r := range rs {
		emit(event.ToolCallStarted{CallID: r.callID, ToolName: r.block.Name, Summary: r.summary})
	}

	// Build the per-call results map (CallID → result), populated by executors and
	// pre-execution failures alike. A mutex guards it across the parallel goroutines.
	var mu sync.Mutex
	out := make(map[uuid.UUID]result, len(rs))

	complete := func(r result) {
		mu.Lock()
		out[r.CallID] = r
		mu.Unlock()
		preview, isErr := previewOf(r)
		emit(event.ToolCallCompleted{CallID: r.CallID, IsError: isErr, ResultPreview: preview})
	}

	// Pre-execution failures complete immediately, in the Started order.
	var executable []*resolved
	for _, r := range rs {
		if r.failed {
			complete(failureResult(r))
			continue
		}
		executable = append(executable, r)
	}

	execute(ctx, executable, ts, gateReg, emit, complete)

	// Assemble results in call order from the map.
	final := make([]result, len(rs))
	for i, r := range rs {
		mu.Lock()
		final[i] = out[r.callID]
		mu.Unlock()
	}
	return final
}

// newResolved builds the per-call working state: assigns a CallID, looks up the
// tool, validates args JSON, queries WriteTarget, and computes the redacted
// Summary. Pre-execution failures (unknown tool, invalid args, WriteTarget error)
// are recorded here; permission is resolved later (sequentially).
func newResolved(ctx context.Context, c content.ToolUseBlock, ts ToolSet) *resolved {
	r := &resolved{block: c, argsstr: string(c.Input)}
	if cid, err := uuid.New(); err == nil {
		r.callID = cid
	}

	r.t = lookupTool(ctx, ts.Registry, c.Name)
	if r.t == nil {
		r.summary = c.Name // no tool → Summary is just the requested name
		r.fail(errPrefixUnknownTool + c.Name)
		return r
	}

	r.summary = summaryOf(r.t, c.Name, r.argsstr)

	if !json.Valid(c.Input) {
		r.fail(errInvalidArgs)
		return r
	}

	if wt, ok := r.t.(tool.WriteTarget); ok {
		key, has, err := wt.WriteTarget(r.argsstr)
		if err != nil {
			r.fail(errWriteTargetPrefix + err.Error())
			return r
		}
		r.writeKey, r.hasWrite = key, has
	}

	if sq, ok := r.t.(tool.Sequential); ok {
		r.sequential = sq.Sequential()
	}
	return r
}

// fail marks r as a pre-execution failure with the given model-visible message.
func (r *resolved) fail(msg string) {
	r.failed = true
	r.failedMsg = msg
}

// lookupTool resolves a tool by its Info(ctx).Name. Returns nil for an unknown
// name (so the caller produces a tool-result error, never a panic).
func lookupTool(ctx context.Context, registry []tool.InvokableTool, name string) tool.InvokableTool {
	for _, t := range registry {
		info, err := t.Info(ctx)
		if err != nil || info == nil {
			continue
		}
		if info.Name == name {
			return t
		}
	}
	return nil
}

// summaryOf returns the redacted ToolCallStarted.Summary: via Auditable when the
// tool implements it (it must tolerate invalid JSON, yielding just the name), else
// the tool name. Summary is NEVER built from raw args.
func summaryOf(t tool.InvokableTool, name, argsJSON string) string {
	if a, ok := t.(tool.Auditable); ok {
		return a.AuditSummary(argsJSON)
	}
	return name
}

// resolvePermission resolves the permission Effect for one (resolvable) call. On
// EffectAsk it opens a gatePermission gate (ctx-aware register → ack → emit →
// block), validates the reply's CallID, persists a non-ScopeOnce grant
// (best-effort — a Grant error never fails the call), and marks r.failed on deny.
// A returned non-nil error is either ctx.Err() (batch torn down) or a gate
// interruption; in both cases r is left in a safe state (failed or to-be-discarded).
func resolvePermission(
	ctx context.Context,
	r *resolved,
	ts ToolSet,
	gateReg chan<- gateRegistration,
	emit func(event.Event),
) error {
	if ts.Permission == nil {
		// No gate wired → fail-secure: deny rather than fall through.
		r.fail(errPermissionDenied)
		return nil
	}

	switch ts.Permission.Check(ctx, r.t, r.block.Name, r.argsstr) {
	case EffectAutoApprove:
		return nil
	case EffectDeny:
		r.fail(errPermissionDenied)
		return nil
	default: // EffectAsk (the fail-secure zero value)
		return askPermission(ctx, r, ts, gateReg, emit)
	}
}

// askPermission opens a permission gate and blocks for the user's decision,
// mirroring RequestUserInput's ctx-aware register→ack→emit→block pattern.
func askPermission(
	ctx context.Context,
	r *resolved,
	ts ToolSet,
	gateReg chan<- gateRegistration,
	emit func(event.Event),
) error {
	req := buildRequest(r.t, r.block.Name, r.summary, r.argsstr)

	// reply is buffered(1) (runner is the sole reader, so the actor's routed send
	// never blocks). ack is unbuffered: the actor closes it to signal installation.
	reply := make(chan command.Command, 1)
	ack := make(chan struct{})

	select {
	case gateReg <- gateRegistration{callID: r.callID, reply: reply, kind: gatePermission, ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Install-before-emit: only now is the gate guaranteed installed, so the
	// matching Approve/Deny cannot be dropped on a race.
	emit(event.PermissionRequested{CallID: r.callID, Request: req})

	select {
	case cmd := <-reply:
		return applyDecision(ctx, r, ts, cmd)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// applyDecision applies an Approve/Deny reply to r. listen already matched by
// CallID + kind; the CallID is re-validated as cheap defence in depth. A non-once
// approval persists via Grant — a Grant error NEVER fails the call (the user
// approved THIS call; Grant is best-effort persistence for future calls).
func applyDecision(ctx context.Context, r *resolved, ts ToolSet, cmd command.Command) error {
	switch c := cmd.(type) {
	case command.ApproveToolCall:
		if c.GateCallID() != r.callID {
			// Defence in depth: a mismatched CallID is fail-secure → deny.
			r.fail(errPermissionDenied)
			return nil
		}
		if c.Scope != tool.ScopeOnce {
			if err := ts.Permission.Grant(ctx, r.block.Name, r.argsstr, c.Scope); err != nil {
				slog.Warn("loop: permission grant did not persist; proceeding with approved call",
					"tool", r.block.Name, "scope", c.Scope, "error", err)
			}
		}
		return nil
	case command.DenyToolCall:
		r.fail(errPermissionDenied)
		return nil
	default:
		// Unexpected command kind on a permission gate — fail-secure.
		r.fail(errPermissionDenied)
		return nil
	}
}

// buildRequest derives the approval-prompt request: via PermissionPrompter when
// the tool implements it (falling back to UnknownRequest if BuildRequest errors),
// else an UnknownRequest carrying the redacted summary (never raw args).
func buildRequest(t tool.InvokableTool, name, summary, argsJSON string) tool.PermissionRequest {
	if p, ok := t.(tool.PermissionPrompter); ok {
		if req, err := p.BuildRequest(argsJSON); err == nil && req != nil {
			return req
		}
	}
	return tool.UnknownRequest{Tool: name, Summary: summary}
}

// execute runs the executable calls: the serial batch (Sequential()==true) drains
// first, then the parallel batch runs bounded by a semaphore of width
// MaxParallelToolCalls, with same-WriteTarget calls serialized in call order. Each
// finished call is reported via complete. execute does not return until every
// executable call has completed.
func execute(
	ctx context.Context,
	executable []*resolved,
	ts ToolSet,
	gateReg chan<- gateRegistration,
	emit func(event.Event),
	complete func(result),
) {
	var serial, parallel []*resolved
	for _, r := range executable {
		if r.sequential {
			serial = append(serial, r)
		} else {
			parallel = append(parallel, r)
		}
	}

	// Serial batch drains fully first (in call order).
	for _, r := range serial {
		complete(runOne(ctx, r, ts, gateReg, emit))
	}

	if len(parallel) == 0 {
		return
	}

	// Per-WriteTarget-key mutex so same-key calls serialize (in call order)
	// even within the parallel batch.
	keyLocks := make(map[string]*sync.Mutex)
	for _, r := range parallel {
		if r.hasWrite {
			if _, ok := keyLocks[r.writeKey]; !ok {
				keyLocks[r.writeKey] = &sync.Mutex{}
			}
		}
	}

	// Semaphore bounds peak concurrency to MaxParallelToolCalls.
	sem := make(chan struct{}, resolveMaxParallelToolCalls(ts.MaxParallelToolCalls))
	var wg sync.WaitGroup
	for _, r := range parallel {
		wg.Add(1)
		go func(r *resolved) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if r.hasWrite {
				lk := keyLocks[r.writeKey]
				lk.Lock()
				defer lk.Unlock()
			}
			complete(runOne(ctx, r, ts, gateReg, emit))
		}(r)
	}
	wg.Wait()
}

// runOne executes a single resolved call: builds the per-call ctx (CallID + emit +
// gateReg injected so the tool can emit / request user input), wraps InvokableRun
// in the middleware chain (first listed = outermost), recovers a panic into an
// error result, and normalizes the outcome to a result. It never aborts the batch.
func runOne(
	ctx context.Context,
	r *resolved,
	ts ToolSet,
	gateReg chan<- gateRegistration,
	emit func(event.Event),
) (res result) {
	ctx2 := withGateReg(withEmit(withCallID(ctx, r.callID), emit), gateReg)

	defer func() {
		if rec := recover(); rec != nil {
			res = errResult(r, fmt.Sprintf("%s%v", errPanicPrefix, rec))
		}
	}()

	exec := chain(r.t, ts.Middlewares)
	tr, err := exec(ctx2, r.argsstr)
	if err != nil {
		return errResult(r, "error: "+err.Error())
	}
	if tr == nil || len(tr.Content) == 0 {
		return errResult(r, errEmptyResult)
	}
	return result{
		CallID:    r.callID,
		ToolUseID: r.block.ID,
		Content:   tr.Content,
		IsError:   isErrorResult(tr),
	}
}

// isErrorResult reports whether a successful (non-nil, non-empty) ToolResult is an
// error result. A tool signals an error by setting IsError on a ToolResultBlock it
// returns; absent that, a value result is a success.
func isErrorResult(tr *tool.ToolResult) bool {
	for _, b := range tr.Content {
		if trb, ok := b.(*content.ToolResultBlock); ok && trb.IsError {
			return true
		}
	}
	return false
}

// chain composes the middleware chain around the tool's InvokableRun. The
// first-listed middleware is the OUTERMOST wrapper.
func chain(t tool.InvokableTool, mws []tool.ToolMiddleware) tool.ToolExecuteFunc {
	next := t.InvokableRun
	for i := len(mws) - 1; i >= 0; i-- {
		mw := mws[i]
		inner := next
		next = func(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
			return mw(ctx, t, argsJSON, inner)
		}
	}
	return next
}

// failureResult builds the fixed result for a pre-execution failure.
func failureResult(r *resolved) result {
	return errResult(r, r.failedMsg)
}

// errResult builds an error tool-result carrying the given message.
func errResult(r *resolved, msg string) result {
	return result{
		CallID:    r.callID,
		ToolUseID: r.block.ID,
		Content:   []content.Block{&content.TextBlock{Text: msg}},
		IsError:   true,
	}
}

// collectResults gathers whatever results exist (used on the ctx-cancel teardown
// path). A torn-down batch's results are discarded by runTurn's rollback, so this
// just returns a best-effort, call-ordered slice with empty entries for
// unresolved calls.
func collectResults(rs []*resolved) []result {
	out := make([]result, len(rs))
	for i, r := range rs {
		out[i] = result{CallID: r.callID, ToolUseID: r.block.ID}
	}
	return out
}

// previewOf returns the (capped) ResultPreview and the IsError flag for a result.
func previewOf(r result) (string, bool) {
	return capPreview(flattenToText(r.Content)), r.IsError
}

// flattenToText renders a block slice to text for the ResultPreview AND for the
// ToolMessage runTurn builds (runTurn REUSES this). Text/TextBlock content passes
// through (concatenated); any non-text block becomes a visible
// "[unsupported <type>]" placeholder — NEVER empty/silent, so a tool-result is
// always non-empty on the wire.
func flattenToText(blocks []content.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch v := b.(type) {
		case *content.TextBlock:
			sb.WriteString(v.Text)
		case *content.ToolResultBlock:
			sb.WriteString(flattenToText(v.Content))
		default:
			sb.WriteString("[unsupported " + string(blockTypeOf(b)) + "]")
		}
	}
	return sb.String()
}

// blockTypeOf returns the BlockType tag for a non-text block, used to build a
// visible placeholder in flattenToText.
func blockTypeOf(b content.Block) content.BlockType {
	switch b.(type) {
	case *content.ImageBlock:
		return content.TypeImage
	case *content.AudioBlock:
		return content.TypeAudio
	case *content.DocumentBlock:
		return content.TypeDocument
	case *content.ThinkingBlock:
		return content.TypeThinking
	case *content.ToolUseBlock:
		return content.TypeToolUse
	case *content.ToolResultBlock:
		return content.TypeToolResult
	default:
		return content.BlockType("unknown")
	}
}

// capPreview caps a preview string by BOTH a byte budget and a line budget, with a
// visible truncation marker appended when either is exceeded.
func capPreview(s string) string {
	truncated := false

	if lines := strings.Split(s, "\n"); len(lines) > previewMaxLines {
		s = strings.Join(lines[:previewMaxLines], "\n")
		truncated = true
	}
	if len(s) > previewMaxBytes {
		s = s[:previewMaxBytes]
		truncated = true
	}
	if truncated {
		return s + truncationMarker
	}
	return s
}
