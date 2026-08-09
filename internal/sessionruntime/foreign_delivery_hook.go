package sessionruntime

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
)

// foreignDeliveryPhase is the in-memory state machine for one foreign delivery
// request. The durable command/event records remain the source of truth across
// restore; this state only prevents an actor from issuing contradictory live
// transitions through its loop-scoped capability.
type foreignDeliveryPhase uint8

const (
	foreignDeliveryAbsent foreignDeliveryPhase = iota
	foreignDeliveryIntent
	foreignDeliveryReserved
	foreignDeliveryRestored
	foreignDeliveryFallback
	foreignDeliveryInjected
	foreignDeliveryUnknown
	foreignDeliveryUntrackable
	foreignDeliveryRestoredDone
	foreignDeliveryAbandoned
)

// Tombstones preserve fail-closed duplicate handling without retaining every
// completed request for the lifetime of a session. Eviction only changes a
// duplicate into the same invalid-transition result produced for an unknown ID.
const foreignDeliveryTombstoneLimit = 256

var (
	errForeignDeliveryInvalidCoordinate  = errors.New("foreign delivery: invalid coordinate")
	errForeignDeliveryInvalidTransition  = errors.New("foreign delivery: invalid transition")
	errForeignDeliveryCommandUnavailable = errors.New("foreign delivery: command unavailable")
	errForeignDeliveryPublicationFailed  = errors.New("foreign delivery: durable publication failed")
)

// foreignDeliveryPublicationError keeps the public diagnostic fixed and
// bounded. Journal and adapter errors are not safe to expose: they may contain
// storage paths, bearer material, or provider response text.
type foreignDeliveryPublicationError struct {
}

func (e *foreignDeliveryPublicationError) Error() string {
	return errForeignDeliveryPublicationFailed.Error()
}

func (e *foreignDeliveryPublicationError) Unwrap() []error {
	return []error{errForeignDeliveryPublicationFailed}
}

func foreignDeliveryPublicationErrorFrom(_ error) error {
	return &foreignDeliveryPublicationError{}
}

// foreignDeliveryHook is the private session-owned implementation of the
// public foreign.DeliveryHook. Its only authority is the exact loop selected
// when it is constructed. Command payloads are registered by the session before
// actor admission; the actor receives only IDs through the public hook and can
// never obtain a session, journal, or controller from it.
type foreignDeliveryHook struct {
	session   *Session
	sessionID uuid.UUID
	loopID    uuid.UUID

	mu             sync.Mutex
	commands       map[uuid.UUID]command.UserInput
	phases         map[uuid.UUID]foreignDeliveryPhase
	folds          map[uuid.UUID]uuid.UUID
	tombstones     map[uuid.UUID]foreignDeliveryPhase
	tombstoneOrder []uuid.UUID
}

var _ foreign.DeliveryHook = (*foreignDeliveryHook)(nil)

func newForeignDeliveryHook(session *Session, loopID uuid.UUID) *foreignDeliveryHook {
	hook := &foreignDeliveryHook{
		session:    session,
		commands:   make(map[uuid.UUID]command.UserInput),
		phases:     make(map[uuid.UUID]foreignDeliveryPhase),
		folds:      make(map[uuid.UUID]uuid.UUID),
		tombstones: make(map[uuid.UUID]foreignDeliveryPhase),
		loopID:     loopID,
	}
	if session != nil {
		hook.sessionID = session.sessionID
		session.registerForeignDeliveryHook(hook)
	}
	return hook
}

func (s *Session) registerForeignDeliveryHook(hook *foreignDeliveryHook) {
	if s == nil || hook == nil || hook.loopID.IsZero() {
		return
	}
	s.foreignDeliveryMu.Lock()
	if s.foreignDeliveryHooks == nil {
		s.foreignDeliveryHooks = make(map[uuid.UUID]*foreignDeliveryHook)
	}
	s.foreignDeliveryHooks[hook.loopID] = hook
	s.foreignDeliveryMu.Unlock()
}

func (s *Session) unregisterForeignDeliveryHook(hook *foreignDeliveryHook) {
	if s == nil || hook == nil {
		return
	}
	s.foreignDeliveryMu.Lock()
	if current := s.foreignDeliveryHooks[hook.loopID]; current == hook {
		delete(s.foreignDeliveryHooks, hook.loopID)
	}
	s.foreignDeliveryMu.Unlock()
}

