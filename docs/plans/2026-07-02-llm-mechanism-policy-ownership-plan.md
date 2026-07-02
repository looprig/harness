# LLM mechanism/policy ownership Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make `looprig`'s `pkg/llm` ship provider *mechanism* only — move the model catalogue and the Phala attestation pins to the consumer (`swe`), own base URLs inside the clients, and make attestation fail *closed*.

**Architecture:** Three threads. **B (base URLs):** each provider client self-defaults its canonical endpoint; `Model.BaseURL == ""` means "use the default"; `Validate` and transport binding tolerate an empty base. **A (attestation):** an under-pinned `aci.Policy` is rejected at both public aci entries (`New`, `VerifyReport`); `phala.DefaultPolicy()` and its fixture-derived pins are deleted; `auto.New` dispatches Phala to a typed "construct directly" error, mirroring Bedrock. **Cleanup:** delete `catalog.go`; reword the `OriginCatalog` doc.

**Tech Stack:** Go (stdlib-first per CLAUDE.md), `-race` table-driven tests, `go.work` (build/test `looprig` with `GOWORK=off`).

**Design source:** `docs/plans/2026-07-02-llm-mechanism-policy-ownership-design.md` (Approved).

**Conventions for every task:**
- All test commands: `GOWORK=off go test -race <pkg> -run <Name> -v` (repo is `GOWORK=off` for vendored `looprig`).
- Tests are table-driven with `t.Parallel()` per CLAUDE.md.
- Commit messages use conventional-commit prefixes; end the body with the `Co-Authored-By` trailer from the harness rules.
- After each task the whole module must build (`GOWORK=off CGO_ENABLED=0 go build -trimpath ./...`) and the touched package's tests pass.

---

## Task 0: Feature branch

**Step 1: Branch off main (never commit this work to `main`).**

```bash
git checkout -b feat/llm-mechanism-policy-ownership
```

**Step 2: Confirm a clean baseline.**

Run: `GOWORK=off go build ./... && GOWORK=off go test -race ./pkg/llm/... 2>&1 | tail -5`
Expected: builds, tests pass.

---

## Task 1: `Provider.allowsEmptyBaseURL()` + generalized `Validate` (design B3)

**Files:**
- Modify: `pkg/llm/provider.go` (add predicate)
- Modify: `pkg/llm/model.go:55-57` (replace the Bedrock-only carve-out)
- Test: `pkg/llm/provider_test.go`, `pkg/llm/model_test.go`

**Step 1: Write the failing predicate test** in `provider_test.go`:

```go
func TestProviderAllowsEmptyBaseURL(t *testing.T) {
	tests := []struct {
		name string
		p    Provider
		want bool
	}{
		{"bedrock region-routed", ProviderBedrock, true},
		{"chutes self-defaults", ProviderChutes, true},
		{"phala self-defaults", ProviderPhala, true},
		{"openrouter self-defaults", ProviderOpenRouter, true},
		{"lmstudio self-defaults", ProviderLMStudio, true},
		{"google self-defaults", ProviderGoogle, true},
		{"unknown fails closed", Provider("nope"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.p.allowsEmptyBaseURL(); got != tt.want {
				t.Errorf("allowsEmptyBaseURL() = %v, want %v", got, tt.want)
			}
		})
	}
}
```

**Step 2: Run — expect FAIL** (`allowsEmptyBaseURL` undefined):
Run: `GOWORK=off go test -race ./pkg/llm/ -run TestProviderAllowsEmptyBaseURL -v`

**Step 3: Add the predicate** to `provider.go`:

```go
// allowsEmptyBaseURL reports whether an empty Model.BaseURL is acceptable for the
// provider — true when the provider is region-routed (Bedrock) or its client
// self-defaults a canonical endpoint (every current HTTP provider). Fail-closed: an
// unknown/future provider with no default returns false, so Validate keeps requiring
// an explicit base for it.
func (p Provider) allowsEmptyBaseURL() bool {
	switch p {
	case ProviderBedrock, ProviderChutes, ProviderPhala, ProviderOpenRouter, ProviderLMStudio, ProviderGoogle:
		return true
	default:
		return false
	}
}
```

