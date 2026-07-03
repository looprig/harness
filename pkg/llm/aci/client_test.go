package aci

// Task 5.2 — Client.Invoke (buffer-until-verified) tests.
//
// These tests exercise the HEART of the client: the ordered, fail-closed flow
//
//	attest(cached) -> encode -> seal -> POST -> buffer full response ->
//	open -> VerifyReceipt -> ONLY THEN decode.
//
// The hard part is a FAKE GATEWAY (fakeDoer) that honors the seal/receipt
// contract using THIS package's own functions (open/seal/canonicalReceipt), so
// the round-trip is real cryptography end to end: the gateway opens the sealed
// request fields with a test model key, seals a canned response to the client's
// per-request public key, and signs a §9 receipt with a test receipt key whose
// public half lives in the synthetic *VerifiedReport. The synthetic report is
// injected via WithAttestFunc, bypassing the real DCAP chain (already covered by
// Phase-2 tests) so these tests run fully offline.
//
// The happy path asserts Invoke returns the decoded *llm.Response (content
// "hello"), nil error. Every failure case asserts Invoke returns (nil, typed
// error) with NO partial *Response — and that the API key never leaks into any
// returned error string.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/llm"
	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// ----------------------------------------------------------------------------
// Test fixtures: the API key, base URL, model, and the canned response content.
// ----------------------------------------------------------------------------

const (
	testBaseURL      = "https://gateway.example.test"
	testAPIKey       = "sk-test-SUPER-SECRET-KEY-do-not-leak"
	testClientModel  = "gpt-4o"
	testReceiptID    = "rcpt-1"
	testRespContent  = "hello"
	testReceiptKeyID = "rcpt-key-1"
)

// testNow is the fixed clock the client and gateway share so timestamps,
// freshness, and the e2ee replay window are all deterministic.
func testNow() time.Time { return time.Unix(1750000000, 0) }

// ----------------------------------------------------------------------------
// Synthetic VerifiedReport carrying test e2ee + receipt-signing keys.
// ----------------------------------------------------------------------------

// gatewayKeys holds the PRIVATE halves the fake gateway needs to honor the
// contract: the model e2ee private key (to OPEN sealed request fields) and the
// receipt-signing private key (to SIGN receipts). The matching public halves
// live in the synthetic *VerifiedReport the client attests to.
type gatewayKeys struct {
	verified     *VerifiedReport
	modelPriv    *secp256k1.PrivateKey
	receiptPriv  ed25519.PrivateKey
	receiptKeyID string
}

// newGatewayKeys builds a synthetic *VerifiedReport whose keyset carries a test
// e2ee keypair (model side) and a test receipt-signing keypair, returning the
// report alongside the private halves the gateway uses.
func newGatewayKeys(t *testing.T) gatewayKeys {
	t.Helper()
	modelPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate model e2ee key: %v", err)
	}
	modelPubHex := hex.EncodeToString(modelPriv.PubKey().SerializeUncompressed())

	receiptPub, receiptPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate receipt key: %v", err)
	}

	vr := &VerifiedReport{
		WorkloadID:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		WorkloadKeysetDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		Keyset: Keyset{
			E2EEPublicKeys: []KeyEntry{
				{KeyID: "e2ee-1", Algo: e2eeRequestAlgo, PublicKeyHex: modelPubHex},
			},
			ReceiptSigningKeys: []KeyEntry{
				{KeyID: testReceiptKeyID, Algo: algoEd25519, PublicKeyHex: hex.EncodeToString(receiptPub)},
			},
		},
	}
	return gatewayKeys{
		verified:     vr,
		modelPriv:    modelPriv,
		receiptPriv:  receiptPriv,
		receiptKeyID: testReceiptKeyID,
	}
}

// ----------------------------------------------------------------------------
// The fake gateway httpDoer. It routes by method+path and honors the contract.
// ----------------------------------------------------------------------------

// gatewayTweaks lets a test perturb exactly one aspect of the gateway's honest
// behavior to drive a single failure case, leaving everything else valid.
type gatewayTweaks struct {
	// status overrides the POST response status (0 means 200 OK).
	status int
	// tamperWire mutates the sealed wire response bytes so open fails.
	tamperWire bool
	// wrongBodyHash injects a bogus request body_hash into the receipt.
	wrongBodyHash bool
	// wrongWireHash injects a bogus response wire_hash into the receipt.
	wrongWireHash bool
	// signWithWrongKey signs the receipt with a fresh key not in the keyset.
	signWithWrongKey bool
	// upstreamResult overrides the upstream.verified result ("" => "verified").
	upstreamResult string
}

