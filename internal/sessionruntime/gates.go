package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/tool"
)

// gateState is the live state of a directory entry. It is never persisted — the
// durable state is reconstructed from GatePreparedRecord/GateOpened/GateResolved
// on restore. preparing and claiming are not client-visible.
type gateState uint8

const (
	gatePreparing gateState = iota // durable GatePreparedRecord, no public GateOpened
	gateOpen                       // durable GateOpened, listed and answerable
	gateClaiming                   // in-memory only, between lock-claim and durable GateResolved
	gateClosed                     // durable GateResolved, removed from directory
)

// gateEntry is the session-private directory entry: the public Gate envelope PLUS
// the internal route, the typed resolver payload (never shipped to clients), the
// event coordinates stamped at prepare time, and the live state. The route is set
// only after ActivateGate appends GateOpened.
type gateEntry struct {
	gate        gate.Gate
	route       gate.Route
	payload     gate.Payload
	coordinates identity.Coordinates
	state       gateState
}

// gateAppender is the STRICT durable append seam for gate prepare/open/resolve.
// Unlike the hub's PublishEvent (which faults and returns nil on append failure),
// this seam returns the append error so PrepareGateOpen/ActivateGate can fail
// closed — a failed prepare installs no directory entry, a failed activate leaves
// the gate preparing. The nop default (nopGateAppender) keeps headless mode
// unchanged; the composition root wires the real journal+hub adapter.
type gateAppender interface {
	AppendGatePrepared(ctx context.Context, rec journal.GatePreparedRecord) error
	AppendGateOpened(ctx context.Context, ev event.GateOpened) error
	AppendGateResolved(ctx context.Context, ev event.GateResolved) error
}

// liveGateAppender keeps the private prepared payload on the gate journal seam, while
// routing public GateOpened/GateResolved events through the session's checked hub path.
// The latter is essential: public gate transitions must be both durably appended and
// delivered to live subscribers, and PublishEventChecked preserves durable-first failure
// semantics without double-appending them.
type liveGateAppender struct {
	prepared  gateAppender
	publisher interface {
		PublishEventChecked(context.Context, event.Event) error
	}
}

func (a *liveGateAppender) AppendGatePrepared(ctx context.Context, rec journal.GatePreparedRecord) error {
	return a.prepared.AppendGatePrepared(ctx, rec)
}

func (a *liveGateAppender) AppendGateOpened(ctx context.Context, ev event.GateOpened) error {
	return a.publisher.PublishEventChecked(ctx, ev)
}

func (a *liveGateAppender) AppendGateResolved(ctx context.Context, ev event.GateResolved) error {
	return a.publisher.PublishEventChecked(ctx, ev)
}

// nopGateAppender is the default gateAppender: all appends succeed without doing
// anything. It keeps headless/no-persistence mode unchanged.
type nopGateAppender struct{}

func (nopGateAppender) AppendGatePrepared(context.Context, journal.GatePreparedRecord) error {
	return nil
}
func (nopGateAppender) AppendGateOpened(context.Context, event.GateOpened) error {
	return nil
}
func (nopGateAppender) AppendGateResolved(context.Context, event.GateResolved) error {
	return nil
}

// hostOwnedGate reports whether a gate's answer belongs to the HOST that opened
// it rather than to a loop.
//
// This is the distinction that decides how an answer is delivered.
// translateGateResponse turns a permission or ask-user answer into a
// command.Command addressed to entry.route.LoopID, because those gates park a
// loop's turn and resuming it IS the answer. A form or open-url gate raised by an
// integration (an MCP server's elicitation) parks no turn: the caller waiting on
// it is the host's own blocked OpenGate call, which no loop command can reach.
//
// Both conditions are required, and the resolver is not redundant with the kind:
//
//   - The KIND must be one whose answer this session can validate against a
//     schema and hand back as gate.Values. Only form and open-url qualify.
//   - The RESOLVER must be gate.ResolverSession. ResolverSession has existed on
//     the envelope since the gate contract was written and no production code
//     ever produced one — it is the declared, unused name for exactly this case,
//     so this fills a vacant seam rather than inventing one.
//
// A form gate marked ResolverLoop is therefore NOT host-owned and is refused at
// answer time (GateKindMismatch) rather than silently delivered somewhere. There
// is no loop-side form resolver to route it to, so accepting it would mean
// dropping a human's answer on the floor.
func hostOwnedGate(g gate.Gate) bool {
	if g.Resolver != gate.ResolverSession {
		return false
	}
	switch g.Kind {
	case gate.KindForm, gate.KindOpenURL:
		return true
	case gate.KindPermission, gate.KindAskUser:
		return false
	default:
		return false
	}
}

