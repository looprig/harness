# Foreignloop extraction coverage manifest

This is the final checked ownership record for the completed removal of
`pkg/foreignloop`. Each of the 60 files present immediately before Task 18 has a
principal final owner below, including the five migration/golden files added
during the extraction overlap. Where one old file contained declarations split
across destinations, the note records that split explicitly. Tests and fixtures
moved with the behavior they cover; nothing was silently dropped.

Task 18 removed the temporary source-completeness check with the old directory
and replaced it with a permanent all-source import guard. That guard rejects both
the old concrete package and the extracted module from production files, tests,
nested directories, and inactive build-tag files.

Final counts: 60 of 60 old files classified and removed from Harness; 53 of 53
driver-neutral/backend tests classified; 9 of 9 outside importers migrated or
deleted; and 8 of 8 real session integrations passed in `github.com/looprig/tests`
before their Harness copies were removed.

## Final source ownership (60 of 60)

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
| `pkg/foreignloop/errors.go` | `driver` | Move spawn, process-exit, stream-decode, and authoritative-history errors to `driver`; rehome `ForeignResultError`, `ForeignProtocolError`, config, lock/busy, and snapshot errors in `backend`. |
| `pkg/foreignloop/export.go` | `delete` | Delete after the Claude decoder moves. `DecodeStream` has no public replacement; `driver/claude` calls its own unexported decoder. |
| `pkg/foreignloop/fake_test.go` | `backend` | Port shared backend actor, publisher, stream, and ID test fakes. |
| `pkg/foreignloop/foreignloop.go` | `driver` | Move neutral agent, turn, stream, event, kind, and posture contracts to `driver`; move `SIDMode` to `backend`, and move both `EventPublisher` and `Builder` to `harness/pkg/foreign`. |
| `pkg/foreignloop/foreignloop_test.go` | `driver` | Port zero-value and neutral turn-contract coverage; backend-only enum coverage follows `SIDMode`. |
| `pkg/foreignloop/header.go` | `backend` | Port Harness event-header projection. |
| `pkg/foreignloop/lock.go` | `backend` | Port per-session and temporary late-bound process locks. |
| `pkg/foreignloop/lock_test.go` | `backend` | Port lock path, namespace, stale-PID, and release coverage. |
| `pkg/foreignloop/loop.go` | `backend` | Port the concrete actor lifecycle and managed-input queue. |
| `pkg/foreignloop/loop_test.go` | `backend` | Port backend construction, actor, queue, interrupt, shutdown, and binding coverage. |
| `pkg/foreignloop/mapper.go` | `backend` | Port driver-event to Harness-event mapping. |
| `pkg/foreignloop/mapper_test.go` | `backend` | Port mapper ordering, correlation, and error coverage. |
| `pkg/foreignloop/migration_export.go` | `delete` | Delete the temporary legacy decoder export after cross-module parity passes. |
| `pkg/foreignloop/restored.go` | `harness/pkg/foreign` | Move `RestoredForeign` and `RestoredBuilder` to the Harness seam; move restored backend construction and `BuildRestoredWith` to `backend`. |
| `pkg/foreignloop/restored_test.go` | `backend` | Port restored-state validation, folding, snapshot, and turn-index coverage. |
| `pkg/foreignloop/snapshot.go` | `backend` | Port defensive snapshot request and clone behavior. |
| `pkg/foreignloop/testdata/stream/empty.jsonl` | `driver/claude` | Copy as a Claude stream decoder fixture. |
| `pkg/foreignloop/testdata/stream/garbage.jsonl` | `driver/claude` | Copy as a Claude stream decoder fixture. |
| `pkg/foreignloop/testdata/stream/happy.jsonl` | `driver/claude` | Copy as a Claude stream decoder fixture. |
| `pkg/foreignloop/testdata/stream/unknown.jsonl` | `driver/claude` | Copy as a Claude stream decoder fixture. |
| `pkg/foreignloop/testdata/transcript/empty.golden.json` | `driver/claude` | Retain the permanent empty-history projection golden in the extracted decoder suite. |
| `pkg/foreignloop/testdata/transcript/empty.jsonl` | `driver/claude` | Copy as a Claude transcript decoder fixture and golden input. |
| `pkg/foreignloop/testdata/transcript/happy.golden.json` | `driver/claude` | Retain the permanent happy-history projection golden in the extracted decoder suite. |
| `pkg/foreignloop/testdata/transcript/happy.jsonl` | `driver/claude` | Copy as a Claude transcript decoder fixture and golden input. |
| `pkg/foreignloop/testdata/transcript/missing.golden.json` | `driver/claude` | Retain the permanent provider-neutral missing-history projection golden in the extracted decoder suite. |
| `pkg/foreignloop/testdata/transcript/truncated.golden.json` | `driver/claude` | Retain the permanent truncated-history projection golden in the extracted decoder suite. |
| `pkg/foreignloop/testdata/transcript/truncated.jsonl` | `driver/claude` | Copy as a Claude transcript decoder fixture and golden input. |
| `pkg/foreignloop/turn.go` | `backend` | Port turn execution, close-before-history commit, and fallback behavior. |
| `pkg/foreignloop/turn_test.go` | `backend` | Port event ordering, history/transcript fallback, queue, and terminal coverage. |

