// Package session exposes the live session data-plane and control-plane contracts.
// Session construction and restoration are owned exclusively by package rig.
package session

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/workspacestore"
)

// Session is the ordinary data-plane view of one live rig execution.
type Session interface {
	SessionID() uuid.UUID
	ActiveLoop() loop.Handle
	Loop(uuid.UUID) (loop.Handle, bool)
	Submit(context.Context, []content.Block) (uuid.UUID, error)
	SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error)
	Compact(context.Context) (uuid.UUID, error)
	CompactToLoop(context.Context, uuid.UUID) (uuid.UUID, error)
	SubscribeEvents(event.EventFilter) (event.Subscription, error)
	RespondGate(context.Context, gate.GateResponse) error
	Interrupt(context.Context) (bool, error)
}

// GateHost is the capability to raise a HOST-OWNED gate: to put a structured
// question or an out-of-band action to a human and receive the answer directly.
//
// It is a SEPARATE contract rather than three more methods on SessionController,
// for two independent reasons.
//
// The first is segregation. Opening a gate is not part of running a session:
// almost every consumer of a SessionController — the TUI, the CLI, a test —
// submits work, watches events, and answers gates, and none of them raise one.
// Widening SessionController would force every implementation to grow three
// methods that only an integration host calls, which is the exact coupling the
// interface rules forbid.
//
// The second is that the two contracts have different holders. A SessionController
// is the session's operator. A GateHost is whatever opened a particular gate and
// is blocked on its answer — an MCP binding servicing an elicitation, say. Those
// are the two ends of the same gate, and RespondGate (on Session) is the other
// end: a client answers, the host receives. Keeping them separate keeps that
// asymmetry visible instead of collapsing both roles into one god-interface.
//
// A live session implements it, so a host obtains one by asserting on the
// controller rig returns:
//
//	host, ok := controller.(session.GateHost)
//
// The contract is host-owned gates ONLY (gate.KindForm and gate.KindOpenURL with
// gate.ResolverSession). There is deliberately no way to open a permission or
// ask-user gate through it: those are answered by resuming a parked loop, and a
// host that could mint one could park — or forge an approval against — a loop
// that is not its own. An implementation MUST refuse anything else at open time
// rather than at answer time, so a caller learns its request was invalid before a
// human is shown a prompt that can never be delivered.
type GateHost interface {
	// OpenHostGate opens g and returns its id. The gate is public and answerable
	// when it returns. The caller MUST then either AwaitGateAnswer or CloseGate;
	// abandoning it without either leaks the answer slot for the session's life.
	OpenHostGate(context.Context, uuid.UUID, gate.Gate, gate.Payload) (gate.ID, error)
	// AwaitGateAnswer blocks until the gate is answered and returns the validated
	// answer, including the form values that are absent from every durable record.
	// An answer is delivered exactly once. Cancelling the context abandons the
	// wait and frees the slot but does NOT close the gate — the gate is durable
	// state and the context is the caller's, so an opener that gives up must
	// CloseGate.
	AwaitGateAnswer(context.Context, gate.ID) (gate.Answer, error)
	// CloseGate withdraws a gate without answering it, waking any awaiter with a
	// *GateError{GateNotFound}. It is how an opener cleans up after a cancelled or
	// timed-out request.
	CloseGate(context.Context, gate.ID, gate.CloseReason) error
}

// SessionController is the trusted policy and lifecycle view of a Session.
type SessionController interface {
	Session
	SetActiveLoop(context.Context, uuid.UUID) error
	LoopController(uuid.UUID) (loop.Controller, bool)
	CheckpointWorkspace(context.Context) (workspacestore.Ref, error)
	RestoreWorkspace(context.Context, workspacestore.Ref) error
	Shutdown(context.Context) error
}