// fakeDoer is the fake gateway. It captures the receipt JSON it builds during the
// POST so the subsequent receipt GET can return it, and records the API key seen
// on every request so a test can assert it was sent (and never leaked).
type fakeDoer struct {
	t      *testing.T
	keys   gatewayKeys
	tweaks gatewayTweaks

	// stream, when true, makes the POST handler serve a sealed SSE stream
	// (multiple data: {chunk}\n\n events + data: [DONE]\n\n) instead of a single
	// buffered chat.completion — the shape Client.Stream buffers and verifies.
	stream bool

	receipt     []byte // set during POST, returned on the receipt GET
	sawAuthPost bool
	sawAuthGet  bool
}

// Do routes the request to the gateway handler for its method+path.
func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/v1/chat/completions":
		return f.handleInference(req)
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/v1/aci/receipts/"):
		return f.handleReceipt(req)
	default:
		f.t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		return nil, nil
	}
}

// handleInference opens the sealed request, reconstructs the cleartext request
// body, seals a canned response to the client pubkey, builds + signs the receipt,
// and returns the sealed wire response with the x-receipt-id header.
func (f *fakeDoer) handleInference(req *http.Request) (*http.Response, error) {
	f.sawAuthPost = req.Header.Get("Authorization") == "Bearer "+testAPIKey
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		f.t.Fatalf("POST Content-Type = %q, want application/json", ct)
	}

	hdrModel := f.headerModel(req)
	nonce := req.Header.Get(hdrE2EENonce)
	tsStr := req.Header.Get(hdrE2EETimestamp)
	ts, err := strconv.ParseUint(tsStr, 10, 64)
	if err != nil {
		f.t.Fatalf("X-E2EE-Timestamp %q not a uint: %v", tsStr, err)
	}
	clientPubHex := req.Header.Get(hdrClientPubKey)
	clientPub := f.parsePub(clientPubHex)

	sealedBody := f.readBody(req)

	// Recover the cleartext request body: parse the sealed body, open the chat
	// content field in place with the model key under the request AAD, then
	// CompactJSON — this is byte-for-byte the cleartext the client compacted
	// before sealing (the receipt's body_hash preimage).
	cleartextReqBody := f.openRequest(sealedBody, hdrModel, nonce, ts)

	if f.stream {
		return f.handleStreamInference(cleartextReqBody, clientPub, hdrModel, nonce, ts)
	}

	// Build the canned cleartext response and SEAL its content to the client
	// pubkey under the response AAD; everything else passes through.
	respID := "resp-1"
	cleartextResp := `{"id":"` + respID + `","object":"chat.completion","model":"` + hdrModel +
		`","choices":[{"index":0,"message":{"role":"assistant","content":"` + testRespContent +
		`"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	cleartextRespBytes := mustCompact(f.t, mustParseBody(f.t, cleartextResp))

	sealedResp := mustParseBody(f.t, cleartextResp)
	msg := walkPath(f.t, sealedResp, "choices", 0, "message").(*Object)
	contentAAD := chatResponseAAD(e2eeRequestAlgo, hdrModel, respID, 0, respFieldContent, nonce, ts)
	msg.Set(respFieldContent, String(mustSealPub(f.t, clientPub, []byte(testRespContent), contentAAD)))
	wireBytes := mustCompact(f.t, sealedResp)
	if f.tweaks.tamperWire {
		wireBytes = tamperBytes(wireBytes)
	}

	// Build + sign the receipt binding the request body, the response cleartext,
	// and the wire bytes.
	f.receipt = f.buildReceipt(cleartextReqBody, cleartextRespBytes, wireBytes, hdrModel)

	status := http.StatusOK
	if f.tweaks.status != 0 {
		status = f.tweaks.status
		// On a non-2xx the gateway returns a plaintext error body, not a sealed one.
		return f.response(status, []byte(`{"error":"upstream rejected"}`), ""), nil
	}
	return f.response(status, wireBytes, testReceiptID), nil
}

// handleReceipt returns the receipt JSON captured during the POST.
func (f *fakeDoer) handleReceipt(req *http.Request) (*http.Response, error) {
	f.sawAuthGet = req.Header.Get("Authorization") == "Bearer "+testAPIKey
	if f.receipt == nil {
		f.t.Fatalf("receipt GET before POST built a receipt")
	}
	return f.response(http.StatusOK, f.receipt, ""), nil
}

// headerModel returns the request model. The gateway binds the request model
// from ctx in production; here we read it back off the sealed body so the AAD
// reconstruction matches what sealRequest produced.
func (f *fakeDoer) headerModel(req *http.Request) string {
	// The model is the body's "model" field; read it from the sealed body (it is
	// cleartext — only the message content is sealed).
	body := f.peekBody(req)
	obj := mustParseBody(f.t, string(body))
	v, ok := objectGet(obj, bodyKeyModel)
	if !ok {
		f.t.Fatalf("sealed body has no model field")
	}
	s, ok := v.(String)
	if !ok {
		f.t.Fatalf("sealed body model is not a string")
	}
	return string(s)
}

// peekBody reads and RESTORES the request body so a later read still works.
func (f *fakeDoer) peekBody(req *http.Request) []byte {
	b := f.readBody(req)
	req.Body = io.NopCloser(strings.NewReader(string(b)))
	return b
}

// readBody reads the full request body.
func (f *fakeDoer) readBody(req *http.Request) []byte {
	if req.Body == nil {
		return nil
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		f.t.Fatalf("read request body: %v", err)
	}
	return b
}

// openRequest opens the sealed chat content field in place and returns the
// CompactJSON of the recovered cleartext request body.
func (f *fakeDoer) openRequest(sealedBody []byte, model, nonce string, ts uint64) []byte {
	obj := mustParseBody(f.t, string(sealedBody))
	messagesV, ok := objectGet(obj, bodyKeyMessages)
	if !ok {
		f.t.Fatalf("sealed body has no messages")
	}
	msgs, ok := messagesV.(Array)
	if !ok {
		f.t.Fatalf("messages is not an array")
	}
	for m := range msgs {
		msg, ok := msgs[m].(*Object)
		if !ok {
			continue
		}
		cv, ok := objectGet(msg, bodyKeyContent)
		if !ok {
			continue
		}
		s, ok := cv.(String)
		if !ok {
			continue
		}
		aad := chatRequestAAD(e2eeRequestAlgo, model, m, chatContentString, nonce, ts)
		plaintext, err := open(f.keys.modelPriv, string(s), aad)
		if err != nil {
			f.t.Fatalf("gateway open request content: %v", err)
		}
		msg.Set(bodyKeyContent, String(string(plaintext)))
	}
	return mustCompact(f.t, obj)
}

// buildReceipt assembles the flattened §9 wire receipt binding the three hashes
// and the upstream event, then signs canonicalReceipt with the receipt key
// (or a wrong key, per tweaks), returning the wire JSON.
func (f *fakeDoer) buildReceipt(reqBody, respCleartext, respWire []byte, model string) []byte {
	bodyHash := mustHash(f.t, reqBody)
	if f.tweaks.wrongBodyHash {
		bodyHash = "sha256:" + strings.Repeat("00", 32)
	}
	cleartextHash := mustHash(f.t, respCleartext)
	wireHash := mustHash(f.t, respWire)
	if f.tweaks.wrongWireHash {
		wireHash = "sha256:" + strings.Repeat("00", 32)
	}

	upstreamResult := f.tweaks.upstreamResult
	if upstreamResult == "" {
		upstreamResult = upstreamResultVerified
	}

	events := []json.RawMessage{
		flatEvent(f.t, 0, eventTypeRequestReceived, map[string]string{fieldBodyHash: bodyHash}),
		flatEvent(f.t, 1, eventTypeResponseReturned, map[string]string{
			fieldCleartextHash: cleartextHash,
			fieldWireHash:      wireHash,
		}),
		flatEvent(f.t, 2, eventTypeUpstreamVerified, map[string]string{
			fieldResult:   upstreamResult,
			fieldProvider: "openai",
			fieldModelID:  model,
		}),
	}
	eventLog, err := json.Marshal(events)
	if err != nil {
		f.t.Fatalf("marshal event_log: %v", err)
	}

	receiptObj := map[string]json.RawMessage{
		"api_version":            mustJSON(f.t, SupportedAPIVersion),
		"receipt_id":             mustJSON(f.t, testReceiptID),
		"chat_id":                json.RawMessage("null"),
		"workload_id":            mustJSON(f.t, f.keys.verified.WorkloadID),
		"workload_keyset_digest": mustJSON(f.t, f.keys.verified.WorkloadKeysetDigest),
		"endpoint":               mustJSON(f.t, "/v1/chat/completions"),
		"method":                 mustJSON(f.t, "POST"),
		"served_at":              json.RawMessage("1750000001"),
		"event_log":              eventLog,
		"signature":              mustJSON(f.t, ReceiptSignature{Algo: algoEd25519, KeyID: f.keys.receiptKeyID, ValueHex: ""}),
	}
	unsigned, err := json.Marshal(receiptObj)
	if err != nil {
		f.t.Fatalf("marshal unsigned receipt: %v", err)
	}
	r, err := ParseReceipt(unsigned)
	if err != nil {
		f.t.Fatalf("ParseReceipt(unsigned): %v", err)
	}
	canonical, err := canonicalReceipt(r)
	if err != nil {
		f.t.Fatalf("canonicalReceipt: %v", err)
	}

	signKey := f.keys.receiptPriv
	if f.tweaks.signWithWrongKey {
		_, wrong, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			f.t.Fatalf("generate wrong key: %v", err)
		}
		signKey = wrong
	}
	sig := ed25519.Sign(signKey, canonical)

	receiptObj["signature"] = mustJSON(f.t, ReceiptSignature{
		Algo:     algoEd25519,
		KeyID:    f.keys.receiptKeyID,
		ValueHex: hex.EncodeToString(sig),
	})
	signed, err := json.Marshal(receiptObj)
	if err != nil {
		f.t.Fatalf("marshal signed receipt: %v", err)
	}
	return signed
}

// response builds an *http.Response with the given status, body, and optional
// x-receipt-id header.
func (f *fakeDoer) response(status int, body []byte, receiptID string) *http.Response {
	h := make(http.Header)
	if receiptID != "" {
		h.Set("x-receipt-id", receiptID)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

// parsePub parses an uncompressed-hex secp256k1 public key.
func (f *fakeDoer) parsePub(h string) *secp256k1.PublicKey {
	b, err := hex.DecodeString(h)
	if err != nil {
		f.t.Fatalf("decode client pub hex: %v", err)
	}
	pub, err := secp256k1.ParsePubKey(b)
	if err != nil {
		f.t.Fatalf("parse client pub: %v", err)
	}
	return pub
}

// ----------------------------------------------------------------------------
// Small test helpers local to this file.
// ----------------------------------------------------------------------------

// mustHash returns Sha256HexBytes(b) or fails.
func mustHash(t *testing.T, b []byte) string {
	t.Helper()
	h, err := Sha256HexBytes(b)
	if err != nil {
		t.Fatalf("Sha256HexBytes: %v", err)
	}
	return h
}

// mustSealPub seals plaintext for clientPub under aad (the gateway's response
// seal), or fails. aad is the []byte AAD from chatResponseAAD.
func mustSealPub(t *testing.T, clientPub *secp256k1.PublicKey, plaintext, aad []byte) string {
	t.Helper()
	ct, err := seal(clientPub, plaintext, aad)
	if err != nil {
		t.Fatalf("seal response: %v", err)
	}
	return ct
}

// flipHexByte flips the last hex nibble of a sealed ciphertext hex string so its
// AEAD open fails (the blob stays valid hex of the same length, so only the
// authenticated decrypt breaks — not the surrounding JSON framing).
func flipHexByte(h string) string {
	if h == "" {
		return h
	}
	b := []byte(h)
	last := b[len(b)-1]
	if last == '0' {
		b[len(b)-1] = '1'
	} else {
		b[len(b)-1] = '0'
	}
	return string(b)
}

// tamperBytes flips the last byte of b so a sealed blob no longer opens.
func tamperBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	if len(out) > 0 {
		out[len(out)-1] ^= 0xff
	}
	return out
}

// testRequest builds a minimal chat Request with one user message.
func testRequest() llm.Request {
	return llm.Request{
		Model: llm.Model{
			Provider:  llm.ProviderPhala,
			APIFormat: llm.APIFormatOpenAI,
			BaseURL:   testBaseURL,
			Name:      testClientModel,
		},
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []content.Block{&content.TextBlock{Text: "hi there"}},
			}},
		},
	}
}

// newTestClient builds a Client wired to the fake gateway, the synthetic
// VerifiedReport (via WithAttestFunc), and the fixed clock.
func newTestClient(t *testing.T, doer *fakeDoer, keys gatewayKeys) llm.LLM {
	t.Helper()
	attest := func(_ context.Context, _ string) (*VerifiedReport, error) {
		return keys.verified, nil
	}
	return New(testBaseURL, testAPIKey, Policy{},
		WithHTTPDoer(doer),
		WithNow(testNow),
		WithAttestFunc(attest),
	)
}

// asAttestReason asserts err is a fail-closed *llm.AttestationError with reason.
func asAttestReason(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want *llm.AttestationError(%s)", reason)
	}
	var ae *llm.AttestationError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %T (%v), want *llm.AttestationError", err, err)
	}
	if ae.Reason != reason {
		t.Fatalf("AttestationError.Reason = %q, want %q", ae.Reason, reason)
	}
}

// ----------------------------------------------------------------------------
// Happy path.
// ----------------------------------------------------------------------------

// TestInvokeHappyPath drives the full buffer-until-verified flow against the
// honest fake gateway and asserts Invoke returns the decoded *llm.Response with
// content "hello", nil error — and that the request carried the bearer token.
func TestInvokeHappyPath(t *testing.T) {
	t.Parallel()
	keys := newGatewayKeys(t)
	doer := &fakeDoer{t: t, keys: keys}
	client := newTestClient(t, doer, keys)

	resp, err := client.Invoke(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Invoke() error = %v, want nil", err)
	}
	if resp == nil {
		t.Fatalf("Invoke() response = nil, want non-nil")
	}
	if resp.Message == nil || len(resp.Message.Blocks) == 0 {
		t.Fatalf("response has no message blocks: %+v", resp)
	}
	tb, ok := resp.Message.Blocks[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("first block is %T, want *content.TextBlock", resp.Message.Blocks[0])
	}
	if tb.Text != testRespContent {
		t.Fatalf("response content = %q, want %q", tb.Text, testRespContent)
	}
	if resp.Model != testClientModel {
		t.Fatalf("response model = %q, want %q", resp.Model, testClientModel)
	}
	if !doer.sawAuthPost {
		t.Errorf("POST did not carry the bearer token")
	}
	if !doer.sawAuthGet {
		t.Errorf("receipt GET did not carry the bearer token")
	}
}

// ----------------------------------------------------------------------------
// Failure cases: every one returns (nil, typed error) with no partial response.
// ----------------------------------------------------------------------------

// TestInvokeFailures table-drives the fail-closed paths: each tweak/override
// makes exactly one stage fail, and Invoke must return (nil, <typed error>)
// without a partial response. The error string must never leak the API key.
func TestInvokeFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// tweaks perturbs the gateway; attestErr (when set) replaces the attest
		// func to return that error so attestation failures propagate.
		tweaks    gatewayTweaks
		attestErr error
		// check asserts the returned error is the expected typed error.
		check func(t *testing.T, err error)
	}{
		{
			name:   "wrong request body_hash -> receipt_invalid",
			tweaks: gatewayTweaks{wrongBodyHash: true},
			check:  func(t *testing.T, err error) { asAttestReason(t, err, reasonReceiptInvalid) },
		},
		{
			name:   "tampered sealed response -> e2ee_failed",
			tweaks: gatewayTweaks{tamperWire: true},
			check:  func(t *testing.T, err error) { asAttestReason(t, err, reasonE2EEFailed) },
		},
		{
			name:   "receipt signed by wrong key -> receipt_invalid",
			tweaks: gatewayTweaks{signWithWrongKey: true},
			check:  func(t *testing.T, err error) { asAttestReason(t, err, reasonReceiptInvalid) },
		},
		{
			name:   "upstream result failed -> upstream_unverified",
			tweaks: gatewayTweaks{upstreamResult: "failed"},
			check:  func(t *testing.T, err error) { asAttestReason(t, err, reasonUpstreamUnverified) },
		},
		{
			name:   "non-2xx POST -> transport error",
			tweaks: gatewayTweaks{status: http.StatusBadGateway},
			check: func(t *testing.T, err error) {
				var apiErr *llm.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("error = %T (%v), want *llm.APIError", err, err)
				}
				if apiErr.Status != http.StatusBadGateway {
					t.Errorf("APIError.Status = %d, want %d", apiErr.Status, http.StatusBadGateway)
				}
			},
		},
		{
			name:      "attestation error propagated",
			attestErr: attestErr(reasonStaleReport, nil),
			check:     func(t *testing.T, err error) { asAttestReason(t, err, reasonStaleReport) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			keys := newGatewayKeys(t)
			doer := &fakeDoer{t: t, keys: keys, tweaks: tt.tweaks}

			var client llm.LLM
			if tt.attestErr != nil {
				attest := func(_ context.Context, _ string) (*VerifiedReport, error) {
					return nil, tt.attestErr
				}
				client = New(testBaseURL, testAPIKey, Policy{},
					WithHTTPDoer(doer), WithNow(testNow), WithAttestFunc(attest))
			} else {
				client = newTestClient(t, doer, keys)
			}

			resp, err := client.Invoke(context.Background(), testRequest())
			if resp != nil {
				t.Fatalf("Invoke() response = %+v, want nil on failure", resp)
			}
			if err == nil {
				t.Fatalf("Invoke() error = nil, want a typed error")
			}
			tt.check(t, err)
			if strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("API key leaked into error string: %q", err.Error())
			}
		})
	}
}

// TestInvokeTransportError asserts a transport-level Do failure surfaces as a
// typed *llm.NetworkError with no partial response (and no key leak).
func TestInvokeTransportError(t *testing.T) {
	t.Parallel()
	keys := newGatewayKeys(t)
	attest := func(_ context.Context, _ string) (*VerifiedReport, error) {
		return keys.verified, nil
	}
	failDoer := &errDoer{err: errors.New("dial tcp: connection refused")}
	client := New(testBaseURL, testAPIKey, Policy{},
		WithHTTPDoer(failDoer), WithNow(testNow), WithAttestFunc(attest))

	resp, err := client.Invoke(context.Background(), testRequest())
	if resp != nil {
		t.Fatalf("Invoke() response = %+v, want nil", resp)
	}
	var netErr *llm.NetworkError
	if !errors.As(err, &netErr) {
		t.Fatalf("error = %T (%v), want *llm.NetworkError", err, err)
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("API key leaked into error string: %q", err.Error())
	}
}

// errDoer is an httpDoer that always returns a transport error.
type errDoer struct{ err error }

func (e *errDoer) Do(*http.Request) (*http.Response, error) { return nil, e.err }

// ----------------------------------------------------------------------------
// Task 5.3 — Client.Stream (buffer-until-verified) tests.
//
// Stream buffers the FULL sealed SSE response, opens + verifies (wire_hash +
// body_hash + upstream + signature; cleartext_hash SKIPPED — the client only
// sees the wire bytes, not the raw upstream framing), and ONLY THEN returns a
// reader replaying the already-verified opened deltas. On ANY verification
// failure it returns (nil, typed error) and the caller observes ZERO chunks.
//
// The streaming fake gateway builds a multi-chunk cleartext SSE, seals each
// chunk's delta.content to the client pubkey under chatResponseAAD keyed by that
// chunk's OWN id, hashes the WIRE bytes for wire_hash, and signs a receipt whose
// cleartext_hash the client will NOT check.
// ----------------------------------------------------------------------------

// streamDelta is one cleartext content delta the streaming gateway emits: the
// chunk id (bound into that chunk's response AAD) and the plaintext content.
type streamDelta struct {
	id      string
	content string
}

// testStreamDeltas is the canned two-chunk content stream. Draining Stream's
// reader must yield exactly these two strings, in order, as *content.TextChunk.
var testStreamDeltas = []streamDelta{
	{id: "chunk-0", content: "he"},
	{id: "chunk-1", content: "llo"},
}

// handleStreamInference serves a sealed SSE stream: for each canned delta it
// builds a chat.completion.chunk with delta.content SEALED to the client pubkey
// under that chunk's response AAD, frames it as a data: {json}\n\n event,
// appends data: [DONE]\n\n, hashes the wire bytes for wire_hash, signs a
// receipt, and returns the wire SSE with the x-receipt-id header.
func (f *fakeDoer) handleStreamInference(cleartextReqBody []byte, clientPub *secp256k1.PublicKey, model, nonce string, ts uint64) (*http.Response, error) {
	var wire strings.Builder
	var cleartextWire strings.Builder // the RAW upstream framing (cleartext_hash preimage; client never checks it)
	for i, d := range testStreamDeltas {
		// Cleartext chunk (the raw upstream SSE the gateway would have hashed).
		cleartextChunk := f.streamChunkJSON(model, d.id, d.content)
		cleartextWire.WriteString("data: " + cleartextChunk + "\n\n")

		// Sealed chunk: same shape, delta.content replaced by the sealed blob.
		aad := chatResponseAAD(e2eeRequestAlgo, model, d.id, 0, respFieldContent, nonce, ts)
		sealedContent := mustSealPub(f.t, clientPub, []byte(d.content), aad)
		// Tamper the FIRST sealed delta's ciphertext so its open fails while every
		// OTHER binding (wire_hash included) still matches the bytes the client
		// receives — isolating the E2EE open failure.
		if f.tweaks.tamperWire && i == 0 {
			sealedContent = flipHexByte(sealedContent)
		}
		sealedChunk := f.streamChunkJSON(model, d.id, sealedContent)
		wire.WriteString("data: " + sealedChunk + "\n\n")
	}
	cleartextWire.WriteString("data: [DONE]\n\n")
	wire.WriteString("data: [DONE]\n\n")

	wireBytes := []byte(wire.String())

	// The receipt binds the request body, the RAW upstream cleartext (cleartext_hash,
	// which the client skips for streaming), and the WIRE bytes (wire_hash, checked).
	f.receipt = f.buildReceipt(cleartextReqBody, []byte(cleartextWire.String()), wireBytes, model)

	if f.tweaks.status != 0 {
		return f.response(f.tweaks.status, []byte(`{"error":"upstream rejected"}`), ""), nil
	}
	return f.streamResponse(http.StatusOK, wireBytes, testReceiptID), nil
}

// streamChunkJSON renders one chat.completion.chunk with a single choice whose
// delta.content is contentValue (cleartext for the hash preimage, or the sealed
// blob for the wire event). index is fixed at 0 (matching the AAD choice=0).
func (f *fakeDoer) streamChunkJSON(model, id, contentValue string) string {
	// Build via the ordered body so the compact bytes are stable and the client's
	// order-preserving parse round-trips them.
	chunk := `{"id":"` + id + `","object":"chat.completion.chunk","model":"` + model +
		`","choices":[{"index":0,"delta":{"content":"` + contentValue + `"}}]}`
	return string(mustCompact(f.t, mustParseBody(f.t, chunk)))
}

