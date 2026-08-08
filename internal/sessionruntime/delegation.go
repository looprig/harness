package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/delegationtool"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

// delegation.go is the session-runtime delegation manager (design §"Synchronous and
// managed delegation"/§"Follow-up request and answer semantics"). It vends a SEPARATE
// parent-scoped tool.DelegateController for each live parent loop and injects it into
// that loop's atomic agent-tool bundle. A scoped controller addresses ONLY children owned by its
// bound parent (registry-derived ownership, restore-safe): it rejects siblings,
// ancestors, unrelated loop ids, unavailable actions, and invalid modes. The parent
// model never receives the session or the manager — only the narrow scoped controller.
//
// OWNERSHIP survives restore because the direct-child index is rebuilt from the loop
// registry's durable parent links after attachRestoredLoop re-seeds each loop. The
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
	requests map[uuid.UUID]*requestTracker
	// resolved is the DURABLE request→terminal index reconstructed at restore from each
	// loop's folded history (request id → the terminal of the correlated turn). It is the
	// private correlation state needed by future restore idempotence: live request trackers
	// do not survive a process restart, but committed child terminals do. It is deliberately
	// not exposed through a request-ID lookup API. Guarded by mu.
	resolved               map[uuid.UUID]resolvedRequest
	runtimeCatalog         loop.RuntimeCatalog
	hasRuntimeCatalog      bool
	runtimeCatalogProvider RuntimeCatalogProvider
}

