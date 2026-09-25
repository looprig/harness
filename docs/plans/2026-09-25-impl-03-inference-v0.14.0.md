# inference v0.14.0: inference call with no execution ceiling — implementation plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

> **Commits:** conventional messages with **no Co-Authored-By trailer** (master plan rule; it overrides any tool default). Never commit a `go.work` and never add a `replace` directive.

**Goal:** Let a caller opt a single inference call out of `transport`'s execution
ceilings: Invoke's whole-request timeout (default 5 minutes) and Stream's 60-second
response-header timeout. The opt-in is `transport.WithoutExecutionTimeout(ctx)`. Every
other safeguard stays, and unmarked calls behave exactly as in v0.13.0. Oxy can then
delete its patched `third_party/looprig-inference`.

**Architecture:** An unexported context key marks the call. `transport.Client` builds a
third HTTP client, `hcUnlimited` (`newInvokeHTTPClient(0, roots)`: no `Timeout`, no
`ResponseHeaderTimeout`, same dial/TLS/idle budget, TLS 1.2 floor and redirect refusal).
The constructor and `WithTLSRootCAs` build it next to `hcInvoke`/`hcStream`, and
`applyRoundTripper` wires it. `executionHTTPClient(ctx, normal)` picks it for a marked
context at both `Do` sites (Invoke and Stream). Nothing is sent on the wire.

**Tech Stack:** Go 1.26.8, `net/http`, `net/http/httptest`. Verify with
`GOWORK=off GOTOOLCHAIN=go1.26.8`.

**Binding design:** `harness/docs/plans/2026-09-25-unbounded-execution-and-opaque-tool-input-design.md`,
item 4 (approved 2026-09-25). **Master plan:** `2026-09-25-impl-00-master-plan.md`, row 03.
This plan has no Looprig prerequisite. It can run in parallel with plans 01, 02 and 04.
Plan 05 (harness v0.41.0) pins its tag.

**Reference implementation:** Oxy branch `feat/looprig-current`,
`third_party/looprig-inference` ("Patch set 6" in its `OXY-PATCHES.md`). Diffed against
`git -C inference archive v0.13.0`, the patch is exactly:
- the new `transport/execution_timeout.go`;
- edits in `transport/client.go`: the new field, the constructor, `WithTLSRootCAs`,
  `applyRoundTripper`, both `Do` sites, and the `executionHTTPClient` helper;
- Oxy's one white-box test, `transport/execution_timeout_test.go`.

This plan keeps the production code as it is. It moves the helper into
`execution_timeout.go` so the feature lives in one file, and it replaces Oxy's single
test with behavioural tests over `httptest` plus white-box guards. **Every test and code
block below was run against a scratch copy of inference `main` (`c469e46`) and passes
under `-race`. The two guards were mutation-checked.**

**Out of scope (YAGNI):**
- `WithInvokeTimeout(0)` still means "no-op", and no new `Option` is added. The marker is
  per call so provider wrappers can pass it through unchanged.
- The ceiling constants stay `const`. There are no test hooks.
- No `CHANGELOG.md` is created. inference has none; its release notes are the annotated
  tag message (see Task 4).

**Consumers — llm needs no change.** Every `llm` provider built on `transport.New` /
`transport.NewWithAuth` passes the caller's context to `Invoke`/`Stream`, so the marker
reaches the `Do` site as-is. This covers `providers/internal/compat` (most gateways),
`openai`, `anthropic`, `azure`, `xai`, `openrouter`, `google-vertex`, `sap-ai-core` and
`auto`. The `inference/retry` decorator also forwards ctx.

**Non-claim, for the release notes:** `llm`'s `gemini` and `bedrock` providers own
their own `http.Client`, with a 60s `ResponseHeaderTimeout`, and never reach
`transport.Client`. The marker has **no effect** on them. `chutes` uses a bare
`&http.Client{}` and never had a ceiling. Extending the marker to them would be an `llm`
change and is not part of this plan.

---

## Task 0: Preflight

**Step 1: Confirm a clean `main` at or past v0.13.0**

```sh
git -C /Users/ipotter/code/looprig/inference status --short
git -C /Users/ipotter/code/looprig/inference switch main
git -C /Users/ipotter/code/looprig/inference describe --tags
```

Expected: no status output, and `v0.13.0-1-gc469e46` (one README-only commit past the
tag) or later. If `status` shows changes you did not make, stop and ask.

**Step 2: Baseline is green**

