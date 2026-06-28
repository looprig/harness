package aci

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/ciram-co/looprig/pkg/llm/tee"
)

// This file implements attestation step 5 of the Dstack ACI ("aci/1") report
// verification: event-log replay + app-id extraction + provenance/app-id policy.
//
// It parses evidence.event_log (a double-encoded JSON-in-string), replays the
// IMR3 measurement register from the imr==3 events, and compares the replayed
// value to the RTMR3 the (separately verified) TDX quote attests via
// tee.TDXQuoteRTMR3. A match proves the event log describes the same boot the
// quote signed; from that trusted log it extracts the workload app-id. Both are
// pure integrity checks: every failure maps to the fail-closed reason
// quote_invalid (a log that does not match the attested quote is tampered
// evidence).
//
// The two policy checks (checkAppIDPolicy, checkProvenancePolicy) are separate:
// they decide whether a genuine workload is allow-listed, so their failures map
// to policy_rejected. They take the accepted sets as parameters — Task 2.8's
// policy struct is wired in at the call site, not here. Both are "when
// configured": an empty (or nil) accepted set skips the check.
//
// The RTMR3 algorithm reproduces the authoritative Rust replay
// (src/aci/verifier/dstack.rs, replay_dstack_rtmr): mr starts as 48 zero bytes;
// for each imr==3 event in order, mr = SHA384(mr ‖ pad48(hex_decode(digest)))
// where pad48 right-pads digests shorter than 48 bytes with zeros (and does NOT
// truncate longer ones). SHA-384 is stdlib crypto/sha512.Sum384.

// rtmrSize is the byte length of an RTMR / IMR measurement (a SHA-384 digest).
const rtmrSize = 48

// imr3 selects the third runtime measurement register: the one the Dstack
// event log extends and the TDX quote exposes as RTMR3.
const imr3 = 3

// Event names the IMR3 replay recognizes. appIDEvent marks the workload app-id
// record; systemReadyEvent terminates the app-id scan (take_while stop) so an
// "app-id" event after system-ready is never accepted.
const (
	appIDEvent       = "app-id"
	systemReadyEvent = "system-ready"
)

// provenanceKey is the comparable allow-list key for source provenance: the
// {repo_url, repo_commit} pair. Task 2.8's policy uses this exact key form for
// its AcceptedSourceProvenance set so checkProvenancePolicy can do a single map
// lookup. It is a struct (not a joined string) so neither field can be confused
// with a delimiter inside the other.
type provenanceKey struct {
	RepoURL    string
	RepoCommit string
}

// replayRTMR3 parses the double-encoded event_log JSON string into events and
// replays the IMR3 register from the imr==3 events in order: starting from 48
// zero bytes, mr = SHA384(mr ‖ pad48(digest)) for each. It returns the 48-byte
// replayed measurement. It does NOT compare against any quote — that is the
// caller's job. Errors: a *eventLogParseError on malformed JSON, a
// *digestDecodeError on a non-hex digest.
func replayRTMR3(eventLog string) ([]byte, error) {
	var events []EventLogEntry
	if err := json.Unmarshal([]byte(eventLog), &events); err != nil {
		return nil, &eventLogParseError{cause: err}
	}
	mr := make([]byte, rtmrSize)
	for i := range events {
		if events[i].IMR != imr3 {
			continue
		}
		digest, err := hex.DecodeString(events[i].Digest)
		if err != nil {
			return nil, &digestDecodeError{index: i, cause: err}
		}
		mr = extendIMR(mr, digest)
	}
	return mr, nil
}

// extendIMR folds one digest into the running measurement: it right-pads (or
// uses as-is if already >= 48 bytes) the digest to at least 48 bytes, then
// returns SHA384(mr ‖ padded). pad48 never truncates a longer digest, matching
// the Rust resize(48, 0). The output is always 48 bytes.
func extendIMR(mr, digest []byte) []byte {
	padded := digest
	if len(padded) < rtmrSize {
		grown := make([]byte, rtmrSize)
		copy(grown, padded)
		padded = grown
	}
	sum := sha512.Sum384(append(append([]byte{}, mr...), padded...))
	return sum[:]
}

// extractAppID scans the parsed events for the workload app-id: the event_payload
// bytes of the first imr==3 "app-id" event, scanning in order but STOPPING at
// the first imr==3 "system-ready" event (take_while). It returns a
// *missingAppIDError if no app-id event precedes system-ready, or a
// *appIDDecodeError if its payload is not hex.
func extractAppID(events []EventLogEntry) ([]byte, error) {
	for i := range events {
		if events[i].IMR != imr3 {
			continue
		}
		if events[i].Event == systemReadyEvent {
			break
		}
		if events[i].Event == appIDEvent {
			appID, err := hex.DecodeString(events[i].EventPayload)
			if err != nil {
				return nil, &appIDDecodeError{cause: err}
			}
			return appID, nil
		}
	}
	return nil, &missingAppIDError{}
}