func (s *Session) recordForeignDeliveryFold(ev event.Event) {
	var loopID, requestID uuid.UUID
	switch typed := ev.(type) {
	case event.TurnFoldedInto:
		loopID, requestID = typed.LoopID, typed.Cause.CommandID
	case event.TurnStarted:
		loopID, requestID = typed.LoopID, typed.Cause.CommandID
	case event.InputCancelled:
		loopID, requestID = typed.LoopID, typed.Cause.CommandID
	case event.TurnRejected:
		loopID, requestID = typed.LoopID, typed.Cause.CommandID
	default:
		return
	}
	if loopID.IsZero() || requestID.IsZero() {
		return
	}
	s.foreignDeliveryMu.RLock()
	hook := s.foreignDeliveryHooks[loopID]
	s.foreignDeliveryMu.RUnlock()
	if hook == nil {
		return
	}
	switch typed := ev.(type) {
	case event.TurnFoldedInto:
		hook.observeFold(typed)
	case event.TurnStarted:
		hook.observeRestoredStart(typed)
	case event.InputCancelled:
		hook.observeRestoredTerminal(typed.SessionID, loopID, requestID)
	case event.TurnRejected:
		hook.observeRestoredTerminal(typed.SessionID, loopID, requestID)
	}
}

// bindCommand installs the exact command that carries the actor payload. It is
// intentionally private: only Session creates the command and only the hook
// uses it to write the durable intent/fallback records.
func (h *foreignDeliveryHook) bindCommand(cmd command.UserInput) error {
	if h == nil || cmd.CommandID.IsZero() || cmd.TargetLoopID.IsZero() || cmd.TargetLoopID != h.loopID {
		return errForeignDeliveryInvalidCoordinate
	}
	if err := command.ValidateCommand(cmd); err != nil {
		return err
	}
	// Snapshot the durable command representation so later actor-side mutation
	// of a shared Blocks slice cannot alter the fallback payload. Accepted is
	// restored below because it is process-local and intentionally not durable.
	body, err := command.MarshalCommand(cmd)
	if err != nil {
		return err
	}
	decoded, err := command.UnmarshalCommand(body)
	if err != nil {
		return err
	}
	bound, ok := decoded.(command.UserInput)
	if !ok {
		return errForeignDeliveryInvalidTransition
	}
	bound.Accepted = cmd.Accepted
	h.mu.Lock()
	defer h.mu.Unlock()
	// A terminal/fallback callback has already consumed the durable payload.
	// Do not let a late restore bind or callback recreate an unbounded command
	// entry for a request whose phase is no longer admitting delivery.
	switch h.phaseLocked(cmd.CommandID) {
	case foreignDeliveryFallback, foreignDeliveryInjected, foreignDeliveryUnknown, foreignDeliveryUntrackable, foreignDeliveryRestoredDone, foreignDeliveryAbandoned:
		return errForeignDeliveryInvalidTransition
	}
	if prior, exists := h.commands[cmd.CommandID]; exists {
		// Accepted is a process-local admission channel recreated for every
		// restore dispatch. It is deliberately outside the durable command
		// identity, just like the journal fingerprint, so a rebound command can
		// be idempotent across restore attempts without weakening payload checks.
		prior.Accepted = nil
		candidate := bound
		candidate.Accepted = nil
		if !reflect.DeepEqual(prior, candidate) {
			return errForeignDeliveryInvalidTransition
		}
	}
	h.commands[cmd.CommandID] = bound
	return nil
}

// observeRestoredCommand binds a command being replayed by Restore and marks it
// separately from the live intent/reservation protocol. Its payload is released
// at the first authoritative actor boundary (TurnStarted/TurnFoldedInto).
func (h *foreignDeliveryHook) observeRestoredCommand(cmd command.UserInput) error {
	if err := h.bindCommand(cmd); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phaseLocked(cmd.CommandID) != foreignDeliveryAbsent {
		return errForeignDeliveryInvalidTransition
	}
	h.phases[cmd.CommandID] = foreignDeliveryRestored
	return nil
}

func (h *foreignDeliveryHook) phaseLocked(requestID uuid.UUID) foreignDeliveryPhase {
	if phase, ok := h.phases[requestID]; ok {
		return phase
	}
	if phase, ok := h.tombstones[requestID]; ok {
		return phase
	}
	return foreignDeliveryAbsent
}

func (h *foreignDeliveryHook) finishLocked(requestID uuid.UUID, phase foreignDeliveryPhase) {
	delete(h.commands, requestID)
	delete(h.folds, requestID)
	delete(h.phases, requestID)
	if _, exists := h.tombstones[requestID]; !exists {
		h.tombstoneOrder = append(h.tombstoneOrder, requestID)
	}
	h.tombstones[requestID] = phase
	for len(h.tombstoneOrder) > foreignDeliveryTombstoneLimit {
		oldest := h.tombstoneOrder[0]
		h.tombstoneOrder = h.tombstoneOrder[1:]
		delete(h.tombstones, oldest)
	}
}

