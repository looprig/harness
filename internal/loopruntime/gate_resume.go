package loopruntime

import (
	"context"
	"log/slog"
	"reflect"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// This file is the loop half of resuming a turn that a restored session found
// PARKED at a tool gate. Before it, restore closed such a turn with a synthesized
// TurnInterrupted: the in-memory tool call waiting on the gate died with the old
// runtime, so an answer given to the successor had nothing to reach. Now a gate a
// successor can resume carries a private snapshot of its step
// (event.GatePrepared.Resume), and the restored loop re-enters that step: it runs
// the gated call again against the SAME still-open gate, so the answer reaches it
// and the turn continues exactly as if the runtime had never moved.
//
// EXACTLY-ONCE. A call is re-run only when running it again repeats nothing:
//   - a permission gate is answered during access resolution, which RunBatch
//     completes for the whole batch before executing ANY call, so a step parked at
//     one has executed nothing and the whole batch runs afresh;
//   - a user-input gate is raised mid-execution, so only the calls parked on a gate
//     are re-run, and only when their tool declares tool.UserInputReplaySafe; every
//     other call of the step may or may not have run on the old runtime, and is
//     answered with an explicit "outcome unknown" error result instead of being
//     run again.
// A gate whose answer is durable (GateResolved) before the step committed is not
// open at restore, so no snapshot is resumed and the turn is interrupted as it
// always was: at most once, never twice.

// ParkedGate names one open gate a restored step is parked on.
type ParkedGate struct {
	GateID          gatedomain.ID
	Kind            gatedomain.Kind
	ToolExecutionID uuid.UUID
	ToolUseID       string
	// PermissionRequest is the displayed request the restored permission gate
	// shows. The re-run call adopts the gate only if its re-evaluated prompt still
	// asks for exactly this; otherwise the stale gate is closed and a fresh one is
	// opened, so an answer is never applied to a request it did not see.
	PermissionRequest *tool.Request
	// AskUser is the question and choices the restored user-input gate shows. The
	// re-run call adopts the gate only if it asks exactly this; otherwise the stale
	// gate is closed and the call asks afresh.
	AskUser *gatedomain.AskUserPayload
}

// ParkedStep is the in-flight tool step of a restored loop's open turn. A loop
// seeded with one resumes that turn as soon as it starts, instead of coming up idle.
type ParkedStep struct {
	TurnID uuid.UUID
	// Cause is the open turn's TurnStarted cause, stamped on the resumed turn's
	// hooks and events exactly as on the original.
	Cause identity.Cause
	// TurnStart is the index in RestoredState.Msgs of the open turn's opening user
	// message: messages before it are the turn's base, the rest its staged messages.
	TurnStart int
	StepID    uuid.UUID
	StepIndex StepIndex
	// Message is the step's assistant message as it will be committed.
	Message *content.AIMessage
	Gates   []ParkedGate
}

// permissionPhase reports whether the step is parked during access resolution
// (nothing executed yet) rather than during execution.
func (p *ParkedStep) permissionPhase() bool {
	for _, g := range p.Gates {
		if g.Kind == gatedomain.KindPermission {
			return true
		}
	}
	return false
}

// valid reports whether the snapshot is internally consistent enough to resume:
// every gate names a distinct call the message carries.
func (p *ParkedStep) valid(msgs int) bool {
	if p == nil || p.Message == nil || len(p.Gates) == 0 || p.TurnID.IsZero() {
		return false
	}
	if p.TurnStart < 0 || p.TurnStart >= msgs {
		return false
	}
	uses := make(map[string]int)
	for _, block := range p.Message.Blocks {
		if use, ok := block.(*content.ToolUseBlock); ok {
			uses[use.ID]++
		}
	}
	seen := make(map[string]bool, len(p.Gates))
	for _, g := range p.Gates {
		if g.GateID.IsZero() || g.ToolExecutionID.IsZero() || uses[g.ToolUseID] != 1 || seen[g.ToolUseID] {
			return false
		}
		switch g.Kind {
		case gatedomain.KindPermission, gatedomain.KindAskUser:
		default:
			return false
		}
		seen[g.ToolUseID] = true
	}
	return true
}

// parkedTurn is the live form of a ParkedStep: the reply channel pre-installed for
// each gate. The channels are installed in pendingGates BEFORE the actor starts, so
// an answer that arrives before the re-run call reaches its gate waits in the
// buffer instead of being dropped as addressed to no gate.
type parkedTurn struct {
	step     ParkedStep
	adoption *gateAdoption
}

// gateAdoption hands each re-run call its restored gate. Each gate is adopted at
// most once; a second request for the same call (a tool that asks twice) opens a
// fresh gate as it always would.
type gateAdoption struct {
	mu    sync.Mutex
	gates map[uuid.UUID]adoptableGate
}

type adoptableGate struct {
	id      gatedomain.ID
	kind    gateKind
	reply   <-chan command.Command
	request *tool.Request
	ask     *gatedomain.AskUserPayload
}

func gateKindFor(kind gatedomain.Kind) gateKind {
	if kind == gatedomain.KindAskUser {
		return gateUserInput
	}
	return gatePermission
}

func newParkedTurn(step ParkedStep, pending map[gatedomain.ID]pendingGate) *parkedTurn {
	adoption := &gateAdoption{gates: make(map[uuid.UUID]adoptableGate, len(step.Gates))}
	for _, g := range step.Gates {
		reply := make(chan command.Command, 1)
		kind := gateKindFor(g.Kind)
		pending[g.GateID] = pendingGate{reply: reply, kind: kind}
		adoption.gates[g.ToolExecutionID] = adoptableGate{id: g.GateID, kind: kind, reply: reply, request: g.PermissionRequest, ask: g.AskUser}
	}
	return &parkedTurn{step: step, adoption: adoption}
}

// take removes and returns the restored gate for callID of the given kind.
func (a *gateAdoption) take(callID uuid.UUID, kind gateKind) (adoptableGate, bool) {
	if a == nil {
		return adoptableGate{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.gates[callID]
	if !ok || g.kind != kind {
		return adoptableGate{}, false
	}
	delete(a.gates, callID)
	return g, true
}

// matchesPermission reports whether a re-evaluated displayed request is the one
// the restored gate shows.
func (g adoptableGate) matchesPermission(displayed tool.Request) bool {
	return g.request != nil && reflect.DeepEqual(g.request.Clone(), displayed.Clone())
}

// matchesQuestion reports whether a re-asked question is the one the restored
// user-input gate shows.
func (g adoptableGate) matchesQuestion(question string, choices []string) bool {
	if g.ask == nil || g.ask.Question != question || len(g.ask.Choices) != len(choices) {
		return false
	}
	for i := range choices {
		if g.ask.Choices[i] != choices[i] {
			return false
		}
	}
	return true
}

type gateAdoptionKey struct{}
type stepResumeKey struct{}
type userInputReplaySafeKey struct{}

func withGateAdoption(ctx context.Context, adoption *gateAdoption) context.Context {
	return context.WithValue(ctx, gateAdoptionKey{}, adoption)
}

func gateAdoptionFromContext(ctx context.Context) *gateAdoption {
	adoption, _ := ctx.Value(gateAdoptionKey{}).(*gateAdoption)
	return adoption
}

// stepResumeBase is what the batch boundary knows about the running step: enough
// to build a gate's event.ToolStepResume once the gated call is known.
type stepResumeBase struct {
	index   StepIndex
	message *content.AIMessage
}

func withStepResume(ctx context.Context, base stepResumeBase) context.Context {
	return context.WithValue(ctx, stepResumeKey{}, base)
}

// stepResumeFor builds the snapshot for the call toolUseID, or nil when the batch
// carries no step (a focused test, or a runner used outside a turn).
func stepResumeFor(ctx context.Context, toolUseID string) *event.ToolStepResume {
	base, ok := ctx.Value(stepResumeKey{}).(stepResumeBase)
	if !ok || base.message == nil || toolUseID == "" {
		return nil
	}
	resume := &event.ToolStepResume{
		StepIndex: uint64(base.index),
		Message:   cloneAIMessage(base.message),
		ToolUseID: toolUseID,
	}
	if !resume.Valid() {
		return nil
	}
	return resume
}

func withUserInputReplaySafe(ctx context.Context, t tool.InvokableTool) context.Context {
	safe := false
	if declared, ok := t.(tool.UserInputReplaySafe); ok {
		safe = declared.UserInputReplaySafe()
	}
	return context.WithValue(ctx, userInputReplaySafeKey{}, safe)
}

func userInputReplaySafe(ctx context.Context) bool {
	safe, _ := ctx.Value(userInputReplaySafeKey{}).(bool)
	return safe
}

// resumableGateRegistrar is the optional registrar capability that records a
// gate's resume snapshot in its private prepared record. A registrar without it
// (headless, focused tests) opens the same gate with no snapshot, which restore
// treats as it always did.
type resumableGateRegistrar interface {
	PrepareResumableGateOpen(ctx context.Context, loopID uuid.UUID, g gatedomain.Gate, payload gatedomain.Payload, resume *event.ToolStepResume) (gatedomain.ID, error)
}

// turnResumeActivity is the optional session capability that records the resumed
// turn as live work, so the session is not reported idle while the turn it
// restored is running. It is the resumed-turn counterpart of the activity a
// TurnStarted publication records; a resumed turn publishes no TurnStarted, since
// its opening event is already durable.
type turnResumeActivity interface {
	ResumeTurnActivity(ctx context.Context, loopID, turnID uuid.UUID) error
}

// errUnknownOutcome is the tool result a resumed user-input step reports for a
// sibling call it cannot re-run: the call may or may not have run on the runtime
// the session moved from.
const errUnknownOutcome = "error: this tool call's outcome is unknown: the session moved to another runtime while this step was waiting for the user, and the call is not re-run in case it already took effect; check its effect before retrying it"

// resumedStepResults runs a parked step's calls and returns one result per tool
// use of the step, in order. A permission-phase step runs the whole batch; a
// user-input step runs only its gated calls and answers the rest with
// errUnknownOutcome.
func resumedStepResults(ctx context.Context, parked *parkedTurn, toolUses []content.ToolUseBlock, ts ToolSet, runtime BatchRuntime) []result {
	step := parked.step
	ids := make(map[string]uuid.UUID, len(step.Gates))
	for _, g := range step.Gates {
		ids[g.ToolUseID] = g.ToolExecutionID
	}
	runtime.executionIDs = ids
	ctx = withGateAdoption(ctx, parked.adoption)
	if step.permissionPhase() {
		return RunBatch(ctx, toolUses, ts, runtime)
	}
	var rerun []content.ToolUseBlock
	slots := make([]int, 0, len(step.Gates))
	for i, use := range toolUses {
		if _, gated := ids[use.ID]; gated {
			rerun = append(rerun, use)
			slots = append(slots, i)
		}
	}
	ran := RunBatch(ctx, rerun, ts, runtime)
	idGen := runtime.IDGen
	if idGen == nil {
		idGen = uuid.New
	}
	out := make([]result, len(toolUses))
	for i, use := range toolUses {
		// The answer is committed under a fresh execution id of its own: the
		// original call's id died with the runtime, and a retained result is keyed
		// by one. A failed mint leaves it zero, as a failed mint does for any call.
		id, err := mintToolExecutionID(idGen)
		if err != nil {
			slog.Error("loop: resumed step could not mint an execution id for an unknown-outcome result", "error", boundedDiagnostic(safeErrorText(err)))
		}
		out[i] = result{
			ToolExecutionID: id,
			ToolUseID:       use.ID,
			Content:         []content.Block{&content.TextBlock{Text: errUnknownOutcome}},
			IsError:         true,
		}
	}
	for j, slot := range slots {
		if j < len(ran) {
			out[slot] = ran[j]
		}
	}
	return out
}

// toolUsesOf returns the tool calls of a committed assistant message in order.
func toolUsesOf(message *content.AIMessage) []content.ToolUseBlock {
	if message == nil {
		return nil
	}
	var uses []content.ToolUseBlock
	for _, block := range message.Blocks {
		if use, ok := block.(*content.ToolUseBlock); ok && use != nil {
			uses = append(uses, *content.CloneBlock(use).(*content.ToolUseBlock))
		}
	}
	return uses
}

// committedToolSteps counts the tool-using steps a resumed turn already committed,
// so its runaway cap counts them as the original turn did.
func committedToolSteps(msgs content.AgenticMessages) int {
	steps := 0
	for _, message := range msgs {
		ai, ok := message.(*content.AIMessage)
		if !ok {
			continue
		}
		for _, block := range ai.Blocks {
			if _, isUse := block.(*content.ToolUseBlock); isUse {
				steps++
				break
			}
		}
	}
	return steps
}

// GateCloseError reports a permission gate the batch had to close durably and
// could not. The batch executes nothing and the turn fails: a gate left open in
// the journal is one a restored session would resume, re-running the batch.
type GateCloseError struct {
	GateID gatedomain.ID
	Cause  error
}

func (e *GateCloseError) Error() string {
	return "loop: could not durably close gate " + e.GateID.String()
}

func (e *GateCloseError) Unwrap() error { return e.Cause }

// batchGateFault records the first GateCloseError of one batch. runTurn installs
// one per batch and reads it after RunBatch.
type batchGateFault struct {
	mu  sync.Mutex
	err *GateCloseError
}

type batchGateFaultKey struct{}

func withBatchGateFault(ctx context.Context, fault *batchGateFault) context.Context {
	return context.WithValue(ctx, batchGateFaultKey{}, fault)
}

func batchGateFaultFrom(ctx context.Context) *batchGateFault {
	fault, _ := ctx.Value(batchGateFaultKey{}).(*batchGateFault)
	return fault
}

// recordGateCloseFailure records a failed durable close on ctx's batch and
// returns the typed error.
func recordGateCloseFailure(ctx context.Context, id gatedomain.ID, cause error) error {
	closeErr := &GateCloseError{GateID: id, Cause: cause}
	if fault := batchGateFaultFrom(ctx); fault != nil {
		fault.mu.Lock()
		if fault.err == nil {
			fault.err = closeErr
		}
		fault.mu.Unlock()
	}
	return closeErr
}

func (f *batchGateFault) failure() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		return nil
	}
	return f.err
}

func gateCloseFailed(ctx context.Context) bool {
	return batchGateFaultFrom(ctx).failure() != nil
}

// closeUnadoptedGates durably closes, after the batch's access pass and before
// any execution, every restored gate the pass left unadopted that no call of this
// batch can still reach: every permission gate (a permission gate is only ever
// answered during the access pass), and a user-input gate whose call will not run.
// It reports false, having recorded the failure, if a close fails.
func closeUnadoptedGates(ctx context.Context, rs []*resolved, gateReg chan<- gateRegistration) bool {
	adoption := gateAdoptionFromContext(ctx)
	if adoption == nil {
		return !gateCloseFailed(ctx)
	}
	runnable := make(map[uuid.UUID]bool, len(rs))
	for _, r := range rs {
		if r != nil && !r.failed {
			runnable[r.callID] = true
		}
	}
	for _, g := range adoption.release(func(callID uuid.UUID, g adoptableGate) bool {
		return g.kind == gatePermission || !runnable[callID]
	}) {
		if err := abandonInstalledGate(ctx, ctx, gateReg, g.id); err != nil {
			_ = recordGateCloseFailure(ctx, g.id, err)
			return false
		}
	}
	return !gateCloseFailed(ctx)
}

// release removes and returns every still-held gate for which match is true.
func (a *gateAdoption) release(match func(uuid.UUID, adoptableGate) bool) []adoptableGate {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []adoptableGate
	for callID, g := range a.gates {
		if match(callID, g) {
			out = append(out, g)
			delete(a.gates, callID)
		}
	}
	return out
}
