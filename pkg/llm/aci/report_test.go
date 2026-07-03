package aci

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/harness/pkg/llm"
)

// readFixture loads the authoritative aci/1 report fixture once per test.
func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "report_aci1.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// mutateAPIVersion returns the fixture with its single api_version value flipped
// to "aci/2" (the rest of the document byte-identical) so the version guard is
// exercised against an otherwise well-formed, fully-populated report. The
// "aci/1" literal occurs exactly once in the fixture, so this replacement is
// unambiguous.
func mutateAPIVersion(t *testing.T, raw []byte) []byte {
	t.Helper()
	old := []byte(`"aci/1"`)
	if bytes.Count(raw, old) != 1 {
		t.Fatalf("fixture must contain %q exactly once for the mutation to be unambiguous", old)
	}
	return bytes.Replace(raw, old, []byte(`"aci/2"`), 1)
}

// maxUint64 is the keyset_epoch.not_after sentinel carried by the fixture; it
// overflows int64 and pins the not_after uint64 modeling decision.
const maxUint64 uint64 = 18446744073709551615

func TestParseReportFixture(t *testing.T) {
	t.Parallel()
	raw := readFixture(t)

	rep, err := ParseReport(raw)
	if err != nil {
		t.Fatalf("ParseReport(fixture) error = %v, want nil", err)
	}
	if rep == nil {
		t.Fatal("ParseReport(fixture) returned nil report")
	}

	att := rep.Attestation
	ks := att.Keyset

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"api_version", rep.APIVersion, "aci/1"},
		{"workload_id", rep.WorkloadID, "sha256:3def476b72026924f9d88f7b339b2e211553fc2486c6a974d5f330162f183eda"},
		{"workload_keyset_digest", rep.WorkloadKeysetDigest, "sha256:46cdea445e5f2abcb78a5db0a630e7ba2127bc4f24033aa628b49e310e3928e2"},
		{"attestation.vendor", att.Vendor, "private-ai-gateway-dev"},
		{"attestation.tee_type", att.TEEType, "tdx"},
		{"workload_identity.public_key.algo", ks.Identity.PublicKey.Algo, "ecdsa-secp256k1"},
		{"workload_identity.public_key.public_key", ks.Identity.PublicKey.PublicKeyHex, "04d3b51dcb45d74434a76fc1b7e2bc152cf81190eab43bdbf5c2c624321232c76ac9122f8b93480663b987a38e2b3b1f42c7ca8e84736c9741175c07eeca62d382"},
		{"keyset_epoch.version", ks.Epoch.Version, uint64(1)},
		{"keyset_epoch.not_after", ks.Epoch.NotAfter, maxUint64},
		{"receipt_signing_keys[0].key_id", ks.ReceiptSigningKeys[0].KeyID, "dstack-kms-receipt-v1"},
		{"receipt_signing_keys[0].algo", ks.ReceiptSigningKeys[0].Algo, "ecdsa-secp256k1"},
		{"receipt_signing_keys[0].public_key", ks.ReceiptSigningKeys[0].PublicKeyHex, "04211c7585e727c2a2e5782dafc9d283b6afbfc88bd117aeb484a0a9495eae3d09bd7d2f71ca10020fb1397a08efd67dff76b29c40f6057a38bc8c424cd142406d"},
		{"e2ee_public_keys[0].key_id", ks.E2EEPublicKeys[0].KeyID, "dstack-kms-e2ee-v1"},
		{"e2ee_public_keys[0].algo", ks.E2EEPublicKeys[0].Algo, "secp256k1-aes-256-gcm-hkdf-sha256"},
		{"tls_public_keys[0].domain", ks.TLSPublicKeys[0].Domain, "tee.redpill.ai"},
		{"tls_public_keys[0].spki_sha256", ks.TLSPublicKeys[0].SPKISHA256Hex, "11af02e1c69bb2227e9b65903010abb60fbb626930cd11b0866281bb291a352c"},
		{"report_data", att.ReportDataHex, "659810686f7ef071d736034d8131f13258cad714dcf812df6a9894a82ff98b63"},
		{"keyset_endorsement.algo", att.KeysetEndorsement.Algo, "ecdsa-secp256k1"},
		{"keyset_endorsement.value", att.KeysetEndorsement.ValueHex, "3f2c44ecda30967eaa6a7ae170153692490778602fb3bb1143fb775124e677360802afbfecc3fa9c37a593adce1486dea42725cdf79f1db05b7cf66b91ef96e9"},
		{"source_provenance.repo_url", att.SourceProvenance.RepoURL, "https://github.com/Dstack-TEE/private-ai-gateway.git"},
		{"source_provenance.repo_commit", att.SourceProvenance.RepoCommit, "1b43f76e43c2459856faebe9cd97d8e01cb0df0c"},
		{"freshness.fetched_at", att.Freshness.FetchedAt, int64(1782664738)},
		{"freshness.stale_after", att.Freshness.StaleAfter, int64(1782668338)},
		{"evidence.quote_report_data", att.Evidence.QuoteReportData, "659810686f7ef071d736034d8131f13258cad714dcf812df6a9894a82ff98b630000000000000000000000000000000000000000000000000000000000000000"},
		{"evidence.key_custody.provider", att.Evidence.KeyCustody.Provider, "dstack-kms"},
		{"evidence.key_custody.keys[0].role", att.Evidence.KeyCustody.Keys[0].Role, "identity"},
		{"evidence.key_custody.keys[0].path", att.Evidence.KeyCustody.Keys[0].Path, "aci/identity/v1"},
		{"evidence.key_custody.keys[0].purpose", att.Evidence.KeyCustody.Keys[0].Purpose, "aci.identity.v1"},
		{"evidence.key_custody.keys[0].algo", att.Evidence.KeyCustody.Keys[0].Algo, "ecdsa-secp256k1"},
		{"evidence.key_custody.keys[0].public_key", att.Evidence.KeyCustody.Keys[0].PublicKeyHex, "04d3b51dcb45d74434a76fc1b7e2bc152cf81190eab43bdbf5c2c624321232c76ac9122f8b93480663b987a38e2b3b1f42c7ca8e84736c9741175c07eeca62d382"},
		{"evidence.key_custody.keys[0].signature_chain[0]", att.Evidence.KeyCustody.Keys[0].SignatureChain[0], "f4688f3b050b905c3ff10456570f08dc162d3e9b3a770a327a6821cf2b27329c2298ce6f3bff9ea05c451de990d23b1cee3cbaa19b1aadbdfa96548e4611b23300"},
		{"evidence.downstream_tls_binding.domain", att.Evidence.DownstreamTLSBinding.Domain, "inference.phala.com"},
		{"evidence.downstream_tls_binding.spki_sha256", att.Evidence.DownstreamTLSBinding.SPKISHA256Hex, "698c87b1ed32d3d67f23d14295fc443e91b82ad4e40482041ac1a9158c8212e2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
}