```sh
cd /Users/ipotter/code/looprig/inference && GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./transport/
```

Expected: `ok  	github.com/looprig/inference/transport`.

---

## Task 1: Behavioural tests, then `WithoutExecutionTimeout`

**Files:**
- Create: `transport/execution_timeout_test.go` (package `transport_test`)
- Create: `transport/execution_timeout.go`
- Modify: `transport/client.go`

These tests reuse the existing `transport_test` fixtures: `req`, `firstText`,
`dualPathRouter`, `customCodec` and `ndjsonTextDecoder` from `client_test.go`, and
`roundTripperFunc` from `roundtripper_test.go`.

**The ceilings.** Invoke's ceiling is shortened with the public
`WithInvokeTimeout(50ms)`, and the fixture answers after 300ms.

- **Invoke:** the unmarked call must fail with the transport's own timeout, and the
  marked call must succeed.
- **Stream:** its 60s header ceiling is a `const` and cannot be shortened, so a
  behavioural test cannot outlast it. The behavioural tests prove that a marked Stream
  runs on the unlimited client with the right TLS and RoundTripper. Task 2's white-box
  guard pins `ResponseHeaderTimeout == 0` on that client.

**Why the handlers drain the request body.** `net/http` cancels a handler's
`r.Context()` when the client disconnects, but only after the handler has consumed the
request body. A handler that blocks on `<-r.Context().Done()` without draining first
never unblocks, and `srv.Close()` then hangs until the test binary's 10-minute timeout.
This was measured on the scratch run.

**Step 1: Write the failing tests**

Create `transport/execution_timeout_test.go`:

```go
package transport_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference/auth"
	failure "github.com/looprig/inference/failure"
	"github.com/looprig/inference/transport"
)

// shortInvokeCeiling stands in for Invoke's 5-minute default so a test can
// outlast it; lateBy is how long lateHandler holds each response back.
const (
	shortInvokeCeiling = 50 * time.Millisecond
	lateBy             = 300 * time.Millisecond
)

// lateHandler answers /invoke and /stream after delay, or returns as soon as the
// request's context ends so srv.Close never waits out the delay.
func lateHandler(delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets r.Context() observe a client that gives up
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		switch r.URL.Path {
		case "/invoke":
			_, _ = io.WriteString(w, `{"answer":"late"}`)
		case "/stream":
			_, _ = io.WriteString(w, `{"text":"late-stream"}`+"\n")
		default:
			http.NotFound(w, r)
		}
	}
}

func unlimited() context.Context {
	return transport.WithoutExecutionTimeout(context.Background())
}

// requireCeilingTimeout asserts err is the transport's own ceiling firing: a
// *failure.NetworkError whose cause reports Timeout().
func requireCeilingTimeout(t *testing.T, err error) {
	t.Helper()
	var networkErr *failure.NetworkError
	if !errors.As(err, &networkErr) {
		t.Fatalf("err = %v (%T), want *failure.NetworkError", err, err)
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("err = %v, want a client timeout", err)
	}
}

func requireStreamText(t *testing.T, ctx context.Context, c *transport.Client, want string) {
	t.Helper()
	reader, err := c.Stream(ctx, req("late"))
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer reader.Close()
	chunk, err := reader.Next()
	if err != nil {
		t.Fatalf("Stream.Next error: %v", err)
	}
	if text, ok := chunk.(*content.TextChunk); !ok || text.Text != want {
		t.Fatalf("chunk = %#v, want TextChunk %q", chunk, want)
	}
}

func TestWithoutExecutionTimeoutOutlastsInvokeCeiling(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(lateHandler(lateBy))
	defer srv.Close()
	c := transport.New(
		transport.Endpoint{BaseURL: srv.URL},
		dualPathRouter{},
		customCodec{body: `{}`},
		auth.None(),
		transport.WithInvokeTimeout(shortInvokeCeiling),
		transport.WithStreamDecoder(ndjsonTextDecoder{}),
	)

	_, err := c.Invoke(context.Background(), req("late"))
	requireCeilingTimeout(t, err)

	resp, err := c.Invoke(unlimited(), req("late"))
	if err != nil {
		t.Fatalf("marked Invoke error: %v", err)
	}
	if got := firstText(t, resp); got != `{"answer":"late"}` {
		t.Fatalf("marked Invoke text = %q", got)
	}
	requireStreamText(t, unlimited(), c, "late-stream")
}

func TestWithoutExecutionTimeoutKeepsTLSRootCAs(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(lateHandler(lateBy))
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	trusted := transport.New(
		transport.Endpoint{BaseURL: srv.URL},
		dualPathRouter{},
		customCodec{body: `{}`},
		auth.None(),
		transport.WithInvokeTimeout(shortInvokeCeiling),
		transport.WithTLSRootCAs(roots),
		transport.WithStreamDecoder(ndjsonTextDecoder{}),
	)
	_, err := trusted.Invoke(context.Background(), req("late"))
	requireCeilingTimeout(t, err)
	if _, err := trusted.Invoke(unlimited(), req("late")); err != nil {
		t.Fatalf("marked Invoke over trusted TLS error: %v", err)
	}
	requireStreamText(t, unlimited(), trusted, "late-stream")

	unrelated := x509.NewCertPool()
	unrelated.AddCert(&x509.Certificate{Raw: []byte{1}, RawSubject: []byte{1}})
	untrusted := transport.New(
		transport.Endpoint{BaseURL: srv.URL},
		dualPathRouter{},
		customCodec{body: `{}`},
		auth.None(),
		transport.WithTLSRootCAs(unrelated),
	)
	_, err = untrusted.Invoke(unlimited(), req("late"))
	var verifyErr *tls.CertificateVerificationError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("marked Invoke against an untrusted certificate: err = %v, want *tls.CertificateVerificationError", err)
	}
}

func TestWithoutExecutionTimeoutKeepsCustomRoundTripper(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(lateHandler(lateBy))
	defer srv.Close()

	var calls atomic.Int32
	base := srv.Client().Transport
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return base.RoundTrip(r)
	})
	// WithTLSRootCAs after WithRoundTripper rebuilds every library client. The
	// unrelated root would fail the handshake if a library transport were used,
	// so a successful call proves the caller's RoundTripper carried it.
	unrelated := x509.NewCertPool()
	unrelated.AddCert(&x509.Certificate{Raw: []byte{1}, RawSubject: []byte{1}})
	c := transport.New(
		transport.Endpoint{BaseURL: srv.URL},
		dualPathRouter{},
		customCodec{body: `{}`},
		auth.None(),
		transport.WithInvokeTimeout(shortInvokeCeiling),
		transport.WithRoundTripper(rt),
		transport.WithTLSRootCAs(unrelated),
		transport.WithStreamDecoder(ndjsonTextDecoder{}),
	)

	_, err := c.Invoke(context.Background(), req("late"))
	requireCeilingTimeout(t, err)
	if _, err := c.Invoke(unlimited(), req("late")); err != nil {
		t.Fatalf("marked Invoke through caller RoundTripper error: %v", err)
	}
	requireStreamText(t, unlimited(), c, "late-stream")
	if got := calls.Load(); got != 3 {
		t.Fatalf("caller RoundTripper calls = %d, want 3", got)
	}
}

func TestWithoutExecutionTimeoutKeepsCallerCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Drain the body first: net/http watches for the client going away (and
		// cancels r.Context()) only once the request body has been consumed.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := transport.New(
		transport.Endpoint{BaseURL: srv.URL},
		dualPathRouter{},
		customCodec{body: `{}`},
		auth.None(),
		transport.WithStreamDecoder(ndjsonTextDecoder{}),
	)
	call := func(ctx context.Context, streaming bool) error {
		if streaming {
			reader, err := c.Stream(ctx, req("held"))
			if err == nil {
				_ = reader.Close()
			}
			return err
		}
		_, err := c.Invoke(ctx, req("held"))
		return err
	}

	for _, streaming := range []bool{false, true} {
		name := map[bool]string{false: "invoke", true: "stream"}[streaming]
		t.Run(name+" cancel", func(t *testing.T) {
			ctx, cancel := context.WithCancel(unlimited())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- call(ctx, streaming) }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("returned before reaching the server: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("request never reached the server")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("marked call ignored cancellation")
			}
		})
	}

	t.Run("invoke caller deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(unlimited(), shortInvokeCeiling)
		defer cancel()
		if err := call(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}
```

**Step 2: Run them and watch them fail**

```sh
cd /Users/ipotter/code/looprig/inference && GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./transport/ -run WithoutExecutionTimeout
```

Expected: a build failure, `undefined: transport.WithoutExecutionTimeout`, then
`FAIL	github.com/looprig/inference/transport [build failed]`.

**Step 3: Add the marker and the selector**

Create `transport/execution_timeout.go`:

```go
package transport

import (
	"context"
	"net/http"
)

// noExecutionTimeoutKey is the unexported context key for WithoutExecutionTimeout.
type noExecutionTimeoutKey struct{}

// WithoutExecutionTimeout returns a copy of ctx that marks every Invoke and
// Stream made with it (or with a context derived from it) as having no
// transport-imposed execution ceiling: neither Invoke's whole-request timeout
// (default 5 minutes, WithInvokeTimeout) nor Stream's 60-second response-header
// timeout applies. The call runs on a dedicated HTTP client built alongside the
// normal ones, so the Client's TLS roots, caller-owned RoundTripper, dial and
// TLS-handshake limits, redirect refusal, authorization and response-size
// limits are unchanged. The marker is process-local: nothing is sent on the
// wire.
//
// Cancellation becomes the caller's job. ctx's own cancellation and deadline
// still end the call; a peer that stops responding on a half-open connection
// is detected only by TCP keepalive.
func WithoutExecutionTimeout(ctx context.Context) context.Context {
	return context.WithValue(ctx, noExecutionTimeoutKey{}, true)
}

// executionHTTPClient returns the unlimited client for a marked ctx and normal
// otherwise.
func (c *Client) executionHTTPClient(ctx context.Context, normal *http.Client) *http.Client {
	if unlimited, _ := ctx.Value(noExecutionTimeoutKey{}).(bool); unlimited {
		return c.hcUnlimited
	}
	return normal
}
```

**Step 4: Build and wire the unlimited client in `transport/client.go`**

Apply exactly these hunks. The ceiling constants and `newInvokeHTTPClient` stay
unchanged. `newInvokeHTTPClient(0, roots)` already means no `Timeout` and no
`ResponseHeaderTimeout`, so no new constructor is needed (DRY).

```diff
--- a/transport/client.go
+++ b/transport/client.go
@@ -58,8 +58,11 @@
 	// so it is safe to bound with a real whole-request Timeout. Stream must never
 	// carry a whole-request Timeout — that would abort a long-lived body
 	// mid-flight — so it keeps only the connect/TLS/response-header budget below.
-	hcInvoke *http.Client
-	hcStream *http.Client
+	// hcUnlimited serves calls marked WithoutExecutionTimeout: the same TLS roots,
+	// RoundTripper and connection-setup budget, with neither deadline.
+	hcInvoke    *http.Client
+	hcStream    *http.Client
+	hcUnlimited *http.Client
 }
 
 // Compile-time proof that Client honors the inference.Client contract.
@@ -174,6 +177,7 @@
 		c.tlsRootCAs = cloned.Clone()
 		c.hcInvoke = newInvokeHTTPClient(c.invokeTimeout, c.tlsRootCAs)
 		c.hcStream = newStreamHTTPClient(c.tlsRootCAs)
+		c.hcUnlimited = newInvokeHTTPClient(0, c.tlsRootCAs)
 		c.applyRoundTripper()
 	}
 }
@@ -254,6 +258,7 @@
 		invokeTimeout: defaultInvokeTimeout,
 		hcInvoke:      newInvokeHTTPClient(defaultInvokeTimeout, nil),
 		hcStream:      newStreamHTTPClient(nil),
+		hcUnlimited:   newInvokeHTTPClient(0, nil),
 	}
 	// Optional streaming: a StreamingCodec is its own StreamDecoder.
 	if sd, ok := cdc.(codec.StreamDecoder); ok {
@@ -271,6 +276,7 @@
 	}
 	c.hcInvoke.Transport = c.roundTripper
 	c.hcStream.Transport = c.roundTripper
+	c.hcUnlimited.Transport = c.roundTripper
 }
 
 // baseTransport builds the http.Transport settings shared by both the Invoke and
@@ -371,7 +377,7 @@
 	if err := authorizer.Authorize(ctx, httpReq); err != nil {
 		return nil, err
 	}
-	httpResp, err := c.hcInvoke.Do(httpReq)
+	httpResp, err := c.executionHTTPClient(ctx, c.hcInvoke).Do(httpReq)
 	if err != nil {
 		return nil, &failure.NetworkError{Err: err}
 	}
@@ -442,7 +448,7 @@
 	if err := authorizer.Authorize(ctx, httpReq); err != nil {
 		return nil, err
 	}
-	httpResp, err := c.hcStream.Do(httpReq)
+	httpResp, err := c.executionHTTPClient(ctx, c.hcStream).Do(httpReq)
 	if err != nil {
 		return nil, &failure.NetworkError{Err: err}
 	}
```