// CommittedPublicEventSource is the segregated committed-public-event capability: a
// live event stream on which EVERY delivery carries the exact canonical public body
// the durable append stored, the public EventID it committed under, and a
// CoveredThrough watermark equal to that append's own sequence.
//
// It is a SEPARATE contract from Session.SubscribeEvents, and the difference is not
// cosmetic. SubscribeEvents is the compatibility stream: it serves a TUI/CLI on a
// headless session with no persistence at all, it carries ephemeral events, and it
// promises nothing about bytes. This one promises committed bytes on every delivery,
// which a session whose persistence cannot report the stored bytes is unable to keep.
// Folding it into Session would force every implementation to advertise a guarantee
// only some of them can honor — and a consumer that joins a durable tail to a live
// stream would have no way to learn, before it starts persisting cursors, that this
// session was not one of them.
//
// A consumer obtains one through CommittedPublicEventProvider rather than a bare type
// assertion, because the capability is a property of the session's persistence, not
// of its Go type.
// Two notes for a consumer building tail-join logic on this stream.
//
// First, the live delivery is the more available of the two sources. It always
// carries the committed bytes; the durable public read does not, for a body large
// enough to be offloaded above the released reader's inline ceiling (see the caveat
// on event.Delivery.PublicBody, which states the exact condition). Join by taking the
// live bytes as authoritative for any sequence you already hold, and never discard a
// held body because a read of the same sequence failed.
//
// That covers a consumer that was connected. It does NOT cover a cold start or a
// reconnect: such a consumer never held those bytes, and the reader fails the whole
// PAGE rather than the one record, so its durable tail is unreadable at that page.
// Surface it as a bounded gap rather than retrying the same page forever.
//
// Second, a subscription on this stream can terminate with *hub.SubscriptionLossError
// for DIFFERENT reasons that call for opposite responses. Egress overflow (a nil cause)
// is congestion: resubscribe and resync. A loss wrapping hub.ErrCommittedBodyMissing or
// hub.ErrCommitEventMismatch is a broken invariant — an enduring public event delivered
// without its committed body, or one whose committed append belongs to a different
// event — and resubscribing loops forever against a hub that cannot satisfy the
// contract. Check errors.Is before retrying.
type CommittedPublicEventSource interface {
	// SubscribeCommittedPublicEvents attaches a consumer to the committed public
	// stream with the given filter. The caller must Close the returned subscription.
	SubscribeCommittedPublicEvents(event.EventFilter) (event.Subscription, error)
}

// CommittedPublicEventProvider is implemented by a session that MAY be able to serve
// committed public events. CommittedPublicEvents reports the capability: ok is false
// — with a nil source — when this session's persistence cannot report the exact
// canonical bytes it stored, which includes a headless/no-persistence session and one
// over a journal that predates the committed-bytes seam.
//
// The two-result form is the point. A single-result form would hand back a source
// that fails only once the consumer is already subscribed and already advancing a
// cursor, which is exactly the shape of failure this capability exists to prevent.
type CommittedPublicEventProvider interface {
	CommittedPublicEvents() (CommittedPublicEventSource, bool)
}

// IdleWaiter is the segregated whole-session quiescence capability: WaitIdle
// blocks until the session has no work in flight, the context is done, or the
// session has failed or stopped, in which case it returns that terminal reason
// rather than reporting idleness.
//
// It is a SEPARATE contract rather than another method on SessionController for
// the reason segregation always applies here: almost every controller consumer —
// the TUI, the CLI, a test harness — submits work and watches events without ever
// waiting on whole-session quiescence, and a supervisor that waits usually does
// not submit. It is discovered by assertion, the same way runtimecommand.Provider
// and GateHost are:
//
//	waiter, ok := controller.(session.IdleWaiter)
//
// That discovery asserts on the DYNAMIC type. A wrapper around a live session
// MUST forward WaitIdle, or it silently opts its wrapped session out and the
// caller sees ok == false with no error anywhere — the same hazard pkg/serve
// states for SessionDone, and it applies to all three capabilities here. Nothing
// pins that rig keeps returning the runtime type unwrapped.
//
// The contract here is the SHAPE and the fact that a live session satisfies it.
// The idleness semantics are the runtime's existing ones, unchanged by this
// declaration; in particular a foreign primary loop is a known gap that does not
// reach whole-session idle, and nothing in this interface repairs that.
type IdleWaiter interface {
	WaitIdle(context.Context) error
}

