# Design: explicit unbounded execution, zero hustle timeout, opaque tool input

Status: **approved by the owner 2026-09-25**, not yet implemented. Ships in
harness v0.41.0 (alongside the message principal/metadata/presenter work in
`2026-09-25-message-principal-metadata-presenter-design.md`) and inference
v0.14.0.

## Why

The Oxy household app (github.com/inventivepotter/oxy) carries patched copies of
harness and inference to get four behaviours Looprig lacks. Upstreaming them lets
Oxy pin plain releases with no `replace` directives. Each change is opt-in or a
bug fix, so default behaviour is unchanged. The working, tested Oxy patches are
the reference implementation: branch `feat/looprig-current` in the Oxy repo,
`third_party/looprig-harness` and `third_party/looprig-inference`, described in
their `OXY-PATCHES.md` (rebased onto harness v0.40.2 / inference v0.13.0).

Two further Oxy patches are **not** upstreamed; Oxy drops them itself:

- `session.HustleHost`: Oxy moves its title service out of the agent runtime.
- Legacy JSON-text hustle extractor tolerating thinking blocks: Oxy moves its
  hustles to `hustle.WithOutputSchema`, whose reader already ignores thinking
  (inference `structured_result.go`), provided every Oxy provider supports native
  structured output; otherwise this item is revisited.

## 1. `loop.Unlimited` (harness, minor)

- `pkg/loop`: `const Unlimited = -1`. `ToolLimits.Iterations` and
  `ToolLimits.Calls` accept `Unlimited`, meaning no cap; `0` keeps meaning
  "default"; values below `-1` stay invalid. Mode overrides may also set
  `Unlimited` and it wins like any other override (`mode.go` `resolveLimits`
  ~116, `invalidLimits` ~152). `defaultLimits` only fills `== 0`, so `-1`
  survives. `Parallel`, `ResultBytes` and `CaptureBytes` keep their current
  semantics (no unlimited value).
- `internal/loopruntime/toolset.go` (`resolveMax…` 65-77) keeps `-1`;
  `turn.go:520` skips only the corresponding comparison.
- `pkg/rig.DelegationLimits.Quota` accepts `loop.Unlimited` (`options.go` 307,
  393-398); `internal/sessionruntime/limits.go` 41-49 keeps it and
  `session.go:1589` skips only the quota check. `Depth` stays bounded; there is
  no unlimited depth.
- Restore recounts `spawned` from `LoopStarted` (`restore_constructor.go:1343`)
  and never compares with the quota: safe.
- Tests to change: the "negative treated as unset/defaults" rows in
  `internal/loopruntime/deps_test.go`, `pkg/loop/definition_test.go`
  `TestDefineValidation/negative_limits`, and the rig negative-quota row move to
  `-2`. New tests: unlimited iterations/calls run past former caps; mode override
  to unlimited; unlimited quota exceeds the former lifetime quota; `-2` refused.
- Risk to document: an unlimited loop stops only on interrupt, shutdown or
  context cancellation.

## 2. Zero hustle timeout means no deadline (harness, minor)

- `pkg/hustle/definition.go`: `WithTimeout(0)` is accepted and documented as "no
  execution deadline"; negative stays invalid (`Descriptor.Validate` and `Define`
  change `<= 0` to `< 0`, ~183, ~364, ~511).
- `internal/hustleruntime/execution.go` `executionContextWithTimeout`
  (1073-1085) adds a deadline only when `timeout > 0`; the combined caller and
  session cancellation still applies. Audit and finalization timeouts stay bounded.
- Only an **explicit** `WithTimeout(0)` means no deadline. A hustle defined without
  `WithTimeout` is still refused (`DefinitionInvalidTimeout`), so forgetting the option
  never silently removes the deadline (decided 2026-09-25 while planning impl-04).
- Durable note: `DefinitionDescriptor.Validate` runs on replay
  (`pkg/event/validate.go:672`, `restore.go:69`, `pkg/sessionstore/catalog.go:890`),
  so journals holding `TimeoutNanos: 0` become readable by stock harness. This is
  a relaxation: no one-way rule.
- Tests: the zero-timeout rows in `pkg/hustle/descriptor_test.go` and
  `definition_test.go` assert acceptance (both files keep their negative rows); a
  zero-timeout hustle runs without a deadline and still stops on cancellation.

## 3. Duplicate-key check treats tool input as opaque (harness, patch-level fix)

- Bug: `pkg/event/marshal.go` `rejectDuplicateJSONKeys` / `inspectJSONValue`
  (440-495) recurse into a `tool_use` block's `input`, which holds the model's raw
  argument bytes. A model that emits duplicate keys makes the journal
  unreplayable on restore.
- Fix: track the JSON path; under a message-block path decode `input` as raw and
  skip the duplicate check only when the block's `type` is `tool_use`. Message
  block paths: `.messages[].blocks[]`, `.message.blocks[]`, `.retained[].blocks[]`,
  `.summary.blocks[]`, `.resume.message.blocks[]` (the `GatePrepared.Resume`
  path, `pkg/event/gate.go:40-45`), with nested `.content[]` stripped. Envelope
  and every other key stay strictly checked; original argument bytes are retained.
- Tests: duplicate keys inside `tool_use.input` round-trip on each path; duplicates
  elsewhere (envelope, non-tool blocks) are still refused.

## 4. Inference call with no execution ceiling (inference v0.14.0, minor)

- Today `transport` enforces a 5-minute Invoke ceiling and a 60-second Stream
  response-header ceiling (`transport/client.go:112`); `WithInvokeTimeout` refuses
  `<= 0`; a RoundTripper cannot lift `http.Client.Timeout`.
- Add an explicit, request-scoped opt-in:
  `transport.WithoutExecutionTimeout(ctx) context.Context`. A marked call uses a
  dedicated HTTP client with no whole-request or response-header deadline. It is
  built alongside the normal client in the constructor and in `WithTLSRootCAs`,
  and wired in `applyRoundTripper`; selected by `executionHTTPClient(ctx, normal)`
  at both `Do` sites. Unmarked calls are unchanged; nothing is sent on the wire.
  TLS roots, caller RoundTrippers, connection/TLS setup limits, redirect refusal,
  authentication, response-size limits and caller cancellation are unchanged.
- The context marker (rather than `WithInvokeTimeout(0)`) keeps the choice per
  call and lets provider wrappers (llm) pass it through without new options.
- Document: a half-open connection is detected only by TCP keepalive; cancellation
  belongs to the caller.
- Tests: marked Invoke/Stream exceed the former ceilings; unmarked keep them; the
  marker survives `WithTLSRootCAs` and a custom RoundTripper; `-race`.

## Release

- inference v0.14.0 first; harness v0.41.0 pins it (with core v0.12.0 and
  sessionstore v0.14.0 from the metadata work).
- Then Oxy: delete `third_party/`, drop both `replace` directives, pin
  harness v0.41.0 / inference v0.14.0, keep its regression tests as consumer tests.

## Progress

| Item | Module | Status |
|---|---|---|
| 1 `loop.Unlimited` | harness v0.41.0 | not started |
| 2 zero hustle timeout | harness v0.41.0 | not started |
| 3 opaque tool input | harness v0.41.0 | not started |
| 4 no execution ceiling | inference v0.14.0 | not started |
| Oxy drops HustleHost (titles out of rig) | Oxy Phase 3 | in progress |
| Oxy drops extractor patch (structured output) | Oxy | not started |