// streamResponse builds an SSE *http.Response: text/event-stream content type,
// the wire bytes as the body, and the x-receipt-id header.
func (f *fakeDoer) streamResponse(status int, body []byte, receiptID string) *http.Response {
	resp := f.response(status, body, receiptID)
	resp.Header.Set("Content-Type", "text/event-stream")
	return resp
}

// drainStream pulls every chunk from a reader until io.EOF, returning them in
// order. A non-EOF error fails the test. It also asserts Close is a no-op error.
func drainStream(t *testing.T, r *llm.StreamReader[content.Chunk]) []content.Chunk {
	t.Helper()
	var chunks []content.Chunk
	for {
		chunk, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("StreamReader.Next() error = %v, want nil or io.EOF", err)
		}
		chunks = append(chunks, chunk)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("StreamReader.Close() error = %v, want nil", err)
	}
	return chunks
}

// TestStreamHappyPath drives the full buffer-until-verified streaming flow
// against the honest streaming gateway and asserts Stream returns a reader whose
// drained chunks are the opened deltas in order ("he","llo") as *TextChunks, nil
// error — and that the request carried the bearer token.
func TestStreamHappyPath(t *testing.T) {
	t.Parallel()
	keys := newGatewayKeys(t)
	doer := &fakeDoer{t: t, keys: keys, stream: true}
	client := newTestClient(t, doer, keys)

	reader, err := client.Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	if reader == nil {
		t.Fatalf("Stream() reader = nil, want non-nil")
	}

	chunks := drainStream(t, reader)
	if len(chunks) != len(testStreamDeltas) {
		t.Fatalf("drained %d chunks, want %d", len(chunks), len(testStreamDeltas))
	}
	for i, want := range testStreamDeltas {
		tc, ok := chunks[i].(*content.TextChunk)
		if !ok {
			t.Fatalf("chunk[%d] is %T, want *content.TextChunk", i, chunks[i])
		}
		if tc.Text != want.content {
			t.Fatalf("chunk[%d].Text = %q, want %q", i, tc.Text, want.content)
		}
	}
	if !doer.sawAuthPost {
		t.Errorf("POST did not carry the bearer token")
	}
	if !doer.sawAuthGet {
		t.Errorf("receipt GET did not carry the bearer token")
	}
}

