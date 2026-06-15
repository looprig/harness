package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/tool"
)

// fakeSearchProvider is a canned SearchProvider for unit-testing the WebSearch
// tool in isolation (no network). It records the max it was called with and
// returns either preset results (capped to max) or a preset error.
type fakeSearchProvider struct {
	results []SearchResult
	err     error
	gotMax  int
}

func (f *fakeSearchProvider) Search(_ context.Context, _ string, max int) ([]SearchResult, error) {
	f.gotMax = max
	if f.err != nil {
		return nil, f.err
	}
	if max < len(f.results) {
		return f.results[:max], nil
	}
	return f.results, nil
}

func TestWebSearchInfo(t *testing.T) {
	t.Parallel()
	ws := NewWebSearch(&fakeSearchProvider{})
	info, err := ws.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "WebSearch" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "WebSearch")
	}
	if len(info.Schema) == 0 {
		t.Errorf("Info().Schema is empty")
	}
}

func TestWebSearchInvokableRun(t *testing.T) {
	t.Parallel()

	canned := []SearchResult{
		{Title: "Go Programming Language", URL: "https://go.dev", Snippet: "Build fast, reliable software."},
		{Title: "Effective Go", URL: "https://go.dev/doc/effective_go", Snippet: "Tips for writing clear Go."},
	}

	tests := []struct {
		name        string
		provider    *fakeSearchProvider
		argsJSON    string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "happy path formats results",
			provider:    &fakeSearchProvider{results: canned},
			argsJSON:    `{"query":"golang"}`,
			wantContain: []string{"Go Programming Language", "https://go.dev", "Build fast", "Effective Go"},
		},
		{
			name:        "no results yields a friendly message",
			provider:    &fakeSearchProvider{results: nil},
			argsJSON:    `{"query":"asdkjfhaslkdjfh"}`,
			wantContain: []string{"No results"},
		},
		{
			name:        "provider error becomes a tool-result error",
			provider:    &fakeSearchProvider{err: errors.New("upstream down")},
			argsJSON:    `{"query":"golang"}`,
			wantContain: []string{"error"},
		},
		{
			name:        "missing query is an error result",
			provider:    &fakeSearchProvider{results: canned},
			argsJSON:    `{}`,
			wantContain: []string{"error"},
		},
		{
			name:        "unparseable args is an error result",
			provider:    &fakeSearchProvider{results: canned},
			argsJSON:    `not json`,
			wantContain: []string{"error"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := NewWebSearch(tt.provider)
			res, err := ws.InvokableRun(context.Background(), tt.argsJSON)
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

func TestWebSearchResultsCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		argsJSON string
		wantMax  int
	}{
		{name: "default when omitted", argsJSON: `{"query":"x"}`, wantMax: defaultWebSearchResults},
		{name: "explicit within range", argsJSON: `{"query":"x","results":3}`, wantMax: 3},
		{name: "capped at max", argsJSON: `{"query":"x","results":99}`, wantMax: maxWebSearchResults},
		{name: "non-positive falls back to default", argsJSON: `{"query":"x","results":0}`, wantMax: defaultWebSearchResults},
		{name: "negative falls back to default", argsJSON: `{"query":"x","results":-5}`, wantMax: defaultWebSearchResults},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fp := &fakeSearchProvider{}
			ws := NewWebSearch(fp)
			if _, err := ws.InvokableRun(context.Background(), tt.argsJSON); err != nil {
				t.Fatalf("InvokableRun() unexpected Go error = %v", err)
			}
			if fp.gotMax != tt.wantMax {
				t.Errorf("provider called with max = %d, want %d", fp.gotMax, tt.wantMax)
			}
		})
	}
}

func TestWebSearchBuildRequest(t *testing.T) {
	t.Parallel()

	ws := NewWebSearch(&fakeSearchProvider{})
	tests := []struct {
		name      string
		argsJSON  string
		wantErr   bool
		wantQuery string
	}{
		{name: "valid query", argsJSON: `{"query":"golang generics"}`, wantQuery: "golang generics"},
		{name: "missing query is an error", argsJSON: `{}`, wantErr: true},
		{name: "empty query is an error", argsJSON: `{"query":""}`, wantErr: true},
		{name: "unparseable args is an error", argsJSON: `nope`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := ws.BuildRequest(tt.argsJSON)
			if (err != nil) != tt.wantErr {
				t.Fatalf("BuildRequest() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			wsr, ok := req.(tool.WebSearchRequest)
			if !ok {
				t.Fatalf("want tool.WebSearchRequest, got %T", req)
			}
			if wsr.Query != tt.wantQuery {
				t.Errorf("Query = %q, want %q", wsr.Query, tt.wantQuery)
			}
		})
	}
}

func TestWebSearchAuditSummary(t *testing.T) {
	t.Parallel()

	ws := NewWebSearch(&fakeSearchProvider{})
	tests := []struct {
		name        string
		argsJSON    string
		wantContain []string
	}{
		{name: "query is what the user approves", argsJSON: `{"query":"how to write go"}`, wantContain: []string{"WebSearch", "how to write go"}},
		{name: "unparseable args is generic", argsJSON: `{bad`, wantContain: []string{"WebSearch"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ws.AuditSummary(tt.argsJSON)
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("AuditSummary(%q) = %q, want it to contain %q", tt.argsJSON, got, want)
				}
			}
		})
	}
}

