package serve

import (
	"errors"
	"net/http"
)

// Generic, client-safe 500 messages for the read-plane routes. Each never embeds a
// concrete cause (which may carry backend detail); the cause is confined to the audit
// log via writeErrorCause.
const (
	msgListFailed    = "could not list sessions"
	msgStatusFailed  = "could not read session status"
	msgJournalFailed = "could not read session journal"
)

// handleListSessions serves GET /v1/sessions?skip&limit: the stateless list read.
//
// It validates the paging window at the boundary (skip default 0, reject negative;
// limit default 100, hard-capped at 1000 — an over-cap or non-numeric value is a 400)
// and passes the Page straight through to the injected Reader, which owns the stable
// sort + slice + NextSkip/Done. It NEVER consults the live registry: a list is a pure
// read over persisted history, so it succeeds on any pod with no live session.
func (s *server[S, O]) handleListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	skip, err := parseSkip(q)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidParam, false, err)
		return
	}
	limit, err := parseLimit(q)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidParam, false, err)
		return
	}

	list, err := s.reader.ListSessions(r.Context(), Page{Skip: skip, Limit: limit})
	if err != nil {
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgListFailed, false, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleStatus serves GET /v1/sessions/{sid}/status: one session's projected status.
//
// It parses {sid} at the boundary (malformed => 400) and reads the Reader's
// projection (no replay). A SessionNotFoundError from the Reader (the session has no
// catalog entry) maps to 404; any other error is a generic 500. Like every read, it
// NEVER consults the live registry — the status is projected from durable history, so
// it resolves for a session that is not live on this pod.
func (s *server[S, O]) handleStatus(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}

	status, err := s.reader.ReadStatus(r.Context(), sid)
	if err != nil {
		var notFound SessionNotFoundError
		if errors.As(err, &notFound) {
			writeErrorCause(w, http.StatusNotFound, codeNotFound, msgNotFound, false, err)
			return
		}
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgStatusFailed, false, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleJournal serves GET /v1/sessions/{sid}/journal?from_journal_seq&limit: a page
// of a session's Enduring events.
//
// It parses {sid} (malformed => 400) and the paging window (from_journal_seq: 0/absent
// = beginning; limit default 100, hard-capped at 1000 — an invalid value is a 400) at
// the boundary, then reads the Reader-backed journal page (which owns the
// replayer-backed read and the NextJournalSeq/Done cursor). GatePrepared never appears
// (the replayer filters it). Like every read it NEVER consults the live registry.
func (s *server[S, O]) handleJournal(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}
	q := r.URL.Query()
	from, err := parseFromJournalSeq(q)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidParam, false, err)
		return
	}
	limit, err := parseLimit(q)
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidParam, false, err)
		return
	}

	page, err := s.reader.ReadJournal(r.Context(), sid, JournalPage{From: from, Limit: limit})
	if err != nil {
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgJournalFailed, false, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
