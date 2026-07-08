package session

import (
	"context"
	"encoding/json"

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

// GateCaps bounds the live gate directory. The cap counts preparing + open +
// claiming so failed activations cannot accumulate invisible prepared entries.
// Zero means no cap.
type GateCaps struct {
	MaxOpen int
}

// GateErrorKind names the failure mode of a gate directory operation.
type GateErrorKind string

const (
	GateNotFound      GateErrorKind = "not_found"
	GateNotReady      GateErrorKind = "not_ready"
	GateKindMismatch  GateErrorKind = "kind_mismatch"
	GateActionInvalid GateErrorKind = "action_invalid"
	GateCapacity      GateErrorKind = "capacity"
	GateAppendFailed  GateErrorKind = "append_failed"
)

// GateError is the typed error returned by gate directory operations. Callers
// use errors.As to recover it and switch on Kind to branch on the failure mode.
type GateError struct {
	GateID gate.ID
	Kind   GateErrorKind
	Cause  error
}

func (e *GateError) Error() string {
	prefix := "session: gate"
	if e.GateID != (gate.ID{}) {
		prefix += " " + e.GateID.String()
	}
	switch e.Kind {
	case GateNotFound:
		return prefix + " not found"
	case GateNotReady:
		return prefix + " not ready"
	case GateKindMismatch:
		return prefix + " kind mismatch"
	case GateActionInvalid:
		return prefix + " action invalid"
	case GateCapacity:
		return prefix + " capacity exceeded"
	case GateAppendFailed:
		if e.Cause != nil {
			return prefix + " append failed: " + e.Cause.Error()
		}
		return prefix + " append failed"
	default:
		return prefix + " error"
	}
}

func (e *GateError) Unwrap() error { return e.Cause }

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

	gateID, err := s.mintGateID()
	if err != nil {
		return gate.ID{}, err
	}
	g.ID = gateID

	coords := identity.Coordinates{
		SessionID: s.SessionID,
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
	return gateID, nil
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
	cmd, audit, err := s.translateGateResponse(entry, response)
	if err != nil {
		s.gatesMu.Unlock()
		return err
	}
	entry.state = gateClaiming
	s.gates[response.GateID] = entry
	s.gatesMu.Unlock()

	resolved, err := s.buildGateResolved(entry, response, audit)
	if err != nil {
		s.revertClaiming(response.GateID)
		return err
	}
	if err := s.gateAppender.AppendGateResolved(ctx, resolved); err != nil {
		s.revertClaiming(response.GateID)
		return &GateError{GateID: response.GateID, Kind: GateAppendFailed, Cause: err}
	}

	s.gatesMu.Lock()
	delete(s.gates, response.GateID)
	s.gatesMu.Unlock()

	return s.dispatchGateCommand(entry, cmd)
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

// buildGateResolved stamps and builds the GateResolved event from the response
// and the resolved audit. The scope is extracted from response.Values for
// permission approve responses.
func (s *Session) buildGateResolved(entry gateEntry, response gate.GateResponse, audit gate.ResponseAudit) (event.GateResolved, error) {
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
	if raw, ok := response.Values["scope"]; ok {
		if err := json.Unmarshal(raw, &resolved.ApprovalScope); err != nil {
			return event.GateResolved{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
	}
	return resolved, nil
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

// translateGateResponse validates the payload-specific parts of the response and
// builds the translated command and the redacted audit. It returns a typed
// *GateError on validation failure (invalid grants, missing values, unknown kind).
func (s *Session) translateGateResponse(entry gateEntry, response gate.GateResponse) (command.Command, gate.ResponseAudit, error) {
	cmdID, err := s.newCommandID()
	if err != nil {
		return nil, nil, &GateError{GateID: response.GateID, Kind: GateAppendFailed, Cause: err}
	}
	hdr := command.Header{CommandID: cmdID, Agency: identity.AgencyUser, CreatedAt: s.stampNow()}
	route := command.GateRoute{
		Coordinates:     identity.Coordinates{SessionID: entry.coordinates.SessionID, LoopID: entry.coordinates.LoopID},
		ToolExecutionID: uuid.UUID(entry.route.ToolExecutionID),
	}
	switch entry.gate.Kind {
	case gate.KindPermission:
		return s.translatePermissionResponse(hdr, route, entry.payload, response)
	case gate.KindAskUser:
		return s.translateAskUserResponse(hdr, route, response)
	default:
		return nil, nil, &GateError{GateID: response.GateID, Kind: GateKindMismatch}
	}
}

// translatePermissionResponse builds an ApproveToolCall or DenyToolCall from a
// permission gate response. For approve, it extracts scope and accepted_grants
// from Values and validates the grants against the payload's request.
func (s *Session) translatePermissionResponse(hdr command.Header, route command.GateRoute, payload gate.Payload, response gate.GateResponse) (command.Command, gate.ResponseAudit, error) {
	switch response.Action {
	case "approve":
		scope, grants, audit, err := validatePermissionApprove(payload, response)
		if err != nil {
			return nil, nil, err
		}
		return command.ApproveToolCall{Header: hdr, GateRoute: route, Scope: scope, AcceptedGrants: grants}, audit, nil
	case "deny":
		return command.DenyToolCall{Header: hdr, GateRoute: route}, nil, nil
	default:
		return nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
}

// translateAskUserResponse builds a ProvideUserInput from an ask-user gate
// response. It extracts the answer from Values["answer"].
func (s *Session) translateAskUserResponse(hdr command.Header, route command.GateRoute, response gate.GateResponse) (command.Command, gate.ResponseAudit, error) {
	var answer string
	if raw, ok := response.Values["answer"]; ok {
		if err := json.Unmarshal(raw, &answer); err != nil {
			return nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
	}
	preview := answer
	if len(preview) > 80 {
		preview = preview[:80]
	}
	return command.ProvideUserInput{Header: hdr, GateRoute: route, Answer: answer}, gate.AskUserAudit{AnswerPreview: preview}, nil
}

// validatePermissionApprove extracts scope and accepted_grants from the response
// Values, validates the grant tokens against the payload's BashRequest.Grants,
// and builds the PermissionAudit from the accepted grant descriptions (not
// tokens). An accepted grant not in the request's Grants fails secure.
func validatePermissionApprove(payload gate.Payload, response gate.GateResponse) (tool.ApprovalScope, []string, gate.ResponseAudit, error) {
	scope := tool.ScopeOnce
	if raw, ok := response.Values["scope"]; ok {
		if err := json.Unmarshal(raw, &scope); err != nil {
			return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
	}
	var grants []string
	if raw, ok := response.Values["accepted_grants"]; ok {
		if err := json.Unmarshal(raw, &grants); err != nil {
			return 0, nil, nil, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
	}
	permPayload, ok := payload.(gate.PermissionPayload)
	if !ok {
		return scope, grants, gate.PermissionAudit{}, nil
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
