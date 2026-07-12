package sessionruntime

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
)

// delegation.go is the session-runtime delegation manager (design §"Synchronous and
// managed delegation"/§"Follow-up request and answer semantics"). It vends a SEPARATE
// parent-scoped tool.DelegateController for each live parent loop and injects it into
// that loop's Subagent tool. A scoped controller addresses ONLY children owned by its
// bound parent (registry-derived ownership, restore-safe): it rejects siblings,
// ancestors, unrelated loop ids, unavailable actions, and invalid modes. The parent
// model never receives the session or the manager — only the narrow scoped controller.
//
// OWNERSHIP survives restore because it is derived from the loop registry's parent
// links (attachRestoredLoop re-seeds each loop's parent), not a separate map. The
// cumulative spawn quota also survives restore (countSpawnedLoops re-seeds it). The
// Live pending handles are process-local. Durable machine NoFold intent records plus
// correlated turn terminals reconstruct request resolution across restore; queued work
// that never started is classified Interrupted and is never replayed.

// delegationManager mediates parent-to-child delegation for one session. It is created
// before the session's loops are bound (so restore can bind loop tools against it) and
// attached to the session once it exists. It is safe to construct scoped controllers
// from a not-yet-attached manager; only Execute needs the attached session.
type delegationManager struct {
	// byName resolves a requested delegate name to its immutable child definition. It is
	// the whole topology, so authorization (the parent's allowed set) is enforced
	// separately by each scoped controller.
	byName map[identity.AgentName]loop.Definition

	mu       sync.Mutex
	session  *Session
	requests map[uuid.UUID]*pendingRequest
	// resolved is the DURABLE request→terminal index reconstructed at restore from each
	// loop's folded history (request id → the terminal of the correlated turn). It is the
	// post-restore fallback for wait: the in-memory pending handle does not survive a
	// process restart, but the child's committed turn terminal does. Guarded by mu.
	resolved map[uuid.UUID]resolvedRequest
}

type delegateAdmission struct {
	ctx       context.Context
	message   string
	sub       event.Subscription
	requestID uuid.UUID
	command   command.UserInput
	publisher *delegateAdmissionPublisher
}

// delegateAdmissionPublisher is a one-shot start barrier. A fresh child may accept its
// pre-built initial command, but its first event (and any gate side effect) cannot cross
// into the session until LoopStarted has durably committed.
type delegateAdmissionPublisher struct {
	session *Session
	ready   chan struct{}
	once    sync.Once
}

