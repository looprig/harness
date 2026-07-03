package foreignloop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
)

// deadPID returns a process id guaranteed to be dead: it runs `true` to completion
// (Run waits and reaps the child), so the returned pid no longer names a live
// process. This drives the stale-lock reclaim path deterministically.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

// preWriteLock writes raw content to the (sid,cwd) lockfile and registers cleanup, so
// a test can stage an existing lock (live, dead, or malformed) before acquiring.
func preWriteLock(t *testing.T, sid, cwd, content string) {
	t.Helper()
	path := foreignLockPath(sid, cwd)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir lock dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("pre-write lock: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

func TestForeignLockPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		sidA, cwdA string
		sidB, cwdB string
		wantSame   bool
	}{
		{name: "same inputs are deterministic", sidA: "s", cwdA: "/work/a", sidB: "s", cwdB: "/work/a", wantSame: true},
		{name: "trailing slash cleaned equal", sidA: "s", cwdA: "/work/a/", sidB: "s", cwdB: "/work/a", wantSame: true},
		{name: "different cwd differs", sidA: "s", cwdA: "/work/a", sidB: "s", cwdB: "/work/b", wantSame: false},
		{name: "different sid differs", sidA: "s1", cwdA: "/work/a", sidB: "s2", cwdB: "/work/a", wantSame: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := foreignLockPath(tt.sidA, tt.cwdA)
			b := foreignLockPath(tt.sidB, tt.cwdB)
			if (a == b) != tt.wantSame {
				t.Fatalf("foreignLockPath equality = %v (a=%q b=%q), want %v", a == b, a, b, tt.wantSame)
			}
			if !strings.HasPrefix(a, filepath.Join(os.TempDir(), "looprig-foreign")) {
				t.Fatalf("lock path %q not under looprig-foreign tempdir", a)
			}
			if !strings.HasSuffix(a, ".lock") || !strings.Contains(a, tt.sidA) {
				t.Fatalf("lock path %q missing .lock suffix or sid", a)
			}
		})
	}
}

func TestProcessAlive(t *testing.T) {
	t.Parallel()
	dead := deadPID(t)
	tests := []struct {
		name      string
		pid       int
		wantAlive bool
	}{
		{name: "self is alive", pid: os.Getpid(), wantAlive: true},
		{name: "zero pid not alive", pid: 0, wantAlive: false},
		{name: "negative pid not alive", pid: -1, wantAlive: false},
		{name: "reaped child not alive", pid: dead, wantAlive: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := processAlive(tt.pid); got != tt.wantAlive {
				t.Fatalf("processAlive(%d) = %v, want %v", tt.pid, got, tt.wantAlive)
			}
		})
	}
}

func TestAcquireForeignLock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		pre          func(t *testing.T, sid, cwd string) // stage an existing lock (or nil)
		acquireFirst bool                                // acquire once before the assertion acquire
		wantBusy     bool
	}{
		{name: "fresh path acquires", wantBusy: false},
		{name: "second acquire while held is busy", acquireFirst: true, wantBusy: true},
		{name: "live holder is busy", pre: func(t *testing.T, sid, cwd string) {
			preWriteLock(t, sid, cwd, strconv.Itoa(os.Getpid()))
		}, wantBusy: true},
		{name: "stale dead holder reclaimed", pre: func(t *testing.T, sid, cwd string) {
			preWriteLock(t, sid, cwd, strconv.Itoa(deadPID(t)))
		}, wantBusy: false},
		{name: "malformed pid reclaimed", pre: func(t *testing.T, sid, cwd string) {
			preWriteLock(t, sid, cwd, "not-a-pid")
		}, wantBusy: false},
		{name: "empty lock reclaimed", pre: func(t *testing.T, sid, cwd string) {
			preWriteLock(t, sid, cwd, "")
		}, wantBusy: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const sid = "00000000-0000-0000-0000-000000000001"
			cwd := t.TempDir()
			if tt.pre != nil {
				tt.pre(t, sid, cwd)
			}
			if tt.acquireFirst {
				first, err := acquireForeignLock(sid, cwd)
				if err != nil {
					t.Fatalf("first acquire: %v", err)
				}
				t.Cleanup(first.release)
			}
			lk, err := acquireForeignLock(sid, cwd)
			if tt.wantBusy {
				var busy *ForeignSessionBusyError
				if !errors.As(err, &busy) {
					t.Fatalf("acquire err = %T %v, want *ForeignSessionBusyError", err, err)
				}
				if busy.SID != sid || busy.Cwd != cwd {
					t.Fatalf("busy error coords = %s/%s, want %s/%s", busy.SID, busy.Cwd, sid, cwd)
				}
				return
			}
			if err != nil {
				t.Fatalf("acquire err = %v, want success", err)
			}
			t.Cleanup(lk.release)
			// The freshly written lock records our own pid.
			b, rerr := os.ReadFile(lk.path)
			if rerr != nil {
				t.Fatalf("read lock: %v", rerr)
			}
			if got := strings.TrimSpace(string(b)); got != strconv.Itoa(os.Getpid()) {
				t.Fatalf("lock pid = %q, want %d", got, os.Getpid())
			}
		})
	}
}