// GateCaps bounds the live gate directory. The cap counts preparing + open +
// claiming so failed activations cannot accumulate invisible prepared entries.
// Zero means no cap.
type GateCaps struct {
	MaxOpen    int
	MaxTimeout time.Duration
}

// WithGateAppender injects the strict durable append seam for gate
// prepare/open/resolve. A nil appender is ignored (the nop default stays
// installed). It is the gate-directory counterpart to WithCommandAppender.
func WithGateAppender(a gateAppender) Option {
	return func(s *Session) {
		if a != nil {
			s.gateAppender = a
		}
	}
}

// WithGateCaps injects the live gate directory bounds. Zero (the default) means
// no cap. The cap counts preparing + open + claiming.
func WithGateCaps(caps GateCaps) Option {
	return func(s *Session) {
		s.gateCaps = caps
	}
}

// PrepareGateOpen durably commits the public envelope plus private payload as a
// private GatePreparedRecord. It mints the GateID, stamps the GatePrepared event
// with the session's coordinates and the caller's loopID, appends the record via
// the strict gateAppender, and — only on success — inserts a non-listable
// preparing entry. A failed append returns a typed *GateError{GateAppendFailed}
// and does not mutate the directory. loopID is the producing loop's id (the
// GatePrepared event is loopScoped); TurnID/StepID are read from the gate's
// Subject.
func (s *Session) PrepareGateOpen(ctx context.Context, loopID uuid.UUID, g gate.Gate, payload gate.Payload) (gate.ID, error) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()

	if err := s.checkGateCap(); err != nil {
		return gate.ID{}, err
	}
	// Kind-implied envelope invariants (e.g. an open-url gate may not be
	// Restorable). Every pre-existing kind validates clean, so this rejects only
	// envelopes that were already incoherent.
	if err := gate.ValidateGate(g); err != nil {
		return gate.ID{}, &GateError{Kind: GateKindMismatch, Cause: err}
	}
	policy, err := s.resolveGatePolicy(g)
	if err != nil {
		return gate.ID{}, err
	}

	gateID, err := s.mintGateID()
	if err != nil {
		return gate.ID{}, err
	}
	g.ID = gateID
	g.ResponsePolicy = policy

	coords := identity.Coordinates{
		SessionID: s.sessionID,
		LoopID:    loopID,
		TurnID:    uuid.UUID(g.Subject.TurnID),
		StepID:    uuid.UUID(g.Subject.StepID),
	}
	prepared, err := s.stampGateEvent(coords, g)
	if err != nil {
		return gate.ID{}, err
	}

	openPayload := gate.OpenPayload{GateID: gateID, Payload: payload}
	rec := journal.NewGatePreparedRecord(prepared, openPayload)
	if err := s.gateAppender.AppendGatePrepared(ctx, rec); err != nil {
		return gate.ID{}, &GateError{GateID: gateID, Kind: GateAppendFailed, Cause: err}
	}

	s.gates[gateID] = gateEntry{gate: g, payload: payload, coordinates: coords, state: gatePreparing}
	// The answer slot is installed at PREPARE time, before the gate is public and
	// therefore before anything can answer it. That ordering is what makes
	// AwaitGateAnswer race-free: the opener learns its GateID here, calls
	// ActivateGate, and only then blocks — by which point the slot that a
	// concurrent RespondGate would write to already exists.
	if hostOwnedGate(g) {
		if s.gateAnswers == nil {
			s.gateAnswers = make(map[gate.ID]chan gate.Answer)
		}
		s.gateAnswers[gateID] = make(chan gate.Answer, 1)
	}
	return gateID, nil
}