func newDelegateAdmissionPublisher(session *Session) *delegateAdmissionPublisher {
	return &delegateAdmissionPublisher{session: session, ready: make(chan struct{})}
}
func (p *delegateAdmissionPublisher) release() { p.once.Do(func() { close(p.ready) }) }
func (p *delegateAdmissionPublisher) wait(ctx context.Context) error {
	select {
	case <-p.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *delegateAdmissionPublisher) PublishEvent(ctx context.Context, ev event.Event) error {
	if err := p.wait(ctx); err != nil {
		return err
	}
	return p.session.PublishEvent(ctx, ev)
}
func (p *delegateAdmissionPublisher) PublishEventChecked(ctx context.Context, ev event.Event) error {
	if err := p.wait(ctx); err != nil {
		return err
	}
	return p.session.PublishEventChecked(ctx, ev)
}
func (p *delegateAdmissionPublisher) FaultErr() error { return p.session.FaultErr() }
func (p *delegateAdmissionPublisher) PrepareGateOpen(ctx context.Context, loopID uuid.UUID, g gate.Gate, payload gate.Payload) (gate.ID, error) {
	if err := p.wait(ctx); err != nil {
		return gate.ID{}, err
	}
	return p.session.PrepareGateOpen(ctx, loopID, g, payload)
}
func (p *delegateAdmissionPublisher) ActivateGate(ctx context.Context, id gate.ID, route gate.Route) error {
	if err := p.wait(ctx); err != nil {
		return err
	}
	return p.session.ActivateGate(ctx, id, route)
}
func (p *delegateAdmissionPublisher) CloseGate(ctx context.Context, id gate.ID, reason gate.CloseReason) error {
	if err := p.wait(ctx); err != nil {
		return err
	}
	return p.session.CloseGate(ctx, id, reason)
}

// resolvedRequest is one durable delegate request terminal recovered at restore: the
// owning child and the turn's final answer/status. Empty text with a non-completed status
// is a typed failed/interrupted terminal.
type resolvedRequest struct {
	childID uuid.UUID
	status  tool.DelegateStatusValue
	text    string
}

func newDelegationManager(topology Topology) *delegationManager {
	byName := make(map[identity.AgentName]loop.Definition, len(topology.Definitions))
	for _, def := range topology.Definitions {
		byName[def.Name()] = def
	}
	return &delegationManager{
		byName:   byName,
		requests: make(map[uuid.UUID]*pendingRequest),
		resolved: make(map[uuid.UUID]resolvedRequest),
	}
}

// seedResolvedDelegateRecords reconstructs durable delegate correlation from required
// machine NoFold intents, then overlays exact started-turn terminals and crash closures.
func seedResolvedDelegateRecords(m *delegationManager, records []journal.JournalRecord, replayed, closures []event.Event) error {
	intents := make(map[uuid.UUID]uuid.UUID)
	for _, record := range records {
		commandRecord, ok := record.(journal.CommandRecord)
		if !ok {
			continue
		}
		if err := journal.ValidateCommandRecordRoute(commandRecord); err != nil {
			return err
		}
		input, ok := commandRecord.Command().(command.UserInput)
		if !ok || !input.NoFold || input.Agency != identity.AgencyMachine || input.TargetLoopID.IsZero() || input.CommandID.IsZero() {
			continue
		}
		intents[input.CommandID] = input.TargetLoopID
	}
	combined := make([]event.Event, 0, len(replayed)+len(closures))
	combined = append(combined, replayed...)
	combined = append(combined, closures...)
	index := make(map[uuid.UUID]resolvedRequest)
	for _, ev := range combined {
		var requestID, childID uuid.UUID
		switch accepted := ev.(type) {
		case event.LoopStarted:
			requestID, childID = accepted.InitialRequestID, accepted.LoopID
		case event.DelegateRequestAccepted:
			requestID, childID = accepted.Cause.CommandID, accepted.LoopID
		default:
			continue
		}
		if requestID.IsZero() {
			continue
		}
		target, admitted := intents[requestID]
		if !admitted {
			continue
		}
		if childID != target {
			return &journal.CommandRouteMismatchError{RecordLoopID: childID, TargetLoopID: target}
		}
		index[requestID] = resolvedRequest{childID: target, status: tool.DelegateStatusInterrupted}
	}
	for requestID, terminal := range foldDelegateTerminals(combined) {
		if _, admitted := index[requestID]; admitted {
			index[requestID] = terminal
		}
	}
	m.mu.Lock()
	m.resolved = index
	m.mu.Unlock()
	return nil
}

func (m *delegationManager) getResolved(requestID uuid.UUID) (resolvedRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rr, ok := m.resolved[requestID]
	return rr, ok
}

// foldDelegateTerminals correlates every turn's opening request id (TurnStarted's
// Cause.CommandID) to its terminal (TurnDone answer / TurnFailed / TurnInterrupted). A
// turn with a zero Cause.CommandID (no correlating submit) is skipped. It mirrors the live
// delegate drain exactly: only TurnDone.Message is an answer; StepDone is progress.
func foldDelegateTerminals(events []event.Event) map[uuid.UUID]resolvedRequest {
	type turnKey struct {
		loopID    uuid.UUID
		commandID uuid.UUID
	}
	byTurn := make(map[uuid.UUID]turnKey)
	out := make(map[uuid.UUID]resolvedRequest)
	for _, ev := range events {
		switch e := ev.(type) {
		case event.TurnStarted:
			byTurn[e.Coordinates.TurnID] = turnKey{loopID: e.Coordinates.LoopID, commandID: e.Cause.CommandID}
		case event.TurnDone:
			if k, ok := byTurn[e.Coordinates.TurnID]; ok && !k.commandID.IsZero() {
				text := aiText(e.Message)
				out[k.commandID] = resolvedRequest{childID: k.loopID, status: tool.DelegateStatusCompleted, text: text}
			}
		case event.TurnFailed:
			if k, ok := byTurn[e.Coordinates.TurnID]; ok && !k.commandID.IsZero() {
				out[k.commandID] = resolvedRequest{childID: k.loopID, status: tool.DelegateStatusFailed}
			}
		case event.TurnInterrupted:
			if k, ok := byTurn[e.Coordinates.TurnID]; ok && !k.commandID.IsZero() {
				out[k.commandID] = resolvedRequest{childID: k.loopID, status: tool.DelegateStatusInterrupted}
			}
		}
	}
	return out
}

// attach binds the live session so scoped controllers can spawn and address children.
func (m *delegationManager) attach(s *Session) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.session = s
	m.mu.Unlock()
}