// TestStreamReasoningDelta proves a sealed delta.reasoning_content is opened and
// mapped to a *content.ThinkingChunk (the reasoning path), distinct from content.
func TestStreamReasoningDelta(t *testing.T) {
	t.Parallel()
	keys := newGatewayKeys(t)
	doer := &reasoningStreamDoer{fakeDoer: fakeDoer{t: t, keys: keys, stream: true}}
	attest := func(_ context.Context, _ string) (*VerifiedReport, error) { return keys.verified, nil }
	client := New(testBaseURL, testAPIKey, Policy{},
		WithHTTPDoer(doer), WithNow(testNow), WithAttestFunc(attest))

	reader, err := client.Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	chunks := drainStream(t, reader)
	if len(chunks) != 1 {
		t.Fatalf("drained %d chunks, want 1", len(chunks))
	}
	tc, ok := chunks[0].(*content.ThinkingChunk)
	if !ok {
		t.Fatalf("chunk[0] is %T, want *content.ThinkingChunk", chunks[0])
	}
	if tc.Thinking != testReasoning {
		t.Fatalf("chunk[0].Thinking = %q, want %q", tc.Thinking, testReasoning)
	}
}

const testReasoning = "because"

// reasoningStreamDoer overrides the streaming gateway to emit ONE chunk carrying
// a sealed delta.reasoning_content instead of delta.content.
type reasoningStreamDoer struct{ fakeDoer }