## Checked backend behavior-test classification

Task 14 classifies every test in the old driver-neutral/backend files. The count
is 53 of 53 classified: 52 moved to the extracted module (some into consolidated
table tests), zero retained in Harness, zero assigned to the tests module, and
one implementation-detail test intentionally deleted with the kernel-lock
rationale recorded below. The eight Harness session integrations are a separate
inventory below and remain assigned to the tests module.

| Old Harness test | Classification | Extracted replacement or rationale |
|---|---|---|
| `TestPostureZeroAndForeignTurnShape` | moved | `driver.TestPermissionPostureValues` and `driver.TestTurnRetainsEveryField` |
| `TestForeignLockPath` | moved | `backend.TestForeignLockPath`, `backend.TestForeignLockPathContainsHashedOpaqueIdentifiers`, and `backend.TestForeignLockOpaqueIdentifierCannotCreateOutsideRoot`; the path remains deterministic and workspace-scoped, while the complete opaque SID is now hashed into one immediate lock-root child instead of entering a filename or path. |
| `TestTemporaryForeignLockNamespaceDoesNotCollideWithDurableSID` | moved | `backend.TestTemporaryForeignLockNamespaceDoesNotCollideWithDurableSID` |
| `TestProcessAlive` | intentionally deleted | PID liveness inference was an unsafe implementation detail of unlink/recreate ownership. Kernel `flock` now releases ownership atomically when the descriptor closes or the process exits; `backend.TestForeignLockCrashIsNaturallyReclaimable` proves the observable crash-reclaim invariant without a PID probe race. |
| `TestAcquireForeignLock` | moved | `backend.TestAcquireForeignLock`, `backend.TestForeignLockOnlyOneConcurrentOwner`, `backend.TestForeignLockCrashIsNaturallyReclaimable`, and `backend.TestForeignLockRejectsSymlinkAndHardlinkFiles` prove atomic acquisition, busy classification, concurrency, crash reclaim, and no-follow/owner-only file validation. |
| `TestForeignLockBusyTurnFailed` | moved | `backend.TestBusyAndStaleDurableLocksAtActorBoundary/busy_holder_fails_before_spawn` |
| `TestForeignLockStaleProceeds` | moved | `backend.TestBusyAndStaleDurableLocksAtActorBoundary/unlocked_metadata_is_reclaimed_and_kernel_lock_released` and `backend.TestForeignLockCrashIsNaturallyReclaimable`; stale text metadata no longer owns the lock, while a crashed descriptor is naturally reclaimable. |
| `TestForeignLockReleaseIdempotent` | moved | `backend.TestForeignLockReleaseIdempotent` and `backend.TestForeignLockOldReleaseCannotAffectSuccessor`; release is idempotent, preserves the stable inode, and an obsolete owner cannot unlink or unlock its successor. |
| `TestBackendInterface` | moved | `backend.TestBackendInterfaceAndConstruction` |
| `TestNewValidation` | moved | `backend_test.TestBuildWithEagerlyValidatesConfig`, `backend_test.TestBuildWithRejectsTypedNilAgent`, and `backend.TestNewRuntimeWiringAndInstructionsOnlyParity`; nil and typed-nil dependencies both fail closed before actor or process work starts. |
| `TestValidateWiringAcceptsInstructionsOnlyPrompt` | moved | `backend.TestNewRuntimeWiringAndInstructionsOnlyParity` |
| `TestNewLateBoundSpecReturnsEmptyInitialSID` | moved | `backend.TestNewLateBoundDoesNotMintSID` |
| `TestNewRejectsUnknownSIDMode` | moved | `backend_test.TestBuildWithEagerlyValidatesConfig/unknown_sid_mode` |
| `TestShutdownClosesDone` | moved | `backend.TestBackendInterfaceAndConstruction` (its checked `shutdown` helper asserts ack and `Done`) |
| `TestManagedAcceptanceMintFailureReturnsExactErrorAndStartsNoForeignWork` | moved | `backend.TestManagedAcceptanceMintFailurePreservesExactErrorAndStartsNoWork` |
| `TestManagedAcceptanceAppendFailureReturnsExactErrorAndStartsNoForeignWork` | moved | `backend.TestManagedAcceptanceAppendFailurePreservesExactErrorAndStartsNoWork` |
| `TestSnapshotFreshLoop` | moved | `backend.TestSnapshotFreshLoop` |
| `TestInterruptWhileIdle` | moved | `backend.TestInterruptWhileIdleAndSnapshotAfterExit` |
| `TestSnapshotAfterExit` | moved | `backend.TestInterruptWhileIdleAndSnapshotAfterExit` |
| `TestMapperToEvents` | moved | `backend.TestMapperToEvents` |
| `TestMapperCorrelation` | moved | `backend.TestMapperCorrelation` |
| `TestNewRestoredValidation` | moved | `backend.TestRestoreConstructionValidation` and `backend.TestBuildRestoredWithRejectsTypedNilPublisher` |
| `TestNewRestoredSeedSnapshot` | moved | `backend.TestBuildRestoredWithStartsRestoredActor`, `backend.TestRestoreConstructionDeepClonesSeed`, `backend.TestSnapshotDeepClonesActorStateAndOtherSnapshots`, `backend.TestSnapshotMutationDoesNotRaceActorState`, and `backend.TestDeepClonePreservesNilAndEmptyShape` prove the restored value and every returned snapshot are recursively defensive, including nested blocks, bytes, JSON, usage, and nil/empty shape. |
| `TestNewRestoredMarksForeignSIDBound` | moved | `backend.TestRestoreConstructionSeedsActorState` |
| `TestNewRestoredResumesSession` | moved | `backend.TestRestoredActorResumesSIDAndAppendsAfterSeed` |
| `TestBuildRestoredWith` | moved | `backend.TestBuildRestoredWithRejectsMissingForeignSessionID` and `backend.TestBuildRestoredWithStartsRestoredActor` |
| `TestForeignTurnUsesEffectiveSystemPrompt` | moved | `backend.TestEffectiveSystemPromptParity` |
| `TestManagedFollowUpsQueueFIFOWhileForeignTurnActive` | moved | `backend.TestManagedQueueRunsAllAcceptedInputsFIFO` |
| `TestCancelDelegateRequestRemovesOnlyQueuedForeignRequest` | moved | `backend.TestCancelQueuedRequestPreservesOthers` |
| `TestCancelDelegateRequestInterruptsActiveForeignRequestButPreservesNext` | moved | `backend.TestTargetedCancelActivePreservesNextQueuedInput` |
| `TestInterruptFlushesAcceptedForeignFollowUps` | moved | `backend.TestInterruptFlushesAcceptedQueueWithoutSpawningIt` |
| `TestShutdownFlushesAcceptedForeignFollowUps` | moved | `backend.TestShutdownFlushesAcceptedQueue` |
| `TestManagedForeignQueueRejectsEntrySixtyFiveBeforeAcceptance` | moved | `backend.TestManagedQueueFIFOAndExactCapacity` |
| `TestForeignProviderFailureFlushesAcceptedQueueFailedWithoutStartingIt` | moved | `backend.TestProviderFailureFlushesAcceptedQueueWithoutSpawningIt` |
| `TestUserInputHappyPath` | moved | `backend.TestUserInputPublishesStartedBeforeSpawnAndHistoryBeforeDone` |
| `TestLateBoundSessionPublishesForeignSessionBound` | moved | `backend.TestLateBoundSessionBoundSequenceHeaderAndResumeParity` |
| `TestLateBoundTurnLockLifecycleOrder` | moved | `backend.TestLateBoundTurnLockLifecycleOrderMatchesPredecessor` |
| `TestLateBoundFirstTurnHoldsBoundSIDLock` | moved | `backend.TestLateBoundLockLifecycleAndResume` |
| `TestLateBoundFirstTurnsUseIndependentTemporaryLocks` | moved | `backend.TestLateBoundFirstTurnsUseIndependentTemporaryLocks` |
| `TestLateBoundLockTransitionFailurePersistsSID` | moved | `backend.TestLateBoundTransitionFailurePersistsSIDForBusyResume` |
| `TestSpawnFailureTurnFailed` | moved | `backend.TestSpawnProtocolInterruptAndShutdown/spawn` |
| `TestCloseErrorFailsTurn` | moved | `backend.TestCloseErrorsFailSuccessfulTerminalAndRetainType` |
| `TestTerminalAndCloseErrorsRetainBothTypedCauses` | moved | `backend.TestTurnEventOrderingAndDriverErrorIdentity` |
| `TestEOFWithoutForeignTerminalFailsTurn` | moved | `backend.TestSpawnProtocolInterruptAndShutdown/protocol` |
| `TestLateBoundFailureBeforeInitRetriesStartNew` | moved | `backend.TestLateBoundPreInitFailureRetriesStartNew` |
| `TestLateBoundTerminalOKBeforeInitFailsProtocolAndRetriesStartNew` | moved | `backend.TestLateBoundTerminalBeforeInitRetainsErrorsAndRetriesStartNew/terminal_OK` |
| `TestLateBoundTerminalErrorBeforeInitPreservesResultAndProtocolErrors` | moved | `backend.TestLateBoundTerminalBeforeInitRetainsErrorsAndRetriesStartNew/terminal_error` |
| `TestPreboundTerminalWithoutInitSucceeds` | moved | `backend.TestUserInputPublishesStartedBeforeSpawnAndHistoryBeforeDone` |
| `TestTranscriptLossSoftDegrade` | moved | `backend.TestTypedHistoryFailureUsesFallbackAndPreservesSuccessfulTerminal` and `backend.TestHistoryFallbackWarningParity/missing_transcript_stays_silent` |
| `TestInterruptDuringTurn` | moved | `backend.TestInterruptLeavesSnapshotUncommittedAfterUnsupportedCommand` |
| `TestLateBoundInterruptedFirstTurnResumesBoundSession` | moved | `backend.TestLateBoundLockLifecycleAndResume` |
| `TestDropCommandDuringTurnThenInterrupt` | moved | `backend.TestInterruptLeavesSnapshotUncommittedAfterUnsupportedCommand` |
| `TestShutdownDuringTurn` | moved | `backend.TestSpawnProtocolInterruptAndShutdown/shutdown` |