// ddgFixture is a static snippet shaped like DuckDuckGo's no-JS HTML results
// page (html.duckduckgo.com/html/). The third result uses the redirect-wrapped
// link form (//duckduckgo.com/l/?uddg=<urlencoded target>&rut=...) the parser
// must unwrap. No network is touched.
const ddgFixture = `
<div class="results">
  <div class="result results_links results_links_deep web-result">
    <div class="result__body">
      <h2 class="result__title">
        <a rel="nofollow" class="result__a" href="https://go.dev/">The Go Programming Language</a>
      </h2>
      <a class="result__snippet" href="https://go.dev/">Go is an open source programming language that makes it simple to build secure, scalable systems.</a>
    </div>
  </div>
  <div class="result results_links results_links_deep web-result">
    <div class="result__body">
      <h2 class="result__title">
        <a rel="nofollow" class="result__a" href="https://pkg.go.dev/std">Standard library - pkg.go.dev</a>
      </h2>
      <a class="result__snippet" href="https://pkg.go.dev/std">Documentation for the Go standard library packages.</a>
    </div>
  </div>
  <div class="result results_links results_links_deep web-result">
    <div class="result__body">
      <h2 class="result__title">
        <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgithub.com%2Fgolang%2Fgo&amp;rut=abc123">golang/go: The Go programming language</a>
      </h2>
      <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgithub.com%2Fgolang%2Fgo&amp;rut=abc123">The Go programming language repository on GitHub.</a>
    </div>
  </div>
</div>
`

func TestParseDuckDuckGoHTML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		html    string
		max     int
		want    []SearchResult
		wantLen int
	}{
		{
			name: "parses titles, urls, snippets and unwraps redirect",
			html: ddgFixture,
			max:  10,
			want: []SearchResult{
				{Title: "The Go Programming Language", URL: "https://go.dev/", Snippet: "Go is an open source programming language that makes it simple to build secure, scalable systems."},
				{Title: "Standard library - pkg.go.dev", URL: "https://pkg.go.dev/std", Snippet: "Documentation for the Go standard library packages."},
				{Title: "golang/go: The Go programming language", URL: "https://github.com/golang/go", Snippet: "The Go programming language repository on GitHub."},
			},
			wantLen: 3,
		},
		{
			name:    "caps at max",
			html:    ddgFixture,
			max:     2,
			wantLen: 2,
		},
		{
			name:    "empty html yields no results, no panic",
			html:    "",
			max:     10,
			wantLen: 0,
		},
		{
			name:    "malformed html does not panic, returns what parsed",
			html:    `<div class="result"><h2 class="result__title"><a class="result__a" href="https://ok.test">OK</a><<<broken`,
			max:     10,
			wantLen: 1,
		},
		{
			name:    "no result markers yields nothing",
			html:    `<html><body><p>nothing here</p></body></html>`,
			max:     10,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseDuckDuckGoHTML(strings.NewReader(tt.html), tt.max)
			if len(got) != tt.wantLen {
				t.Fatalf("parsed %d results, want %d: %+v", len(got), tt.wantLen, got)
			}
			if tt.want == nil {
				return
			}
			for i, w := range tt.want {
				if got[i].Title != w.Title {
					t.Errorf("result[%d].Title = %q, want %q", i, got[i].Title, w.Title)
				}
				if got[i].URL != w.URL {
					t.Errorf("result[%d].URL = %q, want %q", i, got[i].URL, w.URL)
				}
				if got[i].Snippet != w.Snippet {
					t.Errorf("result[%d].Snippet = %q, want %q", i, got[i].Snippet, w.Snippet)
				}
			}
		})
	}
}

func TestUnwrapDuckDuckGoRedirect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		href string
		want string
	}{
		{name: "direct url passes through", href: "https://go.dev/", want: "https://go.dev/"},
		{name: "redirect uddg is unwrapped", href: "//duckduckgo.com/l/?uddg=https%3A%2F%2Fgithub.com%2Fx&rut=z", want: "https://github.com/x"},
		{name: "https redirect uddg is unwrapped", href: "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa%20b", want: "https://example.com/a b"},
		{name: "redirect with no uddg returns empty", href: "//duckduckgo.com/l/?rut=z", want: ""},
		{name: "garbage returns as-is", href: "not a url", want: "not a url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := unwrapDuckDuckGoRedirect(tt.href)
			if got != tt.want {
				t.Errorf("unwrapDuckDuckGoRedirect(%q) = %q, want %q", tt.href, got, tt.want)
			}
		})
	}
}
