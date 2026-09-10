package rig

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/workspacestore"
)

// session_options.go defines the per-call NewSession options. The type is variadic so
// further per-session knobs compose without an API break.

// SessionOption configures a single Rig.NewSession call.
type SessionOption func(*sessionOptions) error

// sessionOptions accumulates the resolved per-call NewSession configuration.
type sessionOptions struct {
	seed         workspacestore.Ref
	seedSet      bool
	sessionID    uuid.UUID
	sessionIDSet bool
}

// WithSeedSnapshot materializes ref into the new session's workspace before constructing
// loops and journals it as the first workspace checkpoint (design §"Seeding"). It is valid
// only for per-session roots and an EMPTY exclusive root, never for a shared root, and the
// ref must resolve in the configured workspace store — all enforced at NewSession, which
// fails closed on a bad seed.
func WithSeedSnapshot(ref workspacestore.Ref) SessionOption {
	return func(o *sessionOptions) error {
		if o.seedSet {
			return &SessionOptionError{Kind: SessionOptionDuplicateSeed}
		}
		if ref == "" {
			return &SessionOptionError{Kind: SessionOptionEmptySeed}
		}
		o.seedSet = true
		o.seed = ref
		return nil
	}
}

// WithSessionID makes the new session adopt an externally-minted id instead of minting
// its own. NewSession then runs its whole per-session durable build under that id: the
// single-writer lease is acquired for it, the journal is bound to it and stamps its
// opening LeaseFence, and RestoreSession answers to it afterwards.
//
// WHY A CALLER WOULD WANT THIS. An orchestrator that records the runtime session id in a
// durable record written BEFORE launch — a catalog binding that is immutable once
// created — cannot use an id the runtime mints during launch, because the record naming
// it is already written. The id has to be decidable in advance or the binding cannot
// name the session at all.
//
// IT DOES NOT CREATE A NEW ORDERING PROBLEM. rig already mints the id first and builds
// the lease, journal and appenders from it before the session exists — the journal
// chicken-and-egg the internal sessionruntime.WithSessionID option resolves. This option
// only replaces that minting step with the caller's value, strictly earlier in the same
// order; nothing about the id is observable to the caller before NewSession returns that
// was not already.
//
// THE CALLER OWNS UNIQUENESS, AND THE LEASE IS NOT A SUBSTITUTE FOR IT. Freshness is NOT
// verified. Passing the id of a session that already exists does not fail and does not
// create a second session: it re-opens THAT session's durable stream under a fresh grant
// and appends to it, so a stream can end up with two SessionStarted records and a
// restore afterwards replays both. The single-writer lease refuses only a LIVE holder,
// so it stops a concurrent second resident and says nothing at all about a session that
// has ended or been released. Mint the id with uuid.New (or another source with the same
// collision properties) and use it once.
//
// A zero id is REFUSED rather than quietly replaced by a minted one, because a caller
// that reached here with the zero UUID believed it was naming a session and was not, and
// silently substituting a different id would put a name in its durable record that
// resolves to nothing. Supplying the option twice is refused for the same reason,
// including with the same id both times: two calls mean two beliefs about which id this
// is, and picking one of them is a guess.
//
// It is REQUIRED for NewSession only. RestoreSession takes the id positionally and never
// consults this.
func WithSessionID(id uuid.UUID) SessionOption {
	return func(o *sessionOptions) error {
		if o.sessionIDSet {
			return &SessionOptionError{Kind: SessionOptionDuplicateSessionID}
		}
		if id.IsZero() {
			return &SessionOptionError{Kind: SessionOptionZeroSessionID}
		}
		o.sessionIDSet = true
		o.sessionID = id
		return nil
	}
}

// resolveSessionOptions applies the NewSession options, returning the accumulated config.
func resolveSessionOptions(opts []SessionOption) (sessionOptions, error) {
	var resolved sessionOptions
	for _, opt := range opts {
		if opt == nil {
			return sessionOptions{}, &SessionOptionError{Kind: SessionOptionNil}
		}
		if err := opt(&resolved); err != nil {
			return sessionOptions{}, err
		}
	}
	return resolved, nil
}
