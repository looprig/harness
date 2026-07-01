package aci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzEventLog asserts the event_log parse+replay path never panics on arbitrary
// input. evidence.event_log is a double-encoded JSON-in-string the gateway
// supplies, so it is untrusted external input and the parse must be total: any
// bytes — valid, malformed, truncated, or garbage — must yield a value or a
// typed error, never a crash. The corpus is seeded with the real fixture
// event_log plus mutations that exercise the imr filter, the pad-to-48 path, the
// take_while app-id scan, and structural breakage.
func FuzzEventLog(f *testing.F) {
	seeds := []string{
		fixtureEventLog(f),
		shortDigestLog,
		appIDAfterReadyLog,
		`[]`,
		`[{"imr":3,"digest":"aabbcc","event":"app-id","event_payload":"deadbeef"}]`,
		`[{"imr":0,"digest":"00","event":"x","event_payload":""}]`, // non-imr3 only
		`[{"imr":3,"digest":"","event":"app-id","event_payload":""}]`,
		`[{"imr":3,"digest":"zz","event":"x","event_payload":""}]`, // non-hex digest
		`[{"imr":3}]`,
		`not json`,
		`{`,
		`null`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, eventLog string) {
		// Path 1: replay. The only contract is no panic; an error is fine, and a
		// nil-error result must be a 48-byte measurement.
		mr, err := replayRTMR3(eventLog)
		if err == nil && len(mr) != 48 {
			t.Fatalf("replayRTMR3 returned %d-byte measurement with nil error", len(mr))
		}

		// Path 2: app-id extraction over whatever parsed (independently of
		// replay) — also must not panic. Skip when the bytes are not even a
		// JSON event array; extractAppID operates on parsed events, not raw bytes.
		var events []EventLogEntry
		if json.Unmarshal([]byte(eventLog), &events) == nil {
			_, _ = extractAppID(events)
		}
	})
}

// fixtureEventLog returns the aci/1 fixture's raw event_log string for seeding.
func fixtureEventLog(f *testing.F) string {
	f.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "report_aci1.json"))
	if err != nil {
		f.Fatalf("read fixture: %v", err)
	}
	rep, err := ParseReport(blob)
	if err != nil {
		f.Fatalf("parse fixture: %v", err)
	}
	return rep.Attestation.Evidence.EventLog
}
