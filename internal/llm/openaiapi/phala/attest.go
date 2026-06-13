package phala

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// parsedAttestation holds the per-instance fields the binding+verify code
// needs after parseAttestationReport has resolved the response shape. The
// IsChutes flag selects between the two binding schemes: Shapes A/B use the
// phala layout (report_data = signingAddr || nonce); Shape C uses the chutes
// layout (report_data[:32] = sha256(nonceHex + pubKeyB64)).
type parsedAttestation struct {
	// SigningAddress is 20 raw bytes (ECDSA / Ethereum-padded) or 32 raw
	// bytes (Ed25519 public key). Empty for Shape C (no signing address).
	SigningAddress []byte
	// Algo is "ecdsa" or "ed25519" for Shapes A/B; "chutes" for Shape C
	// (sentinel — no receipt verify, chutes binding).
	Algo string
	// RawQuote is the decoded TDX quote bytes ready for tee.VerifyTDXQuote.
	// Shapes A/B hex-decode the intel_quote field; Shape C base64-decodes it.
	RawQuote []byte
	// NvidiaPayload is the RAW JSON bytes of the gpu_evidence array — for
	// Shapes A/B, the content of the JSON-stringified nvidia_payload field
	// after one json.Unmarshal pass. For Shape C, a json.Marshal of the
	// already-decoded gpu_evidence array (which arrives as a JSON array, not
	// a string) so the downstream json.Unmarshal in attest() works uniformly.
	NvidiaPayload []byte
	// Nonce is the 32 raw bytes the server echoed in request_nonce (Shapes
	// A/B) or in the per-attestation `nonce` field (Shape C).
	Nonce []byte
	// NonceHex is the lowercase hex of Nonce. Shape C needs the string form
	// for the chutes binding hash (sha256 over UTF-8 of nonceHex+pubKeyB64).
	NonceHex string
	// PubKeyB64 is the base64 e2e_pubkey string the chutes binding hash is
	// computed over. Set only for Shape C; ignored for Shapes A/B.
	PubKeyB64 string
}

// shape A: flat per-instance.
type flatReport struct {
	SigningAddress string `json:"signing_address"`
	SigningAlgo    string `json:"signing_algo"`
	RequestNonce   string `json:"request_nonce"`
	IntelQuote     string `json:"intel_quote"`
	NvidiaPayload  string `json:"nvidia_payload"`
}

// shape B: gateway wrapper + per-model entries. Only the array is required
// here; the gateway block is captured (but unused at v1) for future logging.
type twoLayerReport struct {
	GatewayAttestation json.RawMessage `json:"gateway_attestation"`
	ModelAttestations  []struct {
		ModelName      string `json:"model_name"`
		SigningAddress string `json:"signing_address"`
		SigningAlgo    string `json:"signing_algo"`
		RequestNonce   string `json:"request_nonce"`
		IntelQuote     string `json:"intel_quote"`
		NvidiaPayload  string `json:"nvidia_payload"`
	} `json:"model_attestations"`
}

// shape C: chutes-passthrough. RedPill returns this for chutes-backed models
// (e.g. phala/kimi-k2.5). The top-level wraps an array of per-instance chutes
// evidence blocks; each entry is the same kind of evidence that
// llm/chutes/discover.go produces for a single instance. The TDX quote and
// e2e_pubkey are bound via the chutes scheme (report_data[:32] =
// sha256(nonceHex + pubKeyB64)), NOT the phala layout. There is NO receipt
// endpoint for these models — /v1/signature/{id} echoes back another
// shape-C attestation report. See WIRE.md section 3.
type chutesReport struct {
	AttestationType string `json:"attestation_type"`
	AllAttestations []struct {
		InstanceID  string            `json:"instance_id"`
		Nonce       string            `json:"nonce"`
		E2EPubKey   string            `json:"e2e_pubkey"`
		IntelQuote  string            `json:"intel_quote"`
		GPUEvidence []json.RawMessage `json:"gpu_evidence"`
	} `json:"all_attestations"`
}

