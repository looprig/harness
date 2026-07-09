package serve

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// server is the generic HTTP-handler holder: it owns the shared dependencies every
// route needs (the session factory, the live-session registry, and the assembled
// config) so the individual handlers can be methods on it. It is parameterized over
// the concrete live-session type S (constrained to LiveSession) so the real type
// threads through Run/Restore without serve importing it — the composition root
// instantiates server[*session.Session] and serve holds only server[S].
//
// This holder is intentionally minimal: later tasks extend it (a read-plane Reader
// field, the wired mux, and the exported Handler constructor). It carries no request
// state — one server instance serves every request; per-request state lives on the
// stack of each handler invocation.
type server[S LiveSession] struct {
	runner   Runner[S]
	reader   Reader
	registry *registry
	cfg      *config
}

// newServer builds a server over the supplied runner, read-plane reader, and config,
// minting a fresh empty registry. runner, reader, and cfg are wired at the composition
// root; a nil cfg is a programming error (the composition root always builds one via
// newConfig) and is not defended against here. reader is the stateless read plane
// (list/status/journal); it is independent of the live registry — a read never
// consults a live session — so a nil reader is tolerated by the control/lifecycle
// routes that never touch it (the read handlers require it).
func newServer[S LiveSession](runner Runner[S], reader Reader, cfg *config) *server[S] {
	return &server[S]{
		runner:   runner,
		reader:   reader,
		registry: newRegistry(),
		cfg:      cfg,
	}
}

// writeJSON sets the JSON content type, writes status, and encodes v as the success
// response body. This is the single serialization boundary where a serialization
// interface value (any) is permitted; the value handed in is always a concrete typed
// response struct. An encode failure is logged, never surfaced — the status and
// headers are already committed by the time Encode runs.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("serve: encode response", "err", err)
	}
}