// OpenHostGate opens a HOST-OWNED gate and returns its id, ready to be awaited.
//
// It is the ONLY gate-opening entry point on the published session.GateHost
// contract, and it is deliberately not PrepareGateOpen. PrepareGateOpen takes an
// arbitrary kind, resolver, payload, and — through the ActivateGate route that
// must follow it — an arbitrary target loop, which together are exactly the
// loop-owned command path (dispatchGateCommand). A host must not be able to
// reach that: minting an "approve this tool call" gate against someone else's
// loop is not a capability an integration should acquire by being able to ask a
// human a question. This entry point takes the same arguments minus the route
// and refuses everything that is not host-owned, so the loop path is unreachable
// through it by construction rather than by convention.
//
// It collapses prepare and activate into one call, which is safe precisely
// because the gate is host-owned. The two phases exist so a LOOP can install its
// blocker while the gate is still private; a host's blocker is the answer slot,
// and PrepareGateOpen installs that itself. There is no window here for the
// opener to miss.
//
// Every rejection is a *GateError{GateKindMismatch}, checked BEFORE anything is
// journaled:
//
//   - hostOwnedGate — the same predicate that governs answer time, so open time
//     and answer time cannot drift apart. A form gate declaring ResolverLoop is
//     refused here rather than accepted and then refused after a human has
//     already answered it.
//   - the payload must match the kind, so a form gate cannot be opened with an
//     open-url payload (which would strand its answer at ParseFormAnswers).
//   - the payload's own invariants (ValidateFormSchema / ValidateOpenURLPayload)
//     must hold, because an unanswerable or targetless prompt should never reach
//     a human.
//
// The caller MUST either AwaitGateAnswer or CloseGate; both free the slot.
func (s *Session) OpenHostGate(ctx context.Context, loopID uuid.UUID, g gate.Gate, payload gate.Payload) (gate.ID, error) {
	if !hostOwnedGate(g) {
		return gate.ID{}, &GateError{Kind: GateKindMismatch}
	}
	if err := validateHostGatePayload(g.Kind, payload); err != nil {
		return gate.ID{}, &GateError{Kind: GateKindMismatch, Cause: err}
	}

	id, err := s.PrepareGateOpen(ctx, loopID, g, payload)
	if err != nil {
		return gate.ID{}, err
	}
	// The route carries only the gate id. A host-owned answer is delivered to the
	// opener's slot and never translated into a command, so there is no loop to
	// address — and leaving LoopID zero means that even a future miswiring would
	// fail to find a loop rather than reach the wrong one.
	if err := s.ActivateGate(ctx, id, gate.Route{GateID: id}); err != nil {
		// The gate never became public, so nothing can answer it. Drop it so a
		// failed open leaves neither a directory entry nor an answer slot behind.
		_ = s.CloseGate(ctx, id, gate.CloseAbandoned)
		return gate.ID{}, err
	}
	return id, nil
}

// validateHostGatePayload reports whether payload is the right shape for kind and
// satisfies that shape's own invariants. It fails closed: a kind with no
// host-owned payload is rejected rather than opened unvalidated.
func validateHostGatePayload(kind gate.Kind, payload gate.Payload) error {
	switch kind {
	case gate.KindForm:
		form, ok := formPayloadFromGatePayload(payload)
		if !ok {
			return errHostGatePayloadMismatch
		}
		return gate.ValidateFormSchema(form.Schema)
	case gate.KindOpenURL:
		openURL, ok := openURLPayloadFromGatePayload(payload)
		if !ok {
			return errHostGatePayloadMismatch
		}
		return gate.ValidateOpenURLPayload(openURL)
	default:
		return errHostGatePayloadMismatch
	}
}

// errHostGatePayloadMismatch reports a payload whose type does not match the
// gate kind it was offered for.
var errHostGatePayloadMismatch = errors.New("sessionruntime: gate payload does not match gate kind")

func openURLPayloadFromGatePayload(payload gate.Payload) (gate.OpenURLPayload, bool) {
	switch v := payload.(type) {
	case gate.OpenURLPayload:
		return v, true
	case *gate.OpenURLPayload:
		if v == nil {
			return gate.OpenURLPayload{}, false
		}
		return *v, true
	default:
		return gate.OpenURLPayload{}, false
	}
}

// AwaitGateAnswer blocks until a HOST-OWNED gate is answered and returns the
// validated answer, including the form values that are deliberately absent from
// every durable record.
//
// It is the host's half of the loop's command dispatch: the opener of a form or
// open-url gate calls PrepareGateOpen, ActivateGate, then this. It returns a
// typed *GateError{GateNotFound} for a gate that is not host-owned, was never
// prepared, or whose answer was already taken — an answer is delivered exactly
// once.
//
// The opener MUST either await or CloseGate, which is the same obligation
// ActivateGate already places on it. Both paths free the slot.
//
// A ctx cancellation abandons the wait and frees the slot; it does NOT close the
// gate, because the gate is durable state and ctx is the caller's. An opener that
// gives up should CloseGate.
func (s *Session) AwaitGateAnswer(ctx context.Context, id gate.ID) (gate.Answer, error) {
	s.gatesMu.Lock()
	slot, ok := s.gateAnswers[id]
	s.gatesMu.Unlock()
	if !ok {
		return gate.Answer{}, &GateError{GateID: id, Kind: GateNotFound}
	}

	select {
	case answer, ok := <-slot:
		s.releaseGateAnswerSlot(id)
		if !ok {
			// CloseGate closed the slot: the gate was abandoned or withdrawn and
			// will never be answered. Report it as gone rather than returning a
			// zero Answer an opener could mistake for a real one.
			return gate.Answer{}, &GateError{GateID: id, Kind: GateNotFound}
		}
		return answer, nil
	case <-ctx.Done():
		s.releaseGateAnswerSlot(id)
		return gate.Answer{}, ctx.Err()
	}
}

