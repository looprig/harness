package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

func resultText(res *tool.ToolResult) string {
	var sb strings.Builder
	for _, b := range res.Content {
		if tb, ok := b.(*content.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

func resultHasText(res *tool.ToolResult, sub string) bool {
	return strings.Contains(resultText(res), sub)
}

// recordingCoordinator is a test tool.WorkspaceCoordinator that records every Acquire
// (operation + canonical path) and permit Release, and can be configured to fail the
// health check or the acquire. It honors ctx cancellation (a done ctx yields the ctx
// error without recording), so the cancellation paths of the tools are exercised.
type recordingCoordinator struct {
	mu         sync.Mutex
	acquires   []acquireCall
	released   int
	healthErr  error
	acquireErr error
}

type acquireCall struct {
	op   tool.WorkspaceOperation
	path string
}

func (c *recordingCoordinator) Acquire(ctx context.Context, op tool.WorkspaceOperation, path string) (tool.WorkspacePermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.acquireErr != nil {
		return nil, c.acquireErr
	}
	c.acquires = append(c.acquires, acquireCall{op: op, path: path})
	return &recordingPermit{c: c}, nil
}

func (c *recordingCoordinator) Healthy() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.healthErr
}

func (c *recordingCoordinator) snapshot() ([]acquireCall, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]acquireCall, len(c.acquires))
	copy(out, c.acquires)
	return out, c.released
}

type recordingPermit struct {
	c    *recordingCoordinator
	once sync.Once
}

func (p *recordingPermit) Release() {
	p.once.Do(func() {
		p.c.mu.Lock()
		defer p.c.mu.Unlock()
		p.c.released++
	})
}

func observed(obs *fileObservations, key canonicalObservationKey) bool {
	st := obs.state(key)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.observed
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(b)
}

// WriteFile creating a new (absent) file takes exactly one PathMutation permit keyed
// on the canonical path, then releases it.
func TestWriteFileAcquiresPathPermit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	obs := newFileObservations()
	coord := &recordingCoordinator{}
	w := NewWriteFile(root, obs, WithMutationCoordinator(coord))

	if _, err := w.InvokableRun(context.Background(), mustJSON(t, writeFileArgs{Path: "f.txt", Content: "hello"})); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if b, rerr := os.ReadFile(filepath.Join(root, "f.txt")); rerr != nil || string(b) != "hello" {
		t.Fatalf("file = %q, err %v; want %q", string(b), rerr, "hello")
	}

	want, err := containedPath(root, "f.txt")
	if err != nil {
		t.Fatalf("containedPath() error = %v", err)
	}
	acqs, released := coord.snapshot()
	if len(acqs) != 1 || acqs[0].op != tool.WorkspaceOperationPathMutation || acqs[0].path != want {
		t.Fatalf("acquires = %#v, want one PathMutation on %q", acqs, want)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
}

// An unhealthy lease blocks the commit: the permit is acquired then released, no bytes
// are written, and the model sees a lease-unhealthy error.
func TestWriteFileUnhealthyLeaseBlocksCommit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed write error = %v", err)
	}
	obs := newFileObservations()
	coord := &recordingCoordinator{healthErr: errors.New("lease expired")}
	w := NewWriteFile(root, obs, WithMutationCoordinator(coord))

	res, err := w.InvokableRun(context.Background(), mustJSON(t, writeFileArgs{Path: "f.txt", Content: "replacement"}))
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	// The result must be an error mentioning lease health, and the file untouched.
	if b, _ := os.ReadFile(target); string(b) != "original" {
		t.Fatalf("file = %q, want unchanged %q (unhealthy lease must not write)", string(b), "original")
	}
	acqs, released := coord.snapshot()
	if len(acqs) != 1 || released != 1 {
		t.Fatalf("acquires = %d, released = %d; want 1 and 1 (permit taken then released)", len(acqs), released)
	}
	if !resultHasText(res, "not healthy") {
		t.Fatalf("result = %q, want lease-unhealthy error", resultText(res))
	}
}

