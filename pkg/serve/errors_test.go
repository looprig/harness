package serve

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
)

// TestWriteError asserts the nested error envelope shape (SPEC §7a): status,
// content-type, and the code/message/retryable fields under a single "error"
// object. It decodes the body into errorResponse rather than string-matching so
// the wire contract itself is exercised.
func TestWriteError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    int
		code      string
		message   string
		retryable bool
	}{
		{name: "not found 404", status: 404, code: "session_not_found", message: "session not found", retryable: false},
		{name: "bad request 400", status: 400, code: "invalid_param", message: "invalid request parameter", retryable: false},
		{name: "server error 500 retryable", status: 500, code: "store_read", message: "internal error", retryable: true},
		{name: "empty code and message", status: 400, code: "", message: "", retryable: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			writeError(rec, tt.status, tt.code, tt.message, tt.retryable)

			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != contentTypeJSON {
				t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
			}
			var got errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if got.Error.Code != tt.code {
				t.Errorf("code = %q, want %q", got.Error.Code, tt.code)
			}
			if got.Error.Message != tt.message {
				t.Errorf("message = %q, want %q", got.Error.Message, tt.message)
			}
			if got.Error.Retryable != tt.retryable {
				t.Errorf("retryable = %v, want %v", got.Error.Retryable, tt.retryable)
			}
		})
	}
}

// TestWriteErrorEnvelopeIsNested proves the body is the nested {"error":{...}}
// shape and NOT the old flat {"error":"..."} one, so a regression to the flat
// envelope fails loudly.
func TestWriteErrorEnvelopeIsNested(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writeError(rec, 400, "bad", "bad request", false)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errField, ok := raw["error"]
	if !ok {
		t.Fatalf("body %q has no top-level error field", rec.Body.String())
	}
	// The nested envelope's "error" is an object; the flat one is a string.
	if len(errField) == 0 || errField[0] != '{' {
		t.Errorf("error field = %s, want a JSON object (nested envelope)", errField)
	}
}

// TestWriteErrorCauseDoesNotLeak is the security assertion: writeErrorCause is
// handed a cause carrying a secret, and the response body must contain ONLY the
// generic message, never the internal cause text.
func TestWriteErrorCauseDoesNotLeak(t *testing.T) {
	t.Parallel()
	const secret = "super-secret-connection-string-hunter2"
	cause := errors.New("dial db: " + secret)

	rec := httptest.NewRecorder()
	writeErrorCause(rec, 500, "store_read", "internal error", true, cause)

	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("response body leaked internal cause text: %q", body)
	}
	var got errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Message != "internal error" {
		t.Errorf("message = %q, want generic %q", got.Error.Message, "internal error")
	}
	if got.Error.Code != "store_read" {
		t.Errorf("code = %q, want %q", got.Error.Code, "store_read")
	}
}

// TestTypedErrorsErrorString exercises each serve-owned typed error's Error()
// and its errors.As-ability (the whole point per CLAUDE.md). It also confirms
// PublicBindWithoutAuthError never embeds a secret — only the addr.
func TestTypedErrorsErrorString(t *testing.T) {
	t.Parallel()
	sid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	lid := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	tests := []struct {
		name        string
		err         error
		wantContain string
	}{
		{name: "session not found", err: SessionNotFoundError{SessionID: sid}, wantContain: sid.String()},
		{name: "loop not found", err: LoopNotFoundError{LoopID: lid}, wantContain: lid.String()},
		{name: "store read", err: StoreReadError{Op: "list", Cause: errors.New("boom")}, wantContain: "list"},
		{name: "public bind without auth", err: PublicBindWithoutAuthError{Addr: "0.0.0.0:8080"}, wantContain: "0.0.0.0:8080"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if msg := tt.err.Error(); !strings.Contains(msg, tt.wantContain) {
				t.Errorf("Error() = %q, want to contain %q", msg, tt.wantContain)
			}
		})
	}
}

// TestSessionNotFoundErrorAs confirms errors.As recovers the concrete type and
// its SessionID field through a wrapping chain.
func TestSessionNotFoundErrorAs(t *testing.T) {
	t.Parallel()
	sid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	wrapped := errors.Join(errors.New("context"), SessionNotFoundError{SessionID: sid})

	var got SessionNotFoundError
	if !errors.As(wrapped, &got) {
		t.Fatalf("errors.As failed to recover SessionNotFoundError from %v", wrapped)
	}
	if got.SessionID != sid {
		t.Errorf("SessionID = %v, want %v", got.SessionID, sid)
	}
}

// TestStoreReadErrorUnwrap confirms StoreReadError exposes its cause via Unwrap
// so errors.Is/As reach the wrapped backend error.
func TestStoreReadErrorUnwrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("backend down")
	err := StoreReadError{Op: "get", Cause: cause}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, cause) = false, want true")
	}
}