### Lock, snapshot, and composition hardening rationale

These quality fixes preserve the old observable busy, crash-reclaim, release,
restore, and snapshot contracts while removing unsafe implementation details.
They are not durable schema or event changes.

| Concern | Design rationale | Checked evidence |
|---|---|---|
| Path containment and opaque foreign SIDs | Durable and temporary lock names use separate namespaces plus complete hashes of the cleaned workspace and opaque identifier. Neither untrusted value can create a separator, escape the owner-only lock root, expose the SID, or grow the filename. Existing deterministic/same-input and distinct-input behavior remains. | `TestForeignLockPath`, `TestForeignLockPathContainsHashedOpaqueIdentifiers`, `TestForeignLockOpaqueIdentifierCannotCreateOutsideRoot`, `TestTemporaryForeignLockNamespaceDoesNotCollideWithDurableSID`, and `TestForeignLockRejectsSymlinkAndHardlinkFiles` |
| Atomic ownership, concurrency, and crash reclaim | Darwin/Linux use a non-blocking kernel `flock` on an owner-owned, no-follow regular file opened relative to a verified directory descriptor. Exactly one contender owns the lock; the kernel releases a crashed process's descriptor without trusting stale PID-file liveness. PID text remains diagnostic only. | `TestAcquireForeignLock`, `TestForeignLockOnlyOneConcurrentOwner`, `TestForeignLockCrashIsNaturallyReclaimable`, and `TestBusyAndStaleDurableLocksAtActorBoundary` |
| Persistent inode instead of unlink/recreate | Normal release unlocks and closes but never unlinks the stable lock inode. `sync.Once` makes repeated or concurrent release harmless, and an old owner cannot remove or unlock a successor's ownership. This deliberately replaces the predecessor's unsafe unlink/recreate implementation while preserving idempotent release and subsequent acquisition. | `TestForeignLockReleaseIdempotent` and `TestForeignLockOldReleaseCannotAffectSuccessor` |
| Unsupported platforms fail closed | Native lock ownership is supported only on macOS and Linux excluding Android/iOS. Every other build selects `lock_unsupported.go`, whose acquisition returns a typed `LockError` wrapping the unsupported-platform cause before spawning an agent; there is no lock-free fallback. | Cross-platform compile checks select the unsupported implementation; the exact public/error-set guard confirms no new public platform escape hatch or error class. |
| Deep defensive snapshot scope | Restore input and each `Snapshot` result recursively clone every current Core conversation and block variant, nested tool-result content, mutable bytes/JSON, and usage values while preserving nil versus non-nil empty slices. Unknown future variants fail closed instead of silently aliasing actor state. | `TestRestoreConstructionDeepClonesSeed`, `TestDeepClonePreservesNilAndEmptyShape`, `TestSnapshotDeepClonesActorStateAndOtherSnapshots`, and race-enabled `TestSnapshotMutationDoesNotRaceActorState` |
| Typed-nil composition dependencies | Interface values containing nil agent or publisher pointers are rejected as required-field `ConfigError` values before builder construction, actor startup, or method dispatch. Concrete non-nil value implementations remain valid. | `backend_test.TestBuildWithRejectsTypedNilAgent` and `backend.TestBuildRestoredWithRejectsTypedNilPublisher` |

