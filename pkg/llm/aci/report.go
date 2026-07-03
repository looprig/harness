package aci

import (
	"encoding/json"

	"github.com/looprig/harness/pkg/llm"
)

// This file defines the Go model of a Dstack ACI ("aci/1") attestation report
// and ParseReport, the single entry point that decodes report JSON into that
// model and enforces the api_version tripwire. Every downstream verification
// step (quote parsing, keyset-digest binding, endorsement check, freshness,
// receipt verification) consumes the *Report produced here, so the tree models
// the wire document faithfully and uses named types for the recurring
// key/binding shapes Phase 2 reaches for.
//
// MODELING NOTES (the fixture testdata/report_aci1.json is authoritative):
//   - keyset_epoch.not_after is uint64: the live fixture carries 2^64-1
//     (18446744073709551615), which overflows int64. It is the only field that
//     must be unsigned 64-bit; every other numeric field fits int64.
//   - Serde field renames are applied as Go FIELD NAMES; the json TAG keeps the
//     wire key. report_data -> ReportDataHex, value -> ValueHex, public_key ->
//     PublicKeyHex, spki_sha256 -> SPKISHA256Hex. The event-log type ->
//     EventType rename lives on EventLogEntry, modeled here for Task 2.5 to
//     reuse, but event_log itself stays a raw string at this layer.
//   - evidence.event_log and evidence.vm_config are kept as raw JSON-in-string
//     (the gateway double-encodes them); Task 2.5 parses the event log and Task
//     2.7 uses key_custody. Decoding them here would couple this layer to those
//     later parsers.

// Report is a decoded Dstack ACI attestation report. It is the root of the
// model every Phase 2 verification step consumes.
type Report struct {
	APIVersion           string              `json:"api_version"`
	WorkloadID           string              `json:"workload_id"`
	WorkloadKeysetDigest string              `json:"workload_keyset_digest"`
	Attestation          Attestation         `json:"attestation"`
	ServiceCapabilities  ServiceCapabilities `json:"service_capabilities"`
}

// Attestation is the report's attestation envelope: the workload keyset, the
// report_data the quote binds to, the keyset endorsement, build provenance,
// freshness window, and the TEE evidence (quote + event log + custody).
type Attestation struct {
	Vendor            string            `json:"vendor"`
	TEEType           string            `json:"tee_type"`
	Keyset            Keyset            `json:"workload_keyset"`
	ReportDataHex     string            `json:"report_data"`
	KeysetEndorsement KeysetEndorsement `json:"keyset_endorsement"`
	SourceProvenance  SourceProvenance  `json:"source_provenance"`
	Freshness         Freshness         `json:"freshness"`
	Evidence          Evidence          `json:"evidence"`
}

// Keyset is the workload's published key material: the workload identity key,
// the epoch (version + expiry), and the receipt/E2EE/TLS key lists the gateway
// uses to sign receipts, accept E2EE envelopes, and bind TLS endpoints.
type Keyset struct {
	Identity           WorkloadIdentity `json:"workload_identity"`
	Epoch              KeysetEpoch      `json:"keyset_epoch"`
	ReceiptSigningKeys []KeyEntry       `json:"receipt_signing_keys"`
	E2EEPublicKeys     []KeyEntry       `json:"e2ee_public_keys"`
	TLSPublicKeys      []TLSBinding     `json:"tls_public_keys"`
}

// WorkloadIdentity is the workload's identity key plus an optional subject. The
// fixture's subject is null, so Subject is a pointer that stays nil when absent.
type WorkloadIdentity struct {
	PublicKey PublicKey `json:"public_key"`
	Subject   *string   `json:"subject"`
}

// PublicKey is an algorithm-tagged public key (algo + hex-encoded key bytes).
// public_key -> PublicKeyHex applies the serde rename; the wire key stays
// "public_key".
type PublicKey struct {
	Algo         string `json:"algo"`
	PublicKeyHex string `json:"public_key"`
}

// KeysetEpoch is the keyset's monotonic epoch and its expiry. NotAfter is
// uint64 because the fixture carries 2^64-1 (a "never expires" sentinel) which
// does not fit int64. Version is uint64 for symmetry with the unsigned epoch.
type KeysetEpoch struct {
	Version  uint64 `json:"version"`
	NotAfter uint64 `json:"not_after"`
}

// KeyEntry is one entry in the receipt-signing and E2EE key lists: a key id, its
// algorithm, and the hex-encoded public key. The two lists share this shape, so
// they share the type. public_key -> PublicKeyHex applies the serde rename.
type KeyEntry struct {
	KeyID        string `json:"key_id"`
	Algo         string `json:"algo"`
	PublicKeyHex string `json:"public_key"`
}

// TLSBinding binds a domain to the SHA-256 of its TLS SubjectPublicKeyInfo. It
// is reused by both tls_public_keys and evidence.downstream_tls_binding.
// spki_sha256 -> SPKISHA256Hex applies the serde rename.
type TLSBinding struct {
	Domain        string `json:"domain"`
	SPKISHA256Hex string `json:"spki_sha256"`
}