// A ctx already canceled when Acquire is called blocks the write with the ctx error.
func TestWriteFileCanceledCtxBlocksCommit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed write error = %v", err)
	}
	obs := newFileObservations()
	coord := &recordingCoordinator{}
	w := NewWriteFile(root, obs, WithMutationCoordinator(coord))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := w.InvokableRun(ctx, mustJSON(t, writeFileArgs{Path: "f.txt", Content: "replacement"}))
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "original" {
		t.Fatalf("file = %q, want unchanged (canceled ctx must not write)", string(b))
	}
	acqs, released := coord.snapshot()
	if len(acqs) != 0 || released != 0 {
		t.Fatalf("acquires = %d, released = %d; want 0 and 0 (canceled acquire)", len(acqs), released)
	}
	if !resultHasText(res, "canceled") && !resultHasText(res, "context canceled") {
		t.Fatalf("result = %q, want ctx-canceled error", resultText(res))
	}
}

// EditFile takes a PathMutation permit keyed on the canonical path around its commit.
func TestEditFileAcquiresPathPermit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("alpha"), 0o600); err != nil {
		t.Fatalf("seed write error = %v", err)
	}
	obs := newFileObservations()
	coord := &recordingCoordinator{}
	// Record a complete observation via a ReadFile so the edit is authorized.
	rf := NewReadFile(root, newFakeReadGuard(1<<20), obs)
	if _, err := rf.InvokableRun(context.Background(), mustJSON(t, map[string]string{"path": "f.txt"})); err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	ef := NewEditFile(root, obs, WithMutationCoordinator(coord))
	if _, err := ef.InvokableRun(context.Background(), mustJSON(t, editFileArgs{Path: "f.txt", Old: "alpha", New: "beta"})); err != nil {
		t.Fatalf("EditFile error = %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "beta" {
		t.Fatalf("file = %q, want %q", string(b), "beta")
	}
	want, _ := containedPath(root, "f.txt")
	acqs, released := coord.snapshot()
	if len(acqs) != 1 || acqs[0].op != tool.WorkspaceOperationPathMutation || acqs[0].path != want || released != 1 {
		t.Fatalf("acquires = %#v released = %d; want one PathMutation on %q released once", acqs, released, want)
	}
}

// Bash takes the exclusive whole-workspace permit and invalidates the loop's entire
// observation set after the run — for BOTH a success and a non-zero exit.
func TestBashWholePermitAndInvalidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{name: "success", command: "true"},
		{name: "non-zero exit", command: "exit 3"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			obs := newFileObservations()
			key := canonicalObservationKey(filepath.Join(root, "seen.txt"))
			obs.recordPresent(key, [32]byte{1})
			if !observed(obs, key) {
				t.Fatal("precondition: observation not recorded")
			}
			coord := &recordingCoordinator{}
			b := NewBash(root, WithWorkspaceCoordinator(coord), WithObservations(obs))
			if _, err := b.InvokableRun(context.Background(), mustJSON(t, bashArgs{Command: tt.command})); err != nil {
				t.Fatalf("InvokableRun() error = %v", err)
			}
			if observed(obs, key) {
				t.Fatal("Bash did not invalidate the loop observation set after the run")
			}
			acqs, released := coord.snapshot()
			if len(acqs) != 1 || acqs[0].op != tool.WorkspaceOperationWholeMutation || acqs[0].path != "" || released != 1 {
				t.Fatalf("acquires = %#v released = %d; want one WholeMutation (empty path) released once", acqs, released)
			}
		})
	}
}