type delegateAdmission struct {
	ctx             context.Context
	name            string
	message         string
	sub             event.Subscription
	requestID       uuid.UUID
	command         command.UserInput
	publisher       *delegateAdmissionPublisher
	registerRequest func(requestID, childID uuid.UUID) *requestTracker
	tracked         *requestTracker
	background      bool
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

func newDelegationManager(topology Topology, catalogs ...loop.RuntimeCatalog) *delegationManager {
	byName := make(map[identity.AgentName]loop.Definition, len(topology.Definitions))
	for _, def := range topology.Definitions {
		byName[def.Name()] = def
	}
	manager := &delegationManager{
		byName:   byName,
		requests: make(map[uuid.UUID]*requestTracker),
		resolved: make(map[uuid.UUID]resolvedRequest),
	}
	if len(catalogs) > 0 {
		manager.runtimeCatalog = catalogs[0]
		manager.hasRuntimeCatalog = true
	}
	return manager
}

func newDelegationManagerWithCatalogProvider(topology Topology, provider RuntimeCatalogProvider) *delegationManager {
	manager := newDelegationManager(topology)
	manager.runtimeCatalogProvider = provider
	return manager
}

func (m *delegationManager) catalogFor(parent loop.Definition) (loop.RuntimeCatalog, bool) {
	if m == nil {
		return loop.RuntimeCatalog{}, false
	}
	if m.runtimeCatalogProvider != nil {
		return m.runtimeCatalogProvider(parent)
	}
	if m.hasRuntimeCatalog {
		return m.runtimeCatalog, true
	}
	return loop.RuntimeCatalog{}, false
}

// seedResolvedDelegateRecords reconstructs durable delegate correlation from required
// machine NoFold intents, then overlays exact started-turn terminals and crash closures.
func seedResolvedDelegateRecords(m *delegationManager, records []journal.JournalRecord, replayed, closures []event.Event, tombstonedSet ...map[uuid.UUID]struct{}) error {
	tombstoned := map[uuid.UUID]struct{}{}
	if len(tombstonedSet) > 0 && tombstonedSet[0] != nil {
		tombstoned = tombstonedSet[0]
	}
	intents, err := backgroundDelegateIntents(records)
	if err != nil {
		return err
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
	for requestID, childID := range intents {
		if _, ok := tombstoned[childID]; ok {
			index[requestID] = resolvedRequest{childID: childID, status: tool.DelegateStatusFailed}
		}
	}
	// A queued request may be durably cancelled before TurnStarted, so it has no
	// turn terminal to fold. Overlay only cancellations for already admitted
	// intent+acceptance IDs; ordinary/unaccepted cancellations remain invisible.
	for _, ev := range combined {
		cancelled, ok := ev.(event.InputCancelled)
		if !ok || cancelled.Cause.CommandID.IsZero() {
			continue
		}
		admitted, ok := index[cancelled.Cause.CommandID]
		if !ok {
			continue
		}
		if cancelled.LoopID != admitted.childID {
			return &journal.CommandRouteMismatchError{RecordLoopID: cancelled.LoopID, TargetLoopID: admitted.childID}
		}
		index[cancelled.Cause.CommandID] = resolvedRequest{
			childID: admitted.childID,
			status:  delegateStatusFromCancelReason(cancelled.Reason),
		}
	}
	m.mu.Lock()
	m.resolved = index
	m.mu.Unlock()
	return nil
}

// backgroundDelegateIntents returns the durable request-to-child map for the one
// managed request shape whose completion must cross the child-to-parent boundary
// after a process restart. The marker is intentionally narrower than "machine
// NoFold": foreground responses and ordinary machine input must never be replayed
// as parent hand-backs.
func backgroundDelegateIntents(records []journal.JournalRecord) (map[uuid.UUID]uuid.UUID, error) {
	intents := make(map[uuid.UUID]uuid.UUID)
	for _, record := range records {
		commandRecord, ok := record.(journal.CommandRecord)
		if !ok {
			continue
		}
		if err := journal.ValidateCommandRecordRoute(commandRecord); err != nil {
			return nil, err
		}
		input, ok := commandRecord.Command().(command.UserInput)
		if !ok || !input.BackgroundHandBack || !input.NoFold || input.Agency != identity.AgencyMachine || input.TargetLoopID.IsZero() || input.CommandID.IsZero() {
			continue
		}
		intents[input.CommandID] = input.TargetLoopID
	}
	return intents, nil
}

func (m *delegationManager) getResolved(requestID uuid.UUID) (resolvedRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resolved, ok := m.resolved[requestID]
	return resolved, ok
}

// restoredBackgroundPlan is the durable work that must be reconciled after a
// successful restore. A hand-back command is retained verbatim when it exists so
// replay does not mint a second completion command for the same response.
type restoredBackgroundPlan struct {
	requestID uuid.UUID
	childID   uuid.UUID
	parentID  uuid.UUID
	name      string
	resolved  resolvedRequest
	handBack  *command.SubagentResult
}

// planRestoredBackgroundRequests reconstructs the durable child-terminal to
// parent-input correlation before RestoreDone. The plan deliberately treats
// TurnStarted/TurnFoldedInto as processed completion evidence. InputQueued is
// ephemeral and therefore cannot be an idempotence key; a durable
// DelegateRequestAccepted may prove admission, but the original command is still
// replayed because the restored actor does not reconstruct its ephemeral inbox.
func (m *delegationManager) planRestoredBackgroundRequests(s *Session, records []journal.JournalRecord, replayed, closures []event.Event) ([]restoredBackgroundPlan, error) {
	intents, err := backgroundDelegateIntents(records)
	if err != nil {
		return nil, err
	}

	type childInfo struct {
		parent uuid.UUID
		name   string
	}
	children := make(map[uuid.UUID]childInfo)
	for _, ev := range replayed {
		started, ok := ev.(event.LoopStarted)
		if !ok || started.LoopID.IsZero() {
			continue
		}
		name := started.DisplayName
		if name == "" {
			name = string(started.AgentName)
		}
		children[started.LoopID] = childInfo{parent: started.Cause.Coordinates.LoopID, name: name}
	}
	// A degraded/tombstoned child is still registered before this planner runs. Use
	// the live registry as a fallback for legacy LoopStarted records that predate the
	// durable display-name field.
	s.loopsMu.RLock()
	for childID := range intents {
		if _, ok := children[childID]; ok {
			continue
		}
		if handle := s.loops[childID]; handle != nil {
			name := handle.agentName
			if name == "" && handle.bound != nil {
				name = handle.bound.DisplayName()
			}
			if name == "" && handle.bound != nil {
				name = string(handle.bound.Name())
			}
			children[childID] = childInfo{parent: handle.parent.LoopID, name: name}
		}
	}
	s.loopsMu.RUnlock()

	// Keep the latest durable hand-back command for each response. The normal live
	// path appends at most one; choosing the latest makes recovery deterministic even
	// for a journal produced by an older retrying implementation.
	type handBackRecord struct {
		commandID uuid.UUID
		command   command.SubagentResult
		childID   uuid.UUID
		parentID  uuid.UUID
	}
	handBacks := make(map[uuid.UUID]handBackRecord)
	for _, record := range records {
		commandRecord, ok := record.(journal.CommandRecord)
		if !ok {
			continue
		}
		handBack, ok := commandRecord.Command().(command.SubagentResult)
		if !ok {
			continue
		}
		envelope, ok := decodeBackgroundCompletion(handBack.Blocks)
		if !ok {
			continue
		}
		requestID, parseErr := uuid.Parse(envelope.CorrelationID)
		if parseErr != nil || requestID.IsZero() {
			continue
		}
		childID := handBack.Cause.LoopID
		parentID := handBack.Coordinates.LoopID
		if childID.IsZero() || parentID.IsZero() {
			return nil, &journal.CommandRouteMismatchError{RecordLoopID: parentID, TargetLoopID: childID}
		}
		if target, admitted := intents[requestID]; admitted && target != childID {
			return nil, &journal.CommandRouteMismatchError{RecordLoopID: childID, TargetLoopID: target}
		}
		if info, known := children[childID]; known && !info.parent.IsZero() && info.parent != parentID {
			return nil, &journal.CommandRouteMismatchError{RecordLoopID: parentID, TargetLoopID: info.parent}
		}
		if envelope.AgentID != "" {
			agentID, agentErr := uuid.Parse(envelope.AgentID)
			if agentErr != nil || agentID != childID {
				return nil, &journal.CommandRouteMismatchError{RecordLoopID: childID, TargetLoopID: childID}
			}
		}
		handBacks[requestID] = handBackRecord{commandID: handBack.CommandID, command: handBack, childID: childID, parentID: parentID}
	}

	processed := make(map[uuid.UUID]struct{})
	for _, ev := range append(append([]event.Event(nil), replayed...), closures...) {
		commandID := ev.EventHeader().Cause.CommandID
		// The completion envelope is also durable in the parent turn input.
		// This correlation survives even when the audit-only SubagentResult
		// command append failed, so a processed completion cannot be replayed
		// merely because its command record is absent.
		var message *content.UserMessage
		switch typed := ev.(type) {
		case event.TurnStarted:
			message = typed.Message
		case event.TurnFoldedInto:
			message = typed.Message
		}
		if message != nil {
			if envelope, ok := decodeBackgroundCompletion(message.Blocks); ok {
				if requestID, parseErr := uuid.Parse(envelope.CorrelationID); parseErr == nil && !requestID.IsZero() {
					processed[requestID] = struct{}{}
				}
			}
		}
		if commandID.IsZero() {
			continue
		}
		var isProcessed bool
		switch ev.(type) {
		case event.TurnStarted, event.TurnFoldedInto, event.TurnRejected:
			isProcessed = true
		}
		if !isProcessed {
			continue
		}
		for _, handBack := range handBacks {
			if handBack.commandID == commandID && ev.EventHeader().LoopID == handBack.parentID {
				processed[commandID] = struct{}{}
			}
		}
	}

	requestIDs := make([]uuid.UUID, 0, len(intents))
	for requestID := range intents {
		if _, resolved := m.getResolved(requestID); resolved {
			requestIDs = append(requestIDs, requestID)
		}
	}
	sort.Slice(requestIDs, func(i, j int) bool { return requestIDs[i].String() < requestIDs[j].String() })

	plan := make([]restoredBackgroundPlan, 0, len(requestIDs))
	for _, requestID := range requestIDs {
		if _, done := processed[requestID]; done {
			continue
		}
		resolved, ok := m.getResolved(requestID)
		if !ok {
			continue
		}
		childID := intents[requestID]
		if resolved.childID != childID {
			return nil, &journal.CommandRouteMismatchError{RecordLoopID: resolved.childID, TargetLoopID: childID}
		}
		info, known := children[childID]
		if !known || info.parent.IsZero() {
			// A malformed legacy record cannot be safely routed. Leave it in the
			// private resolution index for diagnostics, but do not invent a parent.
			continue
		}
		entry := restoredBackgroundPlan{requestID: requestID, childID: childID, parentID: info.parent, name: info.name, resolved: resolved}
		if handBack, exists := handBacks[requestID]; exists {
			if _, done := processed[handBack.commandID]; !done {
				copy := handBack.command
				entry.handBack = &copy
			}
			// A processed hand-back is intentionally omitted from the plan.
			if _, done := processed[handBack.commandID]; done {
				continue
			}
		}
		plan = append(plan, entry)
	}
	return plan, nil
}

// reconcileRestoredBackgroundRequests admits each planned completion once. Existing
// command records are re-sent verbatim; only a response with no prior hand-back
// command mints and appends a new SubagentResult. Wake ownership is restored before
// dispatch and released by the normal parent event path.
func (m *delegationManager) reconcileRestoredBackgroundRequests(s *Session, plan []restoredBackgroundPlan) {
	for _, entry := range plan {
		s.expectTurn(s.sessionCtx, entry.childID)
		var err error
		if entry.handBack != nil {
			err = s.replaySubagentResult(s.sessionCtx, *entry.handBack)
		} else {
			err = s.deliverSubagentResult(s.sessionCtx, entry.parentID, entry.childID, backgroundCompletionBlocks(entry.childID, entry.name, entry.requestID, entry.resolved.status, entry.resolved.text))
		}
		if err != nil {
			// No parent event can release the restored wake token when transport
			// fails. The next restore can retry from the durable terminal/command.
			s.cancelExpectTurn(context.Background(), entry.childID)
		}
	}
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
				out[k.commandID] = resolvedRequest{childID: k.loopID, status: tool.DelegateStatusFailed, text: delegateFailureDetail(e.Err)}
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

// delegateExtraTools derives the model-facing collaboration tools for a parent definition:
// a loop with a non-empty Delegates() gets exactly one bundle producing four tools whose
// immutable catalogue is its delegate set. A loop with no delegates gets no agent
// collaboration capability. It is a pure function of the frozen
// definition, so the derivation is deterministic across New and Restore (the delegate set
// and style are part of the definition fingerprint). The session injects the result via
// tool.Bindings.ExtraTools at the loop's bind site — the user never hand-adds it.
func delegateExtraTools(def loop.Definition, manager *delegationManager) []tool.Definition {
	delegates := def.Delegates()
	if len(delegates) == 0 {
		return nil
	}
	catalog := make([]delegationtool.AgentCatalogEntry, len(delegates))
	for i, name := range delegates {
		entry := delegationtool.AgentCatalogEntry{Name: name}
		if manager != nil {
			if target, ok := manager.byName[name]; ok {
				entry.Description = target.Description()
				for _, mode := range target.Modes() {
					entry.Modes = append(entry.Modes, mode.Name)
				}
			}
		}
		catalog[i] = entry
	}
	if manager != nil {
		if runtimeCatalog, ok := manager.catalogFor(def); ok {
			return []tool.Definition{delegationtool.Definition(def.Delegation().Style, catalog, runtimeCatalog)}
		}
	}
	return []tool.Definition{delegationtool.Definition(def.Delegation().Style, catalog)}
}

// controllerFor builds the parent-scoped controller injected into one loop's atomic
// agent-tool bundle. The allowed delegate set and delegation style are derived from the PARENT
// definition (least privilege). It tolerates a nil manager receiver so a struct-literal
// session with no delegation manager can still bind loops that carry no agent tools.
func (m *delegationManager) controllerFor(parentLoopID uuid.UUID, parent loop.Definition) tool.DelegateController {
	allowed := make(map[identity.AgentName]struct{})
	for _, name := range parent.Delegates() {
		allowed[name] = struct{}{}
	}
	controller := &scopedController{
		manager:      m,
		parentLoopID: parentLoopID,
		style:        parent.Delegation().Style,
		allowed:      allowed,
	}
	if m != nil {
		controller.runtimeCatalog, controller.hasRuntimeCatalog = m.catalogFor(parent)
	}
	return controller
}

// requestTracker holds only the live mechanical lifecycle needed to count messages queued
// behind a child's active turn. Response collection belongs to the foreground drain or the
// session-owned background hand-back; no response payload is retained here for later lookup.
type requestTracker struct {
	childID uuid.UUID

	mu        sync.Mutex
	lifecycle requestLifecycle
}

type requestLifecycle uint8

const (
	requestQueued requestLifecycle = iota
	requestActive
	requestTerminal
)

func (p *requestTracker) markActive() {
	p.mu.Lock()
	if p.lifecycle == requestQueued {
		p.lifecycle = requestActive
	}
	p.mu.Unlock()
}

func (p *requestTracker) markTerminal() {
	p.mu.Lock()
	p.lifecycle = requestTerminal
	p.mu.Unlock()
}

func (p *requestTracker) queued() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lifecycle == requestQueued
}

func (p *requestTracker) terminal() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lifecycle == requestTerminal
}

// registerRequest records every request immediately after accepted admission, before
// either foreground or background draining begins. Since callers subscribe before
// admission, the correlated TurnStarted remains observable even if it raced ahead.
func (m *delegationManager) registerRequest(requestID, childID uuid.UUID) *requestTracker {
	tracked := &requestTracker{childID: childID, lifecycle: requestActive}
	m.mu.Lock()
	for _, existing := range m.requests {
		if existing.childID == childID && !existing.terminal() {
			tracked.lifecycle = requestQueued
			break
		}
	}
	m.requests[requestID] = tracked
	m.mu.Unlock()
	return tracked
}

// markRequestActive advances only a request already admitted through delegation. An
// unrelated TurnStarted command id never creates tracker state.
func (m *delegationManager) markRequestActive(requestID, childID uuid.UUID) {
	if m == nil || requestID.IsZero() {
		return
	}
	m.mu.Lock()
	tracked := m.requests[requestID]
	m.mu.Unlock()
	if tracked != nil && tracked.childID == childID {
		tracked.markActive()
	}
}

func (m *delegationManager) removeRequest(requestID uuid.UUID, tracked *requestTracker) {
	m.mu.Lock()
	if m.requests[requestID] == tracked {
		delete(m.requests, requestID)
	}
	m.mu.Unlock()
}

// handBackRequest starts the session-owned drain for one background response. The drain
// runs on the session lifetime, not the parent tool-call context. Its only terminal
// delivery is a machine-originated SubagentResult to the direct parent.
func (m *delegationManager) handBackRequest(s *Session, parentID, childID uuid.UUID, name string, tracked *requestTracker, requestID uuid.UUID, sub event.Subscription, timeoutSeconds *int) {
	go func() {
		defer func() { _ = sub.Close() }()
		defer m.removeRequest(requestID, tracked)
		waitCtx, cancel := waitContext(s.sessionCtx, timeoutSeconds)
		defer cancel()
		text, err := drainDelegateAnswerObserved(waitCtx, sub, requestID, func() {
			_, _ = s.cancelDelegateRequest(childID, requestID)
		}, tracked.markActive)
		status := statusFromDrain(err)
		if status == tool.DelegateStatusInterrupted && didTimeout(timeoutSeconds, waitCtx) {
			status = tool.DelegateStatusTimedOut
		}
		if status == tool.DelegateStatusFailed && text == "" {
			text = delegateFailureDetail(err)
		}
		tracked.markTerminal()
		if err := s.deliverSubagentResult(s.sessionCtx, parentID, childID, backgroundCompletionBlocks(childID, name, requestID, status, text)); err != nil {
			// No parent event can release the hand-back token when dispatch itself is
			// impossible (session shutdown, parent exit, or command-id failure).
			s.cancelExpectTurn(context.Background(), childID)
		}
	}()
}

// pendingCount returns the requests waiting behind one child's active turn. Active and
// terminal requests still crossing the hand-back boundary are not queued messages.
func (m *delegationManager) pendingCount(childID uuid.UUID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, pr := range m.requests {
		if pr.childID == childID && pr.queued() {
			count++
		}
	}
	return count
}

// scopedController is the parent-scoped tool.DelegateController for one live parent
// loop. It is the model-facing delegation seam; it holds no session directly, only the
// manager, so the tool never receives the session or a session controller.
type scopedController struct {
	manager           *delegationManager
	parentLoopID      uuid.UUID
	style             loop.DelegationStyle
	allowed           map[identity.AgentName]struct{}
	runtimeCatalog    loop.RuntimeCatalog
	hasRuntimeCatalog bool
}

const maxDelegateStatusChildren = 256

var _ tool.DelegateController = (*scopedController)(nil)

// Execute enforces the action set for the parent's delegation style, then dispatches.
// The style-derived tool schema is only a hint; this method is the security boundary
// that re-enforces the action set, agent authorization, mode validity, and ownership
// regardless of crafted JSON.
func (c *scopedController) Execute(ctx context.Context, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if c.style == loop.DelegationSyncOnly && (req.Operation != tool.DelegateStart || !req.WaitForResponse) {
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
	agent := identity.AgentName(req.AgentType)
	if _, authorized := c.allowed[agent]; !authorized {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateUnauthorizedAgent, Agent: agent}
	}
	childDef, known := c.manager.byName[agent]
	if !known {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateUnknownAgent, Agent: agent}
	}
	mode := loop.ModeName(req.AgentMode)
	if err := validateDelegateMode(childDef, mode); err != nil {
		return tool.DelegateResult{}, err
	}
	var runtime *loop.Resolved
	if c.hasRuntimeCatalog {
		entries := c.runtimeCatalog.EntriesFor(agent)
		if c.runtimeCatalog.HasEntries() && len(entries) == 0 {
			return tool.DelegateResult{}, &DelegateError{Kind: DelegateRuntimeUnavailable}
		}
		if req.Runtime == nil && len(entries) > 0 {
			resolved, err := c.runtimeCatalog.Resolve(agent, "", "", inferencemodel.EffortNone)
			if err != nil {
				return tool.DelegateResult{}, &DelegateError{Kind: DelegateRuntimeInvalid}
			}
			runtime = &resolved
		}
	}
	if req.Runtime != nil {
		if !c.hasRuntimeCatalog {
			return tool.DelegateResult{}, &DelegateError{Kind: DelegateRuntimeUnavailable}
		}
		if !validControllerRuntimeSelection(c.runtimeCatalog, agent, *req.Runtime) {
			return tool.DelegateResult{}, &DelegateError{Kind: DelegateRuntimeInvalid}
		}
		resolved, err := c.runtimeCatalog.ResolveWithExplicitSource(
			agent,
			loop.AgentHarnessName(req.Runtime.Harness),
			loop.RuntimeSourceName(req.Runtime.Source),
			loop.ModelAlias(req.Runtime.Model),
			parseRuntimeEffort(req.Runtime.Effort),
			req.Runtime.Explicit.Effort,
		)
		resolvedEffort := runtimeEffortString(resolved.Effort)
		if resolved.SelectionKind == loop.RuntimeSelectionHarnessManaged {
			resolvedEffort = ""
		}
		if err != nil || string(resolved.AgentHarness) != req.Runtime.Harness || string(resolved.Profile) != req.Runtime.Profile || string(resolved.ModelAlias) != req.Runtime.Model || string(resolved.SmallModel) != req.Runtime.SmallModel || resolvedEffort != req.Runtime.Effort || (req.Runtime.Source != "" && string(resolved.Source) != req.Runtime.Source) || (req.Runtime.SelectionKind != "" && string(resolved.SelectionKind) != req.Runtime.SelectionKind) {
			return tool.DelegateResult{}, &DelegateError{Kind: DelegateRuntimeInvalid}
		}
		runtime = &resolved
	}
	// Interrupt admission gate: a parent under an interrupt barrier admits no NEW machine
	// delegate work (the machine side of the interrupt queue policy), so a parent whose
	// interrupted delegate wait resolves cannot open a fresh delegate step.
	//
	// TODO(loopruntime interrupt): this gate closes only the DELEGATE-admission race. A parent
	// can still take one bounded NON-delegate step (a plain inference/tool step) before its own
	// actor interrupt lands; closing that residual needs a step-boundary guard in the loop actor
	// (internal/loopruntime), which is out of Task 11's session-layer scope.
	if s.loopInterruptPending(c.parentLoopID) {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateInterruptPending, DelegateID: c.parentLoopID}
	}
	parent := loop.Provenance{LoopID: c.parentLoopID}
	childID, requestID, sub, tracked, err := s.startDelegate(ctx, parent, childDef, mode, req.Name, req.Message, req.ParentToolUseID, runtime, c.runtimeCatalog, c.hasRuntimeCatalog, !req.WaitForResponse, c.manager.registerRequest)
	if err != nil {
		return tool.DelegateResult{}, err
	}
	return c.resolveOrQueue(ctx, s, childID, requestID, sub, tracked, req)
}

