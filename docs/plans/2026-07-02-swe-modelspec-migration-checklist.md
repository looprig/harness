# swe Consumer Migration Checklist — post looprig Phase 1 (ModelSpec split)

> **Status:** DEFERRED follow-up. looprig Phase 1 (branch `feat/llm-phase1-modelspec-split`) retired `llm.ModelSpec`, reshaped `Model`, made the client connection-bound, extracted `providers/`, and split `loop.Config`. The `swe` module (module `.../swarms/swe`, at `/Users/ipotter/code/swe`) still compiles against the OLD surface and must be migrated to build against the new looprig. Execute with superpowers:subagent-driven-development once Phase 1 is merged to the workspace's looprig.

## Core conceptual shift
`ModelSpec` fused **model + system + secret**. Post-split those separate by lifetime:
- **secret** binds ONCE to the `Client` via `auto.New(model, auth.APIKey(key))` (or a typed provider constructor) — never on the model/request/catalog.
- **model** is a secret-free `llm.Model` on `loop.Config.Model` (gains `APIFormat`/`Origin`/`Caps`/`Sampling`; loses `Temperature`/`MaxTokens`/`AcceptsImages`/`.Spec()`).
- **system** is a plain string on `loop.Config.System` (and on `llm.Request.System`).
So the `ModelFactory(system) → ModelSpec` closure collapses: the only per-agent-varying thing is the system string, which moves off the model entirely.

## Required (compile-forcing) changes
- **S1 — `registry.go:~82` `ModelFactory`.** `func(systemPrompt string) llm.ModelSpec` → factory no longer carries system or secret. Prefer storing a secret-free `llm.Model` (shared model identity) and setting `System` at each `loop.Config` site; if keeping a factory, make it `func() llm.Model`. Update `var _ ModelFactory = …` in `registry_test.go`.
- **S2 — `model.go` — remove `.Spec()` materialization.** `newModelFactoryFor` (~:37) returns `base.Spec(apiKey, system)` → return the secret-free `base`; drop system here. `auto.New(base.Spec(apiKey, ""))` (~:91) → `auto.New(base, auth.APIKey(apiKey))` (new 2-arg sig; import `pkg/llm/auth`). Same for the economy path (~:116; see S5). `base.Provider.RequiresKey()` (~:55) still compiles (kept); optionally migrate to `RequiredAuth()`.
- **S3 — `model_catalog.go` — redesign the catalog (biggest change).** `ModelCatalog{Economy/Standard/Premium []llm.ModelSpec}` → `[]llm.Model`. Decide where the per-tier key lives (simplest: keep one env key, bind at `auto.New`; drop per-spec key). DELETE `modelFromSpec` (it maps to the OLD flat `Model{Temperature,MaxTokens,AcceptsImages}` — those moved to `Model.Sampling.*` and `Model.Caps.AcceptsImages`); construct entries directly as `llm.Model` or `llm.CustomModel(provider, apiFormat, baseURL, name, opts...)` — **`APIFormat` is now required** (no inference). `validateCatalogSpec`: `spec.Model`→`model.Name`, `spec.Validate()`→`model.Validate()` (now also enforces `APIFormat ∈ provider's supported set` and non-empty `https://`/loopback-`http://` BaseURL — config models missing these are rejected).
- **S4 — `loop.Config` sites — set `Model` + `System` separately.** `swarm.go:~159` `operatorPrimaryConfig` and `spawner.go:~117` `Spawn`: `Model: factory(system)` → `Model: sharedModel, System: system`. Plus any other `loop.Config{Model: <spec>}` literal.
- **S5 — `session_title.go` — economy client (semantic, verify).** `titleSpec func(system string) llm.ModelSpec` (~:47,61) → under connection-binding a client is fixed to one provider+endpoint+key. If economy shares the standard provider/BaseURL/key (only model NAME differs), the shared chutes/aci client serves it (they resolve/attest per `req.Model.Name`) — pass `(economyModel llm.Model, system)`. If economy uses a DIFFERENT provider/endpoint, build a SEPARATE economy client via `auto.New(economyModel, auth.APIKey(key))` and hand THAT to the coordinator. A naive `titleSpec→Model` port that keeps reusing the standard client will misroute at runtime when providers differ — verify which case applies.
- **S6 — `agent.go:~86,106,126`** — `primary.Model.AcceptsImages` → `primary.Model.Caps.AcceptsImages`.
- **S7 — `persistence.go:~403,452`** — `wiring.cfg.Model.System` → `wiring.cfg.System`.
- **S8 — `auto.New` call sites take 2 args** — `model.go:~91`, `operator_eval_integration_test.go:~178` (`auto.New(spec)` → `auto.New(model, auth.APIKey(key))`); import `pkg/llm/auth`.

## Test-side (same deltas)
- `registry_test.go`, `model_test.go`, `session_title_test.go`: drop `llm.ModelSpec`/`spec.System`; factory yields `llm.Model`, system set on config/request.
- `agent_test.go`: `testPrimaryCfg(llm.ModelSpec)` → `(llm.Model, system string)`; build `llm.Model{Provider, APIFormat, BaseURL, Name, Caps:{AcceptsImages}}` with a VALID BaseURL+APIFormat (new `Validate`).
- `model_catalog_test.go`: `[]llm.ModelSpec`→`[]llm.Model`; `llm.ChutesKimiK2().Spec(apiKey,"")` → `llm.ChutesKimiK2()` + key at `auto.New`.
- `operator_eval_integration_test.go`: `spec llm.ModelSpec`→`llm.Model`; `loop.Config{Model:spec}`→`{Model,System}`; `auto.New` 2-arg.
- `greeting_test.go`, `swarm_test.go`, `skills_wiring_test.go`: `cfg.Model.System` assertions → `cfg.System`.
- `persistence_test.go` + `newModelFactory(...)` sites: adjust to new factory/model shape.

## Verification (swe pass)
From `/Users/ipotter/code/swe`: `GOWORK=off go build ./...`, `GOWORK=off go test -race ./...`, `make secure`. Sanity-check S5: confirm the title coordinator's economy call routes through a client bound to the economy model's connection (not silently reusing the standard client across a differing provider/endpoint).

## Also queued for after this: Phase 2 (see memory `llm-phase2-codecs-providers-scope`)
Gemini + other codecs; Bedrock (needs `auth.SigV4` signer + `SigV4Credentials` redaction) + OpenRouter providers. Also carry the Phase-1 follow-ups: align `chutes.New` with the typed-credential fail-closed contract (like `phala.New`); retire `Provider.RequiresKey` once swe stops calling it; capability-gate the `Effort`→`reasoning_effort` mapping when `codec/anthropicapi` lands.
