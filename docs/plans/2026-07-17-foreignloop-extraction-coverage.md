# Foreignloop extraction coverage manifest

This is the checked Phase 0 inventory for extracting `pkg/foreignloop`. Each
tracked regular file below has exactly one principal final owner. Where one
current file contains declarations that split across destinations, the note
records that split explicitly. Tests and fixtures move with the behavior they
cover; nothing is silently dropped.

This manifest is temporary migration evidence. Task 18 finalizes it and removes
the completeness check when Harness deletes `pkg/foreignloop`.

## Current source ownership

| Source file | Final owner | Migration note |
|---|---|---|
| `pkg/foreignloop/claude/args.go` | `driver/claude` | Port Claude argument construction. |
| `pkg/foreignloop/claude/args_test.go` | `driver/claude` | Port with Claude argument construction. |
| `pkg/foreignloop/claude/claude.go` | `driver/claude` | Port process driver and expose the agent-only constructor. |
| `pkg/foreignloop/claude/claude_integration_test.go` | `driver/claude` | Port the bounded subprocess integration coverage. |
| `pkg/foreignloop/claude/claude_test.go` | `driver/claude` | Port process, spawn, close, and error coverage. |
| `pkg/foreignloop/claude/doc.go` | `driver/claude` | Replace with extracted Claude driver package documentation. |
| `pkg/foreignloop/claude/env.go` | `driver/claude` | Port Claude environment whitelisting. |
| `pkg/foreignloop/claude/env_test.go` | `driver/claude` | Port with Claude environment whitelisting. |
| `pkg/foreignloop/claude/spec.go` | `driver/claude` | Replace backend `Spec` construction with agent-only `Config` and `NewAgent`. |
| `pkg/foreignloop/claude/spec_test.go` | `driver/claude` | Port as agent configuration and constructor coverage. |
| `pkg/foreignloop/claude/transcript.go` | `driver/claude` | Port Claude transcript-path validation used by authoritative history. |
| `pkg/foreignloop/claude/transcript_test.go` | `driver/claude` | Port transcript-path validation coverage. |
| `pkg/foreignloop/claude/wrap_test.go` | `driver/claude` | Port process-wrapper seam coverage. |
| `pkg/foreignloop/codex/args.go` | `driver/codex` | Port Codex start/resume argument construction. |
| `pkg/foreignloop/codex/args_test.go` | `driver/codex` | Port with Codex argument construction. |
| `pkg/foreignloop/codex/codex.go` | `driver/codex` | Port the Codex process driver and unavailable-history behavior. |
| `pkg/foreignloop/codex/codex_integration_test.go` | `driver/codex` | Port resume-probe and bounded subprocess integration coverage. |
| `pkg/foreignloop/codex/codex_test.go` | `driver/codex` | Port process, event, cancellation, and close coverage. |
| `pkg/foreignloop/codex/decode.go` | `driver/codex` | Port the Codex JSONL wire decoder. |
| `pkg/foreignloop/codex/decode_fuzz_test.go` | `driver/codex` | Port the Codex decoder fuzz target. |
| `pkg/foreignloop/codex/decode_test.go` | `driver/codex` | Port Codex decoder and terminal-error coverage. |
| `pkg/foreignloop/codex/doc.go` | `driver/codex` | Replace with extracted Codex driver package documentation. |
| `pkg/foreignloop/codex/env.go` | `driver/codex` | Port Codex environment whitelisting. |
| `pkg/foreignloop/codex/env_test.go` | `driver/codex` | Port with Codex environment whitelisting. |
| `pkg/foreignloop/codex/spec.go` | `driver/codex` | Replace backend `Spec` construction with agent-only `Config` and `NewAgent`; keep Codex sandbox and approval types here. |
| `pkg/foreignloop/codex/spec_test.go` | `driver/codex` | Port as agent configuration and constructor coverage. |
| `pkg/foreignloop/decode_fuzz_test.go` | `driver/claude` | Port both stream and transcript decoder fuzz targets. |
| `pkg/foreignloop/decode_stream.go` | `driver/claude` | Port the Claude stream wire decoder as an unexported driver implementation. |
| `pkg/foreignloop/decode_stream_test.go` | `driver/claude` | Port Claude stream fixture coverage. |
| `pkg/foreignloop/decode_transcript.go` | `driver/claude` | Port authoritative Claude history decoding without the obsolete turn offset. |
| `pkg/foreignloop/decode_transcript_test.go` | `driver/claude` | Port transcript fixture and golden-projection coverage. |
| `pkg/foreignloop/errors.go` | `driver` | Move spawn, decode, exit, result, protocol, and transcript/history errors to `driver`; rehome config, busy-session, and snapshot errors in `backend`. |
| `pkg/foreignloop/export.go` | `delete` | Delete after the Claude decoder moves. `DecodeStream` has no public replacement; `driver/claude` calls its own unexported decoder. |
| `pkg/foreignloop/fake_test.go` | `backend` | Port shared backend actor, publisher, stream, and ID test fakes. |
| `pkg/foreignloop/foreignloop.go` | `driver` | Move neutral agent, turn, stream, event, kind, and posture contracts to `driver`; move `SIDMode` to `backend` and `EventPublisher` to `harness/pkg/foreign`. |
| `pkg/foreignloop/foreignloop_test.go` | `driver` | Port zero-value and neutral turn-contract coverage; backend-only enum coverage follows `SIDMode`. |
| `pkg/foreignloop/header.go` | `backend` | Port Harness event-header projection. |
| `pkg/foreignloop/lock.go` | `backend` | Port per-session and temporary late-bound process locks. |
| `pkg/foreignloop/lock_test.go` | `backend` | Port lock path, namespace, stale-PID, and release coverage. |
| `pkg/foreignloop/loop.go` | `backend` | Port the concrete actor lifecycle and managed-input queue. |
| `pkg/foreignloop/loop_test.go` | `backend` | Port backend construction, actor, queue, interrupt, shutdown, and binding coverage. |
| `pkg/foreignloop/mapper.go` | `backend` | Port driver-event to Harness-event mapping. |
| `pkg/foreignloop/mapper_test.go` | `backend` | Port mapper ordering, correlation, and error coverage. |
| `pkg/foreignloop/restored.go` | `harness/pkg/foreign` | Move `RestoredForeign` and `RestoredBuilder` to the Harness seam; move restored backend construction and `BuildRestoredWith` to `backend`. |
| `pkg/foreignloop/restored_test.go` | `backend` | Port restored-state validation, folding, snapshot, and turn-index coverage. |
| `pkg/foreignloop/snapshot.go` | `backend` | Port defensive snapshot request and clone behavior. |
| `pkg/foreignloop/testdata/stream/empty.jsonl` | `driver/claude` | Copy as a Claude stream decoder fixture. |
| `pkg/foreignloop/testdata/stream/garbage.jsonl` | `driver/claude` | Copy as a Claude stream decoder fixture. |
| `pkg/foreignloop/testdata/stream/happy.jsonl` | `driver/claude` | Copy as a Claude stream decoder fixture. |
| `pkg/foreignloop/testdata/stream/unknown.jsonl` | `driver/claude` | Copy as a Claude stream decoder fixture. |
| `pkg/foreignloop/testdata/transcript/empty.jsonl` | `driver/claude` | Copy as a Claude transcript decoder fixture and golden input. |
| `pkg/foreignloop/testdata/transcript/happy.jsonl` | `driver/claude` | Copy as a Claude transcript decoder fixture and golden input. |
| `pkg/foreignloop/testdata/transcript/truncated.jsonl` | `driver/claude` | Copy as a Claude transcript decoder fixture and golden input. |
| `pkg/foreignloop/turn.go` | `backend` | Port turn execution, close-before-history commit, and fallback behavior. |
| `pkg/foreignloop/turn_test.go` | `backend` | Port event ordering, history/transcript fallback, queue, and terminal coverage. |