// releaseGateAnswerSlot drops a host-owned gate's delivery slot once its opener
// is done with it. It does NOT close the channel — the opener is the reader, and
// only CloseGate (which knows no answer is coming) closes. It is idempotent.
func (s *Session) releaseGateAnswerSlot(id gate.ID) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	delete(s.gateAnswers, id)
}

// closeGateAnswerSlotLocked closes and drops a host-owned gate's delivery slot,
// waking an opener blocked in AwaitGateAnswer with GateNotFound.
//
// Closing is safe against a concurrent send for two independent reasons, and the
// second is what this relies on. First, CloseGate and RespondGate can never both
// act on one gate: both claim the entry by flipping it to gateClaiming under
// gatesMu, and the loser returns GateNotReady. Second — and this is the guarantee
// that does not depend on that protocol — deliverGateAnswer performs its send
// while holding gatesMu, so once this has deleted the slot no sender can still
// find it. The caller MUST hold gatesMu.
func (s *Session) closeGateAnswerSlotLocked(id gate.ID) {
	slot, ok := s.gateAnswers[id]
	if !ok {
		return
	}
	close(slot)
	delete(s.gateAnswers, id)
}

// deliverGateAnswer hands a host-owned gate's answer to its opener.
//
// The send holds gatesMu, which is what makes closing a slot safe: a closer
// (CloseGate or shutdown) removes the slot from the map under the same lock, so
// this either finds it and sends, or does not find it and does nothing. There is
// no interleaving in which it sends on a closed channel.
//
// Holding the lock across a channel send is safe here only because the send
// cannot block: the slot is buffered with capacity one and written at most once
// (RespondGate removes the directory entry before delivering, so no second
// response can reach the same gate). The select/default is belt-and-braces — an
// opener that has already given up costs a discarded value, not a stuck session.
func (s *Session) deliverGateAnswer(id gate.ID, answer gate.Answer) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	slot, ok := s.gateAnswers[id]
	if !ok {
		return
	}
	select {
	case slot <- answer:
	default:
	}
}

// ActivateGate is called by the owner after its local blocker/continuation exists.
// It requires a preparing gate, appends the public GateOpened event via the strict
// gateAppender, stores the private route, and flips the entry to open so
// ListGates returns it. A failed append leaves the gate preparing. An unknown or
// non-preparing gate returns a typed *GateError.
func (s *Session) ActivateGate(ctx context.Context, id gate.ID, route gate.Route) error {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()

	entry, ok := s.gates[id]
	if !ok {
		return &GateError{GateID: id, Kind: GateNotFound}
	}
	if entry.state != gatePreparing {
		return &GateError{GateID: id, Kind: GateNotReady}
	}

	stamped, err := s.factory.Stamp(event.Header{Coordinates: entry.coordinates})
	if err != nil {
		return &GateError{GateID: id, Kind: GateAppendFailed, Cause: err}
	}
	opened := event.GateOpened{Header: stamped, Gate: entry.gate}
	if err := s.gateAppender.AppendGateOpened(ctx, opened); err != nil {
		return &GateError{GateID: id, Kind: GateAppendFailed, Cause: err}
	}

	entry.route = route
	entry.state = gateOpen
	s.gates[id] = entry
	s.startGatePolicyTimerLocked(id, entry)
	return nil
}

// ListGates returns the public envelopes of all open gates — preparing, claiming,
// and closed entries are excluded. The returned slice is a snapshot; mutating it
// does not affect the directory.
func (s *Session) ListGates(context.Context) []gate.Gate {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	out := make([]gate.Gate, 0, len(s.gates))
	for _, entry := range s.gates {
		if entry.state == gateOpen {
			out = append(out, entry.gate)
		}
	}
	return out
}

// CloseGate closes or abandons a session-owned gate without dispatching an
// answer to the resolver. Preparing gates were never public, so they are removed
// without a public GateResolved. Open gates are durably resolved before removal.
func (s *Session) CloseGate(ctx context.Context, id gate.ID, reason gate.CloseReason) error {
	s.gatesMu.Lock()
	entry, ok := s.gates[id]
	if !ok {
		s.gatesMu.Unlock()
		return &GateError{GateID: id, Kind: GateNotFound}
	}
	switch entry.state {
	case gatePreparing:
		s.stopGateTimerLocked(id)
		delete(s.gates, id)
		s.closeGateAnswerSlotLocked(id)
		s.gatesMu.Unlock()
		return nil
	case gateOpen:
		entry.state = gateClaiming
		s.gates[id] = entry
		s.gatesMu.Unlock()
	case gateClaiming, gateClosed:
		s.gatesMu.Unlock()
		return &GateError{GateID: id, Kind: GateNotReady}
	default:
		s.gatesMu.Unlock()
		return &GateError{GateID: id, Kind: GateNotReady}
	}

	resolved, err := s.buildGateClosed(entry, id, reason)
	if err != nil {
		s.revertClaiming(id)
		return err
	}
	if err := s.gateAppender.AppendGateResolved(ctx, resolved); err != nil {
		s.revertClaiming(id)
		return &GateError{GateID: id, Kind: GateAppendFailed, Cause: err}
	}

	s.gatesMu.Lock()
	s.stopGateTimerLocked(id)
	delete(s.gates, id)
	// An owner-closed gate is never answered, so its opener must not keep waiting
	// on a slot nothing will ever write to.
	s.closeGateAnswerSlotLocked(id)
	s.gatesMu.Unlock()
	return nil
}

