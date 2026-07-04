package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/core/uuid"
)

// Gate-routing request/response bodies. Domain payloads are typed structs (never
// ad-hoc maps); the any escape hatch lives only inside writeJSON.
type (
	// inputRequest is the POST /input body: a raw JSON array of tagged content
	// blocks, decoded through the content codec at the trust boundary.
	inputRequest struct {
		Blocks json.RawMessage `json:"blocks"`
	}
	// inputResponse echoes the command id the agent minted for the submitted turn.
	inputResponse struct {
		InputID string `json:"input_id"`
	}
	// gateRequest is the POST /gates/{tid} body: the action to take on the open
	// gate, plus the payload each action needs (Answer for user-input, the optional
	// Scope for a permission approve — a nil Scope defaults to ScopeOnce).
	gateRequest struct {
		Action string `json:"action"`
		Answer string `json:"answer"`
		Scope  *int   `json:"scope"`
	}
	// gateView is one open gate in the reconnect snapshot (GET /gates), with every
	// id rendered as its string form.
	gateView struct {
		ToolExecutionID string `json:"tool_execution_id"`
		LoopID          string `json:"loop_id"`
		Kind            string `json:"kind"`
		Prompt          string `json:"prompt"`
	}
	// gatesResponse wraps the open-gate list so an empty registry serializes as
	// {"gates":[]} rather than a bare null.
	gatesResponse struct {
		Gates []gateView `json:"gates"`
	}
)

// Gate action verbs. The client names the control to apply; the handler validates
// the verb is known AND matches the gate Kind before dispatching.
const (
	actionApprove = "approve"
	actionDeny    = "deny"
	actionAnswer  = "answer"
)

// Client-safe input/gate error messages. Generic by design so a response never
// leaks the runner's internal state; the concrete cause is logged via slog.
const (
	msgInvalidInput      = "invalid input"
	msgSubmitFailed      = "could not submit input"
	msgInvalidToolExecID = "invalid tool execution id"
	msgGateNotFound      = "gate not found"
	msgInvalidGate       = "invalid gate request"
	msgInvalidScope      = "invalid approval scope"
	msgGateKindMismatch  = "gate action does not match gate kind"
	msgGateResolveFailed = "could not resolve gate"
	msgGatesUnavailable  = "gate registry unavailable"
)

// invalidToolExecutionIDError reports that the {tid} path value could not be
// parsed as a UUID. It carries the raw value and cause for the audit log only —
// the client sees a generic message. Typed (per CLAUDE.md) so a caller can
// errors.As it to distinguish a bad-input 400 from other failures.
type invalidToolExecutionIDError struct {
	Value string
	Cause error
}

func (e invalidToolExecutionIDError) Error() string {
	return "api: invalid tool execution id " + strconv.Quote(e.Value) + ": " + e.Cause.Error()
}

func (e invalidToolExecutionIDError) Unwrap() error { return e.Cause }

// parseToolExecutionID reads the {tid} path value and parses it as a UUID,
// returning a typed invalidToolExecutionIDError on any malformed input. It
// mirrors parseSessionID: validate at the boundary before the id reaches the
// registry.
func parseToolExecutionID(r *http.Request) (uuid.UUID, error) {
	raw := r.PathValue("tid")
	var id uuid.UUID
	if err := id.UnmarshalText([]byte(raw)); err != nil {
		return uuid.UUID{}, invalidToolExecutionIDError{Value: raw, Cause: err}
	}
	return id, nil
}

// gateDispatchError carries the HTTP status + client-safe message for a gate
// resolution that could not be dispatched (unknown action, kind mismatch, bad
// scope, or an agent method failure). It is typed so handleResolveGate maps each
// failure mode to its status via errors.As rather than a bag of bare returns; the
// Cause (when present) is logged, never returned.
type gateDispatchError struct {
	Status int
	Msg    string
	Cause  error
}

func (e gateDispatchError) Error() string {
	if e.Cause != nil {
		return "api: gate dispatch (" + strconv.Itoa(e.Status) + "): " + e.Msg + ": " + e.Cause.Error()
	}
	return "api: gate dispatch (" + strconv.Itoa(e.Status) + "): " + e.Msg
}