## Harness importers outside `pkg/foreignloop`

These are the current Go files outside the old tree that import
`github.com/looprig/harness/pkg/foreignloop`.

| Harness importer | Final owner or action | Migration note |
|---|---|---|
| `internal/sessionruntime/command_journal.go` | `harness/pkg/foreign` | Repoint builder seam types to the public Harness package. |
| `internal/sessionruntime/foreign_e2e_test.go` | `tests` | Port the eight real backend/session scenarios below, then delete the Harness copies. |
| `internal/sessionruntime/foreign_newloop_test.go` | `harness/pkg/foreign` | Keep Harness builder-selection behavior with seam fakes. |
| `internal/sessionruntime/lifecycle.go` | `harness/pkg/foreign` | Repoint lifecycle builder types to the public Harness package. |
| `internal/sessionruntime/loop_tools_test.go` | `harness/pkg/foreign` | Keep `TestReplaceExternalToolsRefusedOnForeignLoop` in Harness using a seam fake. |
| `internal/sessionruntime/restore_constructor.go` | `harness/pkg/foreign` | Repoint restored value and builder types to the public Harness package. |
| `internal/sessionruntime/session.go` | `harness/pkg/foreign` | Repoint stored builder fields to the public Harness package. |
| `pkg/rig/options.go` | `harness/pkg/foreign` | Repoint the public rig option to the public Harness seam. |
| `pkg/rig/rig_test.go` | `harness/pkg/foreign` | Keep rig option validation with seam builder fakes. |

## Cross-module end-to-end test coverage

The `github.com/looprig/tests` module owns all tests that compose real Harness
sessions with the extracted backend. The old Harness copies are removed only
after these replacements pass through public APIs.

| Existing Harness test | Tests-module replacement |
|---|---|
| `TestForeignPrimaryE2E` | `TestForeignloopPrimary` |
| `TestCodexForeignPrimaryLateBoundPublishesBoundAndTurnDone` | `TestForeignloopCodexPrimaryLateBound` |
| `TestForeignSubagentE2E` | `TestForeignloopSubagent` |
| `TestForeignQueuedDelegateInterruptResolvesWithoutWaitTimeout` | `TestForeignloopQueuedDelegateInterrupt` |
| `TestForeignQueuedDelegateTimeoutCancelsOnlyThatRequest` | `TestForeignloopQueuedDelegateTimeout` |
| `TestForeignProviderFailureResolvesQueuedDelegatesFailedLive` | `TestForeignloopProviderFailureWithQueuedDelegates` |
| `TestCodexForeignSubagentLateBoundReturnsFinalText` | `TestForeignloopCodexSubagentLateBound` |
| `TestForeignSubagentQuotaCap` | `TestForeignloopSubagentQuota` |
