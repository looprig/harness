package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/looprig/harness/pkg/llm"
)

// AWS Signature Version 4 constants (see the AWS "aws-sig-v4-test-suite" reference).
const (
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	sigV4Terminator = "aws4_request"
	sigV4KeyPrefix  = "AWS4"
	// amzDateFormat is the ISO8601 basic timestamp AWS requires (yyyyMMddTHHmmssZ).
	amzDateFormat = "20060102T150405Z"
	// shortDateFormat is the credential-scope date (yyyyMMdd).
	shortDateFormat = "20060102"
	headerAmzDate   = "X-Amz-Date"
	// #nosec G101 -- HTTP header NAME, not a credential; the value is set from creds at runtime.
	headerAmzToken = "X-Amz-Security-Token"
	headerAuthz    = "Authorization"
	headerHost     = "host"
	// serviceS3 is the one service whose canonical URI is single-encoded. Every
	// other service requires the path to be URI-encoded a second time (see
	// canonicalURI).
	serviceS3 = "s3"
)

// MissingSigV4CredentialsError is returned by a SigV4 Authenticator when the AccessKeyID or
// SecretAccessKey is empty. Fail-closed: the request is neither signed nor mutated. Carries only
// the non-secret region/service for diagnostics.
type MissingSigV4CredentialsError struct {
	Region  string
	Service string
}

func (e *MissingSigV4CredentialsError) Error() string {
	return fmt.Sprintf("auth: SigV4 requires non-empty AccessKeyID and SecretAccessKey (region %q, service %q)", e.Region, e.Service)
}

// BodyReadError is returned when the request body cannot be read to compute the payload hash.
// Fail-closed: the request is not signed. Err is the underlying I/O cause.
type BodyReadError struct {
	Err error
}

func (e *BodyReadError) Error() string {
	return "auth: SigV4 could not read request body: " + e.Err.Error()
}

func (e *BodyReadError) Unwrap() error { return e.Err }

// sigV4Auth signs outbound requests with AWS Signature Version 4 using only the standard library.
// now is injected so signing is deterministic in tests; production uses time.Now.
type sigV4Auth struct {
	creds   SigV4Credentials
	region  string
	service string
	now     func() time.Time
}

var _ llm.Authenticator = (*sigV4Auth)(nil)

// SigV4 returns an Authenticator that signs requests with AWS Signature Version 4 for the given
// region and service (e.g. region "us-east-1", service "bedrock"). The signature is computed at
// sign time from the current UTC clock. Empty AccessKeyID/SecretAccessKey make Authorize fail
// closed with *MissingSigV4CredentialsError.
func SigV4(creds SigV4Credentials, region, service string) llm.Authenticator {
	return newSigV4(creds, region, service, time.Now)
}

// newSigV4 is the clock-injectable constructor. Exposed to tests within the package so a pinned
// timestamp yields a deterministic, vector-comparable signature; SigV4 wires time.Now.
func newSigV4(creds SigV4Credentials, region, service string, now func() time.Time) *sigV4Auth {
	return &sigV4Auth{creds: creds, region: region, service: service, now: now}
}