// parseAttestationReport detects the response shape of
// GET /v1/attestation/report and returns the per-instance fields required by
// the binding + TDX/NRAS verification chain. See WIRE.md section 3.
//
// Errors are always *llm.AttestationError:
//   - ReasonAttestationMalformed: body doesn't decode or lacks the required
//     fields for any of the three known shapes.
//   - ReasonNoInstance: two-layer body present but no model_attestations
//     entry whose model_name equals the requested model; or shape C body
//     present but no all_attestations entries.
func parseAttestationReport(body []byte, model string) (parsedAttestation, error) {
	if len(body) == 0 {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			errors.New("empty body"))
	}

	// Shape C dispatch is keyed on the top-level attestation_type sentinel
	// ("chutes"). Detect this first so we never try to parse a chutes-shaped
	// body as flat / two-layer (it would mis-match on missing fields).
	var sniff struct {
		AttestationType string `json:"attestation_type"`
	}
	if err := json.Unmarshal(body, &sniff); err == nil && sniff.AttestationType == "chutes" {
		return decodeChutesEntry(body)
	}

	// Try flat next. A flat body has a non-empty top-level intel_quote.
	var flat flatReport
	if err := json.Unmarshal(body, &flat); err == nil && flat.IntelQuote != "" {
		return decodeEntry(flat.SigningAddress, flat.SigningAlgo, flat.RequestNonce, flat.IntelQuote, flat.NvidiaPayload)
	}

	// Fall back to two-layer.
	var two twoLayerReport
	if err := json.Unmarshal(body, &two); err == nil && len(two.ModelAttestations) > 0 {
		for _, entry := range two.ModelAttestations {
			if entry.ModelName == model {
				return decodeEntry(entry.SigningAddress, entry.SigningAlgo, entry.RequestNonce, entry.IntelQuote, entry.NvidiaPayload)
			}
		}
		return parsedAttestation{}, attestErr(ReasonNoInstance,
			fmt.Errorf("no model_attestations entry for model %q", model))
	}

	return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
		errors.New("body matched none of flat, two-layer, or chutes-passthrough shapes"))
}

func decodeEntry(signingAddress, signingAlgo, requestNonce, intelQuote, nvidiaPayload string) (parsedAttestation, error) {
	if intelQuote == "" {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			errors.New("intel_quote is empty"))
	}

	algo := strings.ToLower(strings.TrimSpace(signingAlgo))
	if algo != "ecdsa" && algo != "ed25519" {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			fmt.Errorf("signing_algo %q: want ecdsa or ed25519", signingAlgo))
	}

	addr, err := hex.DecodeString(strings.TrimPrefix(signingAddress, "0x"))
	if err != nil {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			fmt.Errorf("decode signing_address: %w", err))
	}
	if len(addr) != 20 && len(addr) != 32 {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			fmt.Errorf("signing_address is %d bytes; want 20 (ecdsa) or 32 (ed25519)", len(addr)))
	}

	if requestNonce == "" {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			errors.New("request_nonce is missing"))
	}
	nonce, err := hex.DecodeString(strings.TrimPrefix(requestNonce, "0x"))
	if err != nil {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			fmt.Errorf("decode request_nonce: %w", err))
	}
	if len(nonce) != 32 {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			fmt.Errorf("request_nonce is %d bytes; want 32", len(nonce)))
	}

	// nvidia_payload arrives as a JSON-stringified gpu_evidence array
	// (WIRE.md section 3). The outer struct's `NvidiaPayload string` tag
	// already did the one-level decode: the contents of that string are the
	// raw JSON bytes the consumer (tee.VerifyGPUEvidence) parses next. A
	// missing/null field decodes to the empty string, which we reject.
	if nvidiaPayload == "" {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			errors.New("nvidia_payload is empty"))
	}

	rawQuote, err := hex.DecodeString(strings.TrimPrefix(intelQuote, "0x"))
	if err != nil {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			fmt.Errorf("decode intel_quote hex: %w", err))
	}

	return parsedAttestation{
		SigningAddress: addr,
		Algo:           algo,
		RawQuote:       rawQuote,
		NvidiaPayload:  []byte(nvidiaPayload),
		Nonce:          nonce,
		NonceHex:       strings.ToLower(strings.TrimPrefix(requestNonce, "0x")),
	}, nil
}

