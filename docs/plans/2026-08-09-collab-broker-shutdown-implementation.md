# Collaboration Broker Shutdown Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make cancellation of a collaboration broker bounded for broker-owned lifecycle goroutines while preserving bounded caller joins and cooperative call cancellation.

**Architecture:** Extract the idempotent endpoint/capability revocation phase from `collabBroker.Close`. The session-context watcher performs only that phase and closes its lifecycle completion channel; explicit `Close(ctx)` performs the phase and then waits for `acceptDone` and `handlersDone` under `ctx`. A permanently noncooperative controller may retain its handler until test cleanup releases it, but no watcher waits on it.

**Tech Stack:** Go, `context`, Unix-domain broker listener, `go test`/race detector.

---

### Task 1: Add lifecycle instrumentation and a failing regression test

**Files:**
- Modify: `internal/sessionruntime/collab_broker.go` (add watcher completion state used by the test)
- Modify: `internal/sessionruntime/collab_broker_test.go` (permanently noncooperative lifecycle test and repeated-session assertions)

**Step 1: Write the failing test**

Add a test that starts a broker with a controller blocked until cleanup, admits one request, cancels the broker/session context, and asserts `watchDone` and `acceptDone` close while a bounded `Close` returns its context deadline. Add a repeated canceled-broker loop that asserts each watcher/accept completion channel closes and endpoint paths disappear.

**Step 2: Run test to verify it fails**

Run: `go test ./internal/sessionruntime -run TestCollabBrokerWatcherDoesNotOutliveNonCooperativeHandler -count=1 -v`

Expected: FAIL because the existing watcher calls `Close(context.Background())` and remains blocked on `handlersDone`.

### Task 2: Split stop from the bounded Close join

**Files:**
- Modify: `internal/sessionruntime/collab_broker.go`

**Step 1: Write minimal implementation**

Add an idempotent `stop` method containing the existing `closeOnce` body. Start the context watcher with a `defer` that closes `watchDone`, wait for broker context cancellation, and call only `stop`. Make `Close(ctx)` call `stop` before its existing context-bounded `acceptDone` and `handlersDone` joins.

**Step 2: Run test to verify it passes**

Run: `go test ./internal/sessionruntime -run TestCollabBrokerWatcherDoesNotOutliveNonCooperativeHandler -count=1 -v`

Expected: PASS; endpoint/token authority is gone, caller close remains bounded, and watcher/accept completion channels close.

### Task 3: Verify the focused lifecycle surface

**Files:**
- Test: `internal/sessionruntime/collab_broker_test.go`
- Test: `internal/sessionruntime/collab_lifecycle_test.go`

**Step 1: Run focused tests**

Run: `go test ./internal/sessionruntime -run 'TestCollabBroker|TestCollabLifecycle' -count=20`

Expected: PASS (or deterministic platform skips for unavailable Unix sockets), with no panic or race.

**Step 2: Run race stress**

Run: `go test -race ./internal/sessionruntime -run 'TestCollabBrokerWatcherDoesNotOutliveNonCooperativeHandler|TestCollabBrokerCloseReturnsAtDeadlineForNonCooperativeController|TestCollabLifecycle' -count=10`

Expected: PASS with no race reports.

### Task 4: Run repository verification and commit

**Files:**
- All modified files above.

**Step 1: Run full tests and static checks**

Run: `go test ./...`

Run: `go test -race ./internal/sessionruntime -count=1`

Run: `go vet ./...`

Run: `GOOS=linux GOARCH=amd64 go test ./internal/sessionruntime -run '^$'`

Run: `git diff --check` and `git status --short`.

Expected: all commands pass; only the intended broker/design files are dirty.

**Step 2: Commit**

```bash
git add internal/sessionruntime/collab_broker.go internal/sessionruntime/collab_broker_test.go docs/plans/2026-08-09-collab-broker-shutdown-implementation.md
git commit -m "fix(session): bound collaboration broker watcher shutdown"
```

