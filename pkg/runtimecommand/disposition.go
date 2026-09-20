package runtimecommand

import (
	"context"
	"strconv"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
)

// This file is the DISPOSITION half of the seam: the durable statement a runtime
// writes about ONE authorized dispatch attempt, and the segregated capability a
// successor uses to close an attempt its predecessor never finished.
//
// WHAT "applied" MEANS HERE, in full and no wider: under the attempt's journal
// grant, the runtime durably recorded that it accepted this command into its
// execution path. It does NOT mean a turn started or folded, does NOT mean a later
// TurnRejected cannot follow, and does NOT mean a queued input survives a crash —
// the loop mailbox is in memory, so a crash during a running turn loses a queued
// input while the command stands settled applied. Do not widen this sentence.
//
// WHY THE DISPOSITION IS A SEPARATE RECORD from the effect, rather than atomic with
// it: SessionJournal.Append takes exactly ONE record, encodes one body, frames one
// envelope and does one AppendDefinite. There is no batch. So no effect a runtime
// performs can share a frame with its disposition, and the disposition is written
// AFTER the effect the runtime can observe SYNCHRONOUSLY.

// MaxAttemptIDBytes bounds a dispatch-attempt identity. It is Core's MaxIDBytes,
// which is what the durable envelope's attempt_id field is bounded by; the two must
// not drift, for exactly the reason CommandID's bound must not — an attempt Host has
// already durably recorded must not become unwritable here.
const MaxAttemptIDBytes = 256

// AttemptID is Host's immutable identity for ONE authorized dispatch attempt. Like
// CommandID it is an OPAQUE bounded UTF-8 string: Harness never parses it, never
// derives anything from its shape, and validates exactly what the durable boundary
// validates.
//
// It is the correlation key for settlement. A disposition names the attempt rather
// than the command because a command may be attempted more than once — a successor
// closing a predecessor's attempt writes about THAT attempt — and evidence about one
// attempt is not evidence about another.
type AttemptID string

// Validate reports whether id is a well-formed attempt identity: non-empty, at most
// MaxAttemptIDBytes bytes, valid UTF-8. That is the whole rule and it is the durable
// boundary's rule; see the type doc.
func (id AttemptID) Validate() error {
	switch {
	case id == "":
		return &ValidationError{Field: "AttemptID", Reason: "empty"}
	case len(id) > MaxAttemptIDBytes:
		return &ValidationError{
			Field:  "AttemptID",
			Reason: "longer than " + strconv.Itoa(MaxAttemptIDBytes) + " bytes",
		}
	case !utf8.ValidString(string(id)):
		return &ValidationError{Field: "AttemptID", Reason: "not valid UTF-8"}
	default:
		return nil
	}
}

// DispositionKind is the closed vocabulary of durable runtime dispositions. The four
// spellings are DURABLE BYTES, not labels: they are written verbatim into the
// journal frame and compared verbatim by the settlement verifier, so renaming one is
// a wire change and not a refactor.
type DispositionKind string

const (
	// DispositionApplied is the narrow statement in this file's header doc: under
	// the attempt's grant, the runtime durably recorded that it accepted the command
	// into its execution path.
	DispositionApplied DispositionKind = "applied"
	// DispositionNoOp is an explicit SUCCESSFUL application with no effect — an
	// interrupt of an idle session. It settles applied. Reject-before-dispatch and
	// an applied no-op are different outcomes and must never be merged.
	DispositionNoOp DispositionKind = "no_op"
	// DispositionRefused is this runtime's own statement, under the attempt's OWN
	// grant, that it did not accept the command. It exists for LIVENESS: a command
	// whose effect failed after its prefix under a still-live lease has no other
	// terminal arm, because not_applied requires a strictly later grant and a
	// healthy Host never turns its lease over. Without it such a command sits
	// applying forever.
	DispositionRefused DispositionKind = "refused"
	// DispositionNotApplied is a SUCCESSOR's recovery closure, authored by a
	// strictly later grant. It is the tombstone late dispatch must consult. It is
	// never a substitute for a refusal and nothing may derive one from the other: a
	// refusal is a live runtime's own answer, a closure is a successor's conclusion
	// about a runtime that is gone.
	DispositionNotApplied DispositionKind = "not_applied"
)

