package aci

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/harness/pkg/llm"
)

// Arbiter values, independently verified against the aci/1 fixture
// (testdata/report_aci1.json) and the authoritative Rust replay algorithm
// (src/aci/verifier/dstack.rs).
const (
	// fixtureRTMR3Hex is the quote's RTMR3 and the IMR3 event-log replay result;
	// the two must agree for evidence integrity.
	fixtureRTMR3Hex = "4861f99a6e910713667986c6ae6b4830c562eec3aad9e55d25a15bcf7c8dfc0a6b4fde2c326cdc6b3fcf708df20c10c9"
	// fixtureAppIDHex is hex(app-id bytes): the event_payload of the first
	// imr==3 "app-id" event (20 bytes).
	fixtureAppIDHex = "fdb7a14e5a6675f752e2cb69c9067a98ca402918"
	// fixtureRepoURL / fixtureRepoCommit are the fixture source_provenance.
	fixtureRepoURL    = "https://github.com/Dstack-TEE/private-ai-gateway.git"
	fixtureRepoCommit = "1b43f76e43c2459856faebe9cd97d8e01cb0df0c"
)

// Synthetic event-log vectors with precomputed replays, exercising the
// pad-to-48 path and the take_while stop-at-system-ready behavior. Values
// precomputed with crypto/sha512.Sum384 over mr(48)‖pad48(digest).
const (
	// shortDigestLog is one imr==3 event whose digest is 3 bytes ("aabbcc"),
	// so the replay must right-pad it to 48 before hashing.
	shortDigestLog = `[{"imr": 3, "event_type": 1, "digest": "aabbcc", "event": "short", "event_payload": ""}]`
	// shortDigestRTMR3Hex is SHA384(zeros(48) ‖ pad48(aabbcc)).
	shortDigestRTMR3Hex = "19209749825e1950ee15487f43a28ed9d6cb2f5cacf31ed31b146f522eb5945f39d69c8d42da8c2dde29d862e57a99e6"
	// appIDAfterReadyLog places the only "app-id" event AFTER a "system-ready":
	// the take_while stop means it must NOT be found.
	appIDAfterReadyLog = `[{"imr": 3, "event_type": 1, "digest": "000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000", "event": "system-ready", "event_payload": ""}, {"imr": 3, "event_type": 1, "digest": "111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111", "event": "app-id", "event_payload": "deadbeef"}]`
)

// loadFixtureReport parses the aci/1 fixture into a *Report.
func loadFixtureReport(t *testing.T) *Report {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "report_aci1.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	rep, err := ParseReport(blob)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return rep
}

// mustHex decodes a hex string or fails the test.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

// assertAttestReason asserts err is an *llm.AttestationError with the given reason.
func assertAttestReason(t *testing.T, err error, wantReason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with reason %q, got nil", wantReason)
	}
	var ae *llm.AttestationError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *llm.AttestationError, got %T: %v", err, err)
	}
	if ae.Reason != wantReason {
		t.Fatalf("reason = %q, want %q", ae.Reason, wantReason)
	}
}

// mutateEventLog parses the fixture event_log, applies fn to the events, and
// returns the re-encoded event_log JSON string. Used to build tamper cases.
func mutateEventLog(t *testing.T, rep *Report, fn func([]EventLogEntry) []EventLogEntry) string {
	t.Helper()
	var events []EventLogEntry
	if err := json.Unmarshal([]byte(rep.Attestation.Evidence.EventLog), &events); err != nil {
		t.Fatalf("unmarshal fixture event_log: %v", err)
	}
	events = fn(events)
	out, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("re-marshal event_log: %v", err)
	}
	return string(out)
}

func TestReplayRTMR3(t *testing.T) {
	t.Parallel()
	rep := loadFixtureReport(t)

	tests := []struct {
		name     string
		eventLog string
		wantHex  string
		wantErr  bool
	}{
		{
			name:     "fixture replays to quote RTMR3",
			eventLog: rep.Attestation.Evidence.EventLog,
			wantHex:  fixtureRTMR3Hex,
		},
		{
			name:     "short digest is right-padded to 48",
			eventLog: shortDigestLog,
			wantHex:  shortDigestRTMR3Hex,
		},
		{
			name:     "empty event list replays to zeros",
			eventLog: `[]`,
			wantHex:  "000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name:     "malformed json is an error",
			eventLog: `not json`,
			wantErr:  true,
		},
		{
			name:     "non-hex digest is an error",
			eventLog: `[{"imr":3,"digest":"zz","event":"x","event_payload":""}]`,
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := replayRTMR3(tt.eventLog)
			if (err != nil) != tt.wantErr {
				t.Fatalf("replayRTMR3() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if hex.EncodeToString(got) != tt.wantHex {
				t.Fatalf("replayRTMR3() = %s, want %s", hex.EncodeToString(got), tt.wantHex)
			}
		})
	}
}