// Authorize signs r in place, setting X-Amz-Date, Authorization, and (when a session token is
// present) X-Amz-Security-Token. It reads and restores r.Body so the request stays sendable.
func (s *sigV4Auth) Authorize(_ context.Context, r *http.Request) error {
	if s.creds.AccessKeyID == "" || s.creds.SecretAccessKey == "" {
		return &MissingSigV4CredentialsError{Region: s.region, Service: s.service}
	}

	t := s.now().UTC()
	amzDate := t.Format(amzDateFormat)
	shortDate := t.Format(shortDateFormat)

	// Headers signed alongside host: date always, security token when present. Setting them
	// before canonicalization means they are covered by the signature.
	r.Header.Set(headerAmzDate, amzDate)
	if s.creds.SessionToken != "" {
		r.Header.Set(headerAmzToken, s.creds.SessionToken)
	}

	payloadHash, err := hashAndRestoreBody(r)
	if err != nil {
		return err
	}

	canonicalHeaders, signedHeaders := canonicalizeHeaders(r)
	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI(r, s.service),
		canonicalQueryString(r.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := shortDate + "/" + s.region + "/" + s.service + "/" + sigV4Terminator
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := s.signingKey(shortDate)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	r.Header.Set(headerAuthz, sigV4Algorithm+
		" Credential="+s.creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
	return nil
}

// signingKey derives the SigV4 signing key via the HMAC chain
// AWS4+secret -> date -> region -> service -> aws4_request.
func (s *sigV4Auth) signingKey(shortDate string) []byte {
	kDate := hmacSHA256([]byte(sigV4KeyPrefix+s.creds.SecretAccessKey), []byte(shortDate))
	kRegion := hmacSHA256(kDate, []byte(s.region))
	kService := hmacSHA256(kRegion, []byte(s.service))
	return hmacSHA256(kService, []byte(sigV4Terminator))
}

// canonicalURI returns the SigV4 canonical URI path, defaulting to "/" for an
// empty path. For every service EXCEPT s3, AWS requires the already-escaped path
// to be URI-encoded a SECOND time (RFC-3986 unreserved rules, preserving the "/"
// segment separators). net/url leaves ":" unescaped inside a path segment, so a
// Bedrock model path like /model/anthropic.claude-...-v2:0/invoke keeps a literal
// ":" in EscapedPath(); the second pass turns it into "%3A", which is what the
// AWS server derives when it canonicalizes the received path — without it the
// signature does not match and Bedrock returns 403. s3 canonical URIs are
// single-encoded (the object key is signed verbatim), so the second pass is
// skipped there. Keying this on the service inside the signer keeps callers from
// having to split-brain the canonicalization.
func canonicalURI(r *http.Request, service string) string {
	p := r.URL.EscapedPath()
	if p == "" {
		return "/"
	}
	if service == serviceS3 {
		return p
	}
	return awsEncode(p, true)
}

// canonicalQueryString builds the sorted, AWS-URI-encoded query string. Keys and values are
// sorted so the output is deterministic regardless of caller ordering.
func canonicalQueryString(q map[string][]string) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(q))
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		ek := awsURIEncode(k)
		for _, v := range vals {
			pairs = append(pairs, ek+"="+awsURIEncode(v))
		}
	}
	return strings.Join(pairs, "&")
}

// canonicalizeHeaders lowercases, trims, sorts, and joins the headers to be signed. Host is taken
// from the request target (r.Host, else r.URL.Host) so it always reflects the wire value. Returns
// the canonical-headers block (trailing newline) and the semicolon-joined signed-headers list.
func canonicalizeHeaders(r *http.Request) (string, string) {
	values := make(map[string]string, len(r.Header)+1)
	for name, vs := range r.Header {
		lower := strings.ToLower(name)
		if lower == headerHost {
			continue // host is derived explicitly below
		}
		values[lower] = trimHeaderValue(strings.Join(vs, ","))
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	values[headerHost] = trimHeaderValue(host)

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(values[name])
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// trimHeaderValue trims surrounding whitespace and collapses internal runs of whitespace to a
// single space, per the SigV4 canonicalization rules for unquoted header values.
func trimHeaderValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// hashAndRestoreBody hashes the request body for the SigV4 payload hash and restores Body (and
// GetBody) so the request remains sendable. A nil body hashes as the empty payload.
func hashAndRestoreBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return sha256Hex(nil), nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return "", &BodyReadError{Err: err}
	}
	_ = r.Body.Close()

	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return sha256Hex(data), nil
}

// awsEncode percent-encodes s per RFC 3986, leaving the unreserved set
// (A-Za-z0-9-_.~) untouched and encoding every other byte as uppercase %XX.
// keepSlash preserves "/" (the path-segment separator) for canonical-URI path
// encoding; when false, "/" is encoded too, as a query component requires. It
// operates byte-wise, so applying it to an already-escaped string double-encodes
// each existing "%XX" octet's "%" to "%25" — exactly the non-s3 canonical-URI
// second pass.
func awsEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

// awsURIEncode percent-encodes a query component per RFC 3986, encoding every
// byte outside the unreserved set (including "/"). Canonical-URI path encoding
// preserves "/" via awsEncode(s, true).
func awsURIEncode(s string) string { return awsEncode(s, false) }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// String redacts the credential so %v, %+v, and %s never expose the secret or session token.
func (SigV4Credentials) String() string { return "auth.SigV4Credentials(REDACTED)" }

// LogValue redacts the credential for slog structured logging.
func (SigV4Credentials) LogValue() slog.Value { return slog.StringValue("REDACTED") }

// GoString redacts the credential under the %#v verb (fmt.GoStringer), so even Go-syntax debug
// formatting never exposes the secret/session values.
func (SigV4Credentials) GoString() string { return "auth.SigV4Credentials(REDACTED)" }

var (
	_ fmt.Stringer   = SigV4Credentials{}
	_ fmt.GoStringer = SigV4Credentials{}
	_ slog.LogValuer = SigV4Credentials{}
)
