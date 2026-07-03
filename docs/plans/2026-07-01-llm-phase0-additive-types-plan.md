# pkg/llm Phase 0 — Additive Types & Auth Seam — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Introduce the new codec/auth/model-descriptor symbols from the layout design as pure additions — new declarations only, zero edits to `ModelSpec`/`Model`/`Request` — so nothing existing breaks and Phase 1 can reshape in one coordinated change.

**Architecture:** Everything here is a *new symbol* in package `llm` (or the new `pkg/llm/auth` sub-package). No existing struct gains a field; no call site changes behavior. The one edit to an existing type is an *additive method* (`Provider.RequiredAuth()`) that sits beside `RequiresKey()`. Design source: `docs/plans/2026-07-01-llm-provider-codec-layout-design.md` (§ Interfaces, § ModelSpec redesign, § Auth enforcement, § Migration → Phase 0).

**Tech Stack:** Go (module `github.com/looprig/harness`), stdlib only (`context`, `net/http`, `fmt`, `errors`). Table-driven tests with `t.Parallel()`, run under `-race`. Typed errors per `CLAUDE.md`.

**Scope guardrails:**
- **Do NOT touch** `pkg/llm/model.go` (`Model`), the `ModelSpec` struct, or `Request`/`Response` in `pkg/llm/llm.go`. Those reshape in Phase 1.
- **Do NOT move** `pkg/llm/openaiapi` (that is Phase 0.5).
- `auth.SigV4(...)` (real AWS signing) is **deferred to Phase 2 / Bedrock** — Phase 0 ships the `SigV4Credentials` type and the `AuthSigV4` kind (data only), not the signer. If you want the signer now, stop and confirm with the user first (YAGNI: no Bedrock provider exists yet).

**Before you start:** work on a branch, not `main`.
```bash
git checkout -b feat/llm-phase0-additive-types
git status   # expect clean
```
Every commit message ends with the trailer:
```
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
```

---

### Task 1: `APIFormat` type

**Files:**
- Create: `pkg/llm/apiformat.go`
- Test: `pkg/llm/apiformat_test.go`

**Step 1 — Write the failing test** (`pkg/llm/apiformat_test.go`):
```go
package llm

import "testing"

func TestAPIFormatValid(t *testing.T) {
	tests := []struct {
		name string
		f    APIFormat
		want bool
	}{
		{name: "openai", f: APIFormatOpenAI, want: true},
		{name: "anthropic", f: APIFormatAnthropic, want: true},
		{name: "bedrock converse", f: APIFormatBedrockConverse, want: true},
		{name: "empty is invalid", f: "", want: false},
		{name: "unknown is invalid", f: "gemini", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.f.Valid(); got != tt.want {
				t.Errorf("APIFormat(%q).Valid() = %v, want %v", tt.f, got, tt.want)
			}
		})
	}
}
```

**Step 2 — Run, expect FAIL** (undefined `APIFormat`):
`go test -race ./pkg/llm/ -run TestAPIFormatValid -v`

**Step 3 — Implement** (`pkg/llm/apiformat.go`):
```go
package llm

// APIFormat names the wire dialect a model endpoint speaks. It is data carried on a
// model/catalog row, not structure: one codec implementation per value. See the layout design.
type APIFormat string

const (
	APIFormatOpenAI          APIFormat = "openai"
	APIFormatAnthropic       APIFormat = "anthropic"
	APIFormatBedrockConverse APIFormat = "bedrock-converse"
)

// Valid reports whether f is a known wire dialect.
func (f APIFormat) Valid() bool {
	switch f {
	case APIFormatOpenAI, APIFormatAnthropic, APIFormatBedrockConverse:
		return true
	default:
		return false
	}
}
```

**Step 4 — Run, expect PASS.** **Step 5 — Commit:**
```bash
git add pkg/llm/apiformat.go pkg/llm/apiformat_test.go
git commit -m "feat(llm): add APIFormat wire-dialect type"
```

---

### Task 2: `Effort` type (canonical, alongside existing `ReasoningEffort`)

**Files:** Create `pkg/llm/effort.go`; Test `pkg/llm/effort_test.go`.

> Note: `ReasoningEffort` already exists in `llm.go`. `Effort` is the new dialect-neutral intent; both coexist in Phase 0. Do not delete `ReasoningEffort`.