// verifyEventLogAndAppID is attestation step 5's integrity half. It decodes the
// quote hex, reads its attested RTMR3 (tee.TDXQuoteRTMR3 — an offline parse, no
// network), replays IMR3 from evidence.event_log, requires the replay to equal
// the attested RTMR3, then extracts and returns the workload app-id from the now
// trusted log. Every failure — bad quote hex, RTMR3 accessor error, malformed
// event_log, replay mismatch, missing/undecodable app-id — fails closed as
// quote_invalid (the evidence does not cohere, so it is treated as tampered).
func verifyEventLogAndAppID(rep *Report) ([]byte, error) {
	rawQuote, err := hex.DecodeString(rep.Attestation.Evidence.Quote)
	if err != nil {
		return nil, attestErr(reasonQuoteInvalid, &quoteDecodeError{cause: err})
	}
	quoteRTMR3, err := tee.TDXQuoteRTMR3(rawQuote)
	if err != nil {
		return nil, attestErr(reasonQuoteInvalid, err)
	}

	replayed, err := replayRTMR3(rep.Attestation.Evidence.EventLog)
	if err != nil {
		return nil, attestErr(reasonQuoteInvalid, err)
	}
	if !bytes.Equal(replayed, quoteRTMR3) {
		return nil, attestErr(reasonQuoteInvalid, &rtmr3MismatchError{
			replayed: hex.EncodeToString(replayed),
			quote:    hex.EncodeToString(quoteRTMR3),
		})
	}

	var events []EventLogEntry
	if err := json.Unmarshal([]byte(rep.Attestation.Evidence.EventLog), &events); err != nil {
		return nil, attestErr(reasonQuoteInvalid, &eventLogParseError{cause: err})
	}
	appID, err := extractAppID(events)
	if err != nil {
		return nil, attestErr(reasonQuoteInvalid, err)
	}
	return appID, nil
}

// checkAppIDPolicy enforces the app-id allow-list WHEN CONFIGURED: if accepted is
// non-empty, lowercase hex(appID) must be a member, else policy_rejected. An
// empty or nil accepted set skips the check (returns nil). The key form is
// lowercase hex of the app-id bytes (encoding/hex emits lowercase); Task 2.8's
// AcceptedAppIDs must use that same form.
func checkAppIDPolicy(appID []byte, accepted map[string]struct{}) error {
	if len(accepted) == 0 {
		return nil
	}
	key := hex.EncodeToString(appID)
	if _, ok := accepted[key]; !ok {
		return attestErr(reasonPolicyRejected, &appIDRejectedError{appID: key})
	}
	return nil
}

// checkProvenancePolicy enforces the source-provenance allow-list WHEN
// CONFIGURED: if accepted is non-empty, the report's {repo_url, repo_commit}
// pair must be a member, else policy_rejected. An empty or nil accepted set skips
// the check. The key is the provenanceKey struct; Task 2.8's
// AcceptedSourceProvenance must key on the same struct.
func checkProvenancePolicy(prov SourceProvenance, accepted map[provenanceKey]struct{}) error {
	if len(accepted) == 0 {
		return nil
	}
	key := provenanceKey{RepoURL: prov.RepoURL, RepoCommit: prov.RepoCommit}
	if _, ok := accepted[key]; !ok {
		return attestErr(reasonPolicyRejected, &provenanceRejectedError{
			repoURL:    prov.RepoURL,
			repoCommit: prov.RepoCommit,
		})
	}
	return nil
}

// eventLogParseError is the typed cause wrapped inside the quote_invalid
// *llm.AttestationError when evidence.event_log is not valid JSON. It chains the
// stdlib json error via Unwrap so callers can errors.As to it while keeping the
// uniform typed-cause contract.
type eventLogParseError struct {
	cause error
}

func (e *eventLogParseError) Error() string {
	return "aci/rtmr: event_log parse: " + e.cause.Error()
}

func (e *eventLogParseError) Unwrap() error { return e.cause }

// digestDecodeError is the typed cause when an imr==3 event's digest is not hex.
// index locates the offending event for diagnostics. Digests are public
// measurements, never secrets.
type digestDecodeError struct {
	index int
	cause error
}

func (e *digestDecodeError) Error() string {
	return "aci/rtmr: event digest hex decode (event " + strconv.Itoa(e.index) + "): " + e.cause.Error()
}

func (e *digestDecodeError) Unwrap() error { return e.cause }

// appIDDecodeError is the typed cause when the app-id event's event_payload is
// not hex.
type appIDDecodeError struct {
	cause error
}

func (e *appIDDecodeError) Error() string {
	return "aci/rtmr: app-id payload hex decode: " + e.cause.Error()
}

func (e *appIDDecodeError) Unwrap() error { return e.cause }

// missingAppIDError is the typed cause when no imr==3 "app-id" event precedes
// the first imr==3 "system-ready" event. It carries no fields: absence is the
// whole story.
type missingAppIDError struct{}

func (e *missingAppIDError) Error() string {
	return "aci/rtmr: no app-id event before system-ready"
}

// rtmr3MismatchError is the typed cause when the replayed IMR3 does not equal the
// quote's attested RTMR3. Both values are public 48-byte measurements (hex), not
// secrets, so logging them leaks nothing.
type rtmr3MismatchError struct {
	replayed string
	quote    string
}

func (e *rtmr3MismatchError) Error() string {
	return "aci/rtmr: IMR3 replay " + e.replayed + " != quote RTMR3 " + e.quote
}

// appIDRejectedError is the typed cause when the app-id is not in the configured
// accepted set. appID is its lowercase hex (a public identity, not a secret).
type appIDRejectedError struct {
	appID string
}

func (e *appIDRejectedError) Error() string {
	return "aci/rtmr: app-id " + e.appID + " not in accepted set"
}

// provenanceRejectedError is the typed cause when the report's source provenance
// is not in the configured accepted set. Both fields are public build metadata.
type provenanceRejectedError struct {
	repoURL    string
	repoCommit string
}

func (e *provenanceRejectedError) Error() string {
	return "aci/rtmr: source_provenance " + e.repoURL + "@" + e.repoCommit + " not in accepted set"
}