// Liveness is the segregated teardown-broadcast capability. The channel returned
// by Done is closed when the session begins tearing down, so an out-of-process
// supervisor can select on it alongside its own cancellation instead of polling.
//
// A broadcast, not a poll, is the deliberate shape. A drain supervisor's whole job
// is to block on whichever of several things happens first; an Alive(context.Context) error
// poll cannot be composed into that select and would have to be wrapped in a
// goroutine by every caller. The two are different in kind, not in style.
//
// A receive MUST NOT be read as "teardown finished". The channel closes at the
// START of teardown, deliberately, so a watcher learns immediately that the
// session is going away rather than after the last lease is released.
//
// DUPLICATE, KNOWINGLY: pkg/serve.SessionDone is this interface — same method,
// same semantics, same segregation argument — and neither type references the
// other in code, because serve does not import pkg/session at all. The
// duplication is the price of that independence, not an oversight. Change one and
// change the other.
type Liveness interface {
	Done() <-chan struct{}
}

// Releaser is the segregated NONTERMINAL residency-release capability: it gives up
// this process's resident runtime for the session — subscriptions, actors, leases,
// local contexts — while leaving the logical session restorable elsewhere.
//
// The name is ReleaseResidency and not Release because the distinction from
// Shutdown is the entire content of the contract. Shutdown durably appends
// SessionStopped and makes the logical session terminal. This does not: after it
// returns, the session is cold and restorable, and a registry loser that released
// its runtime has not ended anyone's session. The bare name loses exactly that at
// the boundary where a host is choosing between the two.
//
// It is declared here as a capability discovered by assertion:
//
//	releaser, ok := controller.(session.Releaser)
//
// and a caller MUST treat a false ok as "this session cannot be released
// nonterminally", not as an error. The live runtime satisfies it; a wrapper that does
// not forward the method silently opts its wrapped session out, which is why the
// discovery result is a capability answer rather than an error.
type Releaser interface {
	ReleaseResidency(context.Context) error
}

// PersistenceFaultReporter is the segregated durable-health capability: it lets an
// out-of-process supervisor notice that the session has latched a TERMINAL
// persistence fault — a required journal append failed, so the live runtime's
// in-memory state and its durable log may disagree and the session refuses every
// new Submit for the rest of its life.
//
// THE LATCH IS DELIBERATE AND IS NOT CLEARED IN PLACE. A failed append may or may not
// have landed, and the loop may already have acted on state the journal does not
// hold; nothing the live process can read back proves which. Recovery is therefore a
// RESTORE from the durable journal by a successor, and this capability exists so the
// supervisor can start one instead of keeping a runtime resident that can never
// persist again. A storage outage of any length — one failed append — reaches it.
//
// PersistenceFaulted returns a channel closed once, when the fault latches; every
// call returns the same channel. PersistenceFault returns the latched fault (which
// chains the storage failure) or nil before it latches. A recoverable required-
// checkpoint latch (cleared by a manual checkpoint) is NOT reported here.
//
// It is a capability discovered by assertion, like Releaser; a false ok means "this
// session does not report its durable health", not "this session is healthy".
type PersistenceFaultReporter interface {
	PersistenceFaulted() <-chan struct{}
	PersistenceFault() error
}

// ResidencyAbandoner is the segregated CRASH-EQUIVALENT residency-release
// capability: it gives up this process's resident runtime exactly as a process
// crash would, and writes nothing durable while doing so.
//
// It differs from Releaser in the one respect that makes it usable on a faulted
// session. ReleaseResidency anchors the release to a fresh checkpoint and appends
// SessionResidencyReleased, so it refuses a faulted session whose log it cannot
// trust — and Shutdown would append SessionStopped, making a session that is merely
// unhealthy terminal for good. AbandonResidency first SEALS every durable write the
// session makes: hub publication, the audit intent log, and the runtime-command log
// (application prefix, disposition, recovery closure), waiting for a runtime-command
// append already in flight to finish. From then on no loop, process, checkpoint or
// ApplyRuntimeCommand/CloseAttempt call can append — the latter two are refused with
// a SessionClosing error and write nothing. It then stops the runtime and releases
// its leases. (A hub append already blocked in the provider when the seal lands is
// bounded by the teardown's drain deadline, exactly as for ReleaseResidency; the
// journal's sequence CAS keeps it from interleaving with a successor's appends.) The journal ends where the live process
// last managed to write, and a successor's restore treats whatever was in flight as
// crash debt, exactly as after a crash.
//
// It admits any session, faulted or not, and joins a teardown already underway. A
// lease release that fails (storage still down) is reported, and the lease then
// lapses on its own expiry.
type ResidencyAbandoner interface {
	AbandonResidency(context.Context) error
}

// WorkspaceStatus is what a session reports about the managed workspace it came up
// on: where the workspace is, and which durable checkpoint the live tree was
// materialized from.
//
// It is a REPORT, never a repair. Nothing on it recovers a mutation, bounds how much
// any record lost, or prevents anything. A session with no managed workspace reports
// the zero value — there is no boundary to name and no tree to have lost anything
// from.
//
// The two roots answer different questions and must not be collapsed. Root is THIS
// process's physical location and varies with the Host's runtime root. LogicalRoot is
// the session-derived path the same tree is exposed at inside the agent/tool
// namespace; it is derived from session identity alone, so a session released by one
// Host and restored by another keeps it, which is what makes a previously journalled
// "read this file" still resolve.
//
// FRESHNESS IS NOT UNIFORM ACROSS THESE FIELDS. Root and LogicalRoot are live. The
// boundary fields — CheckpointSeq, HasCheckpoint, PostCheckpointEvents — are AS OF
// RESTORE and never refresh: a session that checkpoints again after coming up still
// reports the boundary it came up on. A caller polling this for a live checkpoint
// position is reading the wrong thing.
type WorkspaceStatus struct {
	// LogicalRoot is the session-derived, model-visible workspace path. Empty when the
	// session has no managed workspace or no session identity.
	LogicalRoot string
	// Root is this process's physical workspace root.
	Root string
	// CheckpointSeq is the JOURNAL sequence of the workspace transition the live tree
	// was materialized from — the last checkpoint or rewind in the replayed stream. It
	// is read from the journal's own sequence, so it is available after a crash and not
	// only after a clean release. Meaningless when HasCheckpoint is false.
	CheckpointSeq uint64
	// HasCheckpoint distinguishes "anchored at sequence 0" from "never checkpointed".
	HasCheckpoint bool
	// PostCheckpointEvents counts the loop-scoped durable records that follow that
	// transition: journalled loop work that MAY have mutated the workspace after the
	// tree the restore materialized. Nothing inspects a record for whether it actually
	// touched the workspace, so this is a deliberate OVER-APPROXIMATION — it is a count
	// of records, not of losses, and it is the input to PostCheckpointLoss rather than a
	// measure of how much was lost.
	PostCheckpointEvents int
}

// PostCheckpointLoss reports whether the journal records loop work after the
// transition the live tree was materialized from — the divergence a restore must not
// present silently.
//
// It requires HasCheckpoint. With no checkpoint in the stream there is no such
// transition: the restore materializes nothing and leaves the live tree exactly as it
// found it, so a never-checkpointed warm restart has lost nothing and must not raise
// an alarm, however much loop work the journal holds. Reading PostCheckpointEvents
// without this gate is the false positive that would fire on every such restart.
//
// A true result is a MAY, not a DID: see PostCheckpointEvents.
func (s WorkspaceStatus) PostCheckpointLoss() bool {
	return s.HasCheckpoint && s.PostCheckpointEvents > 0
}