// validControllerRuntimeSelection re-applies the model-facing capability rules
// at the controller boundary. Preparation normally supplies the Explicit bits,
// but a caller can bypass schema/preparation and invoke the typed controller
// directly, so omitted selectors must still equal catalog defaults and explicit
// selectors must correspond to an actually selectable branch.
func validControllerRuntimeSelection(catalog loop.RuntimeCatalog, agent identity.AgentName, runtime tool.DelegateRuntime) bool {
	entries := catalog.EntriesFor(agent)
	var selected *loop.RuntimeCatalogEntry
	var selectedModel *loop.RuntimeModelOption
	for i := range entries {
		entry := &entries[i]
		if entry.AgentHarness != loop.AgentHarnessName(runtime.Harness) {
			continue
		}
		entrySource := controllerRuntimeEntrySource(*entry)
		if runtime.Source != "" && !controllerRuntimeEntryHasSource(*entry, loop.RuntimeSourceName(runtime.Source)) {
			continue
		}
		if runtime.SelectionKind != "" && string(entry.SelectionKind) != runtime.SelectionKind {
			continue
		}
		if runtime.Model != "" && len(entry.Models) == 0 {
			continue
		}
		if runtime.Model != "" {
			var foundModel *loop.RuntimeModelOption
			for optionIndex := range entry.Models {
				option := &entry.Models[optionIndex]
				optionSource := controllerRuntimeOptionSource(*entry, *option)
				if string(option.Alias) != runtime.Model {
					continue
				}
				if runtime.Source != "" && optionSource != loop.RuntimeSourceName(runtime.Source) {
					continue
				}
				if runtime.Source == "" && optionSource != entrySource {
					continue
				}
				foundModel = option
				break
			}
			if foundModel == nil {
				continue
			}
			selectedModel = foundModel
		}
		if runtime.Model == "" && runtime.Source == "" && runtime.SelectionKind == "" && !entry.Default {
			continue
		}
		selected = entry
		if entry.Default {
			break
		}
	}
	if selected == nil {
		return false
	}
	if runtime.Explicit.Source && !controllerRuntimeSourceSelectable(entries, selected.AgentHarness) {
		return false
	}
	if runtime.Explicit.Source && runtime.Source == "" {
		return false
	}
	seenHarnesses := make(map[loop.AgentHarnessName]struct{}, len(entries))
	for _, entry := range entries {
		seenHarnesses[entry.AgentHarness] = struct{}{}
	}
	harnessSelectable := len(seenHarnesses) > 1
	if runtime.Explicit.Harness {
		if !harnessSelectable {
			return false
		}
	} else if !selected.Default {
		defaultEntry := runtimeDefaultControllerEntry(entries)
		if selected.AgentHarness != defaultEntry.AgentHarness || !runtime.Explicit.Source {
			return false
		}
	}
	selectionKind := selected.SelectionKind
	if selectionKind == "" {
		selectionKind = loop.RuntimeSelectionExplicit
	}
	entrySource := controllerRuntimeEntrySource(*selected)
	source := entrySource
	if selectedModel != nil {
		source = controllerRuntimeOptionSource(*selected, *selectedModel)
	}
	if runtime.Source != "" {
		if source != loop.RuntimeSourceName(runtime.Source) {
			return false
		}
	} else if selectedModel != nil && source != entrySource {
		// The omitted source means the entry's source. An option whose source
		// overrides that entry source requires the explicit source branch.
		return false
	}
	if runtime.SelectionKind != "" && runtime.SelectionKind != string(selectionKind) {
		return false
	}
	if selectionKind == loop.RuntimeSelectionHarnessManaged {
		return source == loop.RuntimeSourceNative && !runtime.Explicit.Model && !runtime.Explicit.Effort && runtime.Model == "" && runtime.SmallModel == "" && runtime.Effort == ""
	}
	sourceModels := controllerRuntimeModelsForSource(*selected, source)
	if runtime.Explicit.Model {
		if !controllerRuntimeModelAdmitted(sourceModels, loop.ModelAlias(runtime.Model)) {
			return false
		}
	} else {
		defaultModel := controllerRuntimeDefaultModel(*selected, source)
		if selectedModel == nil || defaultModel.Alias == "" || runtime.Model != string(defaultModel.Alias) {
			return false
		}
	}
	if selectedModel == nil {
		return false
	}
	if runtime.Explicit.Effort {
		if !controllerRuntimeEffortAdmitted(*selectedModel, parseRuntimeEffort(runtime.Effort)) {
			return false
		}
	} else if runtime.Effort != runtimeEffortString(selectedModel.DefaultEffort) {
		return false
	}
	return true
}

