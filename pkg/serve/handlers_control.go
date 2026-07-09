package serve

import (
	"net/http"

	"github.com/looprig/core/uuid"
)

// inputResponse is the 200 body for POST /v1/sessions/{sid}/input: the id Submit
// minted for the queued input, correlating it against the events the submission
// produces on the session's event stream.
type inputResponse struct {
	CommandID uuid.UUID `json:"command_id"`
}

// interruptResponse is the 200 body for POST /v1/sessions/{sid}/interrupt:
// whether any in-flight turn was actually cancelled.
type interruptResponse struct {
	Interrupted bool `json:"interrupted"`
}

// handleInput serves POST /v1/sessions/{sid}/input: fire-and-forget submit of
// human-authored input to a session that is LIVE in this process.
//
// The route resolves {sid} against the live registry, so it requires an
// in-process session — a session that exists only in durable history must be
// Restored first (a miss here is a generic 404, never a claim about whether the
// id ever existed). Unlike an idle CREATE (where blocks are optional), an input
// with no blocks is meaningless and is rejected 400: absent body, empty body,
// empty object, or an empty "blocks" array all decode to no blocks and fail.
// A malformed body (bad JSON envelope, over the cap, or an unknown block type)
// is likewise 400; a Submit failure is a generic 500 that leaks no cause.
func (s *server[S]) handleInput(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}

	sess, ok := s.registry.get(sid)
	if !ok {
		writeErrorCause(w, http.StatusNotFound, codeNotFound, msgNotFound, false, SessionNotFoundError{SessionID: sid})
		return
	}

	// Reuse the create body decoder (same {"blocks":[...]} shape, same body cap,
	// same delegation to content.UnmarshalBlocks). It returns nil blocks for an
	// absent/empty body; the emptiness check below turns that into a 400 — the
	// one place input diverges from create, where blocks are REQUIRED.
	blocks, err := decodeCreateBlocks(r, s.cfg.maxBodyBytes)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidBody, msgInvalidBody, false, err)
		return
	}
	if len(blocks) == 0 {
		writeError(w, http.StatusBadRequest, codeInvalidBody, msgInvalidBody, false)
		return
	}

	cmdID, err := sess.Submit(r.Context(), blocks)
	if err != nil {
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgSubmitFailed, false, err)
		return
	}
	writeJSON(w, http.StatusOK, inputResponse{CommandID: cmdID})
}

// handleInterrupt serves POST /v1/sessions/{sid}/interrupt: cancel every in-flight
// turn in a session that is LIVE in this process, reporting whether any running
// turn was actually cancelled.
//
// {sid} is parsed at the boundary (malformed => 400) and resolved against the live
// registry (a miss is a generic 404) before Interrupt is called. An Interrupt
// failure is a generic 500 that leaks no cause.
func (s *server[S]) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}

	sess, ok := s.registry.get(sid)
	if !ok {
		writeErrorCause(w, http.StatusNotFound, codeNotFound, msgNotFound, false, SessionNotFoundError{SessionID: sid})
		return
	}

	interrupted, err := sess.Interrupt(r.Context())
	if err != nil {
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgInterruptFailed, false, err)
		return
	}
	writeJSON(w, http.StatusOK, interruptResponse{Interrupted: interrupted})
}
