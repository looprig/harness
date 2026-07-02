package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ciram-co/looprig/pkg/content"
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
)

// Client-safe input error messages. Generic by design so a response never leaks
// the runner's internal state; the concrete cause is logged via slog.
const (
	msgInvalidInput = "invalid input"
	msgSubmitFailed = "could not submit input"
)

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