func (d *reasoningStreamDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && req.URL.Path == "/v1/chat/completions" {
		return d.handleReasoning(req)
	}
	return d.fakeDoer.Do(req)
}

func (d *reasoningStreamDoer) handleReasoning(req *http.Request) (*http.Response, error) {
	d.sawAuthPost = req.Header.Get("Authorization") == "Bearer "+testAPIKey
	hdrModel := d.headerModel(req)
	nonce := req.Header.Get(hdrE2EENonce)
	ts, err := strconv.ParseUint(req.Header.Get(hdrE2EETimestamp), 10, 64)
	if err != nil {
		d.t.Fatalf("bad timestamp: %v", err)
	}
	clientPub := d.parsePub(req.Header.Get(hdrClientPubKey))
	cleartextReqBody := d.openRequest(d.readBody(req), hdrModel, nonce, ts)

	const chunkID = "chunk-r"
	aad := chatResponseAAD(e2eeRequestAlgo, hdrModel, chunkID, 0, respFieldReasoningContent, nonce, ts)
	sealed := mustSealPub(d.t, clientPub, []byte(testReasoning), aad)
	sealedChunk := string(mustCompact(d.t, mustParseBody(d.t,
		`{"id":"`+chunkID+`","object":"chat.completion.chunk","model":"`+hdrModel+
			`","choices":[{"index":0,"delta":{"reasoning_content":"`+sealed+`"}}]}`)))
	cleartextChunk := string(mustCompact(d.t, mustParseBody(d.t,
		`{"id":"`+chunkID+`","object":"chat.completion.chunk","model":"`+hdrModel+
			`","choices":[{"index":0,"delta":{"reasoning_content":"`+testReasoning+`"}}]}`)))

	wireBytes := []byte("data: " + sealedChunk + "\n\ndata: [DONE]\n\n")
	cleartextWire := []byte("data: " + cleartextChunk + "\n\ndata: [DONE]\n\n")
	d.receipt = d.buildReceipt(cleartextReqBody, cleartextWire, wireBytes, hdrModel)
	return d.streamResponse(http.StatusOK, wireBytes, testReceiptID), nil
}