// checkGateCap returns a typed *GateError{GateCapacity} if the directory is at or
// above the configured cap (counting preparing + open + claiming). A zero cap
// means unlimited. The caller MUST hold gatesMu.
func (s *Session) checkGateCap() error {
	if s.gateCaps.MaxOpen <= 0 {
		return nil
	}
	count := 0
	for _, entry := range s.gates {
		if entry.state == gatePreparing || entry.state == gateOpen || entry.state == gateClaiming {
			count++
		}
	}
	if count >= s.gateCaps.MaxOpen {
		return &GateError{Kind: GateCapacity}
	}
	return nil
}

func (s *Session) resolveGatePolicy(g gate.Gate) (gate.ResponsePolicy, error) {
	policy := g.ResponsePolicy
	if policy.Timeout == 0 && policy.OnTimeout == "" && g.Kind == gate.KindPermission {
		policy.Timeout = 5 * time.Minute
		policy.OnTimeout = gate.PolicyRespond
		policy.Response = gate.ResponseTemplate{Action: "deny"}
	}
	if s.gateCaps.MaxTimeout > 0 && policy.Timeout > s.gateCaps.MaxTimeout {
		return gate.ResponsePolicy{}, &GateError{GateID: g.ID, Kind: GateCapacity}
	}
	switch policy.EffectiveAction() {
	case gate.PolicyWait:
		return policy, nil
	case gate.PolicyRespond:
		if policy.Timeout <= 0 || policy.Response.Action == "" {
			return gate.ResponsePolicy{}, &GateError{GateID: g.ID, Kind: GateActionInvalid}
		}
		return policy, nil
	case gate.PolicyModelDecide, gate.PolicySuspendSession:
		return gate.ResponsePolicy{}, &GateError{GateID: g.ID, Kind: GateActionInvalid}
	default:
		return gate.ResponsePolicy{}, &GateError{GateID: g.ID, Kind: GateActionInvalid}
	}
}

// startGatePolicyTimerLocked starts the activation-time timer for PolicyRespond.
// The caller MUST hold gatesMu.
func (s *Session) startGatePolicyTimerLocked(id gate.ID, entry gateEntry) {
	policy := entry.gate.ResponsePolicy
	if policy.EffectiveAction() != gate.PolicyRespond || policy.Timeout <= 0 {
		return
	}
	if s.gateTimers == nil {
		s.gateTimers = make(map[gate.ID]*time.Timer)
	}
	s.stopGateTimerLocked(id)
	template := policy.Response
	s.gateTimers[id] = time.AfterFunc(policy.Timeout, func() {
		_ = s.RespondGate(s.sessionCtx, gate.GateResponse{
			GateID: id,
			Action: template.Action,
			Values: cloneRawValues(template.Values),
			Source: gate.ResponseSource{Kind: gate.ResponseFromPolicy, Reason: "timeout"},
		})
	})
}

// stopGateTimerLocked stops and removes a policy timer. The caller MUST hold
// gatesMu.
func (s *Session) stopGateTimerLocked(id gate.ID) {
	if s.gateTimers == nil {
		return
	}
	if timer := s.gateTimers[id]; timer != nil {
		timer.Stop()
	}
	delete(s.gateTimers, id)
}

func cloneRawValues(in map[string]json.RawMessage) map[string]json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

// mintGateID mints a fresh gate.ID via the session's id generator.
func (s *Session) mintGateID() (gate.ID, error) {
	id, err := s.newID()
	if err != nil {
		return gate.ID{}, &GateError{Kind: GateAppendFailed, Cause: err}
	}
	return gate.ID(id), nil
}

// stampGateEvent stamps a GatePrepared event with the given coordinates and gate.
// The factory mints a fresh EventID and CreatedAt; the coordinates carry the
// session, loop, turn, and step identity the event's stepProfile requires.
func (s *Session) stampGateEvent(coords identity.Coordinates, g gate.Gate) (event.GatePrepared, error) {
	stamped, err := s.factory.Stamp(event.Header{Coordinates: coords})
	if err != nil {
		return event.GatePrepared{}, &GateError{Kind: GateAppendFailed, Cause: err}
	}
	return event.GatePrepared{Header: stamped, Gate: g}, nil
}