func TestVerifyEventLogAndAppID(t *testing.T) {
	t.Parallel()
	base := loadFixtureReport(t)

	// withEventLog returns a copy of the fixture report with a replaced event_log.
	withEventLog := func(log string) *Report {
		r := *base
		r.Attestation.Evidence.EventLog = log
		return &r
	}

	tests := []struct {
		name       string
		rep        *Report
		wantAppID  string
		wantErr    bool
		wantReason string
	}{
		{
			name:      "fixture returns app-id and verifies replay==quote",
			rep:       base,
			wantAppID: fixtureAppIDHex,
		},
		{
			name: "tampered digest breaks replay vs quote",
			rep: withEventLog(mutateEventLog(t, base, func(es []EventLogEntry) []EventLogEntry {
				for i := range es {
					if es[i].IMR == 3 {
						// flip the first byte of the first imr==3 digest
						b := mustHex(t, es[i].Digest)
						b[0] ^= 0xFF
						es[i].Digest = hex.EncodeToString(b)
						break
					}
				}
				return es
			})),
			wantErr:    true,
			wantReason: reasonQuoteInvalid,
		},
		{
			name: "missing app-id event is integrity failure",
			rep: withEventLog(mutateEventLog(t, base, func(es []EventLogEntry) []EventLogEntry {
				out := es[:0]
				for _, e := range es {
					if e.IMR == 3 && e.Event == "app-id" {
						continue // drop the app-id event
					}
					out = append(out, e)
				}
				return out
			})),
			wantErr:    true,
			wantReason: reasonQuoteInvalid,
		},
		{
			name:       "malformed event_log json is integrity failure",
			rep:        withEventLog(`{not valid json`),
			wantErr:    true,
			wantReason: reasonQuoteInvalid,
		},
		{
			name:       "app-id only after system-ready is not found",
			rep:        withEventLog(appIDAfterReadyLog),
			wantErr:    true,
			wantReason: reasonQuoteInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			appID, err := verifyEventLogAndAppID(tt.rep)
			if (err != nil) != tt.wantErr {
				t.Fatalf("verifyEventLogAndAppID() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				assertAttestReason(t, err, tt.wantReason)
				return
			}
			if hex.EncodeToString(appID) != tt.wantAppID {
				t.Fatalf("app-id = %s, want %s", hex.EncodeToString(appID), tt.wantAppID)
			}
		})
	}
}

func TestCheckAppIDPolicy(t *testing.T) {
	t.Parallel()
	appID := mustHex(t, fixtureAppIDHex)

	tests := []struct {
		name     string
		appID    []byte
		accepted map[string]struct{}
		wantErr  bool
	}{
		{
			name:     "app-id in accepted set passes",
			appID:    appID,
			accepted: map[string]struct{}{fixtureAppIDHex: {}},
		},
		{
			name:     "app-id not in non-empty set is rejected",
			appID:    appID,
			accepted: map[string]struct{}{"00112233445566778899aabbccddeeff00112233": {}},
			wantErr:  true,
		},
		{
			name:     "empty set skips the check",
			appID:    appID,
			accepted: map[string]struct{}{},
		},
		{
			name:     "nil set skips the check",
			appID:    appID,
			accepted: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkAppIDPolicy(tt.appID, tt.accepted)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkAppIDPolicy() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				assertAttestReason(t, err, reasonPolicyRejected)
			}
		})
	}
}

func TestCheckProvenancePolicy(t *testing.T) {
	t.Parallel()
	prov := SourceProvenance{RepoURL: fixtureRepoURL, RepoCommit: fixtureRepoCommit}
	accept := func(keys ...ProvenanceKey) map[ProvenanceKey]struct{} {
		m := make(map[ProvenanceKey]struct{}, len(keys))
		for _, k := range keys {
			m[k] = struct{}{}
		}
		return m
	}

	tests := []struct {
		name     string
		prov     SourceProvenance
		accepted map[ProvenanceKey]struct{}
		wantErr  bool
	}{
		{
			name:     "provenance in accepted set passes",
			prov:     prov,
			accepted: accept(ProvenanceKey{RepoURL: fixtureRepoURL, RepoCommit: fixtureRepoCommit}),
		},
		{
			name:     "different commit is rejected",
			prov:     prov,
			accepted: accept(ProvenanceKey{RepoURL: fixtureRepoURL, RepoCommit: "0000000000000000000000000000000000000000"}),
			wantErr:  true,
		},
		{
			name:     "different repo is rejected",
			prov:     prov,
			accepted: accept(ProvenanceKey{RepoURL: "https://example.com/other.git", RepoCommit: fixtureRepoCommit}),
			wantErr:  true,
		},
		{
			name:     "empty set skips the check",
			prov:     prov,
			accepted: map[ProvenanceKey]struct{}{},
		},
		{
			name:     "nil set skips the check",
			prov:     prov,
			accepted: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkProvenancePolicy(tt.prov, tt.accepted)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkProvenancePolicy() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				assertAttestReason(t, err, reasonPolicyRejected)
			}
		})
	}
}
