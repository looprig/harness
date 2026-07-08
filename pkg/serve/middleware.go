package serve

import "net/http"

// codeUnauthorized is the stable, machine-readable error code for an auth failure.
const codeUnauthorized = "unauthorized"

// msgUnauthorized is the generic, client-safe 401 message. It NEVER embeds the
// authenticator's error text — that cause may carry secrets or PII and is confined
// to the audit log (writeErrorCause). See authMiddleware.
const msgUnauthorized = "authentication required"

// wrap composes the request-path middleware around h: authentication first, then
// the body cap. Auth is outermost so an unauthenticated request is rejected at the
// auth seam WITHOUT its body ever being read (fail secure; spend no read work on a
// caller we have not authenticated).
func (c *config) wrap(h http.Handler) http.Handler {
	return c.authMiddleware(c.bodyCapMiddleware(h))
}

// authMiddleware gates next with the configured authenticator. With no
// authenticator (the default) it passes through unchanged. With one, a non-nil
// error denies the request with a 401 nested envelope and next is NOT called
// (authenticate before act). The authenticator's error is recorded as the audit
// cause (writeErrorCause) and never written to the response body — a generic
// message only — so an auth implementation cannot leak its internals on the wire.
func (c *config) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.authn != nil {
			if err := c.authn(r); err != nil {
				writeErrorCause(w, http.StatusUnauthorized, codeUnauthorized, msgUnauthorized, false, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// bodyCapMiddleware wraps r.Body with http.MaxBytesReader so a body exceeding
// c.maxBodyBytes fails when a handler reads it. The cap is enforced lazily, on
// read: a handler decoding past the limit receives an error (the intended
// mechanism). It bounds per-request memory against an oversized-body DoS.
func (c *config) bodyCapMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, c.maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}