// A canceled ctx blocks the Bash run entirely: no whole permit is held, the command
// never runs, and the observation set is NOT invalidated.
func TestBashCanceledCtxDoesNotRunOrInvalidate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	obs := newFileObservations()
	key := canonicalObservationKey(filepath.Join(root, "seen.txt"))
	obs.recordPresent(key, [32]byte{1})
	coord := &recordingCoordinator{}
	b := NewBash(root, WithWorkspaceCoordinator(coord), WithObservations(obs))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := b.InvokableRun(ctx, mustJSON(t, bashArgs{Command: "true"}))
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !observed(obs, key) {
		t.Fatal("canceled Bash run invalidated observations (command never ran)")
	}
	acqs, released := coord.snapshot()
	if len(acqs) != 0 || released != 0 {
		t.Fatalf("acquires = %d released = %d; want 0/0 for a canceled acquire", len(acqs), released)
	}
	if !resultHasText(res, "canceled") {
		t.Fatalf("result = %q, want ctx-canceled error", resultText(res))
	}
}

// The Files definition and the Bash definition, built from the SAME binding, share one
// observation set: a Bash run invalidates the observation a prior ReadFile recorded, so
// a subsequent overwrite without re-reading is refused as stale.
func TestFilesAndBashShareObservations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed error = %v", err)
	}
	shared := NewObservations()
	coord := &recordingCoordinator{}
	bindings := tool.Bindings{
		SessionID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		LoopID:    uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		Workspace: &tool.WorkspaceBinding{Root: root, Coordinator: coord, Observations: shared},
	}
	fileTools, err := Files(newFakeReadGuard(1<<20)).Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("Files.Build() error = %v", err)
	}
	bashTools, err := Bash().Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("Bash.Build() error = %v", err)
	}
	read, write := byName(t, fileTools, "ReadFile"), byName(t, fileTools, "WriteFile")
	bash := byName(t, bashTools, "Bash")

	// Read records a complete observation, so an immediate overwrite is authorized.
	if _, err := read.InvokableRun(context.Background(), mustJSON(t, map[string]string{"path": "f.txt"})); err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	// Bash runs and (sharing the same observation set) invalidates that observation.
	if _, err := bash.InvokableRun(context.Background(), mustJSON(t, bashArgs{Command: "true"})); err != nil {
		t.Fatalf("Bash error = %v", err)
	}
	// The overwrite is now refused as stale (the shared observation was cleared by Bash).
	res, err := write.InvokableRun(context.Background(), mustJSON(t, writeFileArgs{Path: "f.txt", Content: "v2"}))
	if err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if !resultHasText(res, "must be read before writing") {
		t.Fatalf("result = %q, want stale-file refusal proving shared invalidation", resultText(res))
	}
	if b, _ := os.ReadFile(target); string(b) != "v1" {
		t.Fatalf("file = %q, want unchanged v1 (stale write refused)", string(b))
	}
}

// foreignObservations is a tool.WorkspaceObservations implementation that is NOT the
// package's own concrete map, used to prove the fail-loud rejection.
type foreignObservations struct{}

func (foreignObservations) InvalidateAll() {}

// A binding carrying a non-nil Observations of a foreign concrete type fails the Files
// build loudly (fail-secure) rather than silently building a divergent private map that
// would leave Bash invalidating an object the file tools never record into.
func TestFilesRejectsForeignObservations(t *testing.T) {
	t.Parallel()
	bindings := tool.Bindings{
		SessionID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		LoopID:    uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		Workspace: &tool.WorkspaceBinding{Root: t.TempDir(), Coordinator: &recordingCoordinator{}, Observations: foreignObservations{}},
	}
	_, err := Files(newFakeReadGuard(1<<20)).Build(context.Background(), bindings)
	var buildErr *DefinitionBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build() error = %T, want *DefinitionBuildError", err)
	}
	if buildErr.Dependency != "workspace.observations" {
		t.Fatalf("DefinitionBuildError.Dependency = %q, want %q", buildErr.Dependency, "workspace.observations")
	}
}

func byName(t *testing.T, tools []tool.InvokableTool, name string) tool.InvokableTool {
	t.Helper()
	for _, it := range tools {
		info, err := it.Info(context.Background())
		if err == nil && info != nil && info.Name == name {
			return it
		}
	}
	t.Fatalf("tool %q not built", name)
	return nil
}