## Checked public API and error ownership

The extracted module's boundary tests compare these exact top-level symbol sets
against production Go files across build tags. Receiver methods required by
`loop.Backend` (`CommandSink`, `DoneChan`, and `Snapshot`) remain on `backend.Loop`
and are tested through the interface contract.

| Package | Exact public top-level symbols |
|---|---|
| `driver` | `Agent`, `DecodeError`, `Event`, `ExitError`, `History`, `HistoryError`, `Kind`, `KindInit`, `KindTextDelta`, `KindThinkingDelta`, `KindToolUse`, `KindToolResult`, `KindStepComplete`, `KindTerminalOK`, `KindTerminalError`, `PermissionPosture`, `PostureDefault`, `PostureAcceptEdits`, `SpawnError`, `Stream`, `Turn` |
| `backend` | `BuildRestoredWith`, `BuildWith`, `Config`, `ConfigError`, `ForeignProtocolError`, `ForeignResultError`, `ForeignSessionBusyError`, `LockError`, `Loop`, `New`, `SIDLateBound`, `SIDMode`, `SIDPrebound`, `SnapshotContextDone`, `SnapshotError`, `SnapshotErrorReason`, `SnapshotLoopExited` |

The checked typed-error ownership is:

| Owner | Exported error types |
|---|---|
| `driver` | `DecodeError`, `ExitError`, `HistoryError`, `SpawnError` |
| `backend` | `ConfigError`, `ForeignProtocolError`, `ForeignResultError`, `ForeignSessionBusyError`, `LockError`, `SnapshotError` |
| `driver/claude` | `ConfigError`, `PathError`, `PlatformError`, `SpawnConfigError`, `WrapError` |
| `driver/codex` | `ConfigError`, `PlatformError`, `SpawnConfigError` |