func controllerRuntimeModelAdmitted(models []loop.RuntimeModelOption, alias loop.ModelAlias) bool {
	for _, option := range models {
		if option.Alias == alias {
			return true
		}
	}
	return false
}

func controllerRuntimeEffortAdmitted(option loop.RuntimeModelOption, effort inferencemodel.Effort) bool {
	efforts := option.Efforts
	if len(efforts) == 0 {
		efforts = []inferencemodel.Effort{option.DefaultEffort}
	}
	for _, admitted := range efforts {
		if admitted == effort {
			return true
		}
	}
	return false
}

func controllerRuntimeEntrySource(entry loop.RuntimeCatalogEntry) loop.RuntimeSourceName {
	if entry.Source != "" {
		return entry.Source
	}
	switch entry.Credential {
	case loop.CredentialGatewayBacked:
		return loop.RuntimeSourceGateway
	case loop.CredentialNativeAuth:
		return loop.RuntimeSourceNative
	default:
		return ""
	}
}

func controllerRuntimeOptionSource(entry loop.RuntimeCatalogEntry, option loop.RuntimeModelOption) loop.RuntimeSourceName {
	if option.Source != "" {
		return option.Source
	}
	switch option.Credential {
	case loop.CredentialGatewayBacked:
		return loop.RuntimeSourceGateway
	case loop.CredentialNativeAuth:
		return loop.RuntimeSourceNative
	default:
		return controllerRuntimeEntrySource(entry)
	}
}