`WithInvokeTimeout` deliberately does **not** touch `hcUnlimited`: it bounds only the
normal Invoke client.

**Step 5: Run the tests and watch them pass**

```sh
cd /Users/ipotter/code/looprig/inference && gofmt -l transport && GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./transport/ -run WithoutExecutionTimeout -v 2>&1 | grep -E '^\s*--- |^ok|^FAIL'
```

Expected: `gofmt` prints nothing, then:

```text
--- PASS: TestWithoutExecutionTimeoutKeepsCallerCancellation (0.05s)
    --- PASS: TestWithoutExecutionTimeoutKeepsCallerCancellation/invoke_cancel (0.00s)
    --- PASS: TestWithoutExecutionTimeoutKeepsCallerCancellation/stream_cancel (0.00s)
    --- PASS: TestWithoutExecutionTimeoutKeepsCallerCancellation/invoke_caller_deadline (0.05s)
--- PASS: TestWithoutExecutionTimeoutOutlastsInvokeCeiling (0.66s)
--- PASS: TestWithoutExecutionTimeoutKeepsCustomRoundTripper (0.66s)
--- PASS: TestWithoutExecutionTimeoutKeepsTLSRootCAs (0.67s)
ok  	github.com/looprig/inference/transport
```

The timings are approximate and the order may vary.

**Step 6: Commit**

```sh
cd /Users/ipotter/code/looprig/inference && git add transport/execution_timeout.go transport/execution_timeout_test.go transport/client.go && git commit -m "feat(transport): add WithoutExecutionTimeout for calls with no execution ceiling"
```

---

## Task 2: White-box guards for the client wiring

**Files:**
- Create: `transport/execution_timeout_internal_test.go` (package `transport`)

The behavioural tests cannot see every property of the client. These guards pin five:

- the unmarked selection returns the exact normal client;
- the default ceilings are unchanged;
- a marked context, and one derived from it, selects `hcUnlimited`;
- `hcUnlimited` has neither deadline but keeps every setup and TLS safeguard;
- every option reaches `hcUnlimited` in either order, and `WithInvokeTimeout` never
  bounds it.

`identityRoundTripper` is a pointer type because a `roundTripperFunc` is not comparable
inside an interface.

**Step 1: Write the guards**