**Step 1 — Failing test:**
```go
package llm

import "testing"

func TestEffortValid(t *testing.T) {
	tests := []struct {
		name string
		e    Effort
		want bool
	}{
		{name: "none/empty", e: EffortNone, want: true},
		{name: "low", e: EffortLow, want: true},
		{name: "medium", e: EffortMedium, want: true},
		{name: "high", e: EffortHigh, want: true},
		{name: "max", e: EffortMax, want: true},
		{name: "unknown", e: "xhigh", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.e.Valid(); got != tt.want {
				t.Errorf("Effort(%q).Valid() = %v, want %v", tt.e, got, tt.want)
			}
		})
	}
}
```

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement** (`pkg/llm/effort.go`):
```go
package llm

// Effort is dialect-neutral "how hard to think" intent. Each codec maps it to its wire
// mechanism (openaiapi → reasoning_effort; anthropicapi → adaptive thinking + effort). Zero
// value (EffortNone) means the model decides / thinking off.
type Effort string

const (
	EffortNone   Effort = ""
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortMax    Effort = "max"
)

// Valid reports whether e is a known effort level (the empty value is valid = unset).
func (e Effort) Valid() bool {
	switch e {
	case EffortNone, EffortLow, EffortMedium, EffortHigh, EffortMax:
		return true
	default:
		return false
	}
}
```

**Step 4 — Run, expect PASS.** **Step 5 — Commit:**
```bash
git add pkg/llm/effort.go pkg/llm/effort_test.go
git commit -m "feat(llm): add canonical Effort intent type"
```

---

### Task 3: `Sampling` + `Capabilities` value types

**Files:** Create `pkg/llm/sampling.go`, `pkg/llm/capabilities.go`; Test `pkg/llm/sampling_test.go`.

> `Sampling.Clone()` deep-copies pointer fields so a returned value never aliases a shared catalog row — reuse the existing `cloneFloat64Ptr`/`cloneIntPtr` helpers in `model.go`. `Capabilities` is plain data (no logic → no dedicated test).

**Step 1 — Failing test** (`sampling_test.go`):
```go
package llm

import "testing"

func TestSamplingCloneIsDeep(t *testing.T) {
	t.Parallel()
	temp := 0.7
	max := 4096
	orig := Sampling{Temperature: &temp, MaxTokens: &max, Stop: []string{"a"}, Effort: EffortHigh}

	clone := orig.Clone()
	*clone.Temperature = 0.1
	*clone.MaxTokens = 1
	clone.Stop[0] = "b"

	if *orig.Temperature != 0.7 {
		t.Errorf("clone mutated original Temperature: got %v", *orig.Temperature)
	}
	if *orig.MaxTokens != 4096 {
		t.Errorf("clone mutated original MaxTokens: got %v", *orig.MaxTokens)
	}
	if orig.Stop[0] != "a" {
		t.Errorf("clone mutated original Stop: got %v", orig.Stop[0])
	}
}

func TestSamplingCloneNilSafe(t *testing.T) {
	t.Parallel()
	clone := Sampling{Effort: EffortLow}.Clone()
	if clone.Temperature != nil || clone.MaxTokens != nil || clone.Stop != nil {
		t.Errorf("nil fields should clone to nil, got %+v", clone)
	}
	if clone.Effort != EffortLow {
		t.Errorf("Effort not preserved: got %q", clone.Effort)
	}
}
```

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement.** `pkg/llm/capabilities.go`:
```go
package llm

// Capabilities is secret-free gating/informational data about a model: never serialized onto
// the wire, read locally (e.g. a TUI deciding whether to allow image attachments).
type Capabilities struct {
	AcceptsImages bool
	MaxContext    int
	Tools         bool
	Thinking      bool
}
```
`pkg/llm/sampling.go`:
```go
package llm

// Sampling is dialect-neutral sampling intent. Each Codec maps it to its wire mechanism; the
// dialect-specific validity rules (e.g. Anthropic's Temperature==1.0 for thinking) live in the
// codec, not here.
type Sampling struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	Stop        []string
	Effort      Effort
}

// Clone returns a deep copy: pointer and slice fields are duplicated so the result never aliases
// the receiver's state (reuses cloneFloat64Ptr/cloneIntPtr from model.go).
func (s Sampling) Clone() Sampling {
	out := s
	out.Temperature = cloneFloat64Ptr(s.Temperature)
	out.TopP = cloneFloat64Ptr(s.TopP)
	out.MaxTokens = cloneIntPtr(s.MaxTokens)
	if s.Stop != nil {
		out.Stop = append([]string(nil), s.Stop...)
	}
	return out
}
```

**Step 4 — Run, expect PASS.** **Step 5 — Commit:**
```bash
git add pkg/llm/sampling.go pkg/llm/capabilities.go pkg/llm/sampling_test.go
git commit -m "feat(llm): add Sampling (deep-clonable) and Capabilities value types"
```

---

### Task 4: `Origin` + `RequestMode` enums

**Files:** Create `pkg/llm/origin.go`, `pkg/llm/requestmode.go`; Test `pkg/llm/origin_test.go`.

> `Origin`'s zero value must be `OriginCustom` (fail-safe: a bare `Model{}` is treated as user-asserted, not verified).

**Step 1 — Failing test** (`origin_test.go`):
```go
package llm

import "testing"

func TestOriginZeroValueIsCustom(t *testing.T) {
	t.Parallel()
	var o Origin
	if o != OriginCustom {
		t.Fatalf("zero Origin = %v, want OriginCustom (fail-safe)", o)
	}
}

func TestOriginString(t *testing.T) {
	tests := []struct {
		name string
		o    Origin
		want string
	}{
		{name: "custom", o: OriginCustom, want: "custom"},
		{name: "catalog", o: OriginCatalog, want: "catalog"},
		{name: "unknown falls back to custom", o: Origin(99), want: "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.o.String(); got != tt.want {
				t.Errorf("Origin(%d).String() = %q, want %q", tt.o, got, tt.want)
			}
		})
	}
}
```

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement.** `pkg/llm/origin.go`:
```go
package llm

// Origin is a model descriptor's provenance. The zero value is OriginCustom (fail-safe): a raw
// Model{} literal is treated as user-asserted, so gating stays conservative until proven curated.
type Origin uint8

const (
	OriginCustom  Origin = iota // user-supplied; capabilities are asserted, not verified
	OriginCatalog               // curated row; capabilities are trusted
)

func (o Origin) String() string {
	if o == OriginCatalog {
		return "catalog"
	}
	return "custom"
}
```
`pkg/llm/requestmode.go`:
```go
package llm

// RequestMode is the typed encode mode a Codec receives: the wire body differs because
// streaming sets "stream": true. Typed, not a bool, per CLAUDE.md.
type RequestMode uint8

const (
	RequestModeInvoke RequestMode = iota
	RequestModeStream
)
```

**Step 4 — Run, expect PASS.** **Step 5 — Commit:**
```bash
git add pkg/llm/origin.go pkg/llm/requestmode.go pkg/llm/origin_test.go
git commit -m "feat(llm): add Origin provenance and RequestMode enums"
```

---

### Task 5: `Codec` and `Authenticator` interfaces

**Files:** Create `pkg/llm/codec.go`, `pkg/llm/authenticator.go`; Test `pkg/llm/interfaces_test.go`.

> These are declarations only; nothing implements `Codec` yet (openaiapi is adapted in Phase 0.5/1). Prove they compile and are satisfiable with a fake.

**Step 1 — Failing test** (`interfaces_test.go`):
```go
package llm

import (
	"context"
	"net/http"
	"testing"

	"github.com/looprig/harness/pkg/content"
)

type fakeCodec struct{}

func (fakeCodec) EncodeRequest(Request, RequestMode) ([]byte, error)   { return []byte("{}"), nil }
func (fakeCodec) DecodeResponse([]byte) (*Response, error)             { return &Response{}, nil }
func (fakeCodec) DecodeEvent([]byte) ([]content.Chunk, error)          { return nil, nil }

type fakeAuth struct{ called bool }

func (f *fakeAuth) Authorize(_ context.Context, r *http.Request) error {
	f.called = true
	r.Header.Set("X-Test", "1")
	return nil
}

func TestCodecSatisfiable(t *testing.T) {
	t.Parallel()
	var _ Codec = fakeCodec{} // compile-time assertion
}

func TestAuthenticatorSatisfiable(t *testing.T) {
	t.Parallel()
	var a Authenticator = &fakeAuth{}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test", nil)
	if err := a.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize error: %v", err)
	}
	if req.Header.Get("X-Test") != "1" {
		t.Errorf("Authorize did not mutate request header")
	}
}
```

**Step 2 — Run, expect FAIL** (undefined `Codec`/`Authenticator`).

**Step 3 — Implement.** `pkg/llm/codec.go`:
```go
package llm

import "github.com/looprig/harness/pkg/content"

// Codec owns one wire dialect's JSON + stream-event semantics. It does NOT own wire framing:
// the transport de-frames the response (SSE / AWS eventstream, via the shared codec/sse helper)
// and hands the codec one already-de-framed event payload at a time.
type Codec interface {
	EncodeRequest(req Request, mode RequestMode) ([]byte, error) // typed mode, not a bool
	DecodeResponse(body []byte) (*Response, error)               // non-streaming body → Response
	DecodeEvent(event []byte) ([]content.Chunk, error)           // one de-framed stream event → chunks
}
```
`pkg/llm/authenticator.go`:
```go
package llm

import (
	"context"
	"net/http"
)

// Authenticator mutates an outbound request to carry credentials. Orthogonal to dialect.
// Implementations live in pkg/llm/auth (Key/Header/None; SigV4 lands with Bedrock).
type Authenticator interface {
	Authorize(ctx context.Context, r *http.Request) error
}
```

**Step 4 — Run, expect PASS.** **Step 5 — Commit:**
```bash
git add pkg/llm/codec.go pkg/llm/authenticator.go pkg/llm/interfaces_test.go
git commit -m "feat(llm): add Codec and Authenticator interfaces"
```

---

### Task 6: `AuthKind` enum + `AuthRequiredError` + `ModelMismatchError`

**Files:** Modify `pkg/llm/errors.go` (append); Test `pkg/llm/errors_test.go` (append or new `autherrors_test.go`).

> Put `AuthKind` here too (it is used by both the error and `Provider.RequiredAuth` in Task 8). Errors must be typed structs with `Error()` and be `errors.As`-able, per `CLAUDE.md`. Never include secrets in the message.

**Step 1 — Failing test** (`pkg/llm/autherrors_test.go`):
```go
package llm

import (
	"errors"
	"strings"
	"testing"
)

func TestAuthRequiredError(t *testing.T) {
	t.Parallel()
	err := error(&AuthRequiredError{Provider: ProviderPhala, Kind: AuthAPIKey})
	var are *AuthRequiredError
	if !errors.As(err, &are) {
		t.Fatalf("errors.As failed for *AuthRequiredError")
	}
	if are.Provider != ProviderPhala || are.Kind != AuthAPIKey {
		t.Errorf("fields not preserved: %+v", are)
	}
	if msg := err.Error(); !strings.Contains(msg, "phala") || !strings.Contains(msg, string(AuthAPIKey)) {
		t.Errorf("message missing provider/kind: %q", msg)
	}
}

func TestModelMismatchError(t *testing.T) {
	t.Parallel()
	err := error(&ModelMismatchError{
		BoundProvider: ProviderPhala, RequestProvider: ProviderChutes,
		BoundEndpoint: "https://a", RequestEndpoint: "https://b",
	})
	var mme *ModelMismatchError
	if !errors.As(err, &mme) {
		t.Fatalf("errors.As failed for *ModelMismatchError")
	}
	if mme.RequestProvider != ProviderChutes || mme.BoundProvider != ProviderPhala {
		t.Errorf("fields not preserved: %+v", mme)
	}
	if err.Error() == "" {
		t.Errorf("empty error message")
	}
}
```

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement** (append to `pkg/llm/errors.go`):
```go
// AuthKind classifies the credential a provider requires. Multi-auth-ready successor to the
// boolean Provider.RequiresKey.
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthAPIKey AuthKind = "api_key"
	AuthSigV4  AuthKind = "sigv4"
)

// AuthRequiredError is returned by the runtime factory when a provider that requires credentials
// is given none. Fail-closed. Carries no secret.
type AuthRequiredError struct {
	Provider Provider
	Kind     AuthKind
}

func (e *AuthRequiredError) Error() string {
	return fmt.Sprintf("provider %q requires %s credentials", e.Provider, e.Kind)
}

// ModelMismatchError is returned before any network I/O when a Request's model names a
// provider/endpoint that differs from the connection the client is bound to. Fail-closed.
type ModelMismatchError struct {
	BoundProvider   Provider
	RequestProvider Provider
	BoundEndpoint   string
	RequestEndpoint string
}

func (e *ModelMismatchError) Error() string {
	return fmt.Sprintf("request model provider %q/endpoint %q does not match bound client %q/%q",
		e.RequestProvider, e.RequestEndpoint, e.BoundProvider, e.BoundEndpoint)
}
```
If `errors.go` does not already import `fmt`, add it.

