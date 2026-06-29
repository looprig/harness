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

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
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
		Model: llm.ModelSpec{Model: testClientModel},
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
