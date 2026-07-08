package serve

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
)

// Machine-readable error codes for the lifecycle routes (SPEC §7a). Each is a
// stable identifier a client can switch on; the paired message is generic and
// client-safe (never internal cause text).
const (
	codeInternal     = "internal"
	codeInvalidBody  = "invalid_body"
	codeInvalidParam = "invalid_parameter"
	codeNotFound     = "session_not_found"
)

// Generic, client-safe messages for the lifecycle routes. They never embed a
// concrete cause (which may carry secrets/PII); the cause is confined to the audit
// log via writeErrorCause.
const (
	msgCreateFailed  = "could not create session"
	msgInvalidBody   = "malformed request body"
	msgSubmitFailed  = "could not submit input"
	msgRestoreFailed = "could not restore session"
	msgInvalidSID    = "invalid session id"
	msgNotFound      = "session not found"

	msgInterruptFailed = "could not interrupt session"
)

// createRequest is the optional POST /v1/sessions body: an idle create sends no
// body (or an empty object); a create-with-input sends {"blocks":[...]}. Blocks is
// captured as raw JSON so the tagged-block decode is delegated wholesale to
// content.UnmarshalBlocks (the single block-decode authority) rather than reparsed
// here — this handler validates shape, not block semantics.
type createRequest struct {
	Blocks json.RawMessage `json:"blocks"`
}

// createResponse is the 201 body for POST /v1/sessions. CommandID is present only
// when the create carried input blocks that were submitted (it is the id Submit
// minted, correlating the input on the event stream); an idle create omits it.
type createResponse struct {
	SessionID uuid.UUID  `json:"session_id"`
	CommandID *uuid.UUID `json:"command_id,omitempty"`
}

// restoreResponse is the 200 body for POST /v1/sessions/{sid}/restore: the id of
// the session that was rebuilt and reattached to the live registry.
type restoreResponse struct {
	SessionID uuid.UUID `json:"session_id"`
}

// handleCreate serves POST /v1/sessions: bring up a fresh session and, if the
// request carried input, submit it.
//
// Ordering is deliberate: the optional body is decoded and validated BEFORE the
// runner is asked to mint a session, so a malformed body fails with 400 without
// ever orphaning a freshly-minted-but-unreachable session. Only once the input is
// known-good does it Run, attach the session to the registry (so subsequent live
// routes resolve its id), then Submit. A freshly minted id cannot collide with a
// live entry, so put (not putIfAbsent) is correct here.
//
// The Idempotency-Key header is intentionally ignored at this phase (Task P2-6).
func (s *server[S]) handleCreate(w http.ResponseWriter, r *http.Request) {
	blocks, err := decodeCreateBlocks(r, s.cfg.maxBodyBytes)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidBody, msgInvalidBody, false, err)
		return
	}

	id, sess, err := s.runner.Run(r.Context())
	if err != nil {
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgCreateFailed, false, err)
		return
	}
	s.registry.put(id, sess)

	resp := createResponse{SessionID: id}
	if len(blocks) > 0 {
		// Submit after attach: the session is already reachable, so any event the
		// submission produces can be observed on the stream by the correlated id.
		// A Submit failure leaves the session attached (it exists and is live); the
		// client may retry Submit or interrupt/delete it — we do not detach here.
		cmdID, err := sess.Submit(r.Context(), blocks)
		if err != nil {
			writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgSubmitFailed, false, err)
			return
		}
		resp.CommandID = &cmdID
	}
	writeJSON(w, http.StatusCreated, resp)
}

// decodeCreateBlocks reads the optional create body and returns the decoded input
// blocks (nil for an idle create: absent body, empty body, or a body with no/empty
// "blocks"). It validates at the boundary — a read failure (including a body over
// the cap), malformed JSON envelope, or malformed tagged blocks all return an error
// the caller maps to 400 — so no invalid input ever reaches Run.
func decodeCreateBlocks(r *http.Request, maxBytes int64) ([]content.Block, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(req.Blocks)) == 0 {
		return nil, nil
	}
	return content.UnmarshalBlocks(req.Blocks)
}

// handleRestore serves POST /v1/sessions/{sid}/restore: rebuild a prior session
// from its durable history and reattach it to the live registry so the live/control
// routes resolve its id again.
//
// The {sid} path segment is parsed and validated at the boundary (malformed => 400)
// before the runner is touched. Restore errors are mapped generically to 500 —
// serve cannot import the session package's error types, so it has no way to tell a
// "no journal / not found" rebuild failure from a transient backend failure. The
// one signal it honors is a serve-level SessionNotFoundError, which a Runner may
// choose to surface for a genuine 404; absent that, every Restore failure is a 500.
func (s *server[S]) handleRestore(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}

	sess, err := s.runner.Restore(r.Context(), sid)
	if err != nil {
		var notFound SessionNotFoundError
		if errors.As(err, &notFound) {
			writeErrorCause(w, http.StatusNotFound, codeNotFound, msgNotFound, false, err)
			return
		}
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgRestoreFailed, false, err)
		return
	}
	s.registry.put(sid, sess)
	writeJSON(w, http.StatusOK, restoreResponse{SessionID: sid})
}
