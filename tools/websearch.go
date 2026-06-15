package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/inventivepotter/urvi/internal/tool"
)

// websearch.go implements the WebSearch tool and its provider seam: the
// SearchProvider interface (Dependency Inversion — the tool depends on the
// abstraction, not a concrete scraper) and the SearchResult value the provider
// yields (design §4b, row WebSearch).
//
// LEAST PRIVILEGE: WebSearch takes only a SearchProvider — NO filesystem access.
//
// The tool itself is a thin formatter: it validates the query, clamps the result
// count, calls the injected provider under the caller's context, and formats the
// results (or the provider's error) into a tool result. The concrete network +
// HTML-parsing concern lives entirely in the provider (duckduckgo.go), so the
// tool can be unit-tested with a fake provider and the provider's parser can be
// unit-tested against static HTML — neither needs the network.
//
// SECURITY — log events, not secrets: the query is the only thing the user
// approves, so AuditSummary surfaces exactly the query (and nothing else).
//
// Failure model: a parse error, an empty query, or a provider error is a
// tool-result error STRING — InvokableRun never returns a Go error.

// webSearchToolName is the EXACT tool name classifyTool keys on for the network
// class — it MUST equal "WebSearch" (check.go's toolWebSearch).
const webSearchToolName = toolWebSearch

// defaultWebSearchResults is the result count used when the caller omits (or
// supplies a non-positive) results field.
const defaultWebSearchResults = 5

// maxWebSearchResults caps how many results WebSearch will return so a single
// search cannot flood the model context (design §4b: results ≤10).
const maxWebSearchResults = 10

const webSearchSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "The search query."},
    "results": {"type": "integer", "minimum": 1, "maximum": 10, "description": "Maximum number of results to return (optional; default 5, hard cap 10)."}
  },
  "required": ["query"]
}`

const webSearchDesc = "Search the web and return a list of result titles, URLs, and snippets (default 5, max 10). Has no filesystem access. Requires approval before each search."

// SearchResult is one web search hit: a human title, the result URL, and a short
// snippet. It is the provider-agnostic value the SearchProvider yields and the
// WebSearch tool formats.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// SearchProvider is the seam between the WebSearch tool and a concrete search
// backend (DuckDuckGo today; pluggable tomorrow). Search runs under ctx (the
// implementation MUST honor its deadline/cancellation), takes the query and a
// caller-validated max (already clamped to (0, maxWebSearchResults]), and returns
// up to max results or a typed error. An implementation must never panic on a
// malformed upstream response — it returns what it could parse.
type SearchProvider interface {
	Search(ctx context.Context, query string, max int) ([]SearchResult, error)
}

// webSearchArgs is the typed decode of WebSearch's untrusted argsJSON.
type webSearchArgs struct {
	Query   string `json:"query"`
	Results int    `json:"results"`
}

// WebSearch performs a web search via an injected SearchProvider. It has no
// filesystem access (least privilege).
type WebSearch struct {
	provider SearchProvider
}

// NewWebSearch constructs a WebSearch tool bound to a SearchProvider.
func NewWebSearch(provider SearchProvider) *WebSearch {
	return &WebSearch{provider: provider}
}

// Info returns WebSearch's self-description. Name MUST equal "WebSearch".
func (w *WebSearch) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   webSearchToolName,
		Desc:   webSearchDesc,
		Schema: json.RawMessage(webSearchSchema),
	}, nil
}

// AuditSummary returns "WebSearch: <query>" — the query is exactly what the user
// approves at the gate, so it is the right (and only) summary. An unparseable
// args document yields a generic summary.
func (w *WebSearch) AuditSummary(argsJSON string) string {
	var a webSearchArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Query == "" {
		return "WebSearch (unparsable args)"
	}
	return "WebSearch: " + a.Query
}

// BuildRequest derives the approval prompt: the query string. An unparseable args
// document or an empty query is a typed error so the runner treats the call as
// invalid.
func (w *WebSearch) BuildRequest(argsJSON string) (tool.PermissionRequest, error) {
	a, err := parseWebSearchArgs(argsJSON)
	if err != nil {
		return nil, err
	}
	return tool.WebSearchRequest{Query: a.Query}, nil
}

// InvokableRun validates the args, clamps the result count, calls the provider
// under ctx, and formats the results. A parse error, empty query, or provider
// error is a tool-result error STRING; it never returns a Go error.
func (w *WebSearch) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	a, err := parseWebSearchArgs(argsJSON)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	max := clampWebSearchResults(a.Results)

	results, err := w.provider.Search(ctx, a.Query, max)
	if err != nil {
		// Surface a generic provider failure — never echo upstream internals that
		// might embed request details.
		return tool.TextResult("error: web search failed"), nil
	}
	return tool.TextResult(formatSearchResults(results)), nil
}

// parseWebSearchArgs decodes + validates the args. A non-object document or an
// empty query is a typed *webSearchError.
func parseWebSearchArgs(argsJSON string) (webSearchArgs, error) {
	var a webSearchArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return webSearchArgs{}, &webSearchError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if strings.TrimSpace(a.Query) == "" {
		return webSearchArgs{}, &webSearchError{reason: "a non-empty 'query' is required"}
	}
	return a, nil
}

// clampWebSearchResults maps a caller-supplied count into (0, maxWebSearchResults]:
// ≤0 → default; otherwise min(count, max).
func clampWebSearchResults(n int) int {
	if n <= 0 {
		return defaultWebSearchResults
	}
	if n > maxWebSearchResults {
		return maxWebSearchResults
	}
	return n
}

// formatSearchResults renders the results as a numbered list of title / URL /
// snippet. An empty list yields a friendly "no results" message.
func formatSearchResults(results []SearchResult) string {
	if len(results) == 0 {
		return "No results found."
	}
	var sb strings.Builder
	for i, r := range results {
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString(". ")
		sb.WriteString(r.Title)
		sb.WriteString("\n   ")
		sb.WriteString(r.URL)
		if r.Snippet != "" {
			sb.WriteString("\n   ")
			sb.WriteString(r.Snippet)
		}
		if i < len(results)-1 {
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// webSearchError is the typed failure for WebSearch arg parsing/validation. It
// carries a non-secret reason; InvokableRun maps every failure to a tool-result
// string, BuildRequest returns it so the runner treats the call as invalid.
type webSearchError struct {
	reason string
	cause  error
}

func (e *webSearchError) Error() string { return e.reason }

func (e *webSearchError) Unwrap() error { return e.cause }

// compile-time assertions: WebSearch is an InvokableTool, a PermissionPrompter
// (Ask), and Auditable. It is NOT a WriteTarget.
var (
	_ tool.InvokableTool      = (*WebSearch)(nil)
	_ tool.PermissionPrompter = (*WebSearch)(nil)
	_ tool.Auditable          = (*WebSearch)(nil)
)
