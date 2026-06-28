package aci

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseReport asserts that ParseReport never panics on arbitrary input:
// well-formed, malformed, truncated, or pure garbage bytes must all return
// cleanly (a *Report or a typed error, never a crash). Parsing untrusted report
// JSON is the package's external-input boundary, so it must be total. The corpus
// is seeded with the real aci/1 fixture plus a few hand mutations that exercise
// the version guard, structural truncation, type confusion, and emptiness.
func FuzzParseReport(f *testing.F) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "report_aci1.json"))
	if err != nil {
		f.Fatalf("read fixture: %v", err)
	}

	seeds := [][]byte{
		fixture,
		[]byte(`{"api_version":"aci/1"}`), // minimal valid version, empty tree
		[]byte(`{"api_version":"aci/2"}`), // unsupported version
		[]byte(`{"api_version":123}`),     // wrong type for version
		[]byte(`{"attestation":{"workload_keyset":{"keyset_epoch":{"not_after":18446744073709551615}}}}`), // max-uint64 epoch
		[]byte(`{"attestation":{"workload_keyset":{"keyset_epoch":{"not_after":-1}}}}`),                   // out-of-range uint64
		[]byte(`{`),    // truncated object
		[]byte(`null`), // bare null
		[]byte(`[]`),   // array, not object
		[]byte(``),     // empty
		[]byte(`{"api_version":"aci/1","attestation":{"evidence":{"event_log":"not json"}}}`), // raw event_log stays a string
		nil,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// The only contract under fuzz is no panic. On a nil error a *Report
		// must come back; on any error it must be nil — assert that invariant so
		// a regression surfaces as a fuzz failure, not silent corruption.
		rep, err := ParseReport(data)
		if err == nil && rep == nil {
			t.Fatalf("ParseReport(%q) returned nil report with nil error", data)
		}
		if err != nil && rep != nil {
			t.Fatalf("ParseReport(%q) returned non-nil report %v with error %v", data, rep, err)
		}
	})
}
