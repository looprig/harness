package serve

import (
	"math"
	"net/url"
	"strconv"

	"github.com/looprig/core/uuid"
)

// Read-plane query-parameter bounds (SPEC §6). limit defaults to 100 and is hard
// capped at 1000; skip and limit must be non-negative. skip has no domain cap of
// its own, so it is bounded only by the int range (math.MaxInt).
const (
	defaultLimit = 100
	maxLimit     = 1000
	minCount     = 0
)

// InvalidParamError reports that an HTTP path- or query-supplied parameter failed
// validation at the request boundary. It carries the parameter name and a
// client-safe Reason; handlers map it to HTTP 400 with a generic message. Typed
// (per CLAUDE.md) so callers errors.As it to distinguish bad input from other
// failures. Reason is a fixed, non-sensitive string (never echoes the raw value).
type InvalidParamError struct {
	Param  string
	Reason string
}

func (e InvalidParamError) Error() string {
	return "serve: invalid parameter " + e.Param + ": " + e.Reason
}

// parseSessionID parses a path segment as a canonical 8-4-4-4-12 UUID session id,
// returning an InvalidParamError on any malformed input (empty, wrong length,
// non-hex). It validates at the boundary via UUID.UnmarshalText before the id
// reaches any read-plane lookup, mirroring pkg/api's parse pattern.
func parseSessionID(s string) (uuid.UUID, error) {
	return parseUUID("session_id", s)
}

// parseGateID parses a path segment as a canonical UUID gate id (identical
// encoding to a session id), returning an InvalidParamError on malformed input.
func parseGateID(s string) (uuid.UUID, error) {
	return parseUUID("gate_id", s)
}

// parseUUID is the shared UUID path-segment parser behind parseSessionID and
// parseGateID. It never echoes the raw value into the error (only the parameter
// name and a fixed reason), so a malformed id cannot smuggle content into a log
// or response.
func parseUUID(param, s string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := id.UnmarshalText([]byte(s)); err != nil {
		return uuid.UUID{}, InvalidParamError{Param: param, Reason: "malformed uuid"}
	}
	return id, nil
}

// parseLimit reads the read-plane "limit" query parameter: absent/empty yields
// the default (100); a value above the hard cap (1000), below zero, or
// non-numeric yields an InvalidParamError.
func parseLimit(values url.Values) (int, error) {
	return parseIntQuery(values, "limit", defaultLimit, minCount, maxLimit)
}

// parseSkip reads the read-plane "skip" query parameter: absent/empty yields 0;
// a negative or non-numeric value yields an InvalidParamError. skip has no domain
// upper cap, so it is bounded only by the int range.
func parseSkip(values url.Values) (int, error) {
	return parseIntQuery(values, "skip", 0, minCount, math.MaxInt)
}

// parseIntQuery reads key from values as a bounded integer in [min, max]. An
// absent or empty value returns def. A non-numeric value, a value below min, or a
// value above max yields an InvalidParamError (mapped to HTTP 400). The reason is
// fixed and never echoes the raw value.
func parseIntQuery(values url.Values, key string, def, min, max int) (int, error) {
	raw := values.Get(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, InvalidParamError{Param: key, Reason: "not an integer"}
	}
	if n < min {
		return 0, InvalidParamError{Param: key, Reason: "below minimum"}
	}
	if n > max {
		return 0, InvalidParamError{Param: key, Reason: "above maximum"}
	}
	return n, nil
}

// parseFromJournalSeq reads the "from_journal_seq" query parameter as a uint64
// cursor where 0 (or absent/empty) means "from the beginning". strconv.ParseUint
// rejects a leading sign, so negative and non-numeric input both yield an
// InvalidParamError; there is no upper cap beyond the uint64 range.
func parseFromJournalSeq(values url.Values) (uint64, error) {
	const key = "from_journal_seq"
	raw := values.Get(key)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, InvalidParamError{Param: key, Reason: "not a non-negative integer"}
	}
	return n, nil
}
