package tool

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
)

func TestProcessContractFakeImplementsInterfaces(t *testing.T) {
	t.Parallel()

	var _ AsyncProcessRunner = (*fakeAsyncProcessRunner)(nil)
	var _ PreparedProcess = (*fakePreparedProcess)(nil)
	var _ Process = (*fakeProcess)(nil)
	var _ ProcessActivitySource = (*fakeProcess)(nil)

	originID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174001")
	deadline := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	request := ProcessRequest{
		Command:           "go test ./...",
		Directory:         "/workspace",
		Grants:            []string{"grant-a", "grant-b"},
		OriginExecutionID: originID,
		Deadline:          deadline,
		PTY:               true,
	}

	runner := &fakeAsyncProcessRunner{}
	prepared, err := runner.PrepareProcess(context.Background(), request)
	if err != nil {
		t.Fatalf("PrepareProcess() error = %v", err)
	}
	if got := prepared.EffectiveWorkspaceAccess(); got.Kind != WorkspaceAccessScopedWrite {
		t.Fatalf("EffectiveWorkspaceAccess().Kind = %v, want %v", got.Kind, WorkspaceAccessScopedWrite)
	}
	process, err := prepared.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if process.Stdout() == nil || process.Stderr() == nil || process.Stdin() == nil {
		t.Fatal("process streams must be available")
	}
	if got := process.StreamMode(); got != ProcessStreamModePTY {
		t.Fatalf("StreamMode() = %v, want %v", got, ProcessStreamModePTY)
	}
	if err := process.Resize(context.Background(), 24, 80); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if err := process.Signal(context.Background(), ProcessSignalInterrupt); err != nil {
		t.Fatalf("Signal() error = %v", err)
	}
	result, err := process.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.ExitCode != 0 || result.Reason != ProcessTerminalExited ||
		!result.StartedAt.Equal(deadline.Add(-time.Second)) || !result.FinishedAt.Equal(deadline) {
		t.Fatalf("Wait() = %#v", result)
	}
	if err := process.Close(context.Background()); err != nil {
		t.Fatalf("Process.Close() error = %v", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("PreparedProcess.Close() error = %v", err)
	}

	source, ok := process.(ProcessActivitySource)
	if !ok {
		t.Fatal("fake process does not expose optional activity capability")
	}
	activity, ok := <-source.Activities()
	if !ok || activity.Kind != WorkspaceActivityBroadWrite {
		t.Fatalf("Activities() = %#v, %v", activity, ok)
	}
	if _, ok := <-source.Activities(); ok {
		t.Fatal("Activities() channel must close")
	}

	gotRequest := runner.request
	if gotRequest.Command != request.Command || gotRequest.Directory != request.Directory ||
		gotRequest.OriginExecutionID != originID || !gotRequest.Deadline.Equal(deadline) || !gotRequest.PTY {
		t.Fatalf("PrepareProcess() request = %#v, want %#v", gotRequest, request)
	}
}

func TestProcessContractEnumsValidateExplicitly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		valid bool
	}{
		{name: "interrupt signal", valid: ProcessSignalInterrupt.Valid()},
		{name: "terminate signal", valid: ProcessSignalTerminate.Valid()},
		{name: "kill signal", valid: ProcessSignalKill.Valid()},
		{name: "invalid signal", valid: ProcessSignal(0).Valid()},
		{name: "pipe stream mode", valid: ProcessStreamModePipes.Valid()},
		{name: "PTY stream mode", valid: ProcessStreamModePTY.Valid()},
		{name: "invalid stream mode", valid: ProcessStreamMode(0).Valid()},
		{name: "exited terminal reason", valid: ProcessTerminalExited.Valid()},
		{name: "timed out terminal reason", valid: ProcessTerminalTimedOut.Valid()},
		{name: "interrupted terminal reason", valid: ProcessTerminalInterrupted.Valid()},
		{name: "terminated terminal reason", valid: ProcessTerminalTerminated.Valid()},
		{name: "killed terminal reason", valid: ProcessTerminalKilled.Valid()},
		{name: "runner shutdown terminal reason", valid: ProcessTerminalRunnerShutdown.Valid()},
		{name: "invalid terminal reason", valid: ProcessTerminalReason(0).Valid()},
		{name: "filesystem activity", valid: WorkspaceActivityWrite.Valid()},
		{name: "broad filesystem activity", valid: WorkspaceActivityBroadWrite.Valid()},
		{name: "invalid filesystem activity", valid: WorkspaceActivityKind(0).Valid()},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			want := !strings.HasPrefix(test.name, "invalid")
			if test.valid != want {
				t.Fatalf("Valid() = %v, want %v", test.valid, want)
			}
		})
	}
}