// WorkspaceReporter is the segregated workspace-boundary reporting capability: it
// answers where the session's managed workspace is and which checkpoint it came up
// on.
//
// It exists because the two facts have no other route out of the runtime. A Host in
// another module holds a SessionController; the boundary lives on the concrete
// runtime type, so without a contract here a caller could not even NAME the return
// type to declare a local interface for it.
//
// Like Releaser it is discovered by assertion:
//
//	reporter, ok := controller.(session.WorkspaceReporter)
//
// and a caller MUST treat a false ok as "this session does not report a workspace
// boundary", not as an error — a session composed without a managed workspace is a
// legitimate configuration, not a failure. It is segregated rather than folded onto
// SessionController for the same reason Releaser is: a wrapper that does not forward
// the method opts its wrapped session out, and that is a capability answer.
//
// It is deliberately READ-ONLY and deliberately separate from CheckpointWorkspace /
// RestoreWorkspace, which are workspace CONTROL and already live on SessionController.
// Reporting a boundary and moving one are different authorities.
type WorkspaceReporter interface {
	WorkspaceStatus() WorkspaceStatus
}

// LeaseEpochReporter is the segregated single-writer-lease-epoch reporting capability:
// it answers which journal lease epoch THIS resident process currently holds for the
// session.
//
// It exists because that number has no other route out of the runtime while the session
// is alive. It is already published on the way OUT — event.SessionResidencyReleased
// carries the epoch the releasing process held — but a live consumer that must STAMP it
// onto something had no way to read it, and the fact is not otherwise derivable: the
// epoch is minted by the storage lease the composition root acquired, and nothing on
// Session or SessionController returns it.
//
// WHICH EPOCH. A deployment typically has two monotonic per-session counters, and they
// are not the same number and must never be substituted for one another. This one is the
// journal's single-writer lease epoch — the fence the runtime stamps into its opening
// LeaseFence and the value runtimecommand.Admitted.LeaseEpoch is checked against. An
// orchestrator's own residency/ownership epoch is a different grant with a different
// issuer; that the two often coincide early in a session's life (each counter starting
// at 1 under a fresh in-memory backend) is an accident of initial conditions, not a
// relationship. Source this from here, never from the caller's own grant.
//
// THE TWO RESULTS ARE THE POINT. ok reports whether this process holds a lease that
// reports an epoch AT ALL; the epoch is meaningful only when ok is true, and is zero
// otherwise. A single-result form would collapse "this session has no single-writer
// lease" — a headless or no-persistence session, or one simply not wired for durable
// commands, all legitimate configurations — into "epoch 0", which is exactly the
// ambiguity a consumer must resolve BEFORE it stamps a value that a fencing check will
// later compare for equality.
//
// ok IS ALSO FALSE ONCE THE LEASE IS GONE. Reporting is gated on the lease still being
// held, so a session whose lease has been released or lost answers (0, false) rather
// than the stale number it used to hold. That is deliberate and fail-secure: the only
// use for this value is to stamp work that a live fencing check will reject anyway, so
// handing back a dead epoch would only move the failure later. It is NOT in tension with
// event.SessionResidencyReleased.LeaseEpoch, whose "Zero when the session was not wired
// to a lease that reports an epoch" describes a snapshot taken while the lease was still
// held; that record is history, this is a live read.
//
// IT IS A REPORT, NOT AN AUTHORIZATION AND NOT A RESERVATION. A true ok is a statement
// about the instant of the call. Nothing here holds the lease, and nothing prevents the
// epoch from being superseded between this read and whatever the caller does with it —
// so a stamped command may still be refused with a stale-epoch error, and a caller must
// handle that rather than treating a true ok as a promise.
//
// Like Releaser and WorkspaceReporter it is discovered by assertion:
//
//	reporter, ok := controller.(session.LeaseEpochReporter)
//
// and a caller MUST treat a false ok as "this session does not report a lease epoch",
// not as an error. A wrapper around a live session that does not forward the method
// silently opts its wrapped session out, which is why the discovery result is a
// capability answer rather than an error.
type LeaseEpochReporter interface {
	// LeaseEpoch reports the epoch of the single-writer lease this process holds, and
	// whether it holds one. It does no I/O and does not block.
	LeaseEpoch() (epoch uint64, held bool)
}