// waitLockReleased polls until the lockfile at path no longer exists, proving the turn
// goroutine's deferred release ran (it races the actor's turnIndex advance, so a bare
// Stat right after waitTurnIndex is timing-dependent).
func waitLockReleased(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("lock %q not released within timeout", path)
}

// findTurnFailed returns the first TurnFailed event recorded, or fails the test.
func findTurnFailed(t *testing.T, pub *fakePublisher) event.TurnFailed {
	t.Helper()
	for _, ev := range pub.snapshot() {
		if tf, ok := ev.(event.TurnFailed); ok {
			return tf
		}
	}
	t.Fatalf("no TurnFailed event published; got %v", eventKinds(pub.snapshot()))
	return event.TurnFailed{}
}

// TestForeignLockBusyTurnFailed drives the lock through the actor: a LIVE holder on the
// loop's own (sid,cwd) must fail the turn with a *ForeignSessionBusyError terminal,
// never spawn the agent, and commit nothing.
func TestForeignLockBusyTurnFailed(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	agent := &fakeAgent{
		transcript: writeTranscript(t, "should not run"),
		events: []ForeignEvent{
			{Kind: ForeignTerminalOK, Message: aiMessage("done")},
		},
	}
	pub := &fakePublisher{}
	l, sid := newTestLoop(t, Spec{Agent: agent, Cwd: cwd}, pub)

	// A live process (us) already holds the (sid,cwd) lock before the turn spawns.
	preWriteLock(t, sid, cwd, strconv.Itoa(os.Getpid()))

	submitUserInput(t, l, "go")
	waitForKind(t, pub, "TurnFailed")

	want := []string{"TurnStarted", "TurnFailed"}
	if got := eventKinds(pub.snapshot()); !eqStrs(got, want) {
		t.Fatalf("published sequence = %v, want %v", got, want)
	}
	tf := findTurnFailed(t, pub)
	var busy *ForeignSessionBusyError
	if !errors.As(tf.Err, &busy) {
		t.Fatalf("TurnFailed.Err = %T %v, want *ForeignSessionBusyError", tf.Err, tf.Err)
	}
	if busy.PID != os.Getpid() {
		t.Fatalf("busy PID = %d, want %d", busy.PID, os.Getpid())
	}
	if agent.calls() != 0 {
		t.Fatalf("agent spawned %d times under a busy lock, want 0", agent.calls())
	}
	// A busy turn commits nothing and does not advance the turn count or hasSpawned.
	msgs, ti, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(msgs) != 0 || ti != 0 {
		t.Fatalf("after busy lock: msgs=%d turnIndex=%d, want 0/0", len(msgs), ti)
	}
	shutdown(t, l)
}

// TestForeignLockStaleProceeds proves a stale (dead-pid) pre-existing lock is reclaimed
// so the turn runs normally to TurnDone.
func TestForeignLockStaleProceeds(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	agent := &fakeAgent{
		transcript: writeTranscript(t, "committed reply"),
		events: []ForeignEvent{
			{Kind: ForeignTerminalOK, Message: aiMessage("done")},
		},
	}
	pub := &fakePublisher{}
	l, sid := newTestLoop(t, Spec{Agent: agent, Cwd: cwd}, pub)

	// A dead process holds the lock: the next acquire must reclaim it.
	preWriteLock(t, sid, cwd, strconv.Itoa(deadPID(t)))

	submitUserInput(t, l, "go")
	waitTurnIndex(t, l, 1)

	if agent.calls() != 1 {
		t.Fatalf("agent spawned %d times after stale reclaim, want 1", agent.calls())
	}
	// The lock is released when the turn goroutine exits (no leak). That deferred
	// release runs concurrently with the actor advancing turnIndex, so poll for it.
	waitLockReleased(t, foreignLockPath(sid, cwd))
	shutdown(t, l)
}

func TestForeignLockReleaseIdempotent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		acquire bool // acquire a real lock vs. release a never-created path
	}{
		{name: "double release of held lock", acquire: true},
		{name: "release of non-existent lock", acquire: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sid := "00000000-0000-0000-0000-000000000001"
			cwd := t.TempDir()
			var lk *foreignLock
			if tt.acquire {
				got, err := acquireForeignLock(sid, cwd)
				if err != nil {
					t.Fatalf("acquire: %v", err)
				}
				lk = got
			} else {
				lk = &foreignLock{path: foreignLockPath(sid, cwd)}
			}
			// Must never panic or error, however many times it runs.
			lk.release()
			lk.release()
			if _, err := os.Stat(lk.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lock file still present after release: %v", err)
			}
		})
	}
}
