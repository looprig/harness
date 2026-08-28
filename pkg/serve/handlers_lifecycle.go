package serve

import (
	"bytes"
	"crypto/sha256"
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
	// codeIdempotencyConflict marks a reuse of an Idempotency-Key with a different
	// request body (SPEC §6): the client sent the same key it used for an earlier,
	// different create. It maps to HTTP 409.
	codeIdempotencyConflict = "idempotency_conflict"
	// headerIdempotencyKey is the request header that opts a create into idempotent
	// replay. An absent or empty value means a normal (non-idempotent) create.
	headerIdempotencyKey = "Idempotency-Key"
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
	msgInvalidParam  = "invalid query parameter"
	msgNotFound      = "session not found"

	msgInterruptFailed = "could not interrupt session"

	// msgIdempotencyConflict is the generic 409 message for a key reused with a
	// different body. It names no request detail (client-safe).
	msgIdempotencyConflict = "idempotency key reused with a different request body"
	// msgIdempotencyKeyTooLong is the generic 400 message for an oversized
	// Idempotency-Key (over maxIdempotencyKeyLen bytes).
	msgIdempotencyKeyTooLong = "idempotency key too long"
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

// restoreResponse is the 200 body for POST /v1/sessions/{sid}/restore.
//
// SessionID is the session now live in the registry. Restored distinguishes the
// route's two success paths, which are otherwise indistinguishable from outside the
// process: true when the rig rebuilt the session from durable history, false when an
// already-live session was reused (attach). It is never omitted — a consumer that
// cannot tell an attach from a rebuild cannot test that the attach path is still
// short-circuiting the rig.
type restoreResponse struct {
	SessionID uuid.UUID `json:"session_id"`
	Restored  bool      `json:"restored"`
}

// handleCreate serves POST /v1/sessions: bring up a fresh session and, if the
// request carried input, submit it.
//
// Ordering is deliberate: the optional body is decoded and validated BEFORE the
// rig is asked to mint a session, so a malformed body fails with 400 without
// ever orphaning a freshly-minted-but-unreachable session. Only once the input is
// known-good does it call NewSession, attach the session to the registry (so subsequent live
// routes resolve its id), then Submit. A freshly minted id cannot collide with a
// live entry, so put (not putIfAbsent) is correct here.
//
// If the request carries an Idempotency-Key header (SPEC §6, Decision #18), the
// create is made per-pod idempotent: a repeat of the same key with a byte-identical
// body replays the original 201 response WITHOUT re-running (the rig is called
// exactly once across the retries); a repeat with a DIFFERENT body is a 409. The
// store is per-pod, not distributed, and TTL-bounded (default 24h). An absent/empty
// key is a normal create that never touches the store.
//
// Ordering is deliberate and layered on the block-decode ordering above:
//  1. read the raw body once (needed both to hash and to decode blocks);
//  2. if a key is present and OVER maxIdempotencyKeyLen → 400 before touching the
//     store (bounded key — cannot pin memory);
//  3. hash the raw body and lookup: a HIT replays the cached 201 (skipping decode +
//     NewSession entirely — the original already validated and created it); a CONFLICT is a 409;
//  4. a MISS falls through to the normal create: decode blocks (400 on malformed),
//     NewSession, attach, Submit, and — only on full success — record the outcome so a
//     later repeat of the key replays it.
//
// CONCURRENCY: the store lock is NEVER held across NewSession/Submit. The guaranteed
// property is the SEQUENTIAL one — a repeat AFTER the first create completes replays
// the cached ids and does not re-run. Two TRULY-CONCURRENT requests with the same key
// can each observe a miss before either stores, and so can both call NewSession (minting
// two sessions); the last store wins the cache. This per-pod duplicate-create window is
// accepted here rather than closed with a request coalescer; a client that needs
// strict single-flight serializes its own retries.
func (s *server[S, O]) handleCreate(w http.ResponseWriter, r *http.Request) {
	body, err := readCreateBody(r, s.cfg.maxBodyBytes)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidBody, msgInvalidBody, false, err)
		return
	}

	key := r.Header.Get(headerIdempotencyKey)
	idempotent := key != ""
	var bodyHash [32]byte
	if idempotent {
		if len(key) > maxIdempotencyKeyLen {
			writeError(w, http.StatusBadRequest, codeInvalidParam, msgIdempotencyKeyTooLong, false)
			return
		}
		bodyHash = sha256.Sum256(body)
		cached, status := s.idem.lookup(key, bodyHash, s.idem.now())
		switch status {
		case idemHit:
			// Replay the original outcome verbatim (same 201, same ids); the first
			// create already completed, so we neither decode nor call NewSession again.
			writeJSON(w, http.StatusCreated, cached)
			return
		case idemConflict:
			writeError(w, http.StatusConflict, codeIdempotencyConflict, msgIdempotencyConflict, false)
			return
		case idemMiss:
			// Fall through to the normal create below.
		}
	}

	blocks, err := decodeBlocksFromBody(body)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidBody, msgInvalidBody, false, err)
		return
	}

	sess, err := s.rig.NewSession(r.Context())
	if err != nil {
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgCreateFailed, false, err)
		return
	}
	id := sess.SessionID()
	s.register(id, sess)

	resp := createResponse{SessionID: id}
	if len(blocks) > 0 {
		// Submit after attach: the session is already reachable, so any event the
		// submission produces can be observed on the stream by the correlated id.
		// A Submit failure leaves the session attached (it exists and is live); the
		// client may retry Submit, or interrupt the session — we do not detach here.
		cmdID, err := sess.Submit(r.Context(), blocks)
		if err != nil {
			writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgSubmitFailed, false, err)
			return
		}
		resp.CommandID = &cmdID
	}

	// Record the completed outcome only on full success, so a repeat of the key
	// replays exactly these ids. A failed create stored nothing — the client's retry
	// is a fresh attempt, not a replay of a failure.
	if idempotent {
		s.idem.store(key, bodyHash, resp, s.idem.now())
	}
	writeJSON(w, http.StatusCreated, resp)
}