func (m *delegationManager) sess() (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session, m.session != nil
}

// delegateExtraTools derives the model-facing delegation tool for a parent definition:
// a loop with a non-empty Delegates() gets exactly ONE Subagent tool whose catalog is
// its delegate set and whose action set follows its Delegation().Style. A loop with NO
// delegates gets nothing (no Subagent capability). It is a pure function of the frozen
// definition, so the derivation is deterministic across New and Restore (the delegate set
// and style are part of the definition fingerprint). The session injects the result via
// tool.Bindings.ExtraTools at the loop's bind site — the user never hand-adds it.
func delegateExtraTools(def loop.Definition, manager *delegationManager) []tool.Definition {
	delegates := def.Delegates()
	if len(delegates) == 0 {
		return nil
	}
	catalog := make([]tools.SubagentCatalogEntry, len(delegates))
	for i, name := range delegates {
		entry := tools.SubagentCatalogEntry{Name: name}
		if manager != nil {
			if target, ok := manager.byName[name]; ok {
				entry.Modes = []loop.ModeName{""}
				for _, mode := range target.Modes() {
					entry.Modes = append(entry.Modes, mode.Name)
				}
			}
		}
		catalog[i] = entry
	}
	return []tool.Definition{tools.Subagent(def.Delegation().Style, catalog)}
}

// controllerFor builds the parent-scoped controller injected into one loop's Subagent
// tool. The allowed delegate set and delegation style are derived from the PARENT
// definition (least privilege). It tolerates a nil manager receiver so a struct-literal
// session with no delegation manager can still bind loops that carry no Subagent tool.
func (m *delegationManager) controllerFor(parentLoopID uuid.UUID, parent loop.Definition) tool.DelegateController {
	allowed := make(map[identity.AgentName]struct{})
	for _, name := range parent.Delegates() {
		allowed[name] = struct{}{}
	}
	return &scopedController{
		manager:      m,
		parentLoopID: parentLoopID,
		style:        parent.Delegation().Style,
		allowed:      allowed,
	}
}

// pendingRequest is one in-flight wait:false delegate request: a background drain fills
// its terminal (text + status) and closes done; a later wait reads it.
type pendingRequest struct {
	childID uuid.UUID
	done    chan struct{}

	mu     sync.Mutex
	text   string
	status tool.DelegateStatusValue
}

func (p *pendingRequest) resolve(text string, status tool.DelegateStatusValue) {
	p.mu.Lock()
	p.text = text
	p.status = status
	p.mu.Unlock()
	close(p.done)
}

func (p *pendingRequest) result() (string, tool.DelegateStatusValue) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.text, p.status
}

// registerPending records a wait:false request and starts the background drain that
// resolves it. The drain runs on the SESSION lifetime (not the parent turn ctx, which
// ends when the tool call returns), so a later wait can still collect the answer.
func (m *delegationManager) registerPending(requestID, childID uuid.UUID, sub event.Subscription, sessionCtx context.Context, interrupt func()) {
	pr := &pendingRequest{childID: childID, done: make(chan struct{})}
	m.mu.Lock()
	m.requests[requestID] = pr
	m.mu.Unlock()
	go func() {
		defer func() { _ = sub.Close() }()
		text, err := drainDelegateAnswer(sessionCtx, sub, requestID, interrupt)
		pr.resolve(text, statusFromDrain(err))
	}()
}

func (m *delegationManager) getPending(requestID uuid.UUID) (*pendingRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr, ok := m.requests[requestID]
	return pr, ok
}

func (m *delegationManager) removePending(requestID uuid.UUID) {
	m.mu.Lock()
	delete(m.requests, requestID)
	m.mu.Unlock()
}

// pendingCount returns the number of unresolved (or resolved-but-uncollected) requests
// for one child — the bounded mechanical figure a status report exposes.
func (m *delegationManager) pendingCount(childID uuid.UUID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, pr := range m.requests {
		if pr.childID == childID {
			count++
		}
	}
	return count
}