func controllerRuntimeEntryHasSource(entry loop.RuntimeCatalogEntry, source loop.RuntimeSourceName) bool {
	if controllerRuntimeEntrySource(entry) == source {
		return true
	}
	for _, option := range entry.Models {
		if controllerRuntimeOptionSource(entry, option) == source {
			return true
		}
	}
	return false
}

func controllerRuntimeModelsForSource(entry loop.RuntimeCatalogEntry, source loop.RuntimeSourceName) []loop.RuntimeModelOption {
	models := make([]loop.RuntimeModelOption, 0, len(entry.Models))
	for _, option := range entry.Models {
		if controllerRuntimeOptionSource(entry, option) == source {
			models = append(models, option)
		}
	}
	return models
}

func controllerRuntimeDefaultModel(entry loop.RuntimeCatalogEntry, source loop.RuntimeSourceName) loop.RuntimeModelOption {
	models := controllerRuntimeModelsForSource(entry, source)
	for _, option := range models {
		if option.Alias == entry.DefaultModel {
			return option
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return loop.RuntimeModelOption{}
}

func controllerRuntimeSourceSelectable(entries []loop.RuntimeCatalogEntry, harness loop.AgentHarnessName) bool {
	sources := make(map[loop.RuntimeSourceName]struct{})
	for _, entry := range entries {
		if entry.AgentHarness != harness {
			continue
		}
		sources[entry.Source] = struct{}{}
		for _, option := range entry.Models {
			source := option.Source
			if source == "" {
				source = entry.Source
			}
			sources[source] = struct{}{}
		}
	}
	return len(sources) > 1
}

func runtimeDefaultControllerEntry(entries []loop.RuntimeCatalogEntry) loop.RuntimeCatalogEntry {
	for _, entry := range entries {
		if entry.Default {
			return entry
		}
	}
	if len(entries) == 0 {
		return loop.RuntimeCatalogEntry{}
	}
	return entries[0]
}

// send enqueues a distinct NON-FOLDING follow-up turn on an owned child and waits or
// returns a queued handle.
func (c *scopedController) send(ctx context.Context, s *Session, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if req.AgentID.IsZero() {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateMissingDelegateID}
	}
	if err := c.ownsChild(s, req.AgentID); err != nil {
		return tool.DelegateResult{}, err
	}
	// Interrupt admission gate: same machine-side flush as start — a parent under an interrupt
	// barrier cannot enqueue a fresh follow-up turn on its child until the barrier releases.
	// TODO(loopruntime interrupt): as in start, this closes only the delegate-admission race; the
	// bounded non-delegate-step residual needs a loop-actor step-boundary guard (out of scope).
	if s.loopInterruptPending(c.parentLoopID) {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateInterruptPending, DelegateID: c.parentLoopID}
	}
	requestID, sub, tracked, err := s.sendDelegate(ctx, req.AgentID, req.Message, !req.WaitForResponse, c.manager.registerRequest, c.manager.removeRequest)
	if err != nil {
		return tool.DelegateResult{}, err
	}
	return c.resolveOrQueue(ctx, s, req.AgentID, requestID, sub, tracked, req)
}

// interrupt interrupts an owned child's current turn without destroying the loop.
func (c *scopedController) interrupt(s *Session, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if req.AgentID.IsZero() {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateMissingDelegateID}
	}
	if err := c.ownsChild(s, req.AgentID); err != nil {
		return tool.DelegateResult{}, err
	}
	previousState := c.childState(s, req.AgentID)
	var err error
	if s.hub == nil {
		// Keep struct-literal controller tests and other headless seams on the
		// original best-effort primitive; runInterrupt's release policy needs the
		// live session hub/idle model.
		err = s.interruptLoopID(req.AgentID)
	} else {
		err = s.interruptSingle(context.Background(), req.AgentID)
	}
	if err != nil {
		return tool.DelegateResult{}, &DelegateError{Kind: DelegateNotOwned, DelegateID: req.AgentID}
	}
	return tool.DelegateResult{AgentID: req.AgentID, PreviousState: previousState, State: tool.AgentStateIdle}, nil
}