func (e gateDispatchError) Unwrap() error { return e.Cause }

// handleInput serves POST /sessions/{sid}/input. It decodes the request's tagged
// content blocks through the content codec (every decode failure — malformed
// JSON, unknown block type, over-limit, or an empty set — fails secure with 400),
// submits them, and returns 200 with the agent's command id as input_id. An
// unknown session is 404, a malformed id is 400, and a Submit error is a 500 that
// leaks nothing.
func (s *server) handleInput(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r)
	if err != nil {
		slog.Warn("api: input rejected invalid session id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidSessionID)
		return
	}
	entry, ok := s.getSession(sid)
	if !ok {
		writeError(w, http.StatusNotFound, msgSessionNotFound)
		return
	}

	var body inputRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		slog.Warn("api: input rejected malformed body", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidInput)
		return
	}
	blocks, err := content.UnmarshalBlocks(body.Blocks)
	if err != nil {
		slog.Warn("api: input rejected undecodable blocks", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidInput)
		return
	}
	if len(blocks) == 0 {
		slog.Warn("api: input rejected empty blocks")
		writeError(w, http.StatusBadRequest, msgInvalidInput)
		return
	}

	cmdID, err := entry.agent.Submit(r.Context(), blocks)
	if err != nil {
		slog.Error("api: input submit failed", "err", err)
		writeError(w, http.StatusInternalServerError, msgSubmitFailed)
		return
	}
	writeJSON(w, http.StatusOK, inputResponse{InputID: cmdID.String()})
}