// scopedController is the parent-scoped tool.DelegateController for one live parent
// loop. It is the model-facing delegation seam; it holds no session directly, only the
// manager, so the tool never receives the session or a session controller.
type scopedController struct {
	manager      *delegationManager
	parentLoopID uuid.UUID
	style        loop.DelegationStyle
	allowed      map[identity.AgentName]struct{}
}

var _ tool.DelegateController = (*scopedController)(nil)

// Execute enforces the action set for the parent's delegation style, then dispatches.
// The style-derived tool schema is only a hint; this method is the security boundary
// that re-enforces the action set, agent authorization, mode validity, and ownership
// regardless of crafted JSON.
func (c *scopedController) Execute(ctx context.Context, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if c.style == loop.DelegationSyncOnly && (req.Operation != tool.DelegateStart || !req.Wait) {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateActionUnavailable}
	}
	s, ok := c.manager.sess()
	if !ok {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateSessionUnavailable}
	}
	switch req.Operation {
	case tool.DelegateStart:
		return c.start(ctx, s, req)
	case tool.DelegateSend:
		return c.send(ctx, s, req)
	case tool.DelegateWait:
		return c.wait(ctx, s, req)
	case tool.DelegateInterrupt:
		return c.interrupt(s, req)
	case tool.DelegateStatus:
		return c.status(s, req)
	default:
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateUnknownOperation}
	}
}

// start resolves + authorizes the agent and mode BEFORE reserving quota, spawns the
// child (newLoop reserves the quota slot before construction and records the selected
// mode on LoopStarted), then waits or returns a queued handle.
func (c *scopedController) start(ctx context.Context, s *Session, req tool.DelegateRequest) (tool.DelegateResult, error) {
	agent := identity.AgentName(req.Agent)
	if _, authorized := c.allowed[agent]; !authorized {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateUnauthorizedAgent, Agent: agent}
	}
	childDef, known := c.manager.byName[agent]
	if !known {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateUnknownAgent, Agent: agent}
	}
	mode := loop.ModeName(req.Mode)
	if err := validateDelegateMode(childDef, mode); err != nil {
		return tool.DelegateResult{}, err
	}
	parent := loop.Provenance{LoopID: c.parentLoopID}
	childID, requestID, sub, err := s.startDelegate(ctx, parent, childDef, mode, req.Message, req.ParentToolUseID)
	if err != nil {
		return tool.DelegateResult{}, err
	}
	return c.resolveOrQueue(ctx, s, childID, requestID, sub, req)
}

// send enqueues a distinct NON-FOLDING follow-up turn on an owned child and waits or
// returns a queued handle.
func (c *scopedController) send(ctx context.Context, s *Session, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if req.DelegateID.IsZero() {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateMissingDelegateID}
	}
	if err := c.ownsChild(s, req.DelegateID); err != nil {
		return tool.DelegateResult{}, err
	}
	requestID, sub, err := s.sendDelegate(ctx, req.DelegateID, req.Message)
	if err != nil {
		return tool.DelegateResult{}, err
	}
	return c.resolveOrQueue(ctx, s, req.DelegateID, requestID, sub, req)
}

