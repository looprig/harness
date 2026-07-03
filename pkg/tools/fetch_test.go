package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/tool"
)

// textOf extracts the single text-block payload from a tool result, failing the
// test if the result is not exactly one *content.TextBlock.
func textOf(t *testing.T, r *tool.ToolResult) string {
	t.Helper()
	if r == nil {
		t.Fatalf("nil ToolResult")
	}
	if len(r.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(r.Content))
	}
	tb, ok := r.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("want *content.TextBlock, got %T", r.Content[0])
	}
	return tb.Text
}

func TestFetchInfo(t *testing.T) {
	t.Parallel()
	f := NewFetch(http.DefaultClient)
	info, err := f.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "Fetch" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "Fetch")
	}
	if len(info.Schema) == 0 {
		t.Errorf("Info().Schema is empty")
	}
}

func TestFetchInvokableRun(t *testing.T) {
	t.Parallel()

	const secretHeader = "Bearer s3cr3t-token"
	const secretBody = "password=hunter2"

	// echoSrv reflects method, a header, and the request body so the happy paths
	// can assert the tool actually performed the request.
	echoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		w.Header().Set("X-Echo-Method", r.Method)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("method=" + r.Method + " body=" + string(body)))
	}))
	t.Cleanup(echoSrv.Close)

	// bigSrv returns far more than the 64 KiB cap so truncation can be asserted.
	bigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("A", maxFetchBodyBytes+4096)))
	}))
	t.Cleanup(bigSrv.Close)

	tests := []struct {
		name        string
		argsJSON    string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "GET returns body and status",
			argsJSON:    `{"url":"` + echoSrv.URL + `","method":"GET"}`,
			wantContain: []string{"200", "method=GET"},
		},
		{
			name:        "POST with body",
			argsJSON:    `{"url":"` + echoSrv.URL + `","method":"POST","body":"` + secretBody + `"}`,
			wantContain: []string{"200", "method=POST", "body=" + secretBody},
		},
		{
			name:        "invalid method PUT is rejected",
			argsJSON:    `{"url":"` + echoSrv.URL + `","method":"PUT"}`,
			wantContain: []string{"error"},
			wantAbsent:  []string{"200"},
		},
		{
			name:        "missing url is rejected",
			argsJSON:    `{"method":"GET"}`,
			wantContain: []string{"error"},
		},
		{
			name:        "non-http scheme is rejected",
			argsJSON:    `{"url":"file:///etc/passwd","method":"GET"}`,
			wantContain: []string{"error"},
			wantAbsent:  []string{"200"},
		},
		{
			name:        "unparseable args is an error result",
			argsJSON:    `not json`,
			wantContain: []string{"error"},
		},
		{
			name:        "oversized body is truncated with notice",
			argsJSON:    `{"url":"` + bigSrv.URL + `","method":"GET"}`,
			wantContain: []string{"200", "truncated"},
		},
		{
			name:        "secret header is sent but never appears in error/result framing",
			argsJSON:    `{"url":"` + echoSrv.URL + `","method":"GET","headers":{"Authorization":"` + secretHeader + `"}}`,
			wantContain: []string{"200"},
		},
	}

	f := NewFetch(echoSrv.Client())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := f.InvokableRun(context.Background(), tt.argsJSON)
			if err != nil {
				t.Fatalf("InvokableRun() unexpected Go error = %v", err)
			}
			got := textOf(t, res)
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("result %q does not contain %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("result %q unexpectedly contains %q", got, absent)
				}
			}
		})
	}
}

func TestFetchTimeout(t *testing.T) {
	t.Parallel()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)

	f := NewFetch(slow.Client())
	// timeout 1s against a 3s server → bounded ctx fires → error result.
	res, err := f.InvokableRun(context.Background(), `{"url":"`+slow.URL+`","method":"GET","timeout":1}`)
	if err != nil {
		t.Fatalf("InvokableRun() unexpected Go error = %v", err)
	}
	got := textOf(t, res)
	if !strings.Contains(got, "error") {
		t.Errorf("expected an error result on timeout, got %q", got)
	}
	if strings.Contains(got, "200") {
		t.Errorf("expected no successful status on timeout, got %q", got)
	}
}

func TestFetchAuditSummaryRedaction(t *testing.T) {
	t.Parallel()

	const secret = "Bearer top-secret"
	const secretBody = "ssn=123456789"
	f := NewFetch(http.DefaultClient)

	tests := []struct {
		name        string
		argsJSON    string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "summary is method + host only",
			argsJSON:    `{"url":"https://api.example.com/secret/path?token=abc","method":"GET","headers":{"Authorization":"` + secret + `"},"body":"` + secretBody + `"}`,
			wantContain: []string{"GET", "api.example.com"},
			wantAbsent:  []string{secret, secretBody, "token=abc", "/secret/path", "Authorization"},
		},
		{
			name:        "POST host only, no path/query/body",
			argsJSON:    `{"url":"https://h.test/p?q=zzz","method":"POST","body":"` + secretBody + `"}`,
			wantContain: []string{"POST", "h.test"},
			wantAbsent:  []string{secretBody, "q=zzz", "/p"},
		},
		{
			name:        "unparseable args yields a generic summary with no secret",
			argsJSON:    `{bad`,
			wantContain: []string{"Fetch"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := f.AuditSummary(tt.argsJSON)
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("AuditSummary(%q) = %q, want it to contain %q", tt.argsJSON, got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("AuditSummary leaked %q in %q", absent, got)
				}
			}
		})
	}
}

func TestFetchBuildRequest(t *testing.T) {
	t.Parallel()

	f := NewFetch(http.DefaultClient)
	tests := []struct {
		name       string
		argsJSON   string
		wantErr    bool
		wantMethod string
		wantURL    string
	}{
		{
			name:       "valid GET",
			argsJSON:   `{"url":"https://example.com/x","method":"GET"}`,
			wantMethod: "GET",
			wantURL:    "https://example.com/x",
		},
		{
			name:     "missing url is an error",
			argsJSON: `{"method":"GET"}`,
			wantErr:  true,
		},
		{
			name:     "invalid method is an error",
			argsJSON: `{"url":"https://example.com","method":"DELETE"}`,
			wantErr:  true,
		},
		{
			name:     "unparseable args is an error",
			argsJSON: `nope`,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := f.BuildRequest(tt.argsJSON, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("BuildRequest() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			fr, ok := req.(tool.FetchRequest)
			if !ok {
				t.Fatalf("want tool.FetchRequest, got %T", req)
			}
			if fr.Method != tt.wantMethod {
				t.Errorf("Method = %q, want %q", fr.Method, tt.wantMethod)
			}
			if fr.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", fr.URL, tt.wantURL)
			}
		})
	}
}
