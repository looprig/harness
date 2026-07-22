# pkg/evalmigration

`pkg/evalmigration` is a **build-tagged compatibility proof** that the
reusable [`github.com/looprig/eval`](https://github.com/looprig/eval)
module can express the evaluation examples the legacy in-tree
`pkg/eval` package used to provide. It is not a runtime package; it
exists to prove the migration is mechanically possible and to keep
proving it as `eval` evolves.

## What is evalmigration?

A single `//go:build evalmigration` test file (`eval_integration_test.go`)
that re-expresses the two original evaluation examples — the
deterministic `Contains` metric and the model `Judge` metric — with
the new `looprig/eval` API:

- `content.AgenticMessages` for the conversation thread (no eval-specific
  string fields).
- `exact.RequiredText` for the deterministic metric.
- `judge.New(rubric.AnswerRelevanceV1, judgeClient, template)` for the
  model-judgment metric.
- `evaltest.RunScenario(t, scenario, target, evaluators...)` to run one
  scenario through `eval.Run` and present it as a Go subtest.

The judge runs against an in-test fake `inference.Client` that returns a
valid structured `ScoreOutput`, so the whole file compiles and passes
offline with no network and no credentials. The dedicated `evalmigration`
build tag keeps it out of the default `go test ./...` suite.

## Why a proof, not a live test

This package supersedes the legacy `pkg/eval` (which has been removed).
New evaluation code lives in `looprig/eval`, not in harness. Keeping
the proof here:

- pins that the **shapes** the legacy package exposed
  (`TestCase.ActualOutput`, `Runner.RunCases`, the `Contains`/`Judge`
  metrics) re-express cleanly against the new vocabulary;
- catches a regression in `looprig/eval` that would silently break the
  migration (a renamed method, a removed evaluator, a changed
  `Revision` contract);
- documents, in code, the before/after mapping for anyone migrating an
  out-of-tree eval suite against the new module.

## How to use

```sh
go test -race -tags evalmigration ./pkg/evalmigration
```

There is no production import path here; nothing in `harness` depends
on `pkg/evalmigration` at build time. The package is tests-only.

## Sibling packages

- `github.com/looprig/eval` — the evaluation framework this proof
  targets. Its root package depends only on `looprig/core`; `judge/`
  and `target/inference/` add `looprig/inference`. Nothing else.
- `github.com/looprig/core` — `content.AgenticMessages`,
  `content.UserMessage`, `content.AIMessage`.
- `github.com/looprig/inference` — `inference.Client`,
  `model.Model`, `stream.Reader` (the fake client satisfies these).

## How it is designed

```
   legacy harness pkg/eval (REMOVED)
              │
              │  re-express against
              ▼
   github.com/looprig/eval
      ├─► exact.RequiredText        (deterministic evidence)
      ├─► judge.New(rubric.*, ...)  (model judgment)
      ├─► evaltest.RunScenario      (go test harness)
      └─► eval.RunConfig            (trials, concurrency, timeouts)
              │
              │  this package proves the mapping compiles + passes
              ▼
   pkg/evalmigration (build tag: evalmigration)
```

### The migration revision

Both the scenario and the fake target's observation agree on a
`migrationRevision` (`eval.Revision = "v1"`). `eval.Run` rejects a
sample whose scenario and observation disagree on the revision as a
**stage error** rather than evaluating it — so a stale observation
can never be scored as a passing or failing run.

### The before/after mapping

The proof re-expresses two legacy shapes:

- `TestCase.ActualOutput` (a string) → an `*content.AIMessage` carrying
  a single `TextBlock`. The agent's answer is a content block, not an
  eval-specific string.
- `Runner.RunCases` (drive the cases and fill `ActualOutput`) → an
  in-test `eval.Target` that echoes the scenario's input thread and
  appends a fixed assistant answer. The target never mutates the
  read-only `Scenario`; it appends into a fresh slice.

The same pattern applies to any out-of-tree migration: write a target
that produces the observation, drive it through `evaltest.RunScenario`,
and assert with `evaltest.RequirePass` (fails on any non-pass) or
`evaltest.RequireVerified` (fails on `error`/`unverified`, tolerates a
recorded `fail`).
