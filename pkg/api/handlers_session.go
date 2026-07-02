package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ciram-co/looprig/pkg/uuid"
)

// Session-lifecycle request/response bodies. Domain responses are typed structs
// (never ad-hoc maps): the map/any escape hatch lives only inside writeJSON, the
// single serialization boundary.
type (
	// sidResponse is the create body: the session id the runner minted (create) or
	// was handed (resume).
	sidResponse struct {
		SID string `json:"sid"`
	}
	// errorResponse is the uniform 4xx/5xx body: a single generic message with no
	// internal detail. The concrete cause is logged, never returned.
	errorResponse struct {
		Error string `json:"error"`
	}
)

// Client-safe error messages. Deliberately generic so a response never leaks the
// runner's internal state or the concrete cause (which is logged via slog).
const (
	msgInvalidSessionID = "invalid session id"
	msgInvalidResumeID  = "invalid resume session id"
	msgCreateFailed     = "could not create session"
	msgSessionNotFound  = "session not found"
)

// invalidSessionIDError reports that a path- or query-supplied session id could
// not be parsed as a UUID. It carries the raw value and cause for the internal
// audit log ONLY — the client sees a generic message. It is a typed error (per
// CLAUDE.md) so the create handler can errors.As it to distinguish a bad-input
// 400 from an id-generation 500.
type invalidSessionIDError struct {
	Value string
	Cause error
}

func (e invalidSessionIDError) Error() string {
	return "api: invalid session id " + strconv.Quote(e.Value) + ": " + e.Cause.Error()
}

func (e invalidSessionIDError) Unwrap() error { return e.Cause }

// parseSessionID reads the {sid} path value and parses it as a UUID, returning a
// typed invalidSessionIDError on any malformed input (empty, wrong length, non-
// hex). It validates at the boundary before the id reaches the registry.
func parseSessionID(r *http.Request) (uuid.UUID, error) {
	raw := r.PathValue("sid")
	var id uuid.UUID
	if err := id.UnmarshalText([]byte(raw)); err != nil {
		return uuid.UUID{}, invalidSessionIDError{Value: raw, Cause: err}
	}
	return id, nil
}

// writeJSON sets the JSON content type, writes status, and encodes v. This is the
// one place any is permitted (the serialization boundary); an encode failure is
// logged, never surfaced (the status/headers are already committed).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("api: encode response", "err", err)
	}
}

// writeError writes a generic JSON error body with status. msg must be caller-
// safe (no internal detail); the concrete cause is logged separately by the
// handler before calling this.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// handleCreateSession serves POST /sessions. With ?resume=<sid> it resumes that
// exact id (bad id => 400); otherwise it mints a fresh v4 id. It builds the
// agent via the Factory, starts the per-session supervisor, registers the
// session, and returns 201 with the sid. Any build/supervisor failure is a 500
// that leaks nothing; a supervisor failure closes the just-built agent first.
func (s *server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	req, err := buildAgentRequest(r)
	if err != nil {
		var badID invalidSessionIDError
		if errors.As(err, &badID) {
			slog.Warn("api: create rejected invalid resume id", "err", err)
			writeError(w, http.StatusBadRequest, msgInvalidResumeID)
			return
		}
		slog.Error("api: create could not mint session id", "err", err)
		writeError(w, http.StatusInternalServerError, msgCreateFailed)
		return
	}

	agent, err := s.factory(r.Context(), req)
	if err != nil {
		slog.Error("api: create factory failed", "err", err)
		writeError(w, http.StatusInternalServerError, msgCreateFailed)
		return
	}

	sup, err := newSupervisor(agent)
	if err != nil {
		slog.Error("api: create supervisor failed", "err", err)
		if cErr := agent.Close(r.Context()); cErr != nil { // best-effort: don't leak the half-built agent
			slog.Error("api: create close after supervisor failure", "err", cErr)
		}
		writeError(w, http.StatusInternalServerError, msgCreateFailed)
		return
	}

	s.putSession(req.SessionID, &sessionEntry{agent: agent, sup: sup})
	writeJSON(w, http.StatusCreated, sidResponse{SID: req.SessionID.String()})
}

// buildAgentRequest derives the AgentRequest from the request: a non-empty
// ?resume=<sid> resumes that parsed id (a malformed value returns a typed
// invalidSessionIDError the caller maps to 400); otherwise it mints a fresh v4
// id (a randomness failure returns the uuid error the caller maps to 500).
func buildAgentRequest(r *http.Request) (AgentRequest, error) {
	if resume := r.URL.Query().Get("resume"); resume != "" {
		var sid uuid.UUID
		if err := sid.UnmarshalText([]byte(resume)); err != nil {
			return AgentRequest{}, invalidSessionIDError{Value: resume, Cause: err}
		}
		return AgentRequest{SessionID: sid, Resume: true}, nil
	}
	newID, err := uuid.New()
	if err != nil {
		return AgentRequest{}, err
	}
	return AgentRequest{SessionID: newID, Resume: false}, nil
}

// handleDeleteSession serves DELETE /sessions/{sid}. It evicts the session under
// the lock (via deleteSession, which returns the entry) and then, OFF-lock, stops
// the supervisor and closes the agent best-effort, answering 204. An unknown id
// is 404 and a malformed id is 400 — both fail secure without touching an agent.
func (s *server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r)
	if err != nil {
		slog.Warn("api: delete rejected invalid id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidSessionID)
		return
	}

	entry, ok := s.deleteSession(sid)
	if !ok {
		writeError(w, http.StatusNotFound, msgSessionNotFound)
		return
	}

	// OFF-lock teardown: deleteSession already released s.mu, so no agent/
	// supervisor call happens under the registry lock. Best-effort — a wedged
	// teardown is logged, never surfaced to the caller.
	if err := entry.sup.stop(); err != nil {
		slog.Error("api: delete supervisor stop", "err", err)
	}
	if err := entry.agent.Close(r.Context()); err != nil {
		slog.Error("api: delete agent close", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