// Valid reports whether k is one of the four known kinds.
func (k DispositionKind) Valid() bool {
	return k == DispositionApplied || k == DispositionNoOp ||
		k == DispositionRefused || k == DispositionNotApplied
}

// authoredByAttemptGrant reports whether a disposition of this kind is authored by
// the ATTEMPT's own grant. Three of the four are; only a recovery closure is not.
func (k DispositionKind) authoredByAttemptGrant() bool { return k != DispositionNotApplied }

// CommandDisposition is the runtime's durable, bodiless statement about ONE attempt.
//
// LeaseEpoch is the grant the AUTHOR actually held when it wrote the record, and
// getting it right matters more than it looks. Harness bypasses the durable store's
// own JournalWriter — it encodes the envelope and appends the bytes itself — so no
// field here is store-stamped and the reader treats this value as a CLAIM. It
// cross-checks it against the nearest preceding opening fence and fails closed when
// the two disagree, which reads exactly like a forged epoch. A wrong LeaseEpoch does
// not degrade settlement; it stops it.
//
// AttemptJournalEpoch is the grant the ATTEMPT was authorized under, which the
// settlement verifier compares against its own immutable attempt record. For the
// three kinds authored by the attempt's grant the two epochs are EQUAL; for a
// recovery closure LeaseEpoch is strictly greater.
//
// The struct tags are the JSON body this package's codec produces for fingerprinting
// and replay. They are NOT the durable frame: the frame is the store's binary
// envelope, whose field tags are its own.
type CommandDisposition struct {
	CommandID           CommandID       `json:"command_id"`
	RuntimeCommandID    uuid.UUID       `json:"runtime_command_id"`
	Kind                Kind            `json:"command_kind"`
	LeaseEpoch          uint64          `json:"lease_epoch"`
	AttemptID           AttemptID       `json:"attempt_id"`
	AttemptJournalEpoch uint64          `json:"attempt_journal_epoch"`
	Disposition         DispositionKind `json:"disposition_kind"`
}

// Validate fails closed on a disposition the durable boundary or the settlement
// verifier would refuse. It deliberately enforces the AUTHOR-GRANT rule as well as
// the per-field shape: the envelope codec cannot check it (it never sees the
// attempt record), so a record with the wrong grant relationship would encode and
// append cleanly and then be refused at settlement, where the refusal reads as an
// attack rather than as a writer bug.
func (d CommandDisposition) Validate() error {
	if err := d.CommandID.Validate(); err != nil {
		return err
	}
	if d.RuntimeCommandID.IsZero() {
		return &ValidationError{Field: "RuntimeCommandID", Reason: "zero"}
	}
	if !d.Kind.Valid() {
		return &ValidationError{Field: "Kind", Reason: "unknown kind " + strconv.Quote(string(d.Kind))}
	}
	if d.LeaseEpoch == 0 {
		return &ValidationError{Field: "LeaseEpoch", Reason: "zero"}
	}
	if err := d.AttemptID.Validate(); err != nil {
		return err
	}
	if d.AttemptJournalEpoch == 0 {
		return &ValidationError{Field: "AttemptJournalEpoch", Reason: "zero"}
	}
	if !d.Disposition.Valid() {
		return &ValidationError{
			Field:  "Disposition",
			Reason: "unknown disposition kind " + strconv.Quote(string(d.Disposition)),
		}
	}
	if d.Disposition.authoredByAttemptGrant() {
		if d.LeaseEpoch != d.AttemptJournalEpoch {
			return &ValidationError{
				Field: "LeaseEpoch",
				Reason: "an " + string(d.Disposition) + " disposition is authored by the attempt's own grant " +
					strconv.FormatUint(d.AttemptJournalEpoch, 10) + ", not " + strconv.FormatUint(d.LeaseEpoch, 10),
			}
		}
		return nil
	}
	if d.LeaseEpoch <= d.AttemptJournalEpoch {
		return &ValidationError{
			Field: "LeaseEpoch",
			Reason: "a recovery closure is authored by a grant strictly later than the attempt's " +
				strconv.FormatUint(d.AttemptJournalEpoch, 10) + ", not " + strconv.FormatUint(d.LeaseEpoch, 10),
		}
	}
	return nil
}