// RespondGate claims an open gate, durably appends GateResolved, and dispatches
// the translated command to the owning loop. It is durable-first: the GateResolved
// append happens BEFORE the command dispatch, so a crash after the append leaves
// the gate closed (not re-answerable) even if the command was not yet consumed.
// A failed append reverts the in-memory claim and leaves the gate answerable.
// Command dispatch uses s.sessionCtx (not the caller's ctx) so a client
// disconnect after the durable commit does not cancel delivery.
func (s *Session) RespondGate(ctx context.Context, response gate.GateResponse) error {
	s.gatesMu.Lock()
	entry, ok := s.gates[response.GateID]
	if !ok {
		s.gatesMu.Unlock()
		return &GateError{GateID: response.GateID, Kind: GateNotFound}
	}
	if entry.state != gateOpen {
		s.gatesMu.Unlock()
		return &GateError{GateID: response.GateID, Kind: GateNotReady}
	}
	if !validateGateAction(entry.gate, response.Action) {
		s.gatesMu.Unlock()
		return &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
	translated, err := s.translateGateResponse(entry, response)
	if err != nil {
		s.gatesMu.Unlock()
		return err
	}
	entry.state = gateClaiming
	s.gates[response.GateID] = entry
	s.gatesMu.Unlock()

	resolved, err := s.buildGateResolved(entry, response, translated.audit, translated.approvalScope)
	if err != nil {
		s.revertClaiming(response.GateID)
		return err
	}
	if err := s.gateAppender.AppendGateResolved(ctx, resolved); err != nil {
		s.revertClaiming(response.GateID)
		return &GateError{GateID: response.GateID, Kind: GateAppendFailed, Cause: err}
	}

	s.gatesMu.Lock()
	s.stopGateTimerLocked(response.GateID)
	delete(s.gates, response.GateID)
	s.gatesMu.Unlock()

	// Delivery happens after the durable commit and after the entry is gone, so a
	// host-owned answer and a loop command are both unrepeatable.
	if translated.answer != nil {
		s.deliverGateAnswer(response.GateID, *translated.answer)
		return nil
	}
	_ = s.dispatchGateCommand(entry, translated.cmd)
	return nil
}

// revertClaiming reverts a gate from claiming back to open after a failed
// durable append. It is safe to call after the entry was already removed.
func (s *Session) revertClaiming(id gate.ID) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	if entry, ok := s.gates[id]; ok && entry.state == gateClaiming {
		entry.state = gateOpen
		s.gates[id] = entry
	}
}

// validateGateAction reports whether action matches one of the gate's prompt
// controls. An empty action or a gate with no controls fails secure.
func validateGateAction(g gate.Gate, action string) bool {
	if action == "" || len(g.Prompt.Controls) == 0 {
		return false
	}
	for _, c := range g.Prompt.Controls {
		if c.Action == action {
			return true
		}
	}
	return false
}

// buildGateResolved stamps and builds the GateResolved event from the response,
// resolved audit, and already-validated approval scope.
func (s *Session) buildGateResolved(entry gateEntry, response gate.GateResponse, audit gate.ResponseAudit, approvalScope *tool.ApprovalScope) (event.GateResolved, error) {
	stamped, err := s.factory.Stamp(event.Header{Coordinates: entry.coordinates})
	if err != nil {
		return event.GateResolved{}, &GateError{GateID: response.GateID, Kind: GateAppendFailed, Cause: err}
	}
	resolved := event.GateResolved{
		Header: stamped,
		GateID: response.GateID,
		Reason: gate.CloseAnswered,
		Action: response.Action,
		Source: response.Source,
		Audit:  audit,
	}
	if approvalScope != nil {
		resolved.ApprovalScope = *approvalScope
	}
	return resolved, nil
}

func (s *Session) buildGateClosed(entry gateEntry, id gate.ID, reason gate.CloseReason) (event.GateResolved, error) {
	if reason == "" {
		reason = gate.CloseOwnerClosed
	}
	stamped, err := s.factory.Stamp(event.Header{Coordinates: entry.coordinates})
	if err != nil {
		return event.GateResolved{}, &GateError{GateID: id, Kind: GateAppendFailed, Cause: err}
	}
	return event.GateResolved{
		Header: stamped,
		GateID: id,
		Reason: reason,
	}, nil
}