// decodeChutesEntry parses a shape-C body and returns the first instance whose
// fields decode cleanly. RedPill exposes multiple chutes instances per model;
// at v1 we only need one to verify so the caller can chat. The selection is
// "first decodable" rather than "all of them" because per-instance failures
// (e.g. one quote that won't parse) should not block chat; we still fail
// closed if NO instance decodes.
func decodeChutesEntry(body []byte) (parsedAttestation, error) {
	var doc chutesReport
	if err := json.Unmarshal(body, &doc); err != nil {
		return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
			fmt.Errorf("decode chutes-passthrough body: %w", err))
	}
	if len(doc.AllAttestations) == 0 {
		return parsedAttestation{}, attestErr(ReasonNoInstance,
			errors.New("chutes-passthrough: all_attestations is empty"))
	}

	var lastErr error
	for _, e := range doc.AllAttestations {
		if e.IntelQuote == "" || e.E2EPubKey == "" || e.Nonce == "" || len(e.GPUEvidence) == 0 {
			lastErr = fmt.Errorf("instance %s: missing required fields", e.InstanceID)
			continue
		}
		// intel_quote is base64-encoded for chutes (NOT hex like phala
		// shapes A/B). The chutes adapter decodes it the same way.
		rawQuote, err := base64.StdEncoding.DecodeString(e.IntelQuote)
		if err != nil {
			lastErr = fmt.Errorf("instance %s: decode intel_quote base64: %w", e.InstanceID, err)
			continue
		}
		nonceHex := strings.ToLower(strings.TrimPrefix(e.Nonce, "0x"))
		nonceBytes, err := hex.DecodeString(nonceHex)
		if err != nil {
			lastErr = fmt.Errorf("instance %s: decode nonce: %w", e.InstanceID, err)
			continue
		}
		if len(nonceBytes) != 32 {
			lastErr = fmt.Errorf("instance %s: nonce is %d bytes; want 32", e.InstanceID, len(nonceBytes))
			continue
		}
		// gpu_evidence arrives as a JSON array of {arch, certificate,
		// evidence}; re-marshal to a flat JSON array so the downstream
		// caller can json.Unmarshal it as []tee.GPUEvidence using the same
		// path as the phala-flat shape.
		gpuJSON, err := json.Marshal(e.GPUEvidence)
		if err != nil {
			lastErr = fmt.Errorf("instance %s: re-marshal gpu_evidence: %w", e.InstanceID, err)
			continue
		}
		// The chutes binding hash is over the bare base64 pubkey string
		// (no "0x" prefix); some RedPill endpoints (notably the signature
		// endpoint) prepend "0x" so we strip it before binding.
		pubKeyB64 := strings.TrimPrefix(e.E2EPubKey, "0x")

		return parsedAttestation{
			Algo:          "chutes",
			RawQuote:      rawQuote,
			NvidiaPayload: gpuJSON,
			Nonce:         nonceBytes,
			NonceHex:      nonceHex,
			PubKeyB64:     pubKeyB64,
		}, nil
	}

	return parsedAttestation{}, attestErr(ReasonAttestationMalformed,
		fmt.Errorf("chutes-passthrough: no decodable instance: %w", lastErr))
}

// verifyChutesBinding checks the report_data binding for shape-C bodies:
// report_data[:32] == sha256(nonceHex + pubKeyB64). This mirrors the chutes
// scheme (see llm/chutes/attest.go bindingHash); chutes hashes the UTF-8 string
// concatenation, NOT raw bytes. report_data[32:64] is the gateway TLS cert
// hash that we cannot verify from this side (we do not terminate the gateway's
// mTLS); skip it, matching the chutes adapter's policy.
func verifyChutesBinding(reportData []byte, nonceHex, pubKeyB64 string) error {
	if len(reportData) != 64 {
		return attestErr(ReasonBindingMismatch,
			fmt.Errorf("report_data is %d bytes; want 64", len(reportData)))
	}
	want := chutesBindingHash(nonceHex, pubKeyB64)
	got := hex.EncodeToString(reportData[:32])
	if got != want {
		return attestErr(ReasonBindingMismatch,
			errors.New("chutes-style binding does not match report_data[:32]"))
	}
	return nil
}

// chutesBindingHash returns the hex SHA-256 of (nonceHex || pubKeyB64) — the
// chutes_version >= 0.6.0 binding hash. Kept separate from verifyChutesBinding
// so tests can spot-check the hash without going through the 64-byte report.
func chutesBindingHash(nonceHex, pubKeyB64 string) string {
	sum := sha256.Sum256([]byte(nonceHex + pubKeyB64))
	return hex.EncodeToString(sum[:])
}

// verifyBinding checks that the (signing_address, nonce) pair reported by
// the server matches the 64-byte report_data extracted from a verified TDX
// quote. See WIRE.md section 4:
//
//	report_data[0:32]  == leftPad(signingAddress, 32)
//	report_data[32:64] == nonce
//
// Any mismatch (including wrong-sized inputs) returns
// *llm.AttestationError{Reason: ReasonBindingMismatch}.
func verifyBinding(reportData, signingAddress, nonce []byte) error {
	if len(reportData) != 64 {
		return attestErr(ReasonBindingMismatch,
			fmt.Errorf("report_data is %d bytes; want 64", len(reportData)))
	}
	if len(signingAddress) > 32 {
		return attestErr(ReasonBindingMismatch,
			fmt.Errorf("signing_address is %d bytes; max 32", len(signingAddress)))
	}
	if len(nonce) != 32 {
		return attestErr(ReasonBindingMismatch,
			fmt.Errorf("nonce is %d bytes; want 32", len(nonce)))
	}
	want := make([]byte, 32)
	copy(want[32-len(signingAddress):], signingAddress)
	if !bytes.Equal(reportData[0:32], want) {
		return attestErr(ReasonBindingMismatch,
			errors.New("signing_address half does not match report_data[0:32]"))
	}
	if !bytes.Equal(reportData[32:64], nonce) {
		return attestErr(ReasonBindingMismatch,
			errors.New("nonce half does not match report_data[32:64]"))
	}
	return nil
}