// abandon drops only process-local admission state after a durable intent could
// not be handed to the backend. The intent remains in the journal for restore,
// where the normal unresolved-intent repair closes it durably as unknown.
func (h *foreignDeliveryHook) abandon(requestID uuid.UUID) {
	if h == nil || requestID.IsZero() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	phase := h.phaseLocked(requestID)
	if phase == foreignDeliveryAbsent {
		delete(h.commands, requestID)
		delete(h.folds, requestID)
		return
	}
	h.finishLocked(requestID, foreignDeliveryAbandoned)
}

func (h *foreignDeliveryHook) observeFold(ev event.TurnFoldedInto) {
	if h == nil || ev.SessionID != h.sessionID || ev.LoopID != h.loopID ||
		(!ev.Cause.LoopID.IsZero() && ev.Cause.LoopID != h.loopID) ||
		ev.Cause.CommandID.IsZero() || ev.TurnID.IsZero() {
		return
	}
	h.mu.Lock()
	phase := h.phaseLocked(ev.Cause.CommandID)
	if phase == foreignDeliveryRestored {
		h.finishLocked(ev.Cause.CommandID, foreignDeliveryRestoredDone)
		h.mu.Unlock()
		return
	}
	if phase != foreignDeliveryReserved {
		h.mu.Unlock()
		return
	}
	if _, exists := h.folds[ev.Cause.CommandID]; !exists {
		h.folds[ev.Cause.CommandID] = ev.TurnID
	}
	h.mu.Unlock()
}

func (h *foreignDeliveryHook) observeRestoredStart(ev event.TurnStarted) {
	if h == nil || ev.SessionID != h.sessionID || ev.LoopID != h.loopID || ev.Cause.CommandID.IsZero() {
		return
	}
	h.mu.Lock()
	if h.phaseLocked(ev.Cause.CommandID) == foreignDeliveryRestored {
		h.finishLocked(ev.Cause.CommandID, foreignDeliveryRestoredDone)
	}
	h.mu.Unlock()
}

func (h *foreignDeliveryHook) observeRestoredTerminal(sessionID, loopID, requestID uuid.UUID) {
	if h == nil || sessionID != h.sessionID || loopID != h.loopID || requestID.IsZero() {
		return
	}
	h.mu.Lock()
	if h.phaseLocked(requestID) == foreignDeliveryRestored {
		h.finishLocked(requestID, foreignDeliveryRestoredDone)
	}
	h.mu.Unlock()
}

func (h *foreignDeliveryHook) validateIntent(intent foreign.DeliveryIntent) error {
	if h == nil || h.session == nil || h.sessionID.IsZero() || h.loopID.IsZero() || intent.LoopID.IsZero() || intent.RequestID.IsZero() {
		return errForeignDeliveryInvalidCoordinate
	}
	if intent.LoopID != h.loopID {
		return &journal.CommandRouteMismatchError{RecordLoopID: intent.LoopID, TargetLoopID: h.loopID}
	}
	return nil
}

func (h *foreignDeliveryHook) validateResolution(resolution foreign.DeliveryResolution) error {
	if err := h.validateIntent(foreign.DeliveryIntent{LoopID: resolution.LoopID, RequestID: resolution.RequestID}); err != nil {
		return err
	}
	switch resolution.State {
	case foreign.DeliveryResolutionInjected:
		if resolution.TurnID.IsZero() {
			return errForeignDeliveryInvalidCoordinate
		}
	case foreign.DeliveryResolutionUnknown, foreign.DeliveryResolutionUntrackable:
		if !resolution.TurnID.IsZero() {
			return errForeignDeliveryInvalidCoordinate
		}
	default:
		return errForeignDeliveryInvalidTransition
	}
	return nil
}

// CreateIntent durably writes the exact machine command with the intent phase.
// The in-memory phase advances only after the checked command append succeeds.
func (h *foreignDeliveryHook) CreateIntent(ctx context.Context, intent foreign.DeliveryIntent) error {
	if err := h.validateIntent(intent); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phaseLocked(intent.RequestID) != foreignDeliveryAbsent {
		return errForeignDeliveryInvalidTransition
	}
	cmd, ok := h.commands[intent.RequestID]
	if !ok || cmd.CommandID != intent.RequestID || cmd.TargetLoopID != h.loopID {
		return errForeignDeliveryCommandUnavailable
	}
	cmd.DelegateDeliveryPhase = command.DelegateDeliveryPhaseIntent
	record := journal.NewCommandRecord(h.sessionID, h.loopID, cmd)
	if err := journal.ValidateCommandRecordRoute(record); err != nil {
		return err
	}
	if _, err := h.session.appendCommandResultChecked(ctx, record); err != nil {
		delete(h.commands, intent.RequestID)
		delete(h.folds, intent.RequestID)
		return foreignDeliveryPublicationErrorFrom(err)
	}
	h.commands[intent.RequestID] = cmd
	h.phases[intent.RequestID] = foreignDeliveryIntent
	return nil
}