// TestStreamFailuresZeroChunks table-drives the fail-closed streaming paths: each
// tweak makes exactly one verification stage fail, and Stream must return a NIL
// reader and a typed error — the caller observes ZERO chunks. The error string
// must never leak the API key.
func TestStreamFailuresZeroChunks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tweaks gatewayTweaks
		reason string
	}{
		{
			name:   "wrong wire_hash -> receipt_invalid",
			tweaks: gatewayTweaks{wrongWireHash: true},
			reason: reasonReceiptInvalid,
		},
		{
			name:   "tampered sealed delta (open fails) -> e2ee_failed",
			tweaks: gatewayTweaks{tamperWire: true},
			reason: reasonE2EEFailed,
		},
		{
			name:   "upstream result failed -> upstream_unverified",
			tweaks: gatewayTweaks{upstreamResult: "failed"},
			reason: reasonUpstreamUnverified,
		},
		{
			name:   "receipt signed by wrong key -> receipt_invalid",
			tweaks: gatewayTweaks{signWithWrongKey: true},
			reason: reasonReceiptInvalid,
		},
		{
			name:   "wrong request body_hash -> receipt_invalid",
			tweaks: gatewayTweaks{wrongBodyHash: true},
			reason: reasonReceiptInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			keys := newGatewayKeys(t)
			doer := &fakeDoer{t: t, keys: keys, tweaks: tt.tweaks, stream: true}
			client := newTestClient(t, doer, keys)

			reader, err := client.Stream(context.Background(), testRequest())
			if reader != nil {
				t.Fatalf("Stream() reader = %+v, want nil on failure (zero chunks observable)", reader)
			}
			if err == nil {
				t.Fatalf("Stream() error = nil, want a typed error")
			}
			asAttestReason(t, err, tt.reason)
			if strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("API key leaked into error string: %q", err.Error())
			}
		})
	}
}

