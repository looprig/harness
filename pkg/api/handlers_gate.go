package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
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
	// gatesResponse wraps the open-gate list so an empty set serializes as
	// {"gates":[]} rather than a bare null.
	gatesResponse struct {
		Gates []gate.Gate `json:"gates"`
	}
)

// Client-safe input/gate error messages. Generic by design so a response never
// leaks the runner's internal state; the concrete cause is logged via slog.
const (
	msgInvalidInput      = "invalid input"
	msgSubmitFailed      = "could not submit input"
	msgInvalidGateID     = "invalid gate id"
	msgGateNotFound      = "gate not found"
	msgInvalidGate       = "invalid gate request"
	msgGateResolveFailed = "could not resolve gate"
)

// invalidGateIDError reports that the {gid} path value could not be parsed as a
// gate.ID. It carries the raw value and cause for the audit log only —
// the client sees a generic message. Typed (per CLAUDE.md) so a caller can
// errors.As it to distinguish a bad-input 400 from other failures.
type invalidGateIDError struct {
	Value string
	Cause error
}

func (e invalidGateIDError) Error() string {
	return "api: invalid gate id " + strconv.Quote(e.Value) + ": " + e.Cause.Error()
}

func (e invalidGateIDError) Unwrap() error { return e.Cause }

// parseGateID reads the {gid} path value and parses it as a gate.ID, returning a
// typed invalidGateIDError on any malformed input. It mirrors parseSessionID:
// validate at the boundary before the id reaches the agent.
func parseGateID(r *http.Request) (gate.ID, error) {
	raw := r.PathValue("gid")
	var id gate.ID
	if err := id.UnmarshalText([]byte(raw)); err != nil {
		return gate.ID{}, invalidGateIDError{Value: raw, Cause: err}
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

// handleResolveGate serves POST /sessions/{sid}/gates/{gid}. It treats {gid} as
// the opaque gate.ID, decodes the generic gate.ResponseRequest, stamps the source
// as a user response, and delegates the resolved envelope to the agent. Unknown
// session is 404; malformed ids/body are 400; agent errors are 500; success is
// 202 Accepted.
func (s *server) handleResolveGate(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r)
	if err != nil {
		slog.Warn("api: gate resolve rejected invalid session id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidSessionID)
		return
	}
	gid, err := parseGateID(r)
	if err != nil {
		slog.Warn("api: gate resolve rejected invalid gate id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidGateID)
		return
	}
	entry, ok := s.getSession(sid)
	if !ok {
		writeError(w, http.StatusNotFound, msgSessionNotFound)
		return
	}

	var req gate.ResponseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("api: gate resolve rejected malformed body", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidGate)
		return
	}
	response := gate.GateResponse{
		GateID: gid,
		Action: req.Action,
		Values: req.Values,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}
	if err := wrapAgentErr(entry.agent.RespondGate(r.Context(), response)); err != nil {
		writeGateDispatchError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
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
// snapshot of every open gate from the authoritative agent/session directory.
// Unknown session is 404, malformed id is 400.
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

	open := entry.agent.ListGates(r.Context())
	if open == nil {
		open = []gate.Gate{}
	}
	writeJSON(w, http.StatusOK, gatesResponse{Gates: open})
}
