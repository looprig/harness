# pkg/serve wire contract — fixtures, schemas, OpenAPI

Everything in this directory is the **reviewed wire contract** for the serve HTTP
session API. A diff to any file here is a **breaking-change review signal** — treat
it the way you treat a change to a public API signature.

## Layout

- `openapi.yaml` — hand-written OpenAPI 3.1 describing every endpoint (paths,
  methods, path/query params and their bounds, request/response bodies, status
  codes, and the nested error envelope), plus the SSE endpoint as
  `text/event-stream`.
- `schema/*.json` — hand-written JSON-Schema (draft 2020-12) for each request/
  response body and the two SSE `data:` frame payloads. Component-style: shared
  pieces (`uuid`, `event_envelope`, `session_summary`, `status_event`) are
  referenced with `$ref`.
- `fixtures/*.json` and `fixtures/*.sse` — golden wire bytes. One canonical example
  per response type, plus one enduring and one ephemeral SSE frame.

## These are hand-authored contracts, not generated

There is **no** OpenAPI generator and **no** JSON-Schema validation library in this
repo (CLAUDE.md: stdlib only, external deps need explicit approval). `openapi.yaml`
and `schema/*.json` are authored and reviewed by hand. The Go tests do **not**
machine-validate bytes against the schema; they pin the exact emitted bytes against
the fixtures (`fixtures_test.go`) and do a shallow structural cross-check that each
fixture's top-level keys line up with the matching schema (`schema_test.go`).

## Regenerating the fixtures

The fixtures are golden files. When a deliberate, reviewed wire change lands,
regenerate them and review the resulting diff:

```
go test ./pkg/serve -run Fixture -update
```

Then confirm the round-trip is clean (no diff on a plain run):

```
go test -race ./pkg/serve -run Fixture
```

Volatile values are pinned so the fixtures are stable: the handlers are fed **fixed**
ids and a **fixed** instant, and a normalizer (`fixtures_test.go`) additionally
collapses every UUID to the zero UUID and every RFC3339 timestamp to
`2026-07-08T12:00:00Z`. Never hand-edit a fixture to make a test pass — change the
DTO/handler, regenerate, and review.