// decodeCreateBlocks reads the optional create body and returns the decoded input
// blocks (nil for an idle create: absent body, empty body, or a body with no/empty
// "blocks"). It validates at the boundary — a read failure (including a body over
// the cap), malformed JSON envelope, or malformed tagged blocks all return an error
// the caller maps to 400 — so no invalid input ever reaches session creation or submit. It is the one-shot
// read-and-decode used by handleInput; handleCreate splits the two steps
// (readCreateBody then decodeBlocksFromBody) so it can hash the raw bytes for
// idempotency before decoding.
func decodeCreateBlocks(r *http.Request, maxBytes int64) ([]content.Block, error) {
	body, err := readCreateBody(r, maxBytes)
	if err != nil {
		return nil, err
	}
	return decodeBlocksFromBody(body)
}

// readCreateBody reads the raw request body up to the cap (nil for an absent body).
// It is the single point that consumes r.Body, so a caller that needs both the raw
// bytes (to hash) and the decoded blocks reads once and reuses. The LimitReader is a
// secondary bound behind the body-cap middleware's MaxBytesReader; a read error
// (including the middleware tripping the cap) surfaces to the caller as a 400.
func readCreateBody(r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
}

// decodeBlocksFromBody decodes the input blocks from already-read raw body bytes. It
// returns nil blocks for an empty body or a body with no/empty "blocks", and an error
// for a malformed JSON envelope or malformed tagged blocks — the same boundary
// validation decodeCreateBlocks applies, factored out so handleCreate can hash the
// raw bytes before this decode runs.
func decodeBlocksFromBody(body []byte) ([]content.Block, error) {
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

// handleRestore serves POST /v1/sessions/{sid}/restore: ATTACH-OR-RESTORE. A session
// still live in this process is attached and returned as-is; otherwise it is rebuilt
// from its durable history and attached to the live registry so the live/control
// routes resolve its id again. Both are 200, distinguished on the wire by
// restoreResponse.Restored.
//
// The live check comes FIRST and short-circuits before the rig is consulted, because
// rebuilding an already-live session is never correct: the rig holds an exclusive
// lease on the workspace root, so a second rebuild either fails (surfacing as a 500
// for a session that is right there and healthy) or, on a permissive backend, mints a
// SECOND runtime over the same journal — and the registration then replaces the entry,
// orphaning every existing subscriber, whose Subscription still points at the
// now-unreachable first runtime. Idempotence is what lets a client open a session it
// just created, open a second tab, or double-click, all of which resolve {sid} here.
//
// The check is liveSession, not a bare registry lookup, and the difference is the whole
// point: a registered session that has already begun shutting down is a corpse. The
// probe evicts it and this route falls through to a genuine rebuild, where a bare
// lookup would attach to the corpse and keep answering {restored:false} for a session
// no subsequent request could drive.
//
// The {sid} path segment is parsed and validated at the boundary (malformed => 400)
// before anything else. RestoreSession errors are mapped generically to 500 —
// serve cannot import the session package's error types, so it has no way to tell a
// "no journal / not found" rebuild failure from a transient backend failure. The
// one signal it honors is a serve-level SessionNotFoundError, which a Rig may
// choose to surface for a genuine 404; absent that, every RestoreSession failure is a 500.
func (s *server[S, O]) handleRestore(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}

	// Already live: attach. The rig is not touched and the existing runtime — with
	// every subscription hanging off it — is preserved untouched.
	if _, live := s.liveSession(sid); live {
		writeJSON(w, http.StatusOK, restoreResponse{SessionID: sid, Restored: false})
		return
	}

	sess, err := s.rig.RestoreSession(r.Context(), sid)
	if err != nil {
		var notFound SessionNotFoundError
		if errors.As(err, &notFound) {
			writeErrorCause(w, http.StatusNotFound, codeNotFound, msgNotFound, false, err)
			return
		}
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgRestoreFailed, false, err)
		return
	}
	// registerIfAbsent, never register: the live check above and this store are not one
	// atomic step, so a second restore of the same cold sid may have rebuilt and attached
	// while this one was rebuilding. That incumbent has already been answered as this
	// sid's session and may already have subscribers, so a lost race yields to it — the
	// same answer, restored=false, that the attach path above gives, because the client is
	// in the same position either way: the sid is live, and the runtime serving it is not
	// the one this call built. Claiming restored=true would name a runtime no route
	// resolves.
	//
	// The rebuilt-but-unstored session cannot be torn down here — LiveSession is
	// deliberately observation-only, the HTTP plane does not own session lifecycle — so it
	// leaks. Nobody was ever subscribed to it, which is what makes that strictly better
	// than clobbering a session someone is actively watching; see registerIfAbsent for why
	// a rig fenced by the session's single-writer lease never reaches this at all.
	if !s.registerIfAbsent(sid, sess) {
		writeJSON(w, http.StatusOK, restoreResponse{SessionID: sid, Restored: false})
		return
	}
	writeJSON(w, http.StatusOK, restoreResponse{SessionID: sid, Restored: true})
}