// dispatchGateCommand routes the translated command to the owning loop using
// s.sessionCtx (not the caller's ctx) so a client disconnect after the durable
// commit does not cancel delivery.
func (s *Session) dispatchGateCommand(entry gateEntry, cmd command.Command) error {
	l, ok := s.loopFor(entry.route.LoopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	return s.routeGate(s.sessionCtx, entry.route.LoopID, l, cmd)
}

// translatedGateResponse is the validated result of a response: what to deliver,
// and what to durably record.
//
// Exactly one of cmd and answer is set. A loop-owned gate yields a cmd addressed
// to the owning loop; a host-owned gate (hostOwnedGate) yields an answer for the
// opener blocked in AwaitGateAnswer. The two are separate fields rather than one
// interface because they are delivered through different seams and only the cmd
// has a loop to fail to find.
type translatedGateResponse struct {
	cmd           command.Command
	answer        *gate.Answer
	audit         gate.ResponseAudit
	approvalScope *tool.ApprovalScope
}

// translateGateResponse validates the payload-specific parts of the response and
// builds the translated command, redacted audit, and validated approval scope. It
// returns a typed *GateError on validation failure (invalid grants, missing
// values, unknown kind).
func (s *Session) translateGateResponse(entry gateEntry, response gate.GateResponse) (translatedGateResponse, error) {
	cmdID, err := s.newCommandID()
	if err != nil {
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateAppendFailed, Cause: err}
	}
	hdr := command.Header{CommandID: cmdID, Agency: identity.AgencyUser, CreatedAt: s.stampNow()}
	route := command.GateRoute{
		Coordinates:     identity.Coordinates{SessionID: entry.coordinates.SessionID, LoopID: entry.coordinates.LoopID},
		GateID:          response.GateID,
		ToolExecutionID: uuid.UUID(entry.route.ToolExecutionID),
	}
	switch entry.gate.Kind {
	case gate.KindPermission:
		return s.translatePermissionResponse(hdr, route, entry.payload, response)
	case gate.KindAskUser:
		return s.translateAskUserResponse(hdr, route, response)
	case gate.KindForm, gate.KindOpenURL:
		// Both kinds are answerable only as host-owned gates. A form or open-url
		// gate declaring ResolverLoop has no loop-side resolver to route to, so it
		// is refused here rather than answered into a void.
		if !hostOwnedGate(entry.gate) {
			return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateKindMismatch}
		}
		if entry.gate.Kind == gate.KindForm {
			return translateFormResponse(entry.payload, response)
		}
		return translateOpenURLResponse(response)
	default:
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateKindMismatch}
	}
}