// handleResolveGate serves POST /sessions/{sid}/gates/{tid}. It resolves the OPEN
// gate keyed by {tid} — pulling the producing LoopID from the supervisor registry
// (NOT the client body, the whole point of the routing surface), validating the
// requested action against the gate Kind, then dispatching Approve/Deny/
// ProvideAnswer. An unknown gate is 404; a kind mismatch is 409; a bad action or
// scope is 400; an agent error is 500; success is 204.
func (s *server) handleResolveGate(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r)
	if err != nil {
		slog.Warn("api: gate resolve rejected invalid session id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidSessionID)
		return
	}
	tid, err := parseToolExecutionID(r)
	if err != nil {
		slog.Warn("api: gate resolve rejected invalid tool execution id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidToolExecID)
		return
	}
	entry, ok := s.getSession(sid)
	if !ok {
		writeError(w, http.StatusNotFound, msgSessionNotFound)
		return
	}
	gate, ok := entry.sup.lookup(tid)
	if !ok {
		writeError(w, http.StatusNotFound, msgGateNotFound)
		return
	}

	var req gateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("api: gate resolve rejected malformed body", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidGate)
		return
	}
	if err := dispatchGate(r.Context(), entry.agent, gate, tid, req); err != nil {
		writeGateDispatchError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dispatchGate validates and routes one gate action. It first checks the action
// is known AND matches the gate Kind (fail-secure: an approve/deny only resolves a
// permission gate, an answer only a user-input gate), then dispatches to the agent
// using gate.LoopID (from the registry) and the tool-execution id. All failures
// are typed gateDispatchErrors carrying the mapped HTTP status.
func dispatchGate(ctx context.Context, agent Agent, gate pendingGate, tid uuid.UUID, req gateRequest) error {
	if err := requireActionForKind(req.Action, gate.Kind); err != nil {
		return err
	}
	switch req.Action {
	case actionApprove:
		scope, err := resolveScope(req.Scope)
		if err != nil {
			return err
		}
		return wrapAgentErr(agent.Approve(ctx, gate.LoopID, tid, scope))
	case actionDeny:
		return wrapAgentErr(agent.Deny(ctx, gate.LoopID, tid))
	case actionAnswer:
		return wrapAgentErr(agent.ProvideAnswer(ctx, gate.LoopID, tid, req.Answer))
	default:
		// Unreachable: requireActionForKind already rejected any unknown action.
		return gateDispatchError{Status: http.StatusBadRequest, Msg: msgInvalidGate}
	}
}

// requireActionForKind rejects an unknown action verb (400) and a well-formed
// verb aimed at the wrong gate kind (409). Approve/Deny require a permission gate;
// Answer requires a user-input gate. Fail-secure: never dispatch the wrong control
// to the session.
func requireActionForKind(action, kind string) error {
	switch action {
	case actionApprove, actionDeny:
		if kind != kindPermission {
			return gateDispatchError{Status: http.StatusConflict, Msg: msgGateKindMismatch}
		}
		return nil
	case actionAnswer:
		if kind != kindUserInput {
			return gateDispatchError{Status: http.StatusConflict, Msg: msgGateKindMismatch}
		}
		return nil
	default:
		return gateDispatchError{Status: http.StatusBadRequest, Msg: msgInvalidGate}
	}
}

// resolveScope maps the optional wire scope to a tool.ApprovalScope: a nil scope
// defaults to ScopeOnce, and any value outside [ScopeOnce, ScopeWorkspace] is a
// fail-secure 400 (an unknown scope is never widened).
func resolveScope(scope *int) (tool.ApprovalScope, error) {
	if scope == nil {
		return tool.ScopeOnce, nil
	}
	if *scope < int(tool.ScopeOnce) || *scope > int(tool.ScopeWorkspace) {
		return tool.ScopeOnce, gateDispatchError{Status: http.StatusBadRequest, Msg: msgInvalidScope}
	}
	return tool.ApprovalScope(*scope), nil
}

// wrapAgentErr maps a nil agent error to nil and any dispatch failure to a typed
// 500 (generic message, cause logged) — the session fault path.
func wrapAgentErr(err error) error {
	if err == nil {
		return nil
	}
	return gateDispatchError{Status: http.StatusInternalServerError, Msg: msgGateResolveFailed, Cause: err}
}

// writeGateDispatchError maps a dispatchGate failure to its HTTP response: a
// gateDispatchError carries the status + client-safe message (logging the cause at
// Error level when it wraps a session fault, Warn otherwise). Any other
// (unexpected) error is a generic 500.
func writeGateDispatchError(w http.ResponseWriter, err error) {
	var de gateDispatchError
	if errors.As(err, &de) {
		if de.Cause != nil {
			slog.Error("api: gate dispatch failed", "err", de)
		} else {
			slog.Warn("api: gate dispatch rejected", "err", de)
		}
		writeError(w, de.Status, de.Msg)
		return
	}
	slog.Error("api: gate dispatch unexpected error", "err", err)
	writeError(w, http.StatusInternalServerError, msgGateResolveFailed)
}

// handleListGates serves GET /sessions/{sid}/gates: the reconnect-discovery
// snapshot of every open gate. It fails secure with 503 when the supervisor's
// subscription has DIED (exitError non-nil) — its registry is frozen and stale, so
// it must not be served as the live set. Otherwise it returns 200 with the
// open-gate list (empty => {"gates":[]}). Unknown session is 404, malformed id is
// 400.
func (s *server) handleListGates(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r)
	if err != nil {
		slog.Warn("api: list gates rejected invalid session id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidSessionID)
		return
	}
	entry, ok := s.getSession(sid)
	if !ok {
		writeError(w, http.StatusNotFound, msgSessionNotFound)
		return
	}
	if err := entry.sup.exitError(); err != nil {
		slog.Error("api: list gates on dead supervisor", "err", err)
		writeError(w, http.StatusServiceUnavailable, msgGatesUnavailable)
		return
	}

	open := entry.sup.list()
	views := make([]gateView, 0, len(open))
	for _, g := range open {
		views = append(views, gateView{
			ToolExecutionID: g.ToolExecutionID.String(),
			LoopID:          g.LoopID.String(),
			Kind:            g.Kind,
			Prompt:          g.Prompt,
		})
	}
	writeJSON(w, http.StatusOK, gatesResponse{Gates: views})
}
