//go:build integration

package tools

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/tool"
)

// web_integration_test.go exercises the two WEB tools against REAL endpoints and
// proves the CLIENT-side TLS 1.2 floor (which the manifest wires and the tools
// merely USE). It is tagged `integration` so it is excluded from the default
// `go test ./...` run — run with `go test -tags integration -race ./tools/`.
//
// SPLIT (documented in fetch.go / websearch.go): the plain-HTTP behavior of Fetch
// and the static-HTML parsing of WebSearch are covered by the UNIT tests
// (httptest.NewServer is plain HTTP). The TLS 1.2 floor enforcement and the live
// DuckDuckGo scrape can only be exercised against a TLS/real endpoint, so they
// live here (T15).

// tls12Client mirrors the client the manifest builds for the web tools: an
// explicit Timeout and a Transport whose TLSClientConfig.MinVersion is TLS 1.2
// with InsecureSkipVerify never set.
func tls12Client() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// TestFetchTLS12FloorRejectsTLS11 proves the injected client refuses a server
// that offers only TLS 1.1: Fetch returns an error result, never a 200. This is
// the CLIENT'S floor (the manifest's config) flowing through the tool unchanged.
func TestFetchTLS12FloorRejectsTLS11(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("should never be read"))
	}))
	// Force the SERVER to cap at TLS 1.1 so a TLS-1.2-floor client cannot handshake.
	srv.TLS = &tls.Config{MaxVersion: tls.VersionTLS11}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// Trust the test server's cert but keep the TLS 1.2 floor (NOT InsecureSkipVerify).
	client := tls12Client()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is not *http.Transport")
	}
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	tr.TLSClientConfig.RootCAs = pool

	f := NewFetch(client)
	res, err := f.InvokableRun(context.Background(), `{"url":"`+srv.URL+`","method":"GET"}`)
	if err != nil {
		t.Fatalf("InvokableRun() unexpected Go error = %v", err)
	}
	got := textOf(t, res)
	if !strings.Contains(got, "error") {
		t.Errorf("expected a TLS handshake error result, got %q", got)
	}
	if strings.Contains(got, "200") || strings.Contains(got, "should never be read") {
		t.Errorf("TLS 1.2 floor did not block a TLS 1.1 server: %q", got)
	}
}

// TestFetchTLS12FloorAcceptsTLS12 is the positive control: against a TLS 1.2
// server the same client succeeds.
func TestFetchTLS12FloorAcceptsTLS12(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	client := tls12Client()
	tr := client.Transport.(*http.Transport)
	tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	f := NewFetch(client)
	res, err := f.InvokableRun(context.Background(), `{"url":"`+srv.URL+`","method":"GET"}`)
	if err != nil {
		t.Fatalf("InvokableRun() unexpected Go error = %v", err)
	}
	got := textOf(t, res)
	if !strings.Contains(got, "200") {
		t.Errorf("expected a 200 over TLS 1.2, got %q", got)
	}
}

// TestDuckDuckGoProviderLiveScrape hits the REAL DuckDuckGo HTML endpoint and
// asserts the scrape returns at least one well-formed result. It is best-effort:
// a network failure or an empty page (DDG throttling/layout change) is skipped,
// not failed, so the integration suite is not flaky on the network.
func TestDuckDuckGoProviderLiveScrape(t *testing.T) {
	p := NewDuckDuckGoProvider(tls12Client())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	results, err := p.Search(ctx, "golang programming language", 5)
	if err != nil {
		t.Skipf("live DuckDuckGo scrape unavailable: %v", err)
	}
	if len(results) == 0 {
		t.Skip("live DuckDuckGo returned no parseable results (throttle or layout change)")
	}
	if len(results) > 5 {
		t.Errorf("scrape returned %d results, exceeds requested max 5", len(results))
	}
	for i, r := range results {
		if r.Title == "" || r.URL == "" {
			t.Errorf("result[%d] missing title or url: %+v", i, r)
		}
		if !strings.HasPrefix(r.URL, "http") {
			t.Errorf("result[%d] url is not absolute http(s): %q", i, r.URL)
		}
	}
}

// TestWebSearchLiveEndToEnd drives the WebSearch tool with the live provider end
// to end (build request, run, format). Best-effort, like the scrape test.
func TestWebSearchLiveEndToEnd(t *testing.T) {
	ws := NewWebSearch(NewDuckDuckGoProvider(tls12Client()))

	req, err := ws.BuildRequest(`{"query":"golang"}`, nil)
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if _, ok := req.(tool.WebSearchRequest); !ok {
		t.Fatalf("want tool.WebSearchRequest, got %T", req)
	}

	res, err := ws.InvokableRun(context.Background(), `{"query":"golang","results":3}`)
	if err != nil {
		t.Fatalf("InvokableRun() unexpected Go error = %v", err)
	}
	got := textOf(t, res)
	if strings.Contains(got, "error: web search failed") {
		t.Skipf("live web search unavailable: %q", got)
	}
	if got == "No results found." {
		t.Skip("live web search returned no results (throttle or layout change)")
	}
	if !strings.Contains(got, "http") {
		t.Errorf("expected at least one URL in results, got %q", got)
	}
}