// TestParseReportFixtureLengthsAndRaw asserts the multi-entry collections and
// the raw JSON-in-string evidence fields that the per-field table can't express
// as simple scalars.
func TestParseReportFixtureLengthsAndRaw(t *testing.T) {
	t.Parallel()
	rep, err := ParseReport(readFixture(t))
	if err != nil {
		t.Fatalf("ParseReport(fixture) error = %v, want nil", err)
	}
	att := rep.Attestation
	ks := att.Keyset

	tests := []struct {
		name string
		got  int
		want int
	}{
		{"receipt_signing_keys length", len(ks.ReceiptSigningKeys), 1},
		{"e2ee_public_keys length", len(ks.E2EEPublicKeys), 3},
		{"tls_public_keys length", len(ks.TLSPublicKeys), 3},
		{"key_custody.keys length", len(att.Evidence.KeyCustody.Keys), 4},
		{"key_custody.keys[0].signature_chain length", len(att.Evidence.KeyCustody.Keys[0].SignatureChain), 2},
		{"service_capabilities.supported_e2ee_versions length", len(rep.ServiceCapabilities.SupportedE2EEVersions), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
			}
		})
	}

	// event_log and vm_config stay raw JSON-in-string at this layer (parsed in
	// later tasks): assert they are non-empty and begin with the expected token.
	if got := att.Evidence.EventLog; len(got) == 0 || got[0] != '[' {
		t.Errorf("evidence.event_log not preserved as raw JSON array string: %.20q", got)
	}
	if got := att.Evidence.VMConfig; len(got) == 0 || got[0] != '{' {
		t.Errorf("evidence.vm_config not preserved as raw JSON object string: %.20q", got)
	}
	if att.Evidence.Quote == "" {
		t.Error("evidence.quote is empty, want the TDX quote hex")
	}
	// subject is null in the fixture: the optional pointer must be nil.
	if ks.Identity.Subject != nil {
		t.Errorf("workload_identity.subject = %v, want nil", *ks.Identity.Subject)
	}
}

func TestParseReportErrors(t *testing.T) {
	t.Parallel()
	raw := readFixture(t)

	tests := []struct {
		name    string
		input   []byte
		wantAPI bool // expect *llm.AttestationError with unsupported_api_version
	}{
		{name: "api_version aci/2 -> unsupported_api_version", input: mutateAPIVersion(t, raw), wantAPI: true},
		{name: "malformed JSON -> typed parse error", input: []byte(`{"api_version": `)},
		{name: "empty input -> typed parse error", input: []byte("")},
		{name: "not an object -> typed parse error", input: []byte(`"aci/1"`)},
		{name: "nil input -> typed parse error", input: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep, err := ParseReport(tt.input)
			if err == nil {
				t.Fatalf("ParseReport() error = nil, want error")
			}
			if rep != nil {
				t.Errorf("ParseReport() report = %v, want nil on error", rep)
			}

			var attErr *llm.AttestationError
			isAttErr := errors.As(err, &attErr)
			if tt.wantAPI {
				if !isAttErr {
					t.Fatalf("ParseReport() error = %T, want *llm.AttestationError", err)
				}
				if attErr.Reason != reasonUnsupportedAPIVersion {
					t.Errorf("AttestationError.Reason = %q, want %q", attErr.Reason, reasonUnsupportedAPIVersion)
				}
				return
			}

			// Non-version failures must be a typed parse error, never the
			// version AttestationError, and never a bare stdlib error.
			if isAttErr {
				t.Fatalf("ParseReport() returned AttestationError for a non-version failure: %v", err)
			}
			var parseErr *reportParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("ParseReport() error = %T (%v), want *reportParseError", err, err)
			}
		})
	}
}