// DispositionFor builds the durable disposition this admitted record's attempt
// resolved to, under grantEpoch — the journal grant the applier ACTUALLY held.
//
// Both epochs come from that grant and neither is copied from Admitted.LeaseEpoch.
// The applier refuses an admitted record whose epoch is not the one it holds, so the
// two agree on every reachable path; taking the held grant rather than the record's
// claim is what keeps that true if the refusal is ever relaxed.
func (a Admitted) DispositionFor(kind DispositionKind, grantEpoch uint64) CommandDisposition {
	return CommandDisposition{
		CommandID:           a.CommandID,
		RuntimeCommandID:    a.RuntimeCommandID,
		Kind:                a.Kind,
		LeaseEpoch:          grantEpoch,
		AttemptID:           a.AttemptID,
		AttemptJournalEpoch: grantEpoch,
		Disposition:         kind,
	}
}

// Closure is a successor runtime's request to close an unfinished attempt.
//
// It carries NO author grant. The closer stamps that from the live lease it holds,
// which is the whole point of the capability: a caller-supplied author epoch would
// be a caller-authored proof, and the one thing a tombstone must not be is something
// a caller can assert.
//
// Kind is the ADMITTED command's kind and Validate accepts every one of the five —
// input, interrupt, gate_response, create and restore. It tracks Kind.Valid rather
// than holding its own narrower set, and that is load-bearing: a kind a predecessor
// can apply but a closure refuses is a command no successor can ever close, so the
// record sits applying for good. That is precisely what happened to create and
// restore before v0.36.0, and it is the second half of the same defect — the first
// being that the applier refused them outright.
type Closure struct {
	CommandID           CommandID
	RuntimeCommandID    uuid.UUID
	Kind                Kind
	AttemptID           AttemptID
	AttemptJournalEpoch uint64
}

// Validate fails closed on a closure request that cannot name an attempt.
func (c Closure) Validate() error {
	if err := c.CommandID.Validate(); err != nil {
		return err
	}
	if c.RuntimeCommandID.IsZero() {
		return &ValidationError{Field: "RuntimeCommandID", Reason: "zero"}
	}
	if !c.Kind.Valid() {
		return &ValidationError{Field: "Kind", Reason: "unknown kind " + strconv.Quote(string(c.Kind))}
	}
	if err := c.AttemptID.Validate(); err != nil {
		return err
	}
	if c.AttemptJournalEpoch == 0 {
		return &ValidationError{Field: "AttemptJournalEpoch", Reason: "zero"}
	}
	return nil
}

// ClosureResult reports where the recovery closure landed. Appended=false means an
// identical closure was already durable — a redelivered recovery, not a second
// tombstone — and Sequence is the ORIGINAL append's.
type ClosureResult struct {
	Sequence uint64
	Appended bool
}

// EffectScan is what a privileged journal scan found about one command identity. It
// is the evidence the closer's second guard rests on, so every member is a fact
// about durable records and none is a conclusion.
//
// PrefixSeq is zero when the journal holds NO application prefix for the command,
// which is the ordinary shape of an attempt that never reached its effect.
// DurableRuntimeID and DurableKind are the mapping that prefix holds, so a closure
// offered against a different mapping can be refused rather than tombstoning
// somebody else's command.
type EffectScan struct {
	PrefixSeq        uint64
	DurableRuntimeID uuid.UUID
	DurableKind      Kind
	EffectFound      bool
	EffectSeq        uint64
}

// AttemptCloser is the SEGREGATED recovery-closure capability: it writes the
// not_applied tombstone for an attempt a previous runtime never finished.
//
// It is a separate interface from Applier, discovered by assertion, exactly as
// session.LeaseEpochReporter is. Folding CloseAttempt into Applier would break every
// existing implementer of that interface — the composed Host adapter, the test
// doubles — for a capability only a recovery path uses. A caller obtains one with
//
//	closer, ok := applier.(runtimecommand.AttemptCloser)
//
// and an implementation that cannot honor it refuses at the call with a
// *CapabilityUnavailableError rather than advertising a closure it cannot write.
type AttemptCloser interface {
	// CloseAttempt durably records not_applied for the named attempt, under THIS
	// runtime's own grant, which must be strictly later than the attempt's.
	//
	// It refuses rather than closing when the runtime holds no live grant, when its
	// grant is not strictly later, when the journal's own prefix binds the command
	// to another mapping, and — the guard that matters most — when the journal holds
	// an enduring event caused by that runtime command ANYWHERE IN THE JOURNAL,
	// because a tombstone over a committed effect is the one error this protocol
	// cannot recover from. It also refuses a journal it cannot fully read.
	//
	// "Anywhere" is deliberate and is not a looser restatement of "after the
	// prefix". An event caused by that runtime id cannot exist unless the command
	// was dispatched, so its POSITION proves nothing extra; and a guard that ignored
	// an event before the prefix would be choosing, in the one journal shape nobody
	// can explain, to tombstone rather than to refuse. Refusing an odd journal costs
	// liveness; tombstoning a committed effect is unrecoverable.
	CloseAttempt(context.Context, Closure) (ClosureResult, error)
}

