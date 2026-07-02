package llm

import (
	"fmt"
	"net/url"
	"strings"
)

// Model is a secret-free model descriptor: which model, which wire dialect
// reaches it, where to reach it, its provenance, its local gating capabilities,
// and its default sampling. It deliberately omits the API key (a secret) and the
// system prompt (a per-agent concern) — those live on Request and the
// Authenticator, never on a Model. Call Validate at the trust boundary before use.
type Model struct {
	Provider  Provider
	APIFormat APIFormat // which codec dialect speaks to this model
	BaseURL   string
	Name      string       // provider-specific model id sent on the wire
	Origin    Origin       // provenance; zero value = OriginCustom (fail-safe)
	Caps      Capabilities // local gating data, never sent on the wire
	Sampling  Sampling     // default sampling; per-call overrides live on Request.Override
}

// Validate checks the descriptor at the trust boundary, returning a
// *ValidationError on the first rule violated. Rules:
//   - Provider must be a known backend AND must speak the given APIFormat
//     (fail-closed: an unclassified provider or an unsupported pair is rejected).
//   - Name must be non-empty.
//   - BaseURL must parse as https, with one exception: http is permitted only for
//     a loopback host (127.0.0.1 or localhost), so a local dev endpoint works
//     while a plaintext remote endpoint is refused.
//
// OriginCustom models validate identically to catalog rows; the lower trust in a
// custom model's Caps is a downstream gating concern, not Validate's.
func (m Model) Validate() error {
	// RequiredAuth is the canonical provider registry: it errors on any provider
	// not yet classified there, which is exactly "unknown provider" here.
	if _, err := m.Provider.RequiredAuth(); err != nil {
		return &ValidationError{Field: "Provider", Reason: fmt.Sprintf("unknown provider %q", m.Provider)}
	}
	if !m.Provider.supportsAPIFormat(m.APIFormat) {
		return &ValidationError{
			Field:  "APIFormat",
			Reason: fmt.Sprintf("provider %q does not support API format %q", m.Provider, m.APIFormat),
		}
	}
	if m.Name == "" {
		return &ValidationError{Field: "Name", Reason: "model name must not be empty"}
	}
	// An empty BaseURL means "use the provider's canonical endpoint": valid for any
	// provider with a default endpoint the SDK supplies, or region-routed Bedrock. A
	// non-empty base is always validated below. Fail-closed for a provider with no
	// default.
	if m.BaseURL == "" && m.Provider.allowsEmptyBaseURL() {
		return nil
	}
	if err := validateHTTPBaseURL(m.BaseURL); err != nil {
		return err
	}
	return nil
}

// URL-validation constants for validateHTTPBaseURL: the two permitted schemes
// and the loopback hosts for which the plaintext-http exception applies.
const (
	schemeHTTPS    = "https"
	schemeHTTP     = "http"
	hostLoopbackV4 = "127.0.0.1"
	hostLocalhost  = "localhost"
	hostLoopbackV6 = "::1"
)

// validateHTTPBaseURL enforces the HTTP-BaseURL provider rule (all current
// providers are HTTP-BaseURL): the URL must be a non-empty, host-bearing https
// URL with no embedded userinfo, except that http is allowed only for a loopback
// host (127.0.0.1, ::1, or localhost, case-insensitive).
func validateHTTPBaseURL(raw string) *ValidationError {
	if raw == "" {
		return &ValidationError{Field: "BaseURL", Reason: "must not be empty"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return &ValidationError{Field: "BaseURL", Reason: "must be a valid URL"}
	}
	// A credential embedded in the URL would violate the secret-free Model
	// invariant and could leak into logs, so reject any userinfo outright.
	if u.User != nil {
		return &ValidationError{Field: "BaseURL", Reason: "must not contain userinfo credentials"}
	}
	if u.Host == "" {
		return &ValidationError{Field: "BaseURL", Reason: "must include a host"}
	}
	switch u.Scheme {
	case schemeHTTPS:
		return nil
	case schemeHTTP:
		switch strings.ToLower(u.Hostname()) {
		case hostLoopbackV4, hostLoopbackV6, hostLocalhost:
			return nil
		}
		return &ValidationError{Field: "BaseURL", Reason: "http scheme is permitted only for a loopback host (127.0.0.1, ::1, or localhost)"}
	default:
		return &ValidationError{Field: "BaseURL", Reason: "scheme must be https (http allowed only for a loopback host)"}
	}
}

// ModelOption mutates a Model built by CustomModel. Because CustomModel defaults
// every capability off (fail-safe), an option is the only way to opt one in.
type ModelOption func(*Model)

// CustomModel builds a user-asserted Model: it forces the four wire-relevant
// fields — provider, API format, endpoint, and model name — and leaves everything
// else at its fail-safe zero value (Origin OriginCustom, all Capabilities false,
// MaxContext 0, empty Sampling) unless an option opts in. The result is still
// subject to Validate before use.
func CustomModel(p Provider, f APIFormat, baseURL, name string, opts ...ModelOption) Model {
	m := Model{
		Provider:  p,
		APIFormat: f,
		BaseURL:   baseURL,
		Name:      name,
		// Origin left zero (OriginCustom); Caps left zero (all-false); Sampling zero.
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// WithMaxContext sets the model's advertised maximum context window (tokens).
func WithMaxContext(n int) ModelOption { return func(m *Model) { m.Caps.MaxContext = n } }

// WithTools marks the model as tool-capable.
func WithTools() ModelOption { return func(m *Model) { m.Caps.Tools = true } }

// WithImages marks the model as accepting image inputs.
func WithImages() ModelOption { return func(m *Model) { m.Caps.AcceptsImages = true } }

// WithThinking marks the model as supporting extended thinking.
func WithThinking() ModelOption { return func(m *Model) { m.Caps.Thinking = true } }

// WithSampling sets the model's default sampling. The argument is deep-copied so
// the Model never aliases the caller's pointer/slice state.
func WithSampling(s Sampling) ModelOption { return func(m *Model) { m.Sampling = s.Clone() } }

// cloneFloat64Ptr returns a fresh pointer to a copy of *p, or nil when p is nil.
// Concrete (not generic) to honor the repo rule against `any` outside
// serialization/plugin boundaries.
func cloneFloat64Ptr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// cloneIntPtr returns a fresh pointer to a copy of *p, or nil when p is nil.
func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
