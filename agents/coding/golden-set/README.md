# Togo golden-set

Golden input/output pairs for evaluating the Togo coding agent.

Each `cases/*.json` file is one `internal/eval.TestCase`:

| field | meaning |
|---|---|
| `name` | case identifier |
| `input` | the prompt given to the agent |
| `expectedOutput` | substring the answer must contain (the `Contains` metric) |
| `context` | optional grounding strings |

These cases are consumed by the offline validity test (`golden_set_test.go`,
which only checks that they parse) and by the build-tagged integration test
(`eval_integration_test.go`), which runs the live agent against them with the
`Contains` and `Judge` metrics. Keep `input`s answerable without
approval-gated tools (no file writes / shell), so the integration run does not
block on a permission gate.