// KeysetEndorsement is the signature over the workload keyset by the KMS root.
// value -> ValueHex applies the serde rename.
type KeysetEndorsement struct {
	Algo     string `json:"algo"`
	ValueHex string `json:"value"`
}

// SourceProvenance is the build provenance of the workload image. ImageDigest
// and ImageProvenance are null in the fixture, so both are optional pointers.
type SourceProvenance struct {
	RepoURL         string  `json:"repo_url"`
	RepoCommit      string  `json:"repo_commit"`
	ImageDigest     *string `json:"image_digest"`
	ImageProvenance *string `json:"image_provenance"`
}

// Freshness is the report's validity window in Unix seconds. Both bounds fit
// int64 comfortably (they are wall-clock seconds, not the epoch sentinel).
type Freshness struct {
	FetchedAt  int64 `json:"fetched_at"`
	StaleAfter int64 `json:"stale_after"`
}

// Evidence is the TEE evidence: the TDX quote and its report_data, the raw
// (double-encoded) event log and VM config strings, the KMS key-custody chain,
// and the downstream TLS binding. quote/event_log/vm_config stay raw strings at
// this layer; later tasks parse them.
type Evidence struct {
	Quote                string     `json:"quote"`
	QuoteReportData      string     `json:"quote_report_data"`
	EventLog             string     `json:"event_log"`
	VMConfig             string     `json:"vm_config"`
	KeyCustody           KeyCustody `json:"key_custody"`
	DownstreamTLSBinding TLSBinding `json:"downstream_tls_binding"`
}

// KeyCustody is the KMS custody record: which provider holds the workload keys
// and, per key, the signature chain proving custody back to the KMS root.
type KeyCustody struct {
	Provider string            `json:"provider"`
	Keys     []KeyCustodyEntry `json:"keys"`
}

// KeyCustodyEntry is one custody record: the key's role/path/purpose, its
// algorithm and hex public key, and the signature chain (hex links) binding it
// to the KMS root. public_key -> PublicKeyHex applies the serde rename.
type KeyCustodyEntry struct {
	Role           string   `json:"role"`
	Path           string   `json:"path"`
	Purpose        string   `json:"purpose"`
	Algo           string   `json:"algo"`
	PublicKeyHex   string   `json:"public_key"`
	SignatureChain []string `json:"signature_chain"`
}

// ServiceCapabilities advertises which E2EE protocol versions the gateway
// supports. Versions are wire strings ("2").
type ServiceCapabilities struct {
	SupportedE2EEVersions []string `json:"supported_e2ee_versions"`
}

// EventLogEntry is one parsed event-log record. evidence.event_log is a raw
// JSON-in-string at THIS layer; this type is modeled now so Task 2.5 can decode
// the log into it. The serde rename type -> EventType applies here (Go has no
// "type" field name and the wire key stays "event_type").
type EventLogEntry struct {
	IMR          uint32 `json:"imr"`
	EventType    uint32 `json:"event_type"`
	Digest       string `json:"digest"`
	Event        string `json:"event"`
	EventPayload string `json:"event_payload"`
}

// reportParseError wraps a stdlib encoding/json failure from ParseReport in a
// typed error, so the exported ParseReport contract is uniformly typed per
// CLAUDE.md (no bare fmt.Errorf/json error escaping the API) while still
// chaining the underlying cause via Unwrap (errors.As to *json.SyntaxError /
// *json.UnmarshalTypeError keeps working). It is distinct from jcs.go's
// parseError, which belongs to the JCS Value parser and carries that namespace.
type reportParseError struct {
	cause error
}

func (e *reportParseError) Error() string {
	return "aci/report: parse: " + e.cause.Error()
}

func (e *reportParseError) Unwrap() error { return e.cause }

// ParseReport decodes a Dstack ACI report document into *Report and enforces the
// api_version tripwire.
//
// It unmarshals the whole document first, THEN guards api_version: a malformed
// body therefore yields a typed *reportParseError (the decode failure) rather
// than being masked by a version check, and a well-formed body with the wrong
// version yields the fail-closed *llm.AttestationError carrying
// reasonUnsupportedAPIVersion (via errUnsupportedAPIVersion). On any error the
// returned *Report is nil. The bytes are untrusted external input; decoding is
// the validation boundary and is performed before the value is handed back.
func ParseReport(data []byte) (*Report, error) {
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, &reportParseError{cause: err}
	}
	if rep.APIVersion != SupportedAPIVersion {
		return nil, errUnsupportedAPIVersion(rep.APIVersion)
	}
	return &rep, nil
}

// compile-time assertions: both report errors satisfy error.
var (
	_ error = (*reportParseError)(nil)
	_ error = (*llm.AttestationError)(nil)
)