// status reports bounded mechanical status for one owned child, or all owned children
// when delegate_id is omitted. It never returns a raw event cursor or child transcript.
func (c *scopedController) status(s *Session, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if !req.AgentID.IsZero() {
		if err := c.ownsChildForStatus(s, req.AgentID); err != nil {
			return tool.DelegateResult{}, err
		}
		return tool.DelegateResult{Agents: []tool.DelegateAgent{c.agentSnapshot(s, req.AgentID)}}, nil
	}
	owned := c.ownedChildren(s)
	truncated := len(owned) > maxDelegateStatusChildren
	if truncated {
		owned = owned[:maxDelegateStatusChildren]
	}
	agents := make([]tool.DelegateAgent, 0, len(owned))
	for _, id := range owned {
		agents = append(agents, c.agentSnapshot(s, id))
	}
	return tool.DelegateResult{Agents: agents, Truncated: truncated}, nil
}

// resolveOrQueue receives the tracker installed during admission, then either returns
// the foreground response or starts the automatic durable parent hand-back.
func (c *scopedController) resolveOrQueue(ctx context.Context, s *Session, childID, requestID uuid.UUID, sub event.Subscription, tracked *requestTracker, req tool.DelegateRequest) (tool.DelegateResult, error) {
	if req.WaitForResponse {
		defer func() { _ = sub.Close() }()
		defer c.manager.removeRequest(requestID, tracked)
		waitCtx, cancel := waitContext(ctx, req.TimeoutSeconds)
		defer cancel()
		text, err := drainDelegateAnswerObserved(waitCtx, sub, requestID, func() {
			_, _ = s.cancelDelegateRequest(childID, requestID)
		}, tracked.markActive)
		status := statusFromDrain(err)
		if status == tool.DelegateStatusInterrupted && didTimeout(req.TimeoutSeconds, waitCtx) {
			status = tool.DelegateStatusTimedOut
		}
		if status == tool.DelegateStatusFailed && text == "" {
			text = delegateFailureDetail(err)
		}
		tracked.markTerminal()
		result := c.responseResult(s, childID, requestID, status, text)
		if req.Name != "" {
			result.Name = req.Name
		}
		return result, nil
	}
	name := req.Name
	if name == "" {
		name = c.agentSnapshot(s, childID).Name
	}
	c.manager.handBackRequest(s, c.parentLoopID, childID, name, tracked, requestID, sub, req.TimeoutSeconds)
	result := c.agentResult(s, childID)
	result.CorrelationID = requestID
	result.State = tool.AgentStateWorking
	if req.Name != "" {
		result.Name = req.Name
	}
	return result, nil
}

type backgroundCompletionEnvelope struct {
	AgentID        string                      `json:"agent_id"`
	Name           string                      `json:"name"`
	State          tool.AgentState             `json:"state"`
	ResponseStatus tool.DelegateResponseStatus `json:"response_status"`
	CorrelationID  string                      `json:"correlation_id"`
	Response       string                      `json:"response"`
}

func decodeBackgroundCompletion(blocks []content.Block) (backgroundCompletionEnvelope, bool) {
	for _, block := range blocks {
		text, ok := block.(*content.TextBlock)
		if !ok || text == nil || text.Text == "" {
			continue
		}
		var envelope backgroundCompletionEnvelope
		if err := json.Unmarshal([]byte(text.Text), &envelope); err != nil || envelope.CorrelationID == "" {
			continue
		}
		return envelope, true
	}
	return backgroundCompletionEnvelope{}, false
}

// backgroundCompletionBlocks creates the single bounded, structured machine input
// delivered to a parent. Correlation stays internal to durable orchestration: agent
// tools never expose or accept it, while the persisted SubagentResult blocks retain it
// for restore-time idempotence.
func backgroundCompletionBlocks(agentID uuid.UUID, name string, correlationID uuid.UUID, status tool.DelegateStatusValue, response string) []content.Block {
	envelope := backgroundCompletionEnvelope{
		AgentID: agentID.String(), Name: boundUTF8(name, 4096), State: tool.AgentStateIdle,
		ResponseStatus: responseStatus(status), CorrelationID: correlationID.String(),
	}
	response = boundUTF8(response, maxDelegateOutputBytes)
	ends := runePrefixEnds(response)
	var best []byte
	low, high := 0, len(ends)
	for low < high {
		mid := low + (high-low)/2
		envelope.Response = response[:ends[mid]]
		encoded, err := json.Marshal(envelope)
		if err != nil {
			break
		}
		if len(encoded) <= maxDelegateOutputBytes {
			best = encoded
			low = mid + 1
		} else {
			high = mid
		}
	}
	if best == nil {
		envelope.Name = ""
		envelope.Response = ""
		best, _ = json.Marshal(envelope)
	}
	return []content.Block{&content.TextBlock{Text: string(best)}}
}

func boundUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

// delegateFailureDetail crosses the child-to-parent boundary only for an error
// that explicitly opts into the narrow model-facing projection. The drain wraps
// TurnFailed.Err; the shared traversal follows only real wrapper/join links and
// never invokes a custom errors.As implementation. A malformed or oversized
// projection is made valid and bounded before it reaches a result or durable
// completion envelope.
func delegateFailureDetail(err error) (detail string) {
	if err == nil {
		return ""
	}
	detail, marked := tool.ModelFacingErrorDetail(err)
	if !marked {
		return ""
	}
	return boundUTF8(detail, maxDelegateOutputBytes)
}

func runePrefixEnds(value string) []int {
	ends := make([]int, 1, utf8.RuneCountInString(value)+1)
	for end := 0; end < len(value); {
		_, size := utf8.DecodeRuneInString(value[end:])
		end += size
		ends = append(ends, end)
	}
	return ends
}

// ownsChild fails closed unless childID is a registered loop whose parent is exactly
// this controller's bound parent — rejecting siblings, ancestors, and unrelated loops.
func (c *scopedController) ownsChild(s *Session, childID uuid.UUID) error {
	return c.ownsChildWithClosed(s, childID, false)
}

func (c *scopedController) ownsChildForStatus(s *Session, childID uuid.UUID) error {
	return c.ownsChildWithClosed(s, childID, true)
}

func (c *scopedController) ownsChildWithClosed(s *Session, childID uuid.UUID, allowClosed bool) error {
	s.loopsMu.RLock()
	handle, ok := s.loops[childID]
	s.loopsMu.RUnlock()
	if !ok {
		return &DelegateError{Kind: DelegateNotOwned, DelegateID: childID}
	}
	if handle.parent.LoopID != c.parentLoopID {
		return &DelegateError{Kind: DelegateNotOwned, DelegateID: childID}
	}
	if handle.tombstoned && !allowClosed {
		return &DelegateError{Kind: DelegateClosed, DelegateID: childID}
	}
	return nil
}