// Reserve appends the session-scoped reservation event before a foreign actor
// can authorize a provider writer. The checked event path prevents a failed
// durable append from becoming an in-memory admission decision.
func (h *foreignDeliveryHook) Reserve(ctx context.Context, reservation foreign.DeliveryReservation) error {
	if err := h.validateIntent(foreign.DeliveryIntent(reservation)); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phases[reservation.RequestID] != foreignDeliveryIntent {
		return errForeignDeliveryInvalidTransition
	}
	if err := h.publishStateChecked(ctx, reservation.RequestID, event.DelegateDeliverySteerAttemptReserved); err != nil {
		return err
	}
	h.phases[reservation.RequestID] = foreignDeliveryReserved
	return nil
}

// QueueFallback appends the exact command payload with the fallback phase. The
// append completes before this method returns, so the actor may safely perform
// normal admission afterward. The one command ID is reused; no second request
// identity can be minted by a retry.
func (h *foreignDeliveryHook) QueueFallback(ctx context.Context, fallback foreign.DeliveryFallback) error {
	if err := h.validateIntent(foreign.DeliveryIntent(fallback)); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	phase := h.phases[fallback.RequestID]
	if phase != foreignDeliveryIntent && phase != foreignDeliveryReserved {
		return errForeignDeliveryInvalidTransition
	}
	cmd, ok := h.commands[fallback.RequestID]
	if !ok || cmd.CommandID != fallback.RequestID || cmd.TargetLoopID != h.loopID {
		return errForeignDeliveryCommandUnavailable
	}
	cmd.DelegateDeliveryPhase = command.DelegateDeliveryPhaseFallbackQueued
	record := journal.NewCommandRecord(h.sessionID, h.loopID, cmd)
	if err := journal.ValidateCommandRecordRoute(record); err != nil {
		return err
	}
	if _, err := h.session.appendCommandResultChecked(ctx, record); err != nil {
		return foreignDeliveryPublicationErrorFrom(err)
	}
	h.finishLocked(fallback.RequestID, foreignDeliveryFallback)
	return nil
}

// Resolve commits terminal foreign delivery evidence. Injected is represented
// by the already-committed authoritative TurnFoldedInto; only ambiguous states
// append a DelegateDeliveryStateChanged event.
func (h *foreignDeliveryHook) Resolve(ctx context.Context, resolution foreign.DeliveryResolution) error {
	if err := h.validateResolution(resolution); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phaseLocked(resolution.RequestID) != foreignDeliveryReserved {
		return errForeignDeliveryInvalidTransition
	}
	switch resolution.State {
	case foreign.DeliveryResolutionInjected:
		if turnID, ok := h.folds[resolution.RequestID]; !ok || turnID != resolution.TurnID {
			return errForeignDeliveryInvalidTransition
		}
		h.finishLocked(resolution.RequestID, foreignDeliveryInjected)
		return nil
	case foreign.DeliveryResolutionUnknown:
		if err := h.publishStateChecked(ctx, resolution.RequestID, event.DelegateDeliveryResolvedUnknown); err != nil {
			return err
		}
		h.finishLocked(resolution.RequestID, foreignDeliveryUnknown)
		return nil
	case foreign.DeliveryResolutionUntrackable:
		if err := h.publishStateChecked(ctx, resolution.RequestID, event.DelegateDeliveryResolvedUntrackable); err != nil {
			return err
		}
		h.finishLocked(resolution.RequestID, foreignDeliveryUntrackable)
		return nil
	default:
		return errForeignDeliveryInvalidTransition
	}
}

func (h *foreignDeliveryHook) publishStateChecked(ctx context.Context, requestID uuid.UUID, state event.DelegateDeliveryState) error {
	if h == nil || h.session == nil || h.session.factory == nil || h.session.hub == nil {
		return errForeignDeliveryPublicationFailed
	}
	stateEvent := event.DelegateDeliveryStateChanged{
		Header:       event.Header{Coordinates: identity.Coordinates{SessionID: h.sessionID}},
		RequestID:    requestID,
		TargetLoopID: h.loopID,
		State:        state,
	}
	header, err := h.session.factory.Stamp(stateEvent.EventHeader())
	if err != nil {
		return foreignDeliveryPublicationErrorFrom(err)
	}
	stateEvent.Header = header
	if err := event.ValidateEvent(stateEvent); err != nil {
		return foreignDeliveryPublicationErrorFrom(err)
	}
	if err := h.session.PublishEventChecked(ctx, stateEvent); err != nil {
		return foreignDeliveryPublicationErrorFrom(err)
	}
	return nil
}