```go
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"
	"time"

	"github.com/looprig/inference/auth"
	"github.com/looprig/inference/codec/openaiapi"
	"github.com/looprig/inference/route"
)

func newSelectionTestClient(opts ...Option) *Client {
	return New(Endpoint{BaseURL: "http://127.0.0.1:1/v1"}, route.StaticChat("/chat/completions"), openaiapi.Codec{}, auth.None(), opts...)
}

// TestExecutionHTTPClientSelection pins which HTTP client a call runs on: an
// unmarked call keeps its normal, bounded client; a marked call (including
// through a derived context) runs on the unlimited client, which drops only the
// whole-request and response-header deadlines.
func TestExecutionHTTPClientSelection(t *testing.T) {
	t.Parallel()
	c := newSelectionTestClient()

	if got := c.executionHTTPClient(context.Background(), c.hcInvoke); got != c.hcInvoke {
		t.Fatal("unmarked Invoke did not keep hcInvoke")
	}
	if got := c.executionHTTPClient(context.Background(), c.hcStream); got != c.hcStream {
		t.Fatal("unmarked Stream did not keep hcStream")
	}
	if c.hcInvoke.Timeout != defaultInvokeTimeout {
		t.Fatalf("hcInvoke.Timeout = %v, want %v", c.hcInvoke.Timeout, defaultInvokeTimeout)
	}
	if got := c.hcStream.Transport.(*http.Transport).ResponseHeaderTimeout; got != streamResponseHeaderTimeout {
		t.Fatalf("hcStream ResponseHeaderTimeout = %v, want %v", got, streamResponseHeaderTimeout)
	}

	ctx, cancel := context.WithCancel(WithoutExecutionTimeout(context.Background()))
	defer cancel()
	for name, normal := range map[string]*http.Client{"invoke": c.hcInvoke, "stream": c.hcStream} {
		if got := c.executionHTTPClient(ctx, normal); got != c.hcUnlimited {
			t.Fatalf("marked %s did not select hcUnlimited", name)
		}
	}

	u := c.hcUnlimited
	tr, ok := u.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("hcUnlimited.Transport = %T, want *http.Transport", u.Transport)
	}
	if u.Timeout != 0 || tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("hcUnlimited keeps an execution deadline: Timeout=%v ResponseHeaderTimeout=%v", u.Timeout, tr.ResponseHeaderTimeout)
	}
	if tr.TLSHandshakeTimeout != tlsHandshakeTimeout ||
		tr.ExpectContinueTimeout != expectContinueTimeout ||
		tr.IdleConnTimeout != idleConnTimeout ||
		tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 ||
		tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("hcUnlimited lost a connection-setup or TLS safeguard")
	}
	if u.CheckRedirect == nil {
		t.Fatal("hcUnlimited lost redirect refusal")
	}
}

type identityRoundTripper struct{}

func (*identityRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrNotSupported
}

// TestUnlimitedHTTPClientFollowsOptions proves every option that rebuilds or
// rewires the library clients also rebuilds or rewires the unlimited one, in
// either option order, and that WithInvokeTimeout never bounds it.
func TestUnlimitedHTTPClientFollowsOptions(t *testing.T) {
	t.Parallel()
	roots := x509.NewCertPool()
	roots.AddCert(&x509.Certificate{Raw: []byte{1}, RawSubject: []byte{1}})

	withRoots := newSelectionTestClient(WithTLSRootCAs(roots))
	tr := withRoots.hcUnlimited.Transport.(*http.Transport)
	if tr.TLSClientConfig.RootCAs == nil || !tr.TLSClientConfig.RootCAs.Equal(roots) {
		t.Fatal("WithTLSRootCAs did not reach hcUnlimited")
	}

	rt := &identityRoundTripper{}
	for name, opts := range map[string][]Option{
		"roundtripper then roots": {WithRoundTripper(rt), WithTLSRootCAs(roots)},
		"roots then roundtripper": {WithTLSRootCAs(roots), WithRoundTripper(rt)},
	} {
		if got := newSelectionTestClient(opts...).hcUnlimited.Transport; got != rt {
			t.Fatalf("%s: hcUnlimited.Transport = %T, want the caller's RoundTripper", name, got)
		}
	}

	if got := newSelectionTestClient(WithInvokeTimeout(time.Second)).hcUnlimited.Timeout; got != 0 {
		t.Fatalf("WithInvokeTimeout bounded hcUnlimited: Timeout = %v", got)
	}
}
```

**Step 2: Run them**

```sh
cd /Users/ipotter/code/looprig/inference && GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./transport/ -run 'ExecutionHTTPClient|UnlimitedHTTPClient' -v 2>&1 | grep -E '^\s*--- |^ok|^FAIL'
```

Expected:

```text
--- PASS: TestExecutionHTTPClientSelection (0.00s)
--- PASS: TestUnlimitedHTTPClientFollowsOptions (0.00s)
ok  	github.com/looprig/inference/transport
```

**Step 3: Prove the guards bite (mutation check, then revert)**

These guards pass on their first run, so show that they fail on the defects they exist
for. Make each mutation, run the tests, then restore the file with `git checkout`.

```sh
cd /Users/ipotter/code/looprig/inference
sed -i '' '/c.hcUnlimited = newInvokeHTTPClient(0, c.tlsRootCAs)/d' transport/client.go
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./transport/ -run 'UnlimitedHTTPClient|WithoutExecutionTimeout' 2>&1 | grep -E '^\s*--- FAIL|_test.go:[0-9]+:'
git checkout transport/client.go
sed -i '' '/c.hcUnlimited.Transport = c.roundTripper/d' transport/client.go
GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./transport/ -run 'UnlimitedHTTPClient|WithoutExecutionTimeout' 2>&1 | grep -E '^\s*--- FAIL|_test.go:[0-9]+:'
git checkout transport/client.go
```

`git checkout` is safe only here, because Task 1 is already committed. Expected, first
mutation (with a random port):

```text
--- FAIL: TestUnlimitedHTTPClientFollowsOptions (0.00s)
    execution_timeout_internal_test.go:86: WithTLSRootCAs did not reach hcUnlimited
--- FAIL: TestWithoutExecutionTimeoutKeepsTLSRootCAs (0.06s)
    execution_timeout_test.go:129: marked Invoke over trusted TLS error: inference: network error: Post "https://127.0.0.1:<port>/invoke": tls: failed to verify certificate: x509: certificate signed by unknown authority
```

Expected, second mutation:

```text
--- FAIL: TestUnlimitedHTTPClientFollowsOptions (0.00s)
    execution_timeout_internal_test.go:95: roundtripper then roots: hcUnlimited.Transport = *http.Transport, want the caller's RoundTripper
--- FAIL: TestWithoutExecutionTimeoutKeepsCustomRoundTripper (0.05s)
    execution_timeout_test.go:179: marked Invoke through caller RoundTripper error: ...x509: certificate signed by unknown authority
```

After both reverts, `git status --short` must show only the new, untracked
`transport/execution_timeout_internal_test.go`.

**Step 4: Commit**

```sh
cd /Users/ipotter/code/looprig/inference && git add transport/execution_timeout_internal_test.go && git commit -m "test(transport): guard the unlimited client's wiring and safeguards"
```

---

## Task 3: Docs (package doc and README)

**Files:**
- Modify: `transport/client.go` (package doc comment, lines 1-8)
- Modify: `README.md`

**Step 1: Package doc**

Append one paragraph to the package comment in `transport/client.go`, after
"…and the StreamDecoder owns wire framing." and before `package transport`:

```go
//
// Execution ceilings: Invoke is bounded by a whole-request timeout (default 5
// minutes, WithInvokeTimeout) and Stream by a 60-second response-header timeout.
// A call whose context carries WithoutExecutionTimeout runs with neither; its
// only end is the caller's own cancellation or deadline, and a half-open
// connection is detected only by TCP keepalive.
```

**Step 2: README section**

In `README.md`, insert this section immediately before `## Consumer obligation: \`Request.SessionID\``:

````markdown
## Calls with no execution ceiling

`transport.Client` bounds Invoke by a whole-request timeout (5 minutes by default,
`WithInvokeTimeout`) and Stream by a 60-second wait for response headers. For a call
that may legitimately run longer, such as a slow local model or a long reasoning turn,
mark its context:

```go
ctx = transport.WithoutExecutionTimeout(ctx)
resp, err := client.Invoke(ctx, req) // or client.Stream(ctx, req)
```

A marked call has neither ceiling. Everything else is unchanged:

- TLS roots and a `WithRoundTripper` transport;
- dial and TLS-handshake limits;
- redirect refusal and authorization;
- response-size limits.

Unmarked calls are unaffected, and nothing is sent on the wire. **Cancellation becomes
the caller's job**: the context's own cancel or deadline is the only end, and a
half-open connection is detected only by TCP keepalive. `llm` providers built on
`transport.Client` honour the marker as they are. `llm`'s `gemini` and `bedrock`
providers use their own HTTP client and do not.
````

**Step 3: Check docs build and formatting**

```sh
cd /Users/ipotter/code/looprig/inference && gofmt -l transport && GOWORK=off GOTOOLCHAIN=go1.26.8 go doc ./transport WithoutExecutionTimeout
```

Expected: `gofmt` prints nothing. `go doc` prints
`func WithoutExecutionTimeout(ctx context.Context) context.Context` followed by its
doc comment.

If the repository's docs-examples check (`.github/workflows/docs-examples.yml`,
`testdata/docs/examples.json`) covers README code blocks, run it:

```sh
cd /Users/ipotter/code/looprig/inference && GOWORK=off GOTOOLCHAIN=go1.26.8 go test ./examples/...
```

Expected: `ok` (or `[no test files]` lines). If it rejects the new fragment because it
is not a full program, add the fragment to the checker's allowlist the way existing
fragments are handled. Do not weaken the check.

**Step 4: Commit**

```sh
cd /Users/ipotter/code/looprig/inference && git add transport/client.go README.md && git commit -m "docs: document WithoutExecutionTimeout and its cancellation contract"
```

---

## Task 4: Full verification

**Step 1: Standalone race suite**

```sh
cd /Users/ipotter/code/looprig/inference && GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./... 2>&1 | grep -v '^ok' | grep -v 'no test files'
```

Expected: no output (every package `ok`).

**Step 2: The repository's native check surface**

```sh
cd /Users/ipotter/code/looprig/inference && GOTOOLCHAIN=go1.26.8 make check
```

Expected: exit 0. That covers gofmt, vet, staticcheck, gosec (scoped), `go mod verify`
+ govulncheck, race tests and build. `vet`, `staticcheck` and `gosec` were
pre-checked clean on the scratch run. govulncheck needs network or a local DB. If it
cannot run, record that rather than skipping it silently.

**Step 3: Tidy is a no-op**

```sh
cd /Users/ipotter/code/looprig/inference && GOWORK=off GOTOOLCHAIN=go1.26.8 go mod tidy && git status --short
```