func TestProcessContractZeroDeadlineMeansNoProcessDeadline(t *testing.T) {
	t.Parallel()

	if (ProcessRequest{}).HasDeadline() {
		t.Fatal("zero ProcessRequest deadline must mean no process deadline")
	}
	request := ProcessRequest{
		Deadline: time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC),
	}
	if !request.HasDeadline() {
		t.Fatal("non-zero ProcessRequest deadline must be reported as present")
	}
}

func TestProcessContractRequestCloneDefendsGrants(t *testing.T) {
	t.Parallel()

	original := ProcessRequest{Grants: []string{"grant-a"}}
	clone := original.Clone()
	clone.Grants[0] = "changed"

	if original.Grants[0] != "grant-a" {
		t.Fatalf("original grants mutated through clone: %v", original.Grants)
	}
}

func TestProcessContractInvalidActivityMapsToBroadInvalidation(t *testing.T) {
	t.Parallel()

	activity := ProcessActivity{Kind: WorkspaceActivityKind(255)}
	if got := activity.EffectiveKind(); got != WorkspaceActivityBroadWrite {
		t.Fatalf("EffectiveKind() = %v, want %v", got, WorkspaceActivityBroadWrite)
	}
}

func TestProcessErrorSupportsClassificationAndWrapping(t *testing.T) {
	t.Parallel()

	cause := errors.New("host detail")
	err := &ProcessError{
		Code:  ProcessErrorSpawnFailed,
		Cause: cause,
	}

	if !errors.Is(err, &ProcessError{Code: ProcessErrorSpawnFailed}) {
		t.Fatalf("errors.Is(%v, spawn failed) = false", err)
	}
	if errors.Is(err, &ProcessError{Code: ProcessErrorWaitFailed}) {
		t.Fatalf("errors.Is(%v, wait failed) = true", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause) = false", err)
	}
	if got, want := err.Error(), "tool: process spawn_failed: host detail"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got := (&ProcessError{Code: ProcessErrorPTYUnavailable}).Error(); got != "tool: process pty_unavailable" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestProcessErrorCodesValidateExplicitly(t *testing.T) {
	t.Parallel()

	valid := []ProcessErrorCode{
		ProcessErrorLifetimeEnforcementUnavailable,
		ProcessErrorSpawnFailed,
		ProcessErrorSetupFailed,
		ProcessErrorPTYUnavailable,
		ProcessErrorSignalFailed,
		ProcessErrorWaitFailed,
		ProcessErrorTeardownFailed,
	}
	for _, code := range valid {
		if !code.Valid() {
			t.Errorf("%q.Valid() = false", code)
		}
	}
	if ProcessErrorCode("").Valid() || ProcessErrorCode("other").Valid() {
		t.Fatal("unknown process error code reported valid")
	}
}

func TestWorkspaceAccessClassifiesAndDefendsScopes(t *testing.T) {
	t.Parallel()

	kinds := []WorkspaceAccessKind{
		WorkspaceAccessReadOnly,
		WorkspaceAccessScopedWrite,
		WorkspaceAccessBroadWrite,
	}
	for _, kind := range kinds {
		if !kind.Valid() {
			t.Errorf("%v.Valid() = false", kind)
		}
	}
	if WorkspaceAccessKind(0).Valid() {
		t.Fatal("zero workspace access kind reported valid")
	}

	writePaths := []string{"/workspace/file.txt"}
	writeTrees := []string{"/workspace/generated"}
	access := NewWorkspaceAccess(WorkspaceAccessScopedWrite, writePaths, writeTrees)
	writePaths[0] = "/workspace/constructor-mutated.txt"
	writeTrees[0] = "/workspace/constructor-mutated"

	gotPaths := access.WritePaths()
	gotTrees := access.WriteTrees()
	if gotPaths[0] != "/workspace/file.txt" || gotTrees[0] != "/workspace/generated" {
		t.Fatalf("constructor inputs mutated access: paths=%v trees=%v", gotPaths, gotTrees)
	}
	gotPaths[0] = "/workspace/accessor-mutated.txt"
	gotTrees[0] = "/workspace/accessor-mutated"
	if got := access.WritePaths()[0]; got != "/workspace/file.txt" {
		t.Fatalf("WritePaths() output mutated access: %q", got)
	}
	if got := access.WriteTrees()[0]; got != "/workspace/generated" {
		t.Fatalf("WriteTrees() output mutated access: %q", got)
	}

	prepared := &fakePreparedProcess{access: access}
	first := prepared.EffectiveWorkspaceAccess()
	first.writePaths[0] = "/workspace/effective-mutated.txt"
	first.writeTrees[0] = "/workspace/effective-mutated"
	second := prepared.EffectiveWorkspaceAccess()
	if got := second.WritePaths()[0]; got != "/workspace/file.txt" {
		t.Fatalf("first EffectiveWorkspaceAccess() mutated repeated result: %q", got)
	}
	if got := second.WriteTrees()[0]; got != "/workspace/generated" {
		t.Fatalf("first EffectiveWorkspaceAccess() mutated repeated result: %q", got)
	}
}

type fakeAsyncProcessRunner struct {
	request ProcessRequest
}

func (r *fakeAsyncProcessRunner) PrepareProcess(_ context.Context, request ProcessRequest) (PreparedProcess, error) {
	r.request = request.Clone()
	return &fakePreparedProcess{
		access: NewWorkspaceAccess(
			WorkspaceAccessScopedWrite,
			[]string{"/workspace/file.txt"},
			nil,
		),
	}, nil
}

type fakePreparedProcess struct {
	access WorkspaceAccess
}

func (p *fakePreparedProcess) EffectiveWorkspaceAccess() WorkspaceAccess {
	return p.access.Clone()
}

func (*fakePreparedProcess) Start(context.Context) (Process, error) {
	activities := make(chan ProcessActivity, 1)
	activities <- ProcessActivity{Kind: WorkspaceActivityBroadWrite}
	close(activities)
	return &fakeProcess{activities: activities}, nil
}

func (*fakePreparedProcess) Close() error { return nil }

type fakeProcess struct {
	activities <-chan ProcessActivity
}

func (*fakeProcess) Stdout() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (*fakeProcess) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (*fakeProcess) Stdin() io.WriteCloser { return nopWriteCloser{Writer: io.Discard} }
func (*fakeProcess) StreamMode() ProcessStreamMode {
	return ProcessStreamModePTY
}
func (*fakeProcess) Wait(context.Context) (ProcessResult, error) {
	finished := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	return ProcessResult{
		ExitCode:   0,
		Reason:     ProcessTerminalExited,
		StartedAt:  finished.Add(-time.Second),
		FinishedAt: finished,
	}, nil
}
func (*fakeProcess) Resize(context.Context, uint16, uint16) error { return nil }
func (*fakeProcess) Signal(context.Context, ProcessSignal) error  { return nil }
func (*fakeProcess) Close(context.Context) error                  { return nil }
func (p *fakeProcess) Activities() <-chan ProcessActivity         { return p.activities }

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }
