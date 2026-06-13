package phala

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

// newCaptureStream wraps an http.Response body in a teeReadCloser that
// captures every byte into captured, then constructs a StreamReader whose
// Close performs per-response receipt verification (Shapes A/B) or is a
// no-op (Shape C / chutes). The caller MUST call Close when done.
//
// For Shape C (chutes) sessions the receipt path is skipped entirely:
// RedPill exposes no per-response receipt for those models. The attestation
// run in Stream() still proves chutes-class TEE instances exist for the model;
// there is no cryptographic chain from the chat response back to those
// instances (the TLS terminates at the RedPill edge).
func newCaptureStream(
	body io.ReadCloser,
	client *Client,
	session *attestedSession,
	model string,
	requestHashHex string,
) *llm.StreamReader[content.Chunk] {
	captured := &bytes.Buffer{}
	teeRC := &teeReadCloser{r: io.TeeReader(body, captured), c: body}

	inner := openaiapi.NewStream(teeRC)

	closer := func() error {
		// inner.Close() already called teeRC.Close (body.Close) via the
		// StreamReader's closer captured at NewStream time. We only need
		// receipt verification here.
		if session.algo == "chutes" {
			return nil
		}
		raw := captured.Bytes()
		chatID, err := extractChatID(raw)
		if err != nil {
			client.evict(model)
			return err
		}
		responseHash := sha256Hex(raw)

		receiptBody, err := fetchReceipt(context.Background(), client.http, client.apiBase, client.apiKey, chatID, model)
		if err != nil {
			client.evict(model)
			return err
		}
		if err := verifyReceipt(receiptBody, session.signingAddr, model, requestHashHex, responseHash); err != nil {
			client.evict(model)
			return err
		}
		return nil
	}

	return llm.NewStreamReader(inner.Next, closer)
}

// teeReadCloser glues an io.TeeReader to its underlying ReadCloser so the
// stream's Close shuts down the network connection.
type teeReadCloser struct {
	r io.Reader
	c io.Closer
}

func (t *teeReadCloser) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *teeReadCloser) Close() error               { return t.c.Close() }

// sha256Hex returns the lowercase hex of SHA-256(b).
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// extractChatID scans the captured SSE response for the first non-DONE chunk
// with a non-empty id field. Tolerant: malformed chunks are skipped rather
// than failing the whole receipt path. An absent id is a hard fail — without
// it we cannot fetch the receipt at all.
func extractChatID(raw []byte) (string, error) {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var chunk struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue // tolerant: skip malformed chunks
		}
		if chunk.ID != "" {
			return chunk.ID, nil
		}
	}
	return "", &llm.AttestationError{
		Reason: string(ReasonReceiptInvalid),
		Err:    errors.New("no chat id found in stream"),
	}
}
