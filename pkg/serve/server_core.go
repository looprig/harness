package serve

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/looprig/core/uuid"
)

// server is the generic HTTP-handler holder: it owns the shared dependencies every
// route needs (the session factory, the live-session registry, and the assembled
// config) so the individual handlers can be methods on it. It is parameterized over
// the concrete live-session type S (constrained to LiveSession) so the real type
// threads through NewSession/RestoreSession without serve importing it — the composition
// root instantiates server with its concrete session and option types while serve keeps
// both as generic parameters. The stateless read plane (list/status/journal/capabilities)
// is embedded via *readServer rather than duplicated here, so it can also be served on
// its own by a process with no rig.
//
// This holder carries no request state — one server instance serves every request
// (the mux is built by Handler in mux.go; per-request state lives on the stack of each
// handler invocation). Its fields are the shared per-pod dependencies: the embedded
// read plane, the session rig, the live-session registry, the config, and the
// idempotency store.
type server[S LiveSession, O any] struct {
	*readServer
	rig      Rig[S, O]
	registry *registry
	cfg      *config
	// idem is the per-pod Idempotency-Key store for POST /v1/sessions. It is built
	// once per server (shared, mutex-guarded) and is never nil; handleCreate only
	// consults it when a request carries the header. Tests set its ttl/now fields
	// directly for deterministic expiry (like config.heartbeat).
	idem *idempotencyStore
}

// newServer builds a server over the supplied rig, read-plane reader, and config,
// minting a fresh empty registry. rig, reader, and cfg are wired at the composition
// root; a nil cfg is a programming error (the composition root always builds one via
// newConfig) and is not defended against here. reader is the stateless read plane
// (list/status/journal); it is independent of the live registry — a read never
// consults a live session — so a nil reader is tolerated by the control/lifecycle
// routes that never touch it (the read handlers require it).
func newServer[S LiveSession, O any](rig Rig[S, O], reader Reader, cfg *config) *server[S, O] {
	return &server[S, O]{
		readServer: &readServer{reader: reader, features: fullFeatures},
		rig:        rig,
		registry:   newRegistry(),
		cfg:        cfg,
		idem:       newIdempotencyStore(defaultIdempotencyTTL),
	}
}

// register makes sess resolvable as id and, if sess can report its own death, starts
// the watcher that evicts it when it does. It is the only way a session should enter
// the registry on a lifecycle route: a bare put leaves the entry to outlive its
// session, so every route keeps resolving a corpse.
//
// The watcher is the EAGER half of eviction and is what fixes the real leak. A client
// holding an open SSE stream may never issue another request, so no amount of checking
// on next use can free it — the stream, its hub subscription and the whole dead session
// stay pinned until the process exits. Something has to be watching.
//
// The goroutine costs one blocked receive per live session and ends at the first of the
// session's death or process exit; it holds only the id and the session it was handed,
// and its delete is identity-guarded, so a session that is replaced under the same sid
// before it dies evicts nothing.
func (s *server[S, O]) register(id uuid.UUID, sess LiveSession) {
	s.registry.put(id, sess)
	reporter, reportsDeath := sess.(SessionDone)
	if !reportsDeath {
		// The session cannot report death, so there is nothing to watch. It stays
		// registered until a lifecycle route removes it — exactly today's behaviour.
		return
	}
	done := reporter.Done()
	go func() {
		<-done
		s.registry.deleteMatching(id, sess)
	}()
}

// liveSession resolves sid to the session a request targets, reporting a miss if no
// session is registered OR if the registered one has already begun shutting down. A
// caller must treat both misses identically: the 404 for a shut-down sid is
// byte-identical to the one for a sid that never existed, so the route is not a
// liveness oracle for an unauthorized prober.
//
// The probe is the LAZY half of eviction and exists as the deterministic backstop for
// the window between the session closing its channel and register's watcher being
// scheduled. Without it a request landing in that window would be handed a corpse and
// answered as if the session were live; with it, a miss is exact rather than racy.
// The receive is non-blocking by construction — an open channel falls straight through
// to the default arm.
//
// A session that does not satisfy SessionDone cannot be probed and is therefore always
// reported live. That is fail-open, and deliberately so: the alternative reading of
// "cannot ask" as "dead" would make every session a consumer registers unreachable.
func (s *server[S, O]) liveSession(sid uuid.UUID) (LiveSession, bool) {
	sess, registered := s.registry.get(sid)
	if !registered {
		return nil, false
	}
	reporter, reportsDeath := sess.(SessionDone)
	if !reportsDeath {
		return sess, true
	}
	select {
	case <-reporter.Done():
		s.registry.deleteMatching(sid, sess)
		return nil, false
	default:
		return sess, true
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