Expected: no output. The change adds no dependency.

**Step 4: API delta is additive only (minor bump)**

```sh
cd /Users/ipotter/code/looprig/inference && GOWORK=off GOTOOLCHAIN=go1.26.8 go run golang.org/x/exp/cmd/gorelease@latest -base=v0.13.0 -version=v0.14.0
```

Expected: one compatible change in `github.com/looprig/inference/transport`,
`WithoutExecutionTimeout: added`, no incompatible changes, and
`v0.14.0 is a valid semantic version for this release.` This needs network. If
`gorelease` is unavailable, `apidiff` against v0.13.0 is equivalent.

**Step 5: Cross-repo sanity (per-package green does not compose)**

`llm` needs no code change. Confirm it still builds and tests against the new
inference through an **uncommitted** workspace:

```sh
cd /tmp && rm -rf infws && mkdir infws && cd infws && GOTOOLCHAIN=go1.26.8 go work init /Users/ipotter/code/looprig/llm /Users/ipotter/code/looprig/inference && cd /Users/ipotter/code/looprig/llm && GOWORK=/tmp/infws/go.work GOTOOLCHAIN=go1.26.8 go test ./... 2>&1 | grep -v '^ok' | grep -v 'no test files'; rm -rf /tmp/infws
```

Expected: no output. Do not create or commit a `go.work` inside any repository.

---

## Task 5: Release inference v0.14.0 — REQUIRES OWNER CONFIRMATION

**Stop here and ask the owner before any push or tag.** Show them the three commits
(`git log --oneline v0.13.0..main`), the Task 4 results, and the tag message below. Do
nothing further without an explicit yes.

**Step 1: Push `main`**

```sh
cd /Users/ipotter/code/looprig/inference && git push origin main
```

**Step 2: Annotated tag (this is the changelog)**

```sh
cd /Users/ipotter/code/looprig/inference && git tag -a v0.14.0 -F - <<'MSG'
inference v0.14.0: calls with no execution ceiling

- transport.WithoutExecutionTimeout(ctx): a request-scoped, process-local
  marker (inherited by derived contexts). A marked Invoke has no
  whole-request timeout (default 5m / WithInvokeTimeout); a marked Stream
  has no 60s response-header timeout. Nothing is sent on the wire.
- The marked call runs on a dedicated HTTP client built in the constructor
  and WithTLSRootCAs and wired by WithRoundTripper, in any option order:
  TLS roots, caller RoundTripper, dial/TLS-handshake limits, the TLS 1.2
  floor, redirect refusal, authorization and response-size limits are
  unchanged. WithInvokeTimeout does not bound it.
- Unmarked calls are byte-for-byte and timing-identical to v0.13.0.
- Cancellation is the caller's: the context's own cancel/deadline is the
  only end; a half-open connection is detected only by TCP keepalive.
- llm providers built on transport.Client honour the marker with no change.
  NON-CLAIM: llm's gemini and bedrock providers own their http.Client (60s
  header timeout) and are unaffected by the marker.
API: additive (one new function) -> minor. Pins unchanged (core v0.11.0,
credentials v0.2.1, secrets v0.2.2).
Upstreams Oxy "Patch set 6" (third_party/looprig-inference).
MSG
git push origin v0.14.0
```

**Step 3: Verify both remote refs**

```sh
cd /Users/ipotter/code/looprig/inference && git ls-remote origin refs/heads/main refs/tags/v0.14.0 'refs/tags/v0.14.0^{}' && git rev-parse main v0.14.0 'v0.14.0^{}'
```

Expected: the remote `main` equals the local `main`, the remote `refs/tags/v0.14.0`
equals the local tag object, and `^{}` equals the `main` commit. Record the commit
hash and the tag object hash for the master plan.

**Step 4: Hand-off (not committed by the agent)**

- Update the master plan's progress row 03 to `released (v0.14.0, <commit>, tag object <obj>)`.
- Tell the coordinator the tag exists, so plan 05 (harness v0.41.0) can pin
  `github.com/looprig/inference v0.14.0` with `go get`.
- The workspace `AGENTS.md`, `repositories.mk` and `go.work` are updated per the master
  plan's rules. The outer repository is not committed by the agent.
- For Oxy (plan 09): once it pins inference v0.14.0, `transport/execution_timeout_test.go`
  in `third_party/looprig-inference` is superseded by this module's tests. Oxy keeps its
  own consumer-side regression test, which marks the call through its cancellable
  inference client.