**Step 4: Generalize `Validate`** — replace `model.go:55-57`:

```go
	if m.Provider == ProviderBedrock && m.BaseURL == "" {
		return nil
	}
```
with:
```go
	// An empty BaseURL means "use the provider's canonical endpoint": valid for any
	// provider whose client self-defaults or is region-routed. A non-empty base is
	// always validated below. Fail-closed for a provider with no default.
	if m.BaseURL == "" && m.Provider.allowsEmptyBaseURL() {
		return nil
	}
```

**Step 5: Extend `model_test.go`** — add cases asserting empty base now validates for chutes/openrouter/etc. and still fails for an unknown provider. Inspect existing `TestModelValidate` (or equivalent) and add rows; adjust any existing row that asserted a non-bedrock empty base is rejected.

**Step 6: Run — expect PASS:**
Run: `GOWORK=off go test -race ./pkg/llm/ -run 'TestProviderAllowsEmptyBaseURL|TestModelValidate' -v`

**Step 7: Commit:**
```bash
git add pkg/llm/provider.go pkg/llm/model.go pkg/llm/provider_test.go pkg/llm/model_test.go
git commit -m "feat(llm): allow empty BaseURL for self-defaulting providers"
```

---

## Task 2: transport `checkBinding` tolerates an empty request base (design B4)

**Files:**
- Modify: `pkg/llm/transport/client.go:162-172`
- Test: `pkg/llm/transport/client_test.go`

**Step 1: Write failing test** — a request whose `Model.BaseURL == ""` must bind to the client's endpoint; a non-empty mismatching base must still fail:

