package serve

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/looprig/harness/pkg/gate"
)

// Machine-readable error codes for the gate-response route (SPEC §7a/§8). Each is
// a stable identifier a client can switch on; the paired message is generic and
// client-safe. codeGateNotFound is distinct from codeNotFound: the latter is a
// session (registry) miss, the former is a gate-directory miss reported by the
// authoritative session.
const (
	codeGateNotFound      = "gate_not_found"
	codeGateActionInvalid = "gate_action_invalid"
	codeGateKindMismatch  = "gate_kind_mismatch"
	codeGateNotReady      = "gate_not_ready"
	codeGateCapacity      = "gate_capacity"
)

// Generic, client-safe messages for the gate-response route. They never embed a
// concrete cause; the cause is confined to the audit log via writeErrorCause.
const (
	msgInvalidGID        = "invalid gate id"
	msgGateNotFound      = "gate not found"
	msgGateActionInvalid = "invalid gate action"
	msgGateKindMismatch  = "gate kind mismatch"
	msgGateNotReady      = "gate not ready"
	msgGateCapacity      = "gate capacity exceeded"
	msgGateFailed        = "could not respond to gate"
)

// Stable string kinds for a gate-directory error (SPEC §8). These MIRROR the
// values (*session.GateError).GateErrorKind() returns, but are re-declared here
// so serve maps by the stable STRING and never imports pkg/session (the
// dependency-inversion boundary the deps guard enforces).
const (
	gateKindNotFound      = "not_found"
	gateKindNotReady      = "not_ready"
	gateKindMismatch      = "kind_mismatch"
	gateKindActionInvalid = "action_invalid"
	gateKindCapacity      = "capacity"
	gateKindAppendFailed  = "append_failed"
)

// gateErrorKinder is the STRUCTURAL view of a gate-directory error: any error in
// the chain that exposes a stable string kind. The concrete *session.GateError
// satisfies it via its GateErrorKind() method, so serve recovers and maps the
// kind with errors.As WITHOUT importing pkg/session.
type gateErrorKinder interface {
	GateErrorKind() string
}

// gateAcceptedResponse is the (empty) 202 body for a durably-accepted gate
// response. Acceptance is durable, not proven consumption, so the body carries no
// fields — it encodes to {} purely to keep the JSON content type consistent.
type gateAcceptedResponse struct{}

// handleGateResponse serves POST /v1/sessions/{sid}/gates/{gid}: deliver a
// human's answer to an open gate on a session that is LIVE in this process.
//
// The boundary is validated in order: {sid} and {gid} are parsed (malformed =>
// 400), {sid} is resolved against the live registry (a miss is a generic 404),
// and the body is decoded into a typed gate.ResponseRequest (malformed => 400).
// The server then STAMPS user provenance — Source is set to ResponseFromUser and
// any client-supplied source in the body is ignored (the request DTO has no
// Source field to read) — and hands the assembled gate.GateResponse to the
// authoritative session via RespondGate.
//
// Success is 202 Accepted: the response was durably accepted, which is NOT the
// same as proven consumption. A RespondGate failure is mapped authoritatively by
// the session's stable GateError kind string (not_found=>404, action_invalid and
// kind_mismatch=>400, not_ready=>409, capacity=>503, append_failed=>500); any
// non-GateError is a generic 500. Every error path leaks no internal cause.
func (s *server[S, O]) handleGateResponse(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}
	gid, err := parseGateID(r.PathValue("gid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidGID, false, err)
		return
	}

	sess, ok := s.registry.get(sid)
	if !ok {
		writeErrorCause(w, http.StatusNotFound, codeNotFound, msgNotFound, false, SessionNotFoundError{SessionID: sid})
		return
	}

	req, err := decodeGateResponse(r, s.cfg.maxBodyBytes)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidBody, msgInvalidBody, false, err)
		return
	}

	// The SERVER stamps provenance: this route is only reachable by a human
	// caller, so the response is always user-sourced. Any Source the client tried
	// to smuggle in the body is never read (ResponseRequest carries none).
	resp := gate.GateResponse{
		GateID: gid,
		Action: req.Action,
		Values: req.Values,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}

	if err := sess.RespondGate(r.Context(), resp); err != nil {
		writeGateError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, gateAcceptedResponse{})
}

// decodeGateResponse reads the request body and decodes it into a typed
// gate.ResponseRequest (action + raw-JSON values). It validates SHAPE at the
// boundary — a read failure (including a body over the cap) or malformed JSON
// returns an error the caller maps to 400. Action SEMANTICS (which actions a gate
// kind accepts) are the authoritative session's job, surfaced as a GateError.
// An absent or empty body decodes to the zero request (empty action), which the
// session rejects as action_invalid.
func decodeGateResponse(r *http.Request, maxBytes int64) (gate.ResponseRequest, error) {
	var req gate.ResponseRequest
	if r.Body == nil {
		return req, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return req, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return req, nil
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, err
	}
	return req, nil
}

// writeGateError maps a RespondGate failure to its HTTP status authoritatively by
// the session's stable GateError kind string, recovered structurally via
// errors.As on gateErrorKinder (no pkg/session import). A non-GateError, an
// append failure, or an unknown kind is a generic 500. Every branch routes
// through writeErrorCause so the cause is logged but never written to the body.
func writeGateError(w http.ResponseWriter, err error) {
	var ge gateErrorKinder
	if !errors.As(err, &ge) {
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgGateFailed, false, err)
		return
	}
	switch ge.GateErrorKind() {
	case gateKindNotFound:
		writeErrorCause(w, http.StatusNotFound, codeGateNotFound, msgGateNotFound, false, err)
	case gateKindActionInvalid:
		writeErrorCause(w, http.StatusBadRequest, codeGateActionInvalid, msgGateActionInvalid, false, err)
	case gateKindMismatch:
		writeErrorCause(w, http.StatusBadRequest, codeGateKindMismatch, msgGateKindMismatch, false, err)
	case gateKindNotReady:
		writeErrorCause(w, http.StatusConflict, codeGateNotReady, msgGateNotReady, false, err)
	case gateKindCapacity:
		// Capacity is transient: the directory may drain, so the client may retry.
		writeErrorCause(w, http.StatusServiceUnavailable, codeGateCapacity, msgGateCapacity, true, err)
	case gateKindAppendFailed:
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgGateFailed, false, err)
	default:
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgGateFailed, false, err)
	}
}