// wait resolves one previously returned request id for an owned child. The request id
// is required because a child may have several queued turns.
func (c *scopedController) wait(ctx context.Context, s *Session, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if req.DelegateID.IsZero() {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateMissingDelegateID}
	}
	if req.RequestID == nil || req.RequestID.IsZero() {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateMissingRequestID}
	}
	if err := c.ownsChild(s, req.DelegateID); err != nil {
		return tool.DelegateResult{}, err
	}
	// Live in-memory handle first: a request registered this process lifetime.
	if pr, ok := c.manager.getPending(*req.RequestID); ok {
		if pr.childID != req.DelegateID {
			return tool.DelegateResult{}, &DelegateError{Kind: DelegateUnknownRequest, RequestID: *req.RequestID}
		}
		waitCtx, cancel := waitContext(ctx, req.TimeoutSeconds)
		defer cancel()
		select {
		case <-pr.done:
			c.manager.removePending(*req.RequestID)
			text, status := pr.result()
			return tool.DelegateResult{DelegateID: req.DelegateID, RequestID: *req.RequestID, Status: status, Output: text}, nil
		case <-waitCtx.Done():
			// The pending request stays registered so a later wait can still collect it.
			status := timeoutOrInterrupted(req.TimeoutSeconds, waitCtx)
			if status == tool.DelegateStatusTimedOut {
				go func() { _ = s.interruptLoopID(req.DelegateID) }()
			}
			return tool.DelegateResult{DelegateID: req.DelegateID, RequestID: *req.RequestID, Status: status}, nil
		}
	}
	// Durable fallback (post-restore): the in-memory handle is gone, but the child's turn
	// terminal survived in the folded history the restore reconstructed. Ownership is
	// already enforced above; the resolved entry must name the same owned child.
	if rr, ok := c.manager.getResolved(*req.RequestID); ok && rr.childID == req.DelegateID {
		return tool.DelegateResult{DelegateID: req.DelegateID, RequestID: *req.RequestID, Status: rr.status, Output: rr.text}, nil
	}
	return tool.DelegateResult{}, &DelegateError{Kind: DelegateUnknownRequest, RequestID: *req.RequestID}
}

// interrupt interrupts an owned child's current turn without destroying the loop.
func (c *scopedController) interrupt(s *Session, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if req.DelegateID.IsZero() {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateMissingDelegateID}
	}
	if err := c.ownsChild(s, req.DelegateID); err != nil {
		return tool.DelegateResult{}, err
	}
	if err := s.interruptLoopID(req.DelegateID); err != nil {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateNotOwned, DelegateID: req.DelegateID}
	}
	return tool.DelegateResult{DelegateID: req.DelegateID, Status: tool.DelegateStatusInterrupted}, nil
}

// status reports bounded mechanical status for one owned child, or all owned children
// when delegate_id is omitted. It never returns a raw event cursor or child transcript.
func (c *scopedController) status(s *Session, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if !req.DelegateID.IsZero() {
		if err := c.ownsChild(s, req.DelegateID); err != nil {
			return tool.DelegateResult{}, err
		}
		return tool.DelegateResult{
			DelegateID:      req.DelegateID,
			Status:          c.childStatus(s, req.DelegateID),
			PendingRequests: c.manager.pendingCount(req.DelegateID),
		}, nil
	}
	children := make([]tool.DelegateChildStatus, 0)
	for _, id := range c.ownedChildren(s) {
		children = append(children, tool.DelegateChildStatus{
			DelegateID:      id,
			Status:          c.childStatus(s, id),
			PendingRequests: c.manager.pendingCount(id),
		})
	}
	return tool.DelegateResult{Children: children}, nil
}

// resolveOrQueue waits for the correlated turn (wait:true) or registers a pending
// request and returns a queued handle (wait:false).
func (c *scopedController) resolveOrQueue(ctx context.Context, s *Session, childID, requestID uuid.UUID, sub event.Subscription, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if req.Wait {
		defer func() { _ = sub.Close() }()
		waitCtx, cancel := waitContext(ctx, req.TimeoutSeconds)
		defer cancel()
		text, err := drainDelegateAnswer(waitCtx, sub, requestID, func() {
			if ierr := s.interruptLoopID(childID); ierr != nil {
				_ = ierr
			}
		})
		status := statusFromDrain(err)
		if status == tool.DelegateStatusInterrupted && didTimeout(req.TimeoutSeconds, waitCtx) {
			status = tool.DelegateStatusTimedOut
		}
		return tool.DelegateResult{DelegateID: childID, RequestID: requestID, Status: status, Output: text}, nil
	}
	c.manager.registerPending(requestID, childID, sub, s.sessionCtx, func() {
		if ierr := s.interruptLoopID(childID); ierr != nil {
			_ = ierr
		}
	})
	return tool.DelegateResult{DelegateID: childID, RequestID: requestID, Status: tool.DelegateStatusQueued}, nil
}

// ownsChild fails closed unless childID is a registered loop whose parent is exactly
// this controller's bound parent — rejecting siblings, ancestors, and unrelated loops.
func (c *scopedController) ownsChild(s *Session, childID uuid.UUID) error {
	s.loopsMu.RLock()
	handle, ok := s.loops[childID]
	s.loopsMu.RUnlock()
	if !ok || handle.parent.LoopID != c.parentLoopID {
		return &DelegateError{Kind: DelegateNotOwned, DelegateID: childID}
	}
	return nil
}

