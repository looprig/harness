package serve

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// contentTypeJSON is the media type of every serve response body.
const contentTypeJSON = "application/json"

// errorResponse is serve's top-level HTTP error envelope (SPEC §7a). It is
// deliberately NESTED — a single "error" object rather than flow's flat
// {"error":"..."} — so machine-readable fields (code, retryable) attach under
// "error" without ever colliding with a future top-level field.
type errorResponse struct {
	Error errorBody `json:"error"`
}

// errorBody is the nested error detail: a stable machine-readable Code, a
// generic client-safe Message (NEVER internal cause text), and whether the
// client may retry the identical request.
type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// writeError sets the JSON content type, writes status, and encodes the nested
// error envelope. message MUST be generic and client-safe — callers never pass
// internal cause text here (use writeErrorCause to log a cause). An encode
// failure is logged, not surfaced: the status and headers are already committed.
func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	body := errorResponse{Error: errorBody{Code: code, Message: message, Retryable: retryable}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("serve: encode error response", "code", code, "err", err)
	}
}

// writeErrorCause is writeError with an internal cause: it logs cause via slog
// (the audit trail) and writes the SAME generic message to the client. The
// internal detail — which may embed secrets, connection strings, or PII — is
// confined to the log and never reaches the response body.
func writeErrorCause(w http.ResponseWriter, status int, code, message string, retryable bool, cause error) {
	slog.Error("serve: request failed", "code", code, "status", status, "err", cause)
	writeError(w, status, code, message, retryable)
}

// SessionNotFoundError reports that no session exists for the requested id. It
// maps to HTTP 404. The SessionID is carried for the audit log; the client sees
// only the generic envelope message.
type SessionNotFoundError struct {
	SessionID uuid.UUID
}

func (e SessionNotFoundError) Error() string {
	return "serve: session not found: " + e.SessionID.String()
}

// LoopNotFoundError reports that no loop exists for the requested loop id within
// a session. It maps to HTTP 404. The LoopID is for the audit log only.
type LoopNotFoundError struct {
	LoopID uuid.UUID
}

func (e LoopNotFoundError) Error() string {
	return "serve: loop not found: " + e.LoopID.String()
}

// StoreReadError reports that a read-plane backend operation failed. It maps to
// HTTP 500. Op names the failed operation (e.g. "list", "get") for the log; the
// Cause is wrapped for errors.As/Is and is NEVER written to the response body.
type StoreReadError struct {
	Op    string
	Cause error
}

func (e StoreReadError) Error() string {
	return "serve: store read " + e.Op + ": " + e.Cause.Error()
}

func (e StoreReadError) Unwrap() error { return e.Cause }

// NonPublicEventError reports that an event reached an outward serialization
// boundary without public visibility. Visibility is retained for errors.As-based
// audit handling; Error deliberately omits event type and payload details.
type NonPublicEventError struct {
	Visibility event.EventVisibility
}

func (e *NonPublicEventError) Error() string { return "serve: event is not public" }

// PublicBindWithoutAuthError reports a fail-secure refusal to bind a
// non-loopback address without authentication configured (used by P1-10's bind
// path; defined here so the typed error exists). Its message carries ONLY the
// offending Addr — never any credential or secret.
type PublicBindWithoutAuthError struct {
	Addr string
}

func (e PublicBindWithoutAuthError) Error() string {
	return "serve: refusing to bind public address " + e.Addr + " without authentication"
}

// InvalidAddrError reports that the bind address handed to Server is malformed
// (e.g. missing port) and could not be parsed by net.SplitHostPort. The bind
// fails secure and returns nothing. Cause is the underlying parse error, exposed
// via Unwrap; the message carries ONLY the offending Addr — never a secret.
type InvalidAddrError struct {
	Addr  string
	Cause error
}

func (e InvalidAddrError) Error() string {
	return "serve: invalid listen address " + e.Addr + ": " + e.Cause.Error()
}

func (e InvalidAddrError) Unwrap() error { return e.Cause }

// The gate.GateError → HTTP status mapping lives in handlers_gate.go
// (writeGateError), which translates gate-package error kinds into these envelopes
// via a structural interface — keeping gate-error handling out of this leaf file.
