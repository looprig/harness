package gate

import (
	"fmt"
	"net/url"
)

// GateValidationErrorKind classifies a rejected gate envelope.
type GateValidationErrorKind string

const (
	// GateRestorableNotAllowed reports a gate marked Restorable whose kind can
	// never be restored.
	GateRestorableNotAllowed GateValidationErrorKind = "restorable_not_allowed"
)

// GateValidationError reports a gate envelope that violates a kind's invariants.
type GateValidationError struct {
	Kind     GateValidationErrorKind
	GateKind Kind
}

func (e *GateValidationError) Error() string {
	return fmt.Sprintf("gate: invalid %q gate (%s)", string(e.GateKind), string(e.Kind))
}

// ValidateGate checks the envelope invariants that a gate's Kind implies. It is
// called on the open path so an invariant cannot be violated by a caller who
// simply forgot it.
//
// It is deliberately narrow. Every kind that predates it validates as nil — this
// is an additive hook, not a retroactive schema check, and it must stay that way
// unless an existing kind's contract is separately tightened.
//
// Today it enforces exactly one rule: an open-url gate may not be Restorable.
// OpenURLPayload's action target is never journaled, so a "restored" open-url
// gate would present a human with an origin and no URL to open — it would fail
// open into a broken prompt. Restore must instead close it as unavailable and
// let a live integration mint a fresh request.
func ValidateGate(g Gate) error {
	switch g.Kind {
	case KindOpenURL:
		if g.Restorable {
			return &GateValidationError{Kind: GateRestorableNotAllowed, GateKind: g.Kind}
		}
		return nil
	default:
		return nil
	}
}

// originErrorKind classifies a rejected DisplayOrigin.
type originErrorKind string

const (
	originEmpty     originErrorKind = "origin_empty"
	originMalformed originErrorKind = "origin_malformed"
	originScheme    originErrorKind = "origin_scheme_unsupported"
	originNotBare   originErrorKind = "origin_not_bare"
)

// DisplayOriginError reports a DisplayOrigin that is not a bare, journal-safe
// origin.
type DisplayOriginError struct {
	kind originErrorKind
}

func (e *DisplayOriginError) Error() string {
	return fmt.Sprintf("gate: invalid display origin (%s)", string(e.kind))
}

// validateDisplayOrigin enforces that an OpenURLPayload's DisplayOrigin is a
// BARE origin: an http/https scheme, a host, and nothing else.
//
// This check is what makes the URL exclusion real rather than nominal. Dropping
// the URL field from the durable record achieves nothing if a caller can pass
// "https://idp.example/authorize?state=SECRET&code_challenge=..." as the
// "origin" and have it journaled verbatim. Rejecting any path, query, fragment,
// or userinfo means the durable record can only ever carry the coarse
// destination a human needs to make a trust decision.
func validateDisplayOrigin(origin string) error {
	if origin == "" {
		return &DisplayOriginError{kind: originEmpty}
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return &DisplayOriginError{kind: originMalformed}
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return &DisplayOriginError{kind: originScheme}
	}
	if parsed.Host == "" {
		return &DisplayOriginError{kind: originMalformed}
	}
	if parsed.User != nil {
		return &DisplayOriginError{kind: originNotBare}
	}
	// A bare "/" carries no information and is a conventional spelling of an
	// origin, so it is accepted; any deeper path is not.
	if parsed.Path != "" && parsed.Path != "/" {
		return &DisplayOriginError{kind: originNotBare}
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return &DisplayOriginError{kind: originNotBare}
	}
	return nil
}