// ownedChildren returns registered loop ids from this controller's direct-child index.
func (c *scopedController) ownedChildren(s *Session) []uuid.UUID {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	children := s.directChildren[c.parentLoopID]
	ids := make([]uuid.UUID, 0, len(children))
	for id := range children {
		if _, ok := s.loops[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func (c *scopedController) agentResult(s *Session, agentID uuid.UUID) tool.DelegateResult {
	agent := c.agentSnapshot(s, agentID)
	return tool.DelegateResult{AgentID: agent.AgentID, Name: agent.Name, State: agent.State}
}

func (c *scopedController) responseResult(s *Session, agentID, responseID uuid.UUID, status tool.DelegateStatusValue, response string) tool.DelegateResult {
	result := c.agentResult(s, agentID)
	result.CorrelationID = responseID
	result.Response = response
	result.ResponseStatus = responseStatus(status)
	// The response drain can observe a correlated terminal before the hub's commit
	// observer updates the persistent loop handle. A terminal foreground response is
	// nevertheless idle by contract; roster snapshots continue to use the live handle.
	if result.ResponseStatus != tool.DelegateResponseUnknown {
		result.State = tool.AgentStateIdle
	}
	return result
}

func responseStatus(status tool.DelegateStatusValue) tool.DelegateResponseStatus {
	switch status {
	case tool.DelegateStatusCompleted:
		return tool.DelegateResponseCompleted
	case tool.DelegateStatusInterrupted:
		return tool.DelegateResponseInterrupted
	case tool.DelegateStatusTimedOut:
		return tool.DelegateResponseTimedOut
	case tool.DelegateStatusFailed:
		return tool.DelegateResponseFailed
	default:
		return tool.DelegateResponseUnknown
	}
}

func (c *scopedController) agentSnapshot(s *Session, agentID uuid.UUID) tool.DelegateAgent {
	s.loopsMu.RLock()
	handle := s.loops[agentID]
	s.loopsMu.RUnlock()
	if handle == nil {
		return tool.DelegateAgent{AgentID: agentID, State: tool.AgentStateUnavailable}
	}
	name := handle.agentName
	if name == "" {
		name = handle.bound.DisplayName()
		if name == "" {
			name = string(handle.bound.Name())
		}
	}
	agentMode := handle.agentMode
	if agentMode == "" {
		agentMode = handle.bound.InitialMode()
	}
	return tool.DelegateAgent{
		AgentID: agentID, Name: name, AgentType: string(handle.bound.Name()),
		State: c.childState(s, agentID), QueuedMessages: c.manager.pendingCount(agentID),
		Runtime: c.agentRuntime(handle), AgentMode: string(agentMode),
	}
}

func (c *scopedController) agentRuntime(handle *loopHandle) tool.DelegateRuntime {
	identity := handle.bound.RuntimeIdentity()
	runtime := tool.DelegateRuntime{
		Profile: string(identity.Profile), Source: string(identity.Source),
		SelectionKind: string(identity.SelectionKind),
	}
	if identity.Profile != "" || identity.Source != "" || identity.SelectionKind != "" || identity.ModelAlias != "" {
		runtime.Effort = runtimeEffortString(identity.Effort)
		if identity.SelectionKind == loop.RuntimeSelectionHarnessManaged {
			runtime.Effort = ""
		}
	}
	if identity.SelectionKind == loop.RuntimeSelectionHarnessManaged {
		runtime.Harness = string(handle.selectedHarness)
		if runtime.Harness == "" {
			for _, entry := range c.runtimeCatalog.EntriesFor(handle.bound.Name()) {
				if entry.Profile == identity.Profile {
					runtime.Harness = string(entry.AgentHarness)
					break
				}
			}
		}
		return runtime
	}
	if resolved, ok := c.publicRuntimeIdentity(handle.bound.Name(), handle.selectedHarness, identity); ok {
		runtime.Harness = string(resolved.AgentHarness)
		runtime.Model = string(resolved.ModelAlias)
	}
	return runtime
}

func (c *scopedController) publicRuntimeIdentity(agent identity.AgentName, selectedHarness loop.AgentHarnessName, durable loop.RuntimeIdentity) (loop.Resolved, bool) {
	if !c.hasRuntimeCatalog || durable.ModelAlias == "" {
		return loop.Resolved{}, false
	}
	var match loop.Resolved
	found := false
	for _, entry := range c.runtimeCatalog.EntriesFor(agent) {
		if (selectedHarness != "" && entry.AgentHarness != selectedHarness) || entry.Profile != durable.Profile || entry.SelectionKind != durable.SelectionKind {
			continue
		}
		resolved, err := c.runtimeCatalog.ResolveTargetAliasWithSource(agent, entry.AgentHarness, durable.Source, durable.ModelAlias, durable.Effort)
		if err != nil || !resolvedMatchesRuntimeIdentity(resolved, durable) {
			continue
		}
		if found && (match.AgentHarness != resolved.AgentHarness || match.ModelAlias != resolved.ModelAlias) {
			return loop.Resolved{}, false
		}
		match = resolved
		found = true
	}
	return match, found
}

func resolvedMatchesRuntimeIdentity(resolved loop.Resolved, durable loop.RuntimeIdentity) bool {
	if resolved.Profile != durable.Profile || resolved.Source != durable.Source || resolved.SelectionKind != durable.SelectionKind ||
		resolved.Effort != durable.Effort || (resolved.TargetAlias != durable.ModelAlias && resolved.ModelAlias != durable.ModelAlias) {
		return false
	}
	if durable.TargetProvider != "" && resolved.Target.Provider != durable.TargetProvider {
		return false
	}
	return durable.TargetModel == "" || resolved.Target.Name == durable.TargetModel
}

func (c *scopedController) childState(s *Session, agentID uuid.UUID) tool.AgentState {
	switch c.childStatus(s, agentID) {
	case tool.DelegateStatusRunning, tool.DelegateStatusQueued:
		return tool.AgentStateWorking
	case tool.DelegateStatusIdle, tool.DelegateStatusCompleted, tool.DelegateStatusInterrupted, tool.DelegateStatusTimedOut:
		return tool.AgentStateIdle
	default:
		return tool.AgentStateUnavailable
	}
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
	if handle.backend == nil {
		return tool.DelegateStatusFailed
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
	var cancelled *drainCancelledError
	if errors.As(err, &cancelled) {
		return delegateStatusFromCancelReason(cancelled.Reason)
	}
	return tool.DelegateStatusFailed
}

// delegateStatusFromCancelReason is the single live/restore mapping for an admitted
// queued request that never opened a turn. Client retraction is conservatively
// Interrupted (cancelled by control action); unknown future reasons fail closed.
func delegateStatusFromCancelReason(reason event.CancelReason) tool.DelegateStatusValue {
	switch reason {
	case event.CancelTurnInterrupted, event.CancelClientRetracted:
		return tool.DelegateStatusInterrupted
	case event.CancelTurnFailed:
		return tool.DelegateStatusFailed
	default:
		return tool.DelegateStatusFailed
	}
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

// startDelegate runs the transactional child admission path: quota reservation and bind,
// subscription, request mint, backend construction, and initial-command acceptance all
// precede the checked LoopStarted commit. The backend's publisher remains blocked until
// that commit, so TurnStarted can neither race ahead nor survive a failed spawn.
func (s *Session) startDelegate(ctx context.Context, parent loop.Provenance, cfg loop.Definition, mode loop.ModeName, name, message, parentToolUseID string, runtime *loop.Resolved, runtimeCatalog loop.RuntimeCatalog, hasRuntimeCatalog, background bool, registerRequest func(requestID, childID uuid.UUID) *requestTracker) (childID, requestID uuid.UUID, sub event.Subscription, tracked *requestTracker, err error) {
	admission := &delegateAdmission{ctx: ctx, name: name, message: message, registerRequest: registerRequest, background: background}
	childID, err = s.newLoopWithAdmission(parent, cfg, parentToolUseID, mode, nil, admission, runtime, runtimeCatalog, hasRuntimeCatalog)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, nil, nil, err
	}
	return childID, admission.requestID, admission.sub, admission.tracked, nil
}

func parseRuntimeEffort(value string) inferencemodel.Effort {
	if value == "none" || value == "" {
		return inferencemodel.EffortNone
	}
	return inferencemodel.Effort(value)
}

func runtimeEffortString(value inferencemodel.Effort) string {
	if value == inferencemodel.EffortNone {
		return "none"
	}
	return string(value)
}

// sendDelegate enqueues a distinct NON-FOLDING follow-up turn on an existing owned
// child. It subscribes BEFORE the enqueue so the correlated turn's opening event is
// never missed.
func (s *Session) sendDelegate(ctx context.Context, childID uuid.UUID, message string, background bool, registerRequest func(requestID, childID uuid.UUID) *requestTracker, removeRequest func(uuid.UUID, *requestTracker)) (requestID uuid.UUID, sub event.Subscription, tracked *requestTracker, err error) {
	sub, err = s.subscribeLoop(childID)
	if err != nil {
		return uuid.UUID{}, nil, nil, err
	}
	wakeOwned := background
	if background {
		s.expectTurn(ctx, childID)
		defer func() {
			if wakeOwned {
				s.cancelExpectTurn(context.Background(), childID)
			}
		}()
	}
	requestID, tracked, err = s.enqueueDelegateTurn(ctx, childID, delegateBlocks(message), background, registerRequest, removeRequest)
	if err != nil {
		_ = sub.Close()
		return uuid.UUID{}, nil, nil, err
	}
	wakeOwned = false
	return requestID, sub, tracked, nil
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
func (s *Session) enqueueDelegateTurn(ctx context.Context, loopID uuid.UUID, blocks []content.Block, background bool, registerRequest func(requestID, childID uuid.UUID) *requestTracker, removeRequest func(uuid.UUID, *requestTracker)) (requestID uuid.UUID, tracked *requestTracker, err error) {
	if err := s.faultIfFaulted(); err != nil {
		return uuid.UUID{}, nil, err
	}
	backend, ok := s.loopFor(loopID)
	if !ok {
		return uuid.UUID{}, nil, &SessionError{Kind: SessionLoopNotFound}
	}
	if backend == nil {
		return uuid.UUID{}, nil, &SessionError{Kind: SessionLoopExited}
	}
	id, err := s.newCommandID()
	if err != nil {
		return uuid.UUID{}, nil, err
	}
	accepted := make(chan error, 1)
	cmd := command.UserInput{
		Header:             command.Header{CommandID: id, Agency: identity.AgencyMachine, CreatedAt: s.stampNow()},
		Blocks:             blocks,
		NoFold:             true,
		TargetLoopID:       loopID,
		BackgroundHandBack: background,
		Accepted:           accepted,
	}
	if err := s.appendDelegateCommand(ctx, loopID, cmd); err != nil {
		return uuid.UUID{}, nil, err
	}
	tracked = registerRequest(id, loopID)
	registered := tracked
	admitted := false
	defer func() {
		if !admitted {
			removeRequest(id, registered)
		}
	}()
	select {
	case backend.CommandSink() <- cmd:
		canceled, err := awaitDelegateAcceptance(ctx, accepted, backend.DoneChan())
		if err != nil {
			return uuid.UUID{}, nil, err
		}
		admitted = true
		if canceled {
			_, _ = s.cancelDelegateRequest(loopID, id)
		}
		return id, tracked, nil
	case <-ctx.Done():
		return uuid.UUID{}, nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-backend.DoneChan():
		return uuid.UUID{}, nil, &SessionError{Kind: SessionLoopExited}
	}
}

// awaitDelegateAcceptance treats the actor's durable acceptance decision as
// authoritative once the command handoff has completed. Context cancellation is
// remembered, but cannot roll back a request the actor may already have committed.
func awaitDelegateAcceptance(ctx context.Context, accepted <-chan error, backendDone <-chan struct{}) (canceled bool, err error) {
	ctxDone := ctx.Done()
	for {
		select {
		case acceptErr := <-accepted:
			if acceptErr != nil {
				return canceled, &SessionError{Kind: SessionDelegateAdmissionCommitFailed, Cause: acceptErr}
			}
			return canceled || ctx.Err() != nil, nil
		case <-ctxDone:
			canceled = true
			ctxDone = nil
		case <-backendDone:
			// A buffered acceptance may become observable with actor exit. Prefer that
			// durable decision before classifying the loop as exited.
			select {
			case acceptErr := <-accepted:
				if acceptErr != nil {
					return canceled, &SessionError{Kind: SessionDelegateAdmissionCommitFailed, Cause: acceptErr}
				}
				return canceled || ctx.Err() != nil, nil
			default:
				return canceled, &SessionError{Kind: SessionLoopExited}
			}
		}
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
	DelegateMissingDelegateID
	DelegateUnknownOperation
	// DelegateInterruptPending: the parent loop is under an interrupt admission barrier, so a
	// NEW machine-delegate request (start/send) is flushed (refused) until the barrier releases.
	// This is the machine-side of the interrupt queue policy: user input stays queued, but a
	// parent whose interrupted delegate wait resolves cannot open a fresh delegate step.
	DelegateInterruptPending
	DelegateRuntimeUnavailable
	DelegateRuntimeInvalid
	DelegateClosed
)

// DelegateError is the typed delegation refusal. Callers errors.As to inspect Kind and
// the offending agent, mode, or delegate ID.
type DelegateError struct {
	Kind       DelegateErrorKind
	Agent      identity.AgentName
	Mode       loop.ModeName
	DelegateID uuid.UUID
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
	case DelegateMissingDelegateID:
		return "delegation: a delegate_id is required"
	case DelegateUnknownOperation:
		return "delegation: unknown operation"
	case DelegateInterruptPending:
		return "delegation: loop is interrupt-pending; new delegate work is not admitted until the interrupt barrier releases"
	case DelegateRuntimeUnavailable, DelegateRuntimeInvalid:
		return "delegation: runtime selection is unavailable"
	case DelegateClosed:
		return "delegation: delegate is closed"
	default:
		return "delegation: refused"
	}
}

// ModelFacingError exposes only the fixed, bounded category needed by the
// model-facing agent result. It deliberately omits all request fields;
// ordinary delegation errors continue to use the generic tool-result error.
func (e *DelegateError) ModelFacingError() string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case DelegateRuntimeUnavailable, DelegateRuntimeInvalid:
		return "runtime selection is unavailable"
	default:
		return ""
	}
}
