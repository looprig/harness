//go:build integration

package aci

// Task 6.1 — LIVE Phala confidential-inference integration test.
//
// Unlike client_test.go (which drives an OFFLINE fake gateway via WithAttestFunc /
// WithHTTPDoer), these tests exercise the REAL full-DCAP path end to end against
// the live Phala gateway: attestation over the wire, DCAP quote verification with
// collateral + revocation via the bounded getter, E2EE seal/open, and signed
// receipt verification — nothing is injected that weakens verification. The client
// is built exactly as production wires it:
//
//	New("https://inference.phala.com", key, testPolicy())
//
// Both tests are GATED on PHALA_API_KEY: with no key set they t.Skip, so the
// default `go test` run (and any CI without the secret) stays green. The runner
// exports the key; the test never reads a .env itself and NEVER logs the key.
//
// Per CLAUDE.md these live over the network, so they carry the `integration` build
// tag and live in a *_integration_test.go file — excluded from the normal build
// and the default `go test ./...`.
//
// Run: PHALA_API_KEY=… go test -tags integration -race ./pkg/llm/aci -run Live -v

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
)

const (
	// livePhalaBaseURL is the production gateway origin the client attests and
	// POSTs against.
	livePhalaBaseURL = "https://inference.phala.com"
	// liveModel is the model to attest + invoke. z-ai/glm-5.2 is the pinned live
	// target for Task 6.1.
	liveModel = "z-ai/glm-5.2"
	// livePrompt is the single-line user prompt. Asking for one word keeps the
	// verified round-trip cheap while still producing non-empty content.
	livePrompt = "Reply with exactly one word."
	// liveTimeout bounds the whole exchange. Live attestation + DCAP collateral +
	// revocation fetch + inference can be slow, so the budget is generous.
	liveTimeout = 120 * time.Second
)

// liveKey reads PHALA_API_KEY (exported by the runner) and skips the test when it
// is absent, so the default suite stays green without the secret. The key is never
// logged.
func liveKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("PHALA_API_KEY")
	if key == "" {
		t.Skip("PHALA_API_KEY not set")
	}
	return key
}

// liveRequest builds the provider-neutral chat request: the secret-free Phala model
// descriptor (real base URL + model; the API key is supplied to New, not the Model)
// and a single one-line user text message.
func liveRequest() llm.Request {
	return llm.Request{
		Model: llm.Model{
			Provider:  llm.ProviderPhala,
			APIFormat: llm.APIFormatOpenAI,
			BaseURL:   livePhalaBaseURL,
			Name:      liveModel,
		},
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []content.Block{&content.TextBlock{Text: livePrompt}},
			}},
		},
	}
}

// TestLiveInvoke runs the real buffer-until-verified Invoke flow against the live
// Phala gateway with FULL DCAP (collateral + revocation) and asserts the verified
// assistant text is non-empty. The client is the production wiring — no seam is
// overridden — so a pass means the whole attestation + E2EE + receipt chain held
// over the wire.
func TestLiveInvoke(t *testing.T) {
	key := liveKey(t)

	client, err := New(livePhalaBaseURL, key, testPolicy())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	resp, err := client.Invoke(ctx, liveRequest())
	if err != nil {
		t.Fatalf("Invoke() error = %v, want nil", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatalf("Invoke() returned resp=%+v, want non-nil resp with non-nil Message", resp)
	}

	text := assistantText(resp.Message)
	if strings.TrimSpace(text) == "" {
		t.Fatalf("verified assistant text is empty; want non-empty (blocks=%+v)", resp.Message.Blocks)
	}
	// The text is verified (E2EE-opened + receipt-checked), so it is safe to log.
	t.Logf("verified Invoke output: %q", text)
}

// TestLiveStream runs the real buffer-until-verified Stream flow against the live
// Phala gateway with FULL DCAP and asserts the concatenation of the verified text
// deltas is non-empty. The reader only ever exists after verification passed, so a
// non-empty drain means every delta was E2EE-opened and receipt-bound.
func TestLiveStream(t *testing.T) {
	key := liveKey(t)

	client, err := New(livePhalaBaseURL, key, testPolicy())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	sr, err := client.Stream(ctx, liveRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	if sr == nil {
		t.Fatalf("Stream() reader = nil, want non-nil")
	}
	defer sr.Close()

	var sb strings.Builder
	for {
		chunk, err := sr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("StreamReader.Next() error = %v, want nil or io.EOF", err)
		}
		if tc, ok := chunk.(*content.TextChunk); ok {
			sb.WriteString(tc.Text)
		}
	}

	got := sb.String()
	if strings.TrimSpace(got) == "" {
		t.Fatalf("concatenated verified stream text is empty; want non-empty")
	}
	// Verified content — safe to log.
	t.Logf("verified Stream output: %q", got)
}

// assistantText concatenates the text of every *content.TextBlock in the assistant
// message, in order, ignoring non-text blocks. It is the read side of the
// content.AIMessage shape: the verified response's message blocks.
func assistantText(msg *content.AIMessage) string {
	var sb strings.Builder
	for _, b := range msg.Blocks {
		if tb, ok := b.(*content.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}