// ownedChildren returns the registered loop ids whose parent is this controller's bound
// parent.
func (c *scopedController) ownedChildren(s *Session) []uuid.UUID {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	var ids []uuid.UUID
	for id, handle := range s.loops {
		if handle.parent.LoopID == c.parentLoopID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// childStatus maps the child's event-derived mechanical runtime state to a bounded value;
// actor exit is a final failed state independent of request-handle collection.
func (c *scopedController) childStatus(s *Session, childID uuid.UUID) tool.DelegateStatusValue {
	s.loopsMu.RLock()
	handle, ok := s.loops[childID]
	s.loopsMu.RUnlock()
	if !ok {
		return tool.DelegateStatusUnknown
	}
	select {
	case <-handle.backend.DoneChan():
		return tool.DelegateStatusFailed
	default:
	}
	return handle.mechanicalState()
}

// validateDelegateMode rejects a non-empty mode that the target definition does not
// declare, WITHOUT spawning. An empty mode uses the definition's initial mode.
func validateDelegateMode(def loop.Definition, mode loop.ModeName) error {
	if mode == "" {
		return nil
	}
	for _, m := range def.Modes() {
		if m.Name == mode {
			return nil
		}
	}
	return &DelegateError{Kind: DelegateUnknownMode, Mode: mode}
}

// statusFromDrain maps a drain terminal error to a delegate status.
func statusFromDrain(err error) tool.DelegateStatusValue {
	if err == nil {
		return tool.DelegateStatusCompleted
	}
	var interrupted *drainInterruptedError
	if errors.As(err, &interrupted) {
		return tool.DelegateStatusInterrupted
	}
	return tool.DelegateStatusFailed
}

// waitContext bounds a waiting operation by an optional non-negative timeout. A nil
// timeout yields an interruptible unbounded wait (only the parent turn ctx ends it).
func waitContext(ctx context.Context, timeoutSeconds *int) (context.Context, context.CancelFunc) {
	if timeoutSeconds == nil {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(*timeoutSeconds)*time.Second)
}

func didTimeout(timeoutSeconds *int, waitCtx context.Context) bool {
	return timeoutSeconds != nil && errors.Is(waitCtx.Err(), context.DeadlineExceeded)
}

func timeoutOrInterrupted(timeoutSeconds *int, waitCtx context.Context) tool.DelegateStatusValue {
	if didTimeout(timeoutSeconds, waitCtx) {
		return tool.DelegateStatusTimedOut
	}
	return tool.DelegateStatusInterrupted
}

// startDelegate runs the transactional child admission path: quota reservation and bind,
// subscription, request mint, backend construction, and initial-command acceptance all
// precede the checked LoopStarted commit. The backend's publisher remains blocked until
// that commit, so TurnStarted can neither race ahead nor survive a failed spawn.
func (s *Session) startDelegate(ctx context.Context, parent loop.Provenance, cfg loop.Definition, mode loop.ModeName, message, parentToolUseID string) (childID, requestID uuid.UUID, sub event.Subscription, err error) {
	admission := &delegateAdmission{ctx: ctx, message: message}
	childID, err = s.newLoopWithAdmission(parent, cfg, parentToolUseID, mode, nil, admission)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, nil, err
	}
	return childID, admission.requestID, admission.sub, nil
}

// sendDelegate enqueues a distinct NON-FOLDING follow-up turn on an existing owned
// child. It subscribes BEFORE the enqueue so the correlated turn's opening event is
// never missed.
func (s *Session) sendDelegate(ctx context.Context, childID uuid.UUID, message string) (requestID uuid.UUID, sub event.Subscription, err error) {
	sub, err = s.subscribeLoop(childID)
	if err != nil {
		return uuid.UUID{}, nil, err
	}
	requestID, err = s.enqueueDelegateTurn(ctx, childID, delegateBlocks(message))
	if err != nil {
		_ = sub.Close()
		return uuid.UUID{}, nil, err
	}
	return requestID, sub, nil
}

// subscribeLoop opens a loop-scoped Enduring subscription (the StepDone + terminals a
// drain needs); Ephemeral is left empty so the child's token firehose never enters the
// egress buffer.
func (s *Session) subscribeLoop(loopID uuid.UUID) (event.Subscription, error) {
	if s.delegateSubscribe != nil {
		return s.delegateSubscribe(event.EventFilter{Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{loopID: {}}}})
	}
	return s.SubscribeEvents(event.EventFilter{
		Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{loopID: {}}},
	})
}