// translateFormResponse validates a form gate's response against the schema in
// its PAYLOAD — the authoritative record of what was asked — and builds the live
// answer plus the durable audit that records it.
//
// Accept is the only action that carries values. Decline and cancel are explicit
// non-answers: they record that a human refused or that the request was
// withdrawn, and any values submitted alongside them are ignored rather than
// validated, because there is no answer to validate. Both actions must still
// appear in the gate's Prompt.Controls (validateGateAction has already checked
// that), so an integration cannot be declined against its will by a control it
// never offered.
func translateFormResponse(payload gate.Payload, response gate.GateResponse) (translatedGateResponse, error) {
	formPayload, ok := formPayloadFromGatePayload(payload)
	if !ok {
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateKindMismatch}
	}

	switch response.Action {
	case gate.FormActionAccept:
		// ParseFormAnswers re-validates the schema before reading any value, so
		// bounds, field kinds (FieldMultiSelect is refused), required fields,
		// unknown fields, over-long values, and select options are all enforced
		// here and not trusted from the opener.
		answers, err := gate.ParseFormAnswers(formPayload.Schema, response.Values)
		if err != nil {
			return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
		return translatedGateResponse{
			answer: &gate.Answer{
				GateID: response.GateID,
				Action: response.Action,
				Values: answers,
				Source: response.Source,
			},
			audit: gate.NewFormAudit(formPayload.Schema, answers),
		}, nil
	case gate.FormActionDecline, gate.FormActionCancel:
		return translatedGateResponse{
			answer: &gate.Answer{
				GateID: response.GateID,
				Action: response.Action,
				Source: response.Source,
			},
		}, nil
	default:
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
}

// translateOpenURLResponse builds the live answer for an open-url gate.
//
// It carries NO audit, and that is a deliberate omission rather than a gap. An
// open-url answer has nothing to redact and nothing to add: the human either
// reported completion or did not, which GateResolved.Action already records in
// the clear, and the only other fact about the request — its DisplayOrigin — is
// already durable in the payload. The URL itself must never reach a durable
// record (gate.OpenURLPayload), and a decoded payload does not even have one. An
// audit member here would be an empty struct whose only effect would be to add a
// codec arm that could later be widened to hold the very thing that must not be
// stored. A nil audit is the same choice permission "deny" already makes.
func translateOpenURLResponse(response gate.GateResponse) (translatedGateResponse, error) {
	switch response.Action {
	case gate.FormActionAccept, gate.FormActionDecline, gate.FormActionCancel:
		return translatedGateResponse{
			answer: &gate.Answer{
				GateID: response.GateID,
				Action: response.Action,
				Source: response.Source,
			},
		}, nil
	default:
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
}

func formPayloadFromGatePayload(payload gate.Payload) (gate.FormPayload, bool) {
	switch v := payload.(type) {
	case gate.FormPayload:
		return v, true
	case *gate.FormPayload:
		if v == nil {
			return gate.FormPayload{}, false
		}
		return *v, true
	default:
		return gate.FormPayload{}, false
	}
}

// translatePermissionResponse builds an ApproveToolCall or DenyToolCall from a
// permission gate response. For approve, it extracts scope and accepted_grants
// from Values and validates the grants against the payload's request.
func (s *Session) translatePermissionResponse(hdr command.Header, route command.GateRoute, payload gate.Payload, response gate.GateResponse) (translatedGateResponse, error) {
	switch response.Action {
	case "approve":
		scope, grants, audit, err := validatePermissionApprove(payload, response)
		if err != nil {
			return translatedGateResponse{}, err
		}
		return translatedGateResponse{
			cmd:           command.ApproveToolCall{Header: hdr, GateRoute: route, Scope: scope, AcceptedGrants: grants},
			audit:         audit,
			approvalScope: &scope,
		}, nil
	case "deny":
		return translatedGateResponse{cmd: command.DenyToolCall{Header: hdr, GateRoute: route}}, nil
	default:
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
}

// translateAskUserResponse builds a ProvideUserInput from an ask-user gate
// response. It extracts the answer from Values["answer"].
func (s *Session) translateAskUserResponse(hdr command.Header, route command.GateRoute, response gate.GateResponse) (translatedGateResponse, error) {
	var answer string
	if raw, ok := response.Values["answer"]; ok {
		if err := json.Unmarshal(raw, &answer); err != nil {
			return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
	}
	preview := answer
	if len(preview) > 80 {
		preview = preview[:80]
	}
	return translatedGateResponse{
		cmd:   command.ProvideUserInput{Header: hdr, GateRoute: route, Answer: answer},
		audit: gate.AskUserAudit{AnswerPreview: preview},
	}, nil
}

// validatePermissionApprove extracts scope and accepted_grants from the response
// Values, validates the scope against the payload request's AllowedScopes,
// validates Bash grant tokens against the request's Grants, and builds the
// PermissionAudit from the accepted grant descriptions (not tokens). A scope the
// request did not offer or an accepted grant not in the request's Grants fails
// secure.
func validatePermissionApprove(payload gate.Payload, response gate.GateResponse) (tool.ApprovalScope, []string, gate.ResponseAudit, error) {
	rawScope, ok := response.Values["scope"]
	if !ok {
		return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
	var scopeValue string
	if err := json.Unmarshal(rawScope, &scopeValue); err != nil {
		return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
	}
	scope, ok := tool.ParseApprovalScopeValue(scopeValue)
	if !ok {
		return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
	permPayload, ok := permissionPayloadFromGatePayload(payload)
	if !ok || permPayload.Request == nil {
		return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
	if !approvalScopeAllowed(scope, permPayload.Request.AllowedScopes()) {
		return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}

	var grants []string
	if raw, ok := response.Values["accepted_grants"]; ok {
		if err := json.Unmarshal(raw, &grants); err != nil {
			return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
	}
	bashReq, ok := permPayload.Request.(tool.BashRequest)
	if !ok {
		return scope, grants, gate.PermissionAudit{}, nil
	}
	validTokens := make(map[string]string, len(bashReq.Grants))
	for _, g := range bashReq.Grants {
		validTokens[g.Token] = g.Description
	}
	descs := make([]string, 0, len(grants))
	for _, t := range grants {
		desc, exists := validTokens[t]
		if !exists {
			return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
		}
		descs = append(descs, desc)
	}
	return scope, grants, gate.PermissionAudit{AcceptedGrantDescriptions: descs}, nil
}

func permissionPayloadFromGatePayload(payload gate.Payload) (gate.PermissionPayload, bool) {
	switch v := payload.(type) {
	case gate.PermissionPayload:
		return v, true
	case *gate.PermissionPayload:
		if v == nil {
			return gate.PermissionPayload{}, false
		}
		return *v, true
	default:
		return gate.PermissionPayload{}, false
	}
}

func approvalScopeAllowed(scope tool.ApprovalScope, allowed []tool.ApprovalScope) bool {
	for _, candidate := range allowed {
		if scope == candidate {
			return true
		}
	}
	return false
}
