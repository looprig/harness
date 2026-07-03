package session

import (
	"context"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/workspacestore"
)

// CheckpointWorkspace durably snapshots the session's configured workspace root and
// records a WorkspaceCheckpointed enduring event pointing at the snapshot — the resume
// token the restore path materializes from. It returns the snapshot Ref (also carried on
// the event) so the caller can correlate the checkpoint.
//
// Snapshot-before-append: the archive bytes are durable (Snapshot's Blobs.Put has
// returned) BEFORE the event is appended, so a crash between the two leaks an
// unreferenced blob (GC's job) — never a dangling ref. That ordering is guaranteed by
// call order here (Snapshot returns before PublishEvent runs), not by any stronger
// signal.
//
// The WorkspaceCheckpointed is Enduring, so it flows through the hub's REQUIRED durable
// tap: the event is appended synchronously BEFORE fan-out, and a durable-append failure
// surfaces as a SESSION FAULT (the hub latches it via ReportFault) exactly like every
// other enduring event — PublishEvent itself returns nil on that path. This method adds
// no stronger durability signal; on the happy path the event IS durably appended by the
// time it returns.
//
// WHEN to call this — a quiescence point (a turn done, a user question, about to suspend)
// — is the composition root's decision, consistent with looprig-as-SDK and the
// foreign-loop quiescence model; the session only exposes the capability. Without
// WithWorkspaceStore the capability is unconfigured and this fails closed with a typed
// *WorkspaceNotConfiguredError, having touched nothing. ctx bounds the snapshot I/O and
// the durable append.
func (s *Session) CheckpointWorkspace(ctx context.Context) (workspacestore.Ref, error) {
	if s.ws == nil {
		return "", &WorkspaceNotConfiguredError{}
	}
	// Snapshot first: on success the archive bytes are durable in Blobs before we append
	// the event that points at them. Any failure is already a typed *workspacestore
	// error (e.g. *SnapshotError), returned unwrapped so the caller can errors.As it.
	ref, err := s.ws.Snapshot(ctx, s.wsRoot)
	if err != nil {
		return "", err
	}
	// Stamp the event's EventID + CreatedAt through the session Factory (same seam as
	// SessionStarted/LoopStarted). A crypto/rand failure fails the checkpoint cleanly
	// before publishing a zero-EventID event — mirrors newLoop's stamp-failure mapping.
	stamped, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID}})
	if err != nil {
		return "", &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	// Publish on the passed ctx (this method has a real caller ctx, unlike newLoop which
	// publishes on the session lifetime). The Enduring event takes the hub's durable-tap
	// branch: appended before fan-out, faulting the session on append failure.
	if err := s.PublishEvent(ctx, event.WorkspaceCheckpointed{Header: stamped, Ref: string(ref)}); err != nil {
		return "", &SessionError{Kind: SessionContextDone, Cause: err}
	}
	return ref, nil
}

// WorkspaceNotConfiguredError reports that CheckpointWorkspace was called on a session
// that was not wired with a workspace store (no WithWorkspaceStore). It carries no
// fields: the failure mode is fully described by its type, which callers match with
// errors.As. The capability fails closed rather than silently no-op'ing, so a caller
// never believes a checkpoint was taken when none was.
type WorkspaceNotConfiguredError struct{}

func (e *WorkspaceNotConfiguredError) Error() string {
	return "session: workspace store not configured (WithWorkspaceStore); cannot checkpoint"
}