**Step 4 — Run, expect PASS** (`go test -race ./pkg/llm/ -run 'TestAuthRequiredError|TestModelMismatchError' -v`).

**Step 5 — Commit:**
```bash
git add pkg/llm/errors.go pkg/llm/autherrors_test.go
git commit -m "feat(llm): add AuthKind, AuthRequiredError, ModelMismatchError"
```

---

### Task 7: `pkg/llm/auth` package (`Key` / `Header` / `None` + credential types)

**Files:** Create `pkg/llm/auth/auth.go`; Test `pkg/llm/auth/auth_test.go`.

> `auth` imports `llm` (for the `Authenticator` interface); `llm` must NOT import `auth` (would cycle). The single `APIKey` here is a **type**; the constructor is `Key` — no type-vs-func clash (the review's #1). `SigV4Credentials` is defined (data) but the `SigV4` signer is **deferred to Phase 2** — do not implement AWS signing now.

**Step 1 — Failing test** (`pkg/llm/auth/auth_test.go`):
```go
package auth

import (
	"context"
	"net/http"
	"testing"
)

func TestKeySetsBearer(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest(http.MethodPost, "https://x.test", nil)
	if err := Key("sekret").Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sekret")
	}
}

func TestHeaderSetsCustom(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest(http.MethodPost, "https://x.test", nil)
	if err := Header("sekret", "x-api-key").Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "sekret" {
		t.Errorf("x-api-key = %q, want %q", got, "sekret")
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Header must not set Authorization")
	}
}

func TestNoneIsNoop(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest(http.MethodPost, "https://x.test", nil)
	if err := None().Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if len(req.Header) != 0 {
		t.Errorf("None must not mutate headers, got %v", req.Header)
	}
}
```

**Step 2 — Run, expect FAIL:** `go test -race ./pkg/llm/auth/ -v`

**Step 3 — Implement** (`pkg/llm/auth/auth.go`):
```go
// Package auth provides Authenticator implementations for the llm client seam. It imports llm
// for the interface; llm never imports auth.
package auth

import (
	"context"
	"net/http"

	"github.com/looprig/harness/pkg/llm"
)

// APIKey is a bearer/API-key secret. Named type so a base URL cannot be passed where a key
// belongs, and so provider constructors can demand it at compile time.
type APIKey string

// SigV4Credentials is an AWS credential set for the (Phase 2) Bedrock signer. Defined now so the
// AuthKind/AuthRequiredError surface is complete; the SigV4 constructor lands with Bedrock.
type SigV4Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

type headerAuth struct{ name, value string }

func (h headerAuth) Authorize(_ context.Context, r *http.Request) error {
	r.Header.Set(h.name, h.value)
	return nil
}

// Key returns an Authenticator that sets "Authorization: Bearer <k>".
func Key(k APIKey) llm.Authenticator {
	return headerAuth{name: "Authorization", value: "Bearer " + string(k)}
}

// Header returns an Authenticator that sets an arbitrary header (e.g. "x-api-key") to the key.
func Header(k APIKey, name string) llm.Authenticator {
	return headerAuth{name: name, value: string(k)}
}

type noneAuth struct{}

func (noneAuth) Authorize(context.Context, *http.Request) error { return nil }

// None returns an Authenticator that adds no credentials — the explicit, visible "no auth"
// value (never a zero-value default).
func None() llm.Authenticator { return noneAuth{} }
```

**Step 4 — Run, expect PASS.** **Step 5 — Commit:**
```bash
git add pkg/llm/auth/auth.go pkg/llm/auth/auth_test.go
git commit -m "feat(llm/auth): add Key/Header/None authenticators and credential types"
```

---

### Task 8: `Provider.RequiredAuth()` (additive, beside `RequiresKey`)

**Files:** Modify `pkg/llm/provider.go` (append method); Test `pkg/llm/provider_test.go` (append).

> Additive only — do not remove or change `RequiresKey`. Mirror its provider classification and its fail-closed default (unknown provider → typed error, never a permissive fall-through).

**Step 1 — Failing test** (append to `pkg/llm/provider_test.go`):
```go
func TestProviderRequiredAuth(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		want     AuthKind
		wantErr  bool
	}{
		{name: "lmstudio needs none", provider: ProviderLMStudio, want: AuthNone},
		{name: "phala needs api key", provider: ProviderPhala, want: AuthAPIKey},
		{name: "chutes needs api key", provider: ProviderChutes, want: AuthAPIKey},
		{name: "empty is error", provider: "", wantErr: true},
		{name: "unknown is error", provider: "bedrock", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.provider.RequiredAuth()
			if (err != nil) != tt.wantErr {
				t.Fatalf("RequiredAuth() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("RequiredAuth() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement** (append to `pkg/llm/provider.go`):
```go
// RequiredAuth reports which credential kind the provider needs, erroring on an unknown provider
// so a newly added one must be classified here before use. Multi-auth-ready successor to
// RequiresKey; fail-closed by the same rationale (a permissive default would fail open).
func (p Provider) RequiredAuth() (AuthKind, error) {
	switch p {
	case ProviderLMStudio:
		return AuthNone, nil
	case ProviderPhala, ProviderChutes:
		return AuthAPIKey, nil
	default:
		return "", &ValidationError{Field: "Provider", Reason: "unknown provider; auth policy undefined"}
	}
}
```

**Step 4 — Run, expect PASS.** **Step 5 — Commit:**
```bash
git add pkg/llm/provider.go pkg/llm/provider_test.go
git commit -m "feat(llm): add Provider.RequiredAuth alongside RequiresKey"
```

---

### Task 9: Full verification & non-breaking proof

**No new files.** Prove the whole module still builds, all tests pass under `-race`, and the security gate is green — confirming Phase 0 changed no existing behavior.

**Step 1 — Build the whole module:**
```bash
CGO_ENABLED=0 go build -trimpath ./...
```
Expected: no output (success).

**Step 2 — Full test suite under race:**
```bash
go test -race ./...
```
Expected: all `ok`, including pre-existing packages (nothing broke — Phase 0 added symbols only).

**Step 3 — Format + security gate:**
```bash
make fmt-check
make secure
```
Expected: clean (gofmt + vet + staticcheck + gosec + govulncheck all pass).

**Step 4 — Confirm no existing struct changed** (self-audit — expect no diff to these definitions):
```bash
git diff main -- pkg/llm/model.go pkg/llm/llm.go | grep -E '^\+' | grep -vE '^\+\+\+' || echo "no additions to model.go/llm.go — correct"
```
Expected: `no additions to model.go/llm.go — correct` (Phase 0 must not touch `Model`, `ModelSpec`, `Request`, `Response`). If this prints diff lines, you edited an existing struct — revert that; it belongs in Phase 1.

**Step 5 — Commit (if `make fmt` reformatted anything) & push branch:**
```bash
git add -A && git commit -m "chore(llm): phase 0 verification" --allow-empty
git push -u origin feat/llm-phase0-additive-types
```

---

## Done criteria

- New symbols exist and are tested: `APIFormat`, `Effort`, `Sampling` (+`Clone`), `Capabilities`, `Origin`, `RequestMode`, `Codec`, `Authenticator`, `AuthKind`, `AuthRequiredError`, `ModelMismatchError`, `pkg/llm/auth` (`APIKey`, `SigV4Credentials`, `Key`, `Header`, `None`), `Provider.RequiredAuth`.
- `git diff main -- pkg/llm/model.go pkg/llm/llm.go` shows **no** changes to `Model`/`ModelSpec`/`Request`/`Response`.
- `go test -race ./...` and `make secure` are green.
- Deferred (not in this plan): `auth.SigV4` signer (Phase 2), the `openaiapi → codec/openaiapi` move (Phase 0.5), all reshapes and `loop.Config`/`FingerprintFrom` changes (Phase 1).