`driver/claude.DecodeTranscriptForMigration`, the corresponding legacy Harness
export, and the tests-module parity test were temporary migration-only artifacts.
Task 18 deleted all three after decoder parity and all eight public integrations
passed. The permanent decoder fixtures, projection goldens, unit tests, and fuzz
targets remain in `driver/claude`.

## Harness importers outside `pkg/foreignloop`

These are the nine Go importers outside the old tree recorded during migration.
All nine final dispositions are complete; no file below imports the old package
or the extracted module from Harness.

| Harness importer | Final owner or action | Migration note |
|---|---|---|
| `internal/sessionruntime/command_journal.go` | `harness/pkg/foreign` | Repointed builder seam types to the public Harness package. |
| `internal/sessionruntime/foreign_e2e_test.go` | `tests` | Ported all eight real backend/session scenarios, proved them through public APIs, then deleted the Harness copies. |
| `internal/sessionruntime/foreign_newloop_test.go` | `harness/pkg/foreign` | Retained builder selection and missing-builder behavior with seam fakes. |
| `internal/sessionruntime/lifecycle.go` | `harness/pkg/foreign` | Repointed lifecycle builder types to the public Harness package. |
| `internal/sessionruntime/loop_tools_test.go` | `harness/pkg/foreign` | Retained `TestReplaceExternalToolsRefusedOnForeignLoop` with a seam fake. |
| `internal/sessionruntime/restore_constructor.go` | `harness/pkg/foreign` | Repointed restored value and builder types to the public Harness package. |
| `internal/sessionruntime/session.go` | `harness/pkg/foreign` | Repointed stored builder fields to the public Harness package. |
| `pkg/rig/options.go` | `harness/pkg/foreign` | Repointed the public rig option to the public Harness seam. |
| `pkg/rig/rig_test.go` | `harness/pkg/foreign` | Retained rig option validation with seam builder fakes. |

## Cross-module end-to-end test coverage

The `github.com/looprig/tests` module owns all tests that compose real Harness
sessions with the extracted backend. Task 18 ran all eight replacements together
with `-tags integration -race`; they passed before the old Harness copies were
removed.

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