// enqueueDelegateTurn is the internal NON-FOLDING delegate enqueue: a distinct
// machine-originated turn whose minted command id correlates the child's turn. It submits
// with NoFold=true, so even a send to a child that is mid-tool-turn NEVER folds into the
// running turn (the loop actor's drainInbox skips non-folding entries); it queues behind
// the running turn and starts its OWN distinct turn when that finishes. The public
// Session.SubmitToLoop keeps its interactive queue/fold semantics (NoFold=false).
func (s *Session) enqueueDelegateTurn(ctx context.Context, loopID uuid.UUID, blocks []content.Block) (uuid.UUID, error) {
	if err := s.faultIfFaulted(); err != nil {
		return uuid.UUID{}, err
	}
	backend, ok := s.loopFor(loopID)
	if !ok {
		return uuid.UUID{}, &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return uuid.UUID{}, err
	}
	accepted := make(chan error, 1)
	cmd := command.UserInput{Header: command.Header{CommandID: id, Agency: identity.AgencyMachine, CreatedAt: s.stampNow()}, Blocks: blocks, NoFold: true, TargetLoopID: loopID, Accepted: accepted}
	if err := s.appendDelegateCommand(ctx, loopID, cmd); err != nil {
		return uuid.UUID{}, err
	}
	select {
	case backend.CommandSink() <- cmd:
		select {
		case err := <-accepted:
			if err != nil {
				return uuid.UUID{}, &SessionError{Kind: SessionDelegateIntentAppendFailed, Cause: err}
			}
			return id, nil
		case <-ctx.Done():
			return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
		case <-backend.DoneChan():
			return uuid.UUID{}, &SessionError{Kind: SessionLoopExited}
		}
	case <-ctx.Done():
		return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-backend.DoneChan():
		return uuid.UUID{}, &SessionError{Kind: SessionLoopExited}
	}
}

func delegateBlocks(message string) []content.Block {
	return []content.Block{&content.TextBlock{Text: message}}
}

// DelegateErrorKind classifies a delegation refusal. Every refusal denies by default
// (fail-secure); the model-facing tool renders it as a tool-result string.
type DelegateErrorKind uint8

const (
	DelegateActionUnavailable DelegateErrorKind = iota + 1
	DelegateUnknownAgent
	DelegateUnauthorizedAgent
	DelegateUnknownMode
	DelegateNotOwned
	DelegateSessionUnavailable
	DelegateUnknownRequest
	DelegateMissingDelegateID
	DelegateMissingRequestID
	DelegateUnknownOperation
)

// DelegateError is the typed delegation refusal. Callers errors.As to inspect Kind and
// the offending agent/mode/ids.
type DelegateError struct {
	Kind       DelegateErrorKind
	Agent      identity.AgentName
	Mode       loop.ModeName
	DelegateID uuid.UUID
	RequestID  uuid.UUID
}

func (e *DelegateError) Error() string {
	switch e.Kind {
	case DelegateActionUnavailable:
		return "delegation: action is unavailable for this loop's delegation style"
	case DelegateUnknownAgent:
		return "delegation: unknown delegate agent " + strconv.Quote(string(e.Agent))
	case DelegateUnauthorizedAgent:
		return "delegation: agent " + strconv.Quote(string(e.Agent)) + " is not an authorized delegate of this loop"
	case DelegateUnknownMode:
		return "delegation: mode " + strconv.Quote(string(e.Mode)) + " is not declared by the target agent"
	case DelegateNotOwned:
		return "delegation: delegate " + strconv.Quote(e.DelegateID.String()) + " is not owned by this loop"
	case DelegateSessionUnavailable:
		return "delegation: session is not available"
	case DelegateUnknownRequest:
		return "delegation: unknown request " + strconv.Quote(e.RequestID.String())
	case DelegateMissingDelegateID:
		return "delegation: a delegate_id is required"
	case DelegateMissingRequestID:
		return "delegation: a request_id is required"
	case DelegateUnknownOperation:
		return "delegation: unknown operation"
	default:
		return "delegation: refused"
	}
}
