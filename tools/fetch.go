package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inventivepotter/urvi/internal/tool"
)

// fetch.go implements the Fetch tool: it performs a single bounded HTTP GET or
// POST against a model-supplied URL using an INJECTED *http.Client and returns
// the status, a short header summary, and a size-capped body as a tool result
// (design §4b, row Fetch).
//
// LEAST PRIVILEGE: Fetch takes only an *http.Client — NO filesystem access at
// all. A web tool literally cannot reach the workspace root.
//
// The injected client is the manifest's responsibility and carries the security
// posture mandated by CLAUDE.md: an explicit Timeout and a Transport whose
// TLSClientConfig.MinVersion is TLS 1.2 with InsecureSkipVerify never set. This
// tool MUST NOT construct a client and MUST NOT set InsecureSkipVerify; it only
// USES the client it is handed. Enforcement of the TLS floor is verified in the
// T15 integration test (a live request), not here — httptest.NewServer is plain
// HTTP, sufficient for behavior tests.
//
// Every request runs under a bounded context (context.WithTimeout) so a slow or
// hung server can never block the agent indefinitely (CLAUDE.md: no unbounded
// I/O).
//
// SECURITY — log events, not secrets: AuditSummary (and every error string)
// carries ONLY the method and host. The path, query, request headers, and body
// are NEVER included, so an auth token in a header, a query secret, or a body
// credential cannot leak into the audit trail.
//
// Failure model: a parse error, an invalid method, a bad URL, a transport error,
// or a timeout is a tool-result error STRING — InvokableRun never returns a Go
// error. A non-2xx HTTP status is a NORMAL result (the model reads the status).

// fetchToolName is the EXACT tool name classifyTool keys on for the network
// class — it MUST equal "Fetch" (check.go's toolFetch).
const fetchToolName = toolFetch

// maxFetchBodyBytes caps the response body read so a large or infinite response
// cannot exhaust memory or flood the model context. Bytes beyond this are
// dropped and a truncation notice is appended (design §4b: 64 KiB cap).
const maxFetchBodyBytes = 64 * 1024 // 64 KiB

// maxFetchTimeout is the hard ceiling on a single Fetch's wall-clock runtime; a
// caller-supplied timeout is clamped into (0, 60s] (design §4b: timeout ≤60s).
const maxFetchTimeout = 60 * time.Second

// defaultFetchTimeout is used when the caller omits (or supplies a non-positive)
// timeout. It is bounded well under the ceiling.
const defaultFetchTimeout = 30 * time.Second

// maxFetchHeaderSummaryLines caps how many response headers are summarized so a
// server returning hundreds of headers cannot bloat the result.
const maxFetchHeaderSummaryLines = 16

// HTTP methods Fetch permits. A coding agent needs only read (GET) and a simple
// write (POST); other verbs are rejected to keep the network surface small.
const (
	methodGET  = http.MethodGet
	methodPOST = http.MethodPost
)

// allowedFetchSchemes are the only URL schemes Fetch will request. file://,
// ftp://, gopher://, etc. are rejected so the tool cannot be coerced into
// reading local files or non-HTTP resources.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

const fetchSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "The absolute http:// or https:// URL to fetch."},
    "method": {"type": "string", "enum": ["GET", "POST"], "description": "HTTP method (GET or POST)."},
    "headers": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Optional request headers."},
    "body": {"type": "string", "description": "Optional request body (POST)."},
    "timeout": {"type": "integer", "minimum": 1, "maximum": 60, "description": "Maximum runtime in seconds (optional; default 30, hard cap 60)."}
  },
  "required": ["url", "method"]
}`

const fetchDesc = "Fetch a URL over HTTP(S) with GET or POST. Returns the status code, a short header summary, and the response body (capped at 64 KiB). Optional request headers and body are supported. Runtime is bounded (default 30s, max 60s). Has no filesystem access. Requires approval before each request."

// fetchArgs is the typed decode of Fetch's untrusted argsJSON.
type fetchArgs struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Timeout int               `json:"timeout"`
}

// Fetch performs a single bounded HTTP request using an injected client. It has
// no filesystem access (least privilege).
type Fetch struct {
	client *http.Client
}

// NewFetch constructs a Fetch tool bound to an injected *http.Client. The client
// is expected to carry an explicit Timeout and a TLS 1.2+ Transport (wired at the
// composition root / manifest). Fetch never constructs a client itself.
func NewFetch(client *http.Client) *Fetch {
	return &Fetch{client: client}
}

// Info returns Fetch's self-description. Name MUST equal "Fetch".
func (f *Fetch) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   fetchToolName,
		Desc:   fetchDesc,
		Schema: json.RawMessage(fetchSchema),
	}, nil
}

// AuditSummary returns "<METHOD> <host>" ONLY — never the path, query, headers,
// or body (CLAUDE.md: log events, not secrets). An unparseable args document or
// an unparseable URL yields a generic, secret-free summary.
func (f *Fetch) AuditSummary(argsJSON string) string {
	var a fetchArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "Fetch (unparsable args)"
	}
	host := hostOnly(a.URL)
	if host == "" {
		return "Fetch (no host)"
	}
	method := normalizeMethod(a.Method)
	if method == "" {
		method = "?"
	}
	return method + " " + host
}

// BuildRequest derives the approval prompt: the method + full URL (the user
// approves the exact target). An unparseable args document, a missing/invalid
// URL, or an unsupported method is a typed error so the runner treats the call as
// invalid (and never prompts with a malformed request).
func (f *Fetch) BuildRequest(argsJSON string, _ tool.PreparedArtifact) (tool.PermissionRequest, error) {
	a, method, _, err := parseFetchArgs(argsJSON)
	if err != nil {
		return nil, err
	}
	return tool.FetchRequest{Method: method, URL: a.URL}, nil
}

// InvokableRun performs the request and returns status + header summary + capped
// body as a tool result. Every failure mode (parse, validation, transport,
// timeout) maps to a tool-result error STRING; it never returns a Go error and
// never leaks the path/query/headers/body into an error.
func (f *Fetch) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	a, method, parsedURL, err := parseFetchArgs(argsJSON)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}

	// Bound the request runtime: clamp the caller timeout into (0, 60s].
	timeout := clampFetchTimeout(a.Timeout)
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if method == methodPOST && a.Body != "" {
		bodyReader = strings.NewReader(a.Body)
	}

	req, err := http.NewRequestWithContext(ctx2, method, parsedURL.String(), bodyReader)
	if err != nil {
		// Do not echo the raw url (it may carry a query secret) — host only.
		return tool.TextResult("error: could not build request for " + method + " " + parsedURL.Hostname()), nil
	}
	for k, v := range a.Headers {
		req.Header.Set(k, v)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		// Redact: report method + host only, never the full url or the transport
		// error string (which can embed the full url with its query).
		return tool.TextResult("error: request failed: " + method + " " + parsedURL.Hostname()), nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, truncated := readCappedBody(resp.Body)
	return tool.TextResult(formatFetchResult(resp, body, truncated)), nil
}

// parseFetchArgs decodes + validates the args into (args, normalized method,
// parsed URL). It is shared by BuildRequest, InvokableRun, and (indirectly) the
// audit summary so validation is identical everywhere. Returns a typed
// *fetchError on any failure; the error message carries the host only (never the
// path/query) so it is safe to surface.
func parseFetchArgs(argsJSON string) (fetchArgs, string, *url.URL, error) {
	var a fetchArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return fetchArgs{}, "", nil, &fetchError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	method := normalizeMethod(a.Method)
	if method != methodGET && method != methodPOST {
		return fetchArgs{}, "", nil, &fetchError{reason: "method must be GET or POST"}
	}
	if a.URL == "" {
		return fetchArgs{}, "", nil, &fetchError{reason: "a non-empty 'url' is required"}
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return fetchArgs{}, "", nil, &fetchError{reason: "url is not a valid URL", cause: err}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != schemeHTTP && scheme != schemeHTTPS {
		return fetchArgs{}, "", nil, &fetchError{reason: "url scheme must be http or https"}
	}
	if u.Hostname() == "" {
		return fetchArgs{}, "", nil, &fetchError{reason: "url must have a host"}
	}
	return a, method, u, nil
}

// normalizeMethod upper-cases and trims a caller-supplied method so "get" and
// " GET " both normalize to "GET".
func normalizeMethod(m string) string {
	return strings.ToUpper(strings.TrimSpace(m))
}

// hostOnly parses a URL string and returns its lower-cased hostname (no port,
// no path, no query). It returns "" when the string does not parse or has no
// host — the audit summary then degrades to a generic, secret-free form.
func hostOnly(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// clampFetchTimeout maps a caller-supplied timeout (seconds) into a bounded
// time.Duration: ≤0 → defaultFetchTimeout; otherwise min(timeout, 60s).
func clampFetchTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultFetchTimeout
	}
	d := time.Duration(seconds) * time.Second
	if d > maxFetchTimeout {
		return maxFetchTimeout
	}
	return d
}

// readCappedBody reads at most maxFetchBodyBytes from r via an io.LimitReader. It
// returns the bytes read and whether the body was truncated (i.e. there were
// MORE than the cap). A read error returns whatever was read so far (best
// effort); the caller still reports the status.
func readCappedBody(r io.Reader) (string, bool) {
	// Read one extra byte so we can detect "there was more than the cap".
	limited := io.LimitReader(r, maxFetchBodyBytes+1)
	b, _ := io.ReadAll(limited)
	if len(b) > maxFetchBodyBytes {
		return string(b[:maxFetchBodyBytes]), true
	}
	return string(b), false
}

// formatFetchResult renders the status line, a short header summary, and the
// (capped) body into the tool-result text. A truncation notice is appended when
// the body was capped.
func formatFetchResult(resp *http.Response, body string, truncated bool) string {
	var sb strings.Builder
	sb.WriteString("HTTP ")
	sb.WriteString(strconv.Itoa(resp.StatusCode))
	sb.WriteString(" ")
	sb.WriteString(http.StatusText(resp.StatusCode))
	sb.WriteString("\n")
	sb.WriteString(headerSummary(resp.Header))
	sb.WriteString("\n")
	sb.WriteString(body)
	if truncated {
		if body != "" && body[len(body)-1] != '\n' {
			sb.WriteString("\n")
		}
		sb.WriteString("[body truncated at ")
		sb.WriteString(strconv.Itoa(maxFetchBodyBytes))
		sb.WriteString(" bytes]")
	}
	return sb.String()
}

// headerSummary renders a deterministic, capped subset of RESPONSE headers (these
// are the server's, not the request's, so no client secret is exposed). Header
// names are sorted for stable output and capped so a hostile server cannot bloat
// the result.
func headerSummary(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxFetchHeaderSummaryLines {
		keys = keys[:maxFetchHeaderSummaryLines]
	}
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(strings.Join(h.Values(k), ", "))
		sb.WriteString("\n")
	}
	return sb.String()
}

// fetchError is the typed failure for Fetch arg parsing/validation. Its message
// carries only a non-secret reason (and, where relevant, the HOST — never the
// path, query, headers, or body). InvokableRun maps every failure to a
// tool-result string; BuildRequest returns it so the runner treats the call as
// invalid.
type fetchError struct {
	reason string
	cause  error
}

func (e *fetchError) Error() string { return e.reason }

func (e *fetchError) Unwrap() error { return e.cause }

// compile-time assertions: Fetch is an InvokableTool, a PermissionPrompter (Ask),
// and Auditable. It is NOT a WriteTarget (it is a network tool, not a path-write
// tool).
var (
	_ tool.InvokableTool      = (*Fetch)(nil)
	_ tool.PermissionPrompter = (*Fetch)(nil)
	_ tool.Auditable          = (*Fetch)(nil)
)