```go
func TestCheckBindingEmptyBase(t *testing.T) {
	c := New(stubCodec{}, Endpoint{Provider: llm.ProviderOpenRouter, BaseURL: "https://openrouter.ai/api/v1"}, auth.None())
	tests := []struct {
		name    string
		model   llm.Model
		wantErr bool
	}{
		{"empty base binds to endpoint", llm.Model{Provider: llm.ProviderOpenRouter, BaseURL: ""}, false},
		{"matching base ok", llm.Model{Provider: llm.ProviderOpenRouter, BaseURL: "https://openrouter.ai/api/v1"}, false},
		{"conflicting base fails", llm.Model{Provider: llm.ProviderOpenRouter, BaseURL: "https://evil.example"}, true},
		{"wrong provider fails", llm.Model{Provider: llm.ProviderChutes, BaseURL: ""}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := c.checkBinding(tt.model)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkBinding() err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}
```
(Reuse the package's existing test codec/stub; if none is exported for tests, use the smallest existing helper in `client_test.go`.)

**Step 2: Run — expect FAIL** (empty base currently mismatches):
Run: `GOWORK=off go test -race ./pkg/llm/transport/ -run TestCheckBindingEmptyBase -v`

**Step 3: Implement** — replace the condition in `checkBinding`:

```go
func (c *Client) checkBinding(m llm.Model) error {
	// An empty request base means "use the bound endpoint" (the Model.BaseURL
	// contract); only a non-empty base that disagrees with the binding is a
	// cross-wiring error.
	if m.Provider != c.ep.Provider || (m.BaseURL != "" && m.BaseURL != c.ep.BaseURL) {
		return &llm.ModelMismatchError{
			BoundProvider:   c.ep.Provider,
			RequestProvider: m.Provider,
			BoundEndpoint:   c.ep.BaseURL,
			RequestEndpoint: m.BaseURL,
		}
	}
	return nil
}
```

**Step 4: Run — expect PASS.** Then run the whole transport package: `GOWORK=off go test -race ./pkg/llm/transport/`

**Step 5: Commit:**
```bash
git add pkg/llm/transport/client.go pkg/llm/transport/client_test.go
git commit -m "feat(llm/transport): bind an empty request base to the client endpoint"
```

---

## Task 3: `chutes.New` self-defaults `apiBase` (design B2)

**Files:**
- Modify: `pkg/llm/providers/chutes/client.go` (const block ~31, `New` ~109)
- Test: `pkg/llm/providers/chutes/client_test.go`

**Step 1: Write failing test** — an empty `apiBase` resolves to the canonical host. Use the existing exported test seam if one exposes `apiBase`; otherwise assert indirectly. Minimal direct test via `export_test.go` accessor if present:

```go
func TestNewDefaultsAPIBase(t *testing.T) {
	c := New("", "sk-test")
	if got := c.APIBaseForTest(); got != "https://api.chutes.ai" {
		t.Errorf("apiBase = %q, want default", got)
	}
}
```
If `client.go` has no test accessor for `apiBase`, add one line to `export_test.go`: `func (c *Client) APIBaseForTest() string { return c.apiBase }`.

**Step 2: Run — expect FAIL** (`apiBase` stays `""`):
Run: `GOWORK=off go test -race ./pkg/llm/providers/chutes/ -run TestNewDefaultsAPIBase -v`

**Step 3: Implement** — add the const beside the others (`client.go:31`):
```go
	defaultAPIBase = "https://api.chutes.ai"
```
and, at the top of `New` (`client.go:109`), before building `c`:
```go
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
```

**Step 4: Run — expect PASS.** Then the whole package: `GOWORK=off go test -race ./pkg/llm/providers/chutes/`

**Step 5: Commit:**
```bash
git add pkg/llm/providers/chutes/client.go pkg/llm/providers/chutes/client_test.go pkg/llm/providers/chutes/export_test.go
git commit -m "feat(llm/chutes): self-default apiBase to the canonical host"
```

---

## Task 4: `auto.genericHTTP` self-defaults OpenRouter/LM Studio base (design B2/B4)

**Files:**
- Modify: `pkg/llm/auto/auto.go` (`genericHTTP` ~106)
- Test: `pkg/llm/auto/auto_test.go`

**Step 1: Write failing test** — an OpenRouter model with `BaseURL == ""` produces a client whose subsequent request does not raise `ModelMismatchError`, and the endpoint resolves to the canonical host. The lightest assertion: build via `auto.New`, then confirm no error and (if a test seam exists) the bound endpoint. If the transport `Client` endpoint isn't introspectable, assert that `auto.New` succeeds and a follow-up `checkBinding`-driven `Invoke` against an `httptest.Server` is reachable. Prefer a focused unit test on a new unexported helper:

```go
func TestDefaultGenericBaseURL(t *testing.T) {
	tests := []struct {
		name string
		p    llm.Provider
		want string
	}{
		{"openrouter", llm.ProviderOpenRouter, "https://openrouter.ai/api/v1"},
		{"lmstudio", llm.ProviderLMStudio, "http://localhost:1234/v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultGenericBaseURL(tt.p); got != tt.want {
				t.Errorf("defaultGenericBaseURL(%s) = %q, want %q", tt.p, got, tt.want)
			}
		})
	}
}
```

**Step 2: Run — expect FAIL** (`defaultGenericBaseURL` undefined):
Run: `GOWORK=off go test -race ./pkg/llm/auto/ -run TestDefaultGenericBaseURL -v`

**Step 3: Implement** in `auto.go` — add the helper + consts, and resolve the base onto the `Endpoint`:

```go
const (
	openRouterBaseURL = "https://openrouter.ai/api/v1"
	lmStudioBaseURL   = "http://localhost:1234/v1"
)

// defaultGenericBaseURL returns the canonical endpoint for a generic-transport
// provider, or "" if it has none (the caller then relies on an explicit base).
func defaultGenericBaseURL(p llm.Provider) string {
	switch p {
	case llm.ProviderOpenRouter:
		return openRouterBaseURL
	case llm.ProviderLMStudio:
		return lmStudioBaseURL
	default:
		return ""
	}
}
```
In `genericHTTP`, resolve before constructing the endpoint:
```go
	baseURL := model.BaseURL
	if baseURL == "" {
		baseURL = defaultGenericBaseURL(model.Provider)
	}
	return transport.New(codec, transport.Endpoint{
		Provider: model.Provider,
		BaseURL:  baseURL,
		ChatPath: transport.DefaultChatPath,
	}, a), nil
```

**Step 4: Run — expect PASS.** Then the whole package: `GOWORK=off go test -race ./pkg/llm/auto/`

**Step 5: Commit:**
```bash
git add pkg/llm/auto/auto.go pkg/llm/auto/auto_test.go
git commit -m "feat(llm/auto): default OpenRouter/LM Studio base URLs in genericHTTP"
```

---

## Task 5: `aci.Policy` pin helpers + unpinned opt-out (design A1, part 1)

**Files:**
- Modify: `pkg/llm/aci/policy.go` (add methods + constructor + gate)
- Create/Modify: `pkg/llm/aci/errors.go` (add `UnpinnedPolicyError`)
- Test: `pkg/llm/aci/policy_test.go`

**Step 1: Write failing tests:**

```go
func TestPolicyIsPinned(t *testing.T) {
	tests := []struct {
		name string
		p    Policy
		want bool
	}{
		{"empty is unpinned", Policy{}, false},
		{"appid pins", Policy{AcceptedAppIDs: map[string]struct{}{"a": {}}}, true},
		{"provenance pins", Policy{AcceptedSourceProvenance: map[ProvenanceKey]struct{}{{}: {}}}, true},
		{"kms root pins", Policy{AcceptedKMSRootPubKeys: map[string]struct{}{"k": {}}}, true},
		{"workload-id-only pins (strictest)", Policy{AcceptedWorkloadIDs: map[string]struct{}{"sha256:x": {}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.p.IsPinned(); got != tt.want {
				t.Errorf("IsPinned() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPolicyRequireAcceptable(t *testing.T) {
	tests := []struct {
		name    string
		p       Policy
		wantErr bool
	}{
		{"empty rejected", Policy{}, true},
		{"pinned ok", Policy{AcceptedAppIDs: map[string]struct{}{"a": {}}}, false},
		{"explicit unpinned ok", UnpinnedPolicy(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.p.requireAcceptable()
			if (err != nil) != tt.wantErr {
				t.Fatalf("requireAcceptable() err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}
```

**Step 2: Run — expect FAIL** (undefined `IsPinned`/`requireAcceptable`/`UnpinnedPolicy`):
Run: `GOWORK=off go test -race ./pkg/llm/aci/ -run 'TestPolicyIsPinned|TestPolicyRequireAcceptable' -v`

**Step 3: Implement** — add to `policy.go` (and the unexported field to the `Policy` struct):

```go
// (add to the Policy struct)
	// allowUnpinned, set ONLY by UnpinnedPolicy(), opts into genuineness-only
	// verification with no allow-listing. Unexported so no struct literal outside this
	// package can select the fail-open path by accident; the constructor is the only door.
	allowUnpinned bool

// IsPinned reports whether the policy pins at least one acceptance set. Any non-empty
// set means VerifyReport runs at least one allow-list check (steps 5/7/9), so the
// policy is not fail-open. AcceptedWorkloadIDs counts: a workload-ID-only policy is the
// strictest pin (one exact keyset digest).
func (p Policy) IsPinned() bool {
	return len(p.AcceptedWorkloadIDs) > 0 ||
		len(p.AcceptedSourceProvenance) > 0 ||
		len(p.AcceptedAppIDs) > 0 ||
		len(p.AcceptedKMSRootPubKeys) > 0
}

// UnpinnedPolicy returns a Policy that explicitly accepts any cryptographically genuine
// report WITHOUT allow-listing a workload. This is a deliberate, greppable opt-out of
// the fail-closed default; prefer a pinned Policy in production.
func UnpinnedPolicy() Policy { return Policy{allowUnpinned: true} }

// requireAcceptable is the shared fail-closed gate applied at every public aci entry
// (New, VerifyReport): a policy that pins nothing and did not opt into unpinned mode is
// rejected before any chain runs or network object exists.
func (p Policy) requireAcceptable() error {
	if !p.IsPinned() && !p.allowUnpinned {
		return &UnpinnedPolicyError{}
	}
	return nil
}
```

Add the typed error to `errors.go`:
```go
// UnpinnedPolicyError is returned by the public aci entry points when a Policy pins no
// acceptance set and did not explicitly opt into unpinned mode via UnpinnedPolicy().
// Fail-secure: attestation refuses to run allow-list-free unless the caller asks for it.
type UnpinnedPolicyError struct{}

func (e *UnpinnedPolicyError) Error() string {
	return "aci: policy pins no acceptance set; supply a pinned Policy or aci.UnpinnedPolicy() to opt out"
}
```

Update the `Policy` type doc comment (`policy.go:29-33`): drop the "zero value Policy{} therefore accepts any genuine report" implication for the *public* entry points — note that `New`/`VerifyReport` now reject an unpinned policy unless `UnpinnedPolicy()` was used.

**Step 4: Run — expect PASS.**

**Step 5: Commit:**
```bash
git add pkg/llm/aci/policy.go pkg/llm/aci/errors.go pkg/llm/aci/policy_test.go
git commit -m "feat(llm/aci): add IsPinned, UnpinnedPolicy opt-out, and fail-closed gate"
```

---

## Task 6: `aci.New` fails closed; fix its callers (design A1, part 2)

**Files:**
- Modify: `pkg/llm/aci/client.go:161-186` (signature + guard + final return)
- Modify: `pkg/llm/providers/phala/phala.go:92-97` (forward the error **and** self-default base — design B2)
- Modify: `pkg/llm/aci/client_test.go` (4 sites), `pkg/llm/aci/live_integration_test.go` (2 sites)
- Test: reuse existing + add one fail-closed case

**Step 1: Update the tests first (they define the new contract).**
- In `client_test.go`, the 4 calls `client = New(testBaseURL, testAPIKey, Policy{}, …)` become
  `client, err := New(testBaseURL, testAPIKey, UnpinnedPolicy(), …)` with an `if err != nil { t.Fatal(err) }` (these tests exercise the invoke/stream mechanism via `WithAttestFunc`, so the policy is irrelevant → the explicit opt-out is correct).
- In `live_integration_test.go`, the 2 calls `New(livePhalaBaseURL, key, testPolicy())` become `client, err := New(...)` + error check (`testPolicy()` is already pinned).
- Add a fail-closed unit test:
```go
func TestNewRejectsUnpinnedPolicy(t *testing.T) {
	_, err := New("https://gw.example", "sk", Policy{})
	var upe *UnpinnedPolicyError
	if !errors.As(err, &upe) {
		t.Fatalf("want *UnpinnedPolicyError, got %v", err)
	}
}
```

**Step 2: Run — expect FAIL/compile error** (New still returns one value):
Run: `GOWORK=off go test -race ./pkg/llm/aci/ -run TestNewRejectsUnpinnedPolicy -v`

**Step 3: Change `aci.New`** (`client.go:161`) to `func New(baseURL, apiKey string, policy Policy, opts ...Option) (llm.LLM, error)`; add the gate as the first statement:
```go
	if err := policy.requireAcceptable(); err != nil {
		return nil, err
	}
```
and change the final `return c` (`client.go:185`) to `return c, nil`.

**Step 4: Fix the production caller** `phala.New` (`phala.go:92`):
```go
func New(baseURL string, key auth.APIKey, p aci.Policy) (llm.LLM, error) {
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderPhala, Kind: llm.AuthAPIKey}
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return aci.New(baseURL, string(key), p)
}
```
Add the const near the top of `phala.go`:
```go
	// defaultBaseURL is the canonical Phala inference host, used when New is given "".
	// PROVISIONAL — confirm the production host with ops before release (see design §4.2).
	defaultBaseURL = "https://inference.phala.com"
```

**Step 5: Run — expect PASS** for `./pkg/llm/aci/` and `./pkg/llm/providers/phala/`.
Run: `GOWORK=off go test -race ./pkg/llm/aci/ ./pkg/llm/providers/phala/`

**Step 6: Commit:**
```bash
git add pkg/llm/aci/client.go pkg/llm/aci/client_test.go pkg/llm/aci/live_integration_test.go pkg/llm/providers/phala/phala.go
git commit -m "feat(llm/aci): fail closed on an unpinned policy in New; self-default phala base"
```

---

## Task 7: `aci.VerifyReport` fails closed (design A1, part 3)

**Files:**
- Modify: `pkg/llm/aci/verify.go:290-292` (guard + doc)
- Modify: `pkg/llm/aci/verifyreport_test.go`
- Test: add a fail-closed case

**Step 1: Update `verifyreport_test.go`** so its existing `VerifyReport(...)` calls pass a pinned policy (or `UnpinnedPolicy()` where the test's intent is genuineness-only). Inspect the file's local `DefaultPolicy`/policy helper and route it through a pinned value. Add:
```go
func TestVerifyReportRejectsUnpinnedPolicy(t *testing.T) {
	_, err := VerifyReport([]byte(`{}`), nil, time.Unix(0, 0), Policy{})
	var upe *UnpinnedPolicyError
	if !errors.As(err, &upe) {
		t.Fatalf("want *UnpinnedPolicyError, got %v", err)
	}
}
```

**Step 2: Run — expect FAIL:**
Run: `GOWORK=off go test -race ./pkg/llm/aci/ -run TestVerifyReportRejectsUnpinnedPolicy -v`

**Step 3: Implement** — guard `VerifyReport` (`verify.go:290`):
```go
func VerifyReport(reportJSON []byte, nonce *string, now time.Time, policy Policy) (*VerifiedReport, error) {
	if err := policy.requireAcceptable(); err != nil {
		return nil, err
	}
	return verifyReport(reportJSON, nonce, now, policy, defaultQuoteVerifier)
}
```
Rewrite the doc comment: remove the "a zero `Policy{}` accepts any genuine report" sentence and the `phala.DefaultPolicy()` reference; state that an unpinned policy is rejected unless `UnpinnedPolicy()` is passed, and that the unexported `verifyReport` remains the unguarded low-level runner.

**Step 4: Run — expect PASS** for `./pkg/llm/aci/`.

**Step 5: Commit:**
```bash
git add pkg/llm/aci/verify.go pkg/llm/aci/verifyreport_test.go
git commit -m "feat(llm/aci): fail closed on an unpinned policy in VerifyReport"
```

---

## Task 8: `auto.New` dispatches Phala to a typed error; drop the phala import (design A3)

**Files:**
- Modify: `pkg/llm/auto/auto.go` (phala case ~63, remove import ~19, add error type)
- Modify: `pkg/llm/auto/auto_test.go`
- Test: assert the typed error

**Step 1: Update `auto_test.go`** — any test that expected `auto.New(phalaModel, key)` to build a client now expects `*PolicyNotConstructibleError`. Add:
```go
func TestNewPhalaNotConstructible(t *testing.T) {
	m := llm.CustomModel(llm.ProviderPhala, llm.APIFormatOpenAI, "https://inference.phala.com", "zai-org/GLM-4.6")
	_, err := New(m, "sk-live")
	var pne *PolicyNotConstructibleError
	if !errors.As(err, &pne) {
		t.Fatalf("want *PolicyNotConstructibleError, got %v", err)
	}
}
```

**Step 2: Run — expect FAIL:**
Run: `GOWORK=off go test -race ./pkg/llm/auto/ -run TestNewPhalaNotConstructible -v`

**Step 3: Implement:**
- Add the error type near `SigV4NotConstructibleError` in `auto.go`:
```go
// PolicyNotConstructibleError is returned by New for a provider that needs an
// attestation acceptance Policy auto.New cannot supply (currently Phala). auto.New's
// inputs are (model, key) only — it carries no Policy — so the caller must construct the
// client directly via the named constructor with their own verified policy. Fail-closed
// and directive, never a silent client with a defaulted policy.
type PolicyNotConstructibleError struct {
	Provider llm.Provider
	Use      string
}

func (e *PolicyNotConstructibleError) Error() string {
	return fmt.Sprintf("provider %q requires an attestation policy auto.New cannot supply; construct it directly via %s", e.Provider, e.Use)
}
```
- Replace the phala case (`auto.go:63-64`):
```go
	case llm.ProviderPhala:
		return nil, &PolicyNotConstructibleError{Provider: llm.ProviderPhala, Use: "phala.New"}
```
- Remove the `phala` import (`auto.go:19`). Update the package/`New` doc to list Phala alongside Bedrock as construct-directly.

**Step 4: Run — expect PASS**; build the module: `GOWORK=off go build ./...`

**Step 5: Commit:**
```bash
git add pkg/llm/auto/auto.go pkg/llm/auto/auto_test.go
git commit -m "feat(llm/auto): dispatch Phala to a typed construct-directly error"
```

---

## Task 9: Remove `phala.DefaultPolicy()` and the fixture pins (design A2)

**Files:**
- Modify: `pkg/llm/providers/phala/phala.go` (delete the four pin consts + `DefaultPolicy`)
- Modify: `pkg/llm/providers/phala/policy_test.go` (drop DefaultPolicy assertions; keep client-construction tests)
- Test: package tests

**Step 1: Delete** from `phala.go` the pin consts `defaultAppID`, `defaultRepoURL`, `defaultRepoCommit`, `defaultRepoCommitDeployed`, `defaultKMSRoot`, and the `DefaultPolicy()` function. Keep `defaultBaseURL` (Task 6). Update the package doc so it no longer claims to own the pinned trust anchors — it now owns only the base default + the aci wiring.

**Step 2: Update `policy_test.go`** — remove `TestDefaultPolicy`-style assertions on the pinned values. For the remaining construction tests (empty-key rejected, non-empty builds), pass a small inline pinned policy, e.g.
```go
func testPinnedPolicy() aci.Policy {
	return aci.Policy{AcceptedAppIDs: map[string]struct{}{"fdb7a14e5a6675f752e2cb69c9067a98ca402918": {}}}
}
```
(The fixture-derived value is fine as *test input*; it is no longer shipped.) Where a test only needs a client and not enforcement, `aci.UnpinnedPolicy()` is also acceptable.

**Step 3: Run — expect PASS:**
Run: `GOWORK=off go test -race ./pkg/llm/providers/phala/ -v`

**Step 4: Grep to confirm no production reference to the removed symbols remains:**
Run: `grep -rn 'DefaultPolicy\|defaultAppID\|defaultKMSRoot' pkg --include='*.go' | grep -v '_test.go'`
Expected: no output.

**Step 5: Commit:**
```bash
git add pkg/llm/providers/phala/phala.go pkg/llm/providers/phala/policy_test.go
git commit -m "refactor(llm/phala): remove DefaultPolicy and fixture-derived trust pins"
```

---

## Task 10: Delete the model catalogue; fix its test consumers (design B1)

**Files:**
- Delete: `pkg/llm/catalog.go`, `pkg/llm/catalog_test.go`
- Modify: `pkg/llm/llm_test.go`, `pkg/llm/auto/auto_test.go`, `pkg/llm/providers/bedrock/body_test.go`

**Step 1: Delete the files:**
```bash
git rm pkg/llm/catalog.go pkg/llm/catalog_test.go
```

**Step 2: Build to surface the breakages:**
Run: `GOWORK=off go build ./... && GOWORK=off go vet ./pkg/llm/... 2>&1 | head`
Expected: compile errors in the three test files referencing `GeminiFlash`/`ClaudeOnBedrock`/etc.

**Step 3: Replace each catalogue call with an inline `CustomModel`.** These are test fixtures, so a local helper keeps them DRY. Example replacements:
- `llm.GeminiFlash()` → `llm.CustomModel(llm.ProviderGoogle, llm.APIFormatGemini, "https://generativelanguage.googleapis.com/v1beta", "gemini-2.5-flash", llm.WithTools(), llm.WithImages(), llm.WithThinking(), llm.WithMaxContext(1_000_000))`
- `llm.ClaudeOnBedrock("…")` → `llm.CustomModel(llm.ProviderBedrock, llm.APIFormatAnthropic, "", "…", llm.WithTools(), llm.WithImages(), llm.WithMaxContext(200_000))`
- Where only the wire fields matter to the test, drop the caps options.

Add a small `func testModel…()` helper in each test file if the same row is used more than once (DRY).

**Step 4: Run the affected packages — expect PASS:**
Run: `GOWORK=off go test -race ./pkg/llm/ ./pkg/llm/auto/ ./pkg/llm/providers/bedrock/`

**Step 5: Commit:**
```bash
git add -A pkg/llm/llm_test.go pkg/llm/auto/auto_test.go pkg/llm/providers/bedrock/body_test.go
git commit -m "refactor(llm): delete the model catalogue; consumers own model rows"
```

---

## Task 11: Reword the `OriginCatalog` doc comment (design 4.4)

**Files:**
- Modify: `pkg/llm/origin.go:9`

**Step 1: Reword** the `OriginCatalog` line comment (no code/enum change):
```go
	OriginCatalog // curated by the consumer or integration layer (not necessarily this SDK); capabilities are trusted
```

**Step 2: Build (doc-only, no test):**
Run: `GOWORK=off go build ./pkg/llm/`

**Step 3: Commit:**
```bash
git add pkg/llm/origin.go
git commit -m "docs(llm): clarify OriginCatalog is consumer/integration provenance"
```

---

## Task 12: Full verification + design-doc status

**Step 1: Whole-module build with the required flags:**
Run: `GOWORK=off CGO_ENABLED=0 go build -trimpath ./...`
Expected: clean.

**Step 2: Full race suite:**
Run: `GOWORK=off go test -race ./...`
Expected: all pass. (Integration/`live_integration` tests are build-tagged out of the default run.)

**Step 3: Security + format gate (CLAUDE.md):**
Run: `GOWORK=off make secure`
Expected: `fmt-check`, `vet`, `staticcheck`, `gosec`, `govulncheck` all pass. Run `make fmt` first if `fmt-check` complains.

**Step 4: Confirm no orphaned references:**
Run: `grep -rn 'DefaultPolicy\|ChutesKimiK2\|GeminiFlash\|GLM46Phala\|ClaudeOnBedrock\|LMStudioLocal' pkg --include='*.go'`
Expected: no output.

**Step 5: Flip the design-doc status** in `docs/plans/2026-07-02-llm-mechanism-policy-ownership-design.md` from "Approved" to note implementation landed on this branch (leave the Phala-host ops-confirmation item open). Commit:
```bash
git add docs/plans/2026-07-02-llm-mechanism-policy-ownership-design.md
git commit -m "docs(plans): mark mechanism/policy-ownership design as implemented"
```

---

## Out of scope (follow-ups, not this plan)

- **swe migration (separate repo/PR):** add swe's own model catalogue (`CustomModel` rows) and its own verified Phala `aci.Policy`; bump the `looprig` version it depends on. This is a **breaking** change to swe — sequence it after this branch merges and `looprig` is tagged.
- **Ops confirmation** of the Phala production host (`inference.phala.com` vs another) — must land before release; update `phala.defaultBaseURL` if it changes.
- Unrelated tracked work: bedrock streaming/Converse, chutes fail-closed discovery.