// TestStreamNon2xx asserts a non-2xx POST surfaces as a typed *llm.APIError with
// a nil reader (zero chunks), no key leak.
func TestStreamNon2xx(t *testing.T) {
	t.Parallel()
	keys := newGatewayKeys(t)
	doer := &fakeDoer{t: t, keys: keys, tweaks: gatewayTweaks{status: http.StatusBadGateway}, stream: true}
	client := newTestClient(t, doer, keys)

	reader, err := client.Stream(context.Background(), testRequest())
	if reader != nil {
		t.Fatalf("Stream() reader = %+v, want nil", reader)
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T (%v), want *llm.APIError", err, err)
	}
	if apiErr.Status != http.StatusBadGateway {
		t.Errorf("APIError.Status = %d, want %d", apiErr.Status, http.StatusBadGateway)
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("API key leaked into error string: %q", err.Error())
	}
}

// TestStreamTransportError asserts a transport-level Do failure surfaces as a
// typed *llm.NetworkError with a nil reader (no key leak).
func TestStreamTransportError(t *testing.T) {
	t.Parallel()
	keys := newGatewayKeys(t)
	attest := func(_ context.Context, _ string) (*VerifiedReport, error) {
		return keys.verified, nil
	}
	failDoer := &errDoer{err: errors.New("dial tcp: connection refused")}
	client := New(testBaseURL, testAPIKey, Policy{},
		WithHTTPDoer(failDoer), WithNow(testNow), WithAttestFunc(attest))

	reader, err := client.Stream(context.Background(), testRequest())
	if reader != nil {
		t.Fatalf("Stream() reader = %+v, want nil", reader)
	}
	var netErr *llm.NetworkError
	if !errors.As(err, &netErr) {
		t.Fatalf("error = %T (%v), want *llm.NetworkError", err, err)
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("API key leaked into error string: %q", err.Error())
	}
}