// DispositionUnsupportedError reports that an attempt-bearing command reached a
// session whose durable log cannot record a disposition. It is raised BEFORE any
// durable write: a command applied with no evidence sits applying forever,
// settleable by nobody, which is strictly worse than a refusal Host can retry
// elsewhere.
//
// IT LIVES HERE, BESIDE CapabilityUnavailableError, BECAUSE HOST IS TOLD TO ACT ON
// IT. It was originally declared in the unexported runtime package, which made that
// instruction unfollowable: a consumer outside this module could only recognise the
// refusal by matching its message text, and a message is not an API. A typed refusal
// a caller cannot name is a refusal a caller cannot distinguish from a transport
// failure, and the difference matters — nothing durable was written, so the command
// may be re-offered elsewhere.
type DispositionUnsupportedError struct {
	CommandID CommandID
	AttemptID AttemptID
}

func (e *DispositionUnsupportedError) Error() string {
	return "runtimecommand: this session's durable log cannot record a command disposition; refusing attempt " +
		strconv.Quote(string(e.AttemptID)) + " of command " + strconv.Quote(string(e.CommandID))
}

// ClosureNotAuthorizedError reports a closure offered without a strictly later
// grant. Held distinguishes "this runtime holds no live grant at all" from "its
// grant is not later than the attempt's": the first is a lost or released lease, the
// second is a runtime trying to close its OWN attempt, and conflating them would
// send an operator looking for the wrong failure.
type ClosureNotAuthorizedError struct {
	AttemptID           AttemptID
	AttemptJournalEpoch uint64
	Current             uint64
	Held                bool
}

func (e *ClosureNotAuthorizedError) Error() string {
	if !e.Held {
		return "runtimecommand: no live journal grant; refusing to close attempt " + strconv.Quote(string(e.AttemptID))
	}
	return "runtimecommand: closing attempt " + strconv.Quote(string(e.AttemptID)) +
		" needs a grant strictly later than " + strconv.FormatUint(e.AttemptJournalEpoch, 10) +
		", but this runtime holds " + strconv.FormatUint(e.Current, 10)
}

// EnduringEffectError reports that the journal holds an enduring event caused by the
// attempt's runtime command, so the predecessor's EFFECT committed even though its
// disposition did not.
//
// PrefixSeq and EffectSeq are WHERE THE SCAN FOUND THINGS, not an ordering claim.
// PrefixSeq is zero when the journal holds no application prefix for the command,
// and EffectSeq is NOT guaranteed to be greater than it — the scan refuses on an
// event caused by that runtime id at any sequence, deliberately. Read each as a
// locator; do not read a relationship between the two.
//
// This is the case the idempotency guard cannot catch. A successor's not_applied
// collides with a predecessor's durable APPLIED disposition and fails closed for
// free, because both key on the attempt id — but if the predecessor's effect
// committed and its disposition append then failed, there is no colliding record and
// nothing but this scan stands between a successor and a tombstone over a real
// effect.
type EnduringEffectError struct {
	AttemptID        AttemptID
	CommandID        CommandID
	RuntimeCommandID uuid.UUID
	PrefixSeq        uint64
	EffectSeq        uint64
}

func (e *EnduringEffectError) Error() string {
	return "runtimecommand: attempt " + strconv.Quote(string(e.AttemptID)) +
		" has a durable enduring effect at seq " + strconv.FormatUint(e.EffectSeq, 10) +
		" caused by runtime command " + e.RuntimeCommandID.String() +
		"; refusing to close it as not_applied"
}
