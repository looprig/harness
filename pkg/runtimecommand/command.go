// Package runtimecommand is the seam between a Host-admitted public command and
// the UUID-keyed runtime command Harness has always dispatched.
//
// Two identities meet here and must never be confused for one another.
//
// The public CommandID is Host's retry-stable identity. It is an OPAQUE bounded
// UTF-8 string: Harness validates exactly what Core's canonical sessionwire/v1
// CommandID validates — non-empty, at most MaxCommandIDBytes bytes, valid UTF-8 —
// and does nothing else with it. It is never parsed as a UUID, never truncated, and
// never substituted for the runtime id; a public id that happens to render a valid
// UUID is still just a string here.
//
// The parity with Core is a CORRECTNESS requirement, not tidiness. The admission
// authority upstream accepts an id under those three rules; an applier that
// additionally rejected, say, a leading space or an embedded tab would refuse a
// command that was already durably admitted, forever. It could only sit pending to
// its apply deadline and become rejected, with the refusal invisible on the
// admission side. Do not add a rule here that Core does not have.
//
// The RuntimeCommandID is the core/uuid.UUID Harness stamps on command headers and
// on the events those commands cause. Host allocates it ONCE, when it admits the
// command, and hands it back on every delivery. Harness does not allocate a
// replacement: a second UUID for the same admitted command would silently split
// one command's correlation across two identities, and the split would be
// invisible — the events would look perfectly well formed under either id.
//
// Application is the durable bridge between them. It is written to the session's
// PRIVATE journal, under the lease epoch that authorizes the effect, BEFORE any
// runtime-visible effect begins. That ordering is the crash-safety property: a
// delivery that finds an existing prefix knows the command was already applied and
// replays the original disposition instead of applying it a second time.
package runtimecommand

import (
	"context"
	"strconv"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"

	"github.com/looprig/harness/pkg/gate"
)

// MaxCommandIDBytes bounds a public CommandID. The id is opaque, so the only thing
// Harness can assert about it is that it is bounded: an unbounded id would enter a
// durable idempotency key and a journal record body. It is Core's MaxIDBytes; the
// two must not drift.
const MaxCommandIDBytes = 256

// CommandID is the public, retry-stable command identity Host owns. It is opaque
// to Harness; see the package doc for why it is never parsed as a UUID.
type CommandID string

// Validate reports whether id is a well-formed opaque public identity: non-empty,
// at most MaxCommandIDBytes bytes, and valid UTF-8. That is the WHOLE rule, and it
// is deliberately Core's rule byte for byte — see the package doc for why an extra
// rule here strands an already-admitted command. Everything else about the id —
// format, meaning, structure, whitespace, control characters — belongs to Host.
//
// Nothing downstream needs a stricter id in the CHARACTER dimension: the durable
// idempotency key namespaces the public id rather than filtering it
// (journal.CommandApplicationRecord), and the record body is JSON-encoded, so no
// byte value is unsafe anywhere on the path.
//
// The LENGTH dimension is enforced by the SAME authority, not by a second one, and
// that is structural rather than test-enforced. The application prefix is persisted
// as the released SessionStore's EnvelopeKindApplicationPrefix, whose identity field
// carries the RAW public id and is validated by Core's own
// sessionwire.CommandID.Validate. No prefix, namespace, or derived record id
// consumes any of the 256-byte budget, so Harness's acceptance and the durable
// boundary's acceptance cannot drift: they are one rule applied twice.
func (id CommandID) Validate() error {
	switch {
	case id == "":
		return &ValidationError{Field: "CommandID", Reason: "empty"}
	case len(id) > MaxCommandIDBytes:
		return &ValidationError{
			Field:  "CommandID",
			Reason: "longer than " + strconv.Itoa(MaxCommandIDBytes) + " bytes",
		}
	case !utf8.ValidString(string(id)):
		return &ValidationError{Field: "CommandID", Reason: "not valid UTF-8"}
	default:
		return nil
	}
}

// Kind is the bounded set of admitted commands Harness can apply through this
// seam. It is deliberately small: every kind here must have an existing runtime
// dispatch path, so a new kind is a new implementation, never a new default.
type Kind string

const (
	// KindInput applies admitted user input to the session's active loop.
	KindInput Kind = "input"
	// KindInterrupt applies a session-wide interrupt.
	KindInterrupt Kind = "interrupt"
	// KindGateResponse answers one open gate through the session's own gate-response
	// path (the one Session.RespondGate uses), so every refusal RespondGate makes —
	// including its refusal of a classifier-sourced response — holds here as well.
	// The spelling is the admitted record's: Factory admits "gate_response", and the
	// settlement reader compares the kind as a string.
	//
	// It REQUIRES an AttemptID (see Admitted.Validate): an answer applied with no
	// attempt writes no disposition, which is the defect this kind exists to fix.
	//
	// ONE-WAY UPGRADE. Once a session journal holds ANY gate_response application —
	// whatever it resolved to, applied, no_op or refused; a no_op against a gate that
	// never existed is enough — harness v0.34.0 and older can neither replay nor
	// reopen that journal: OpenJournal and replay both fail closed with
	// `journal: encode command application: runtimecommand: invalid Kind: unknown kind
	// "gate_response"`. The failing check is the Marshal-side validation
	// (v0.34.0 pkg/journal/record_json.go:191, MarshalCommandApplicationRecord),
	// reached from the replay hydration of the prefix (v0.34.0
	// pkg/sessionstore/replay.go:422). Nothing is lost — it fails closed — but the
	// session is stranded until a runtime at v0.35.0 or later opens it. Do not roll a
	// runtime back below v0.35.0 once it has applied a gate_response.
	KindGateResponse Kind = "gate_response"
	// KindCreate is the command that brings a session into existence on this
	// runtime. By the time it reaches this seam the session IS resident — Host makes
	// it resident before it dispatches — so what remains to apply is the first
	// message the create carries, if it carries one.
	//
	// It MAY carry Blocks and it is the only kind besides KindInput that may: a
	// create with a first message applies that message exactly as an input does,
	// through the same audit-intent-first ordering, and a create with none applies
	// nothing and still settles applied.
	//
	// The spelling is the admitted record's, byte for byte
	// (factory/internal/command/kind.go: "create").
	//
	// WHY IT EXISTS. Factory admits EVERY session's first command as a create.
	// Before v0.36.0 this seam named three kinds, so Host's adapter refused the
	// create after Host had already durably begun its attempt: no disposition frame
	// was ever written, the store could never settle the record, a consumer blocked
	// at that command and never advanced its cursor, and no successor could close it
	// either because Closure.Validate refused the same kind. The session existed and
	// the agent was resident, and the user could never talk to it.
	KindCreate Kind = "create"
	// KindRestore is the command that resumes a session that is not resident. Like
	// a create, residency is Host's work and is already done here, and unlike a
	// create there is no first message: a restore carries NO Blocks, because a
	// restored session already holds its conversation and a payload riding one would
	// be silently dropped.
	//
	// The spelling is the admitted record's, byte for byte
	// (factory/internal/command/kind.go: "restore").
	KindRestore Kind = "restore"
)

// Valid reports whether k is one of the five admitted kinds.
//
// The set is Factory's durable vocabulary (factory/internal/command/kind.go) and
// must stay equal to it. A kind Factory admits and this set omits is a command that
// can never settle — see KindCreate for what that cost.
//
// ONE-WAY UPGRADE, exactly as gate_response was. Once a session journal holds any
// create or restore application prefix or disposition frame, harness v0.35.0 and
// older can neither replay nor reopen that journal: the Marshal-side validation
// fails closed with `journal: encode command application: runtimecommand: invalid
// Kind: unknown kind "create"` (or "restore"). Nothing is lost — it fails closed —
// but the session is stranded until a runtime at v0.36.0 or later opens it. Do not
// roll a runtime back below v0.36.0 once it has applied a create or a restore.
func (k Kind) Valid() bool {
	return k == KindInput || k == KindInterrupt || k == KindGateResponse ||
		k == KindCreate || k == KindRestore
}

// Admitted is one command Host has ALREADY admitted, handed to Harness for
// application. Harness re-validates it — an admitted record it cannot durably
// correlate is refused rather than applied on trust — but it never re-derives an
// identity from it.
type Admitted struct {
	// CommandID is Host's opaque public identity for this command.
	CommandID CommandID
	// RuntimeCommandID is the once-allocated UUID this command's runtime headers
	// and events carry. Harness uses it verbatim.
	RuntimeCommandID uuid.UUID
	// Kind selects the runtime dispatch path.
	Kind Kind
	// LeaseEpoch is the session-lease epoch this command was admitted against. A
	// record admitted under a superseded epoch is refused.
	LeaseEpoch uint64
	// Blocks is the input payload: REQUIRED for KindInput, OPTIONAL for KindCreate
	// (whose first message it carries, if there is one), and forbidden for every
	// other kind — a payload a kind cannot carry would be silently dropped.
	Blocks []content.Block
	// GateResponse is the answer a KindGateResponse command applies, required for
	// that kind and forbidden for every other, for the same reason as Blocks. It must
	// name a gate. Everything else about it — whether the gate is open, whether the
	// action is one of its controls, whether its values satisfy the gate's schema,
	// and whether its source is one a caller may assert — is decided by the session
	// at application time, because only the session knows the gate.
	GateResponse *gate.GateResponse
	// AttemptID is Host's immutable identity for the ONE authorized dispatch
	// attempt this delivery belongs to. It is OPTIONAL for input and interrupt, and
	// the option is the compatibility contract: a legacy admitted record carries
	// none, and an applier handed one writes NO disposition, so a legacy session's
	// journal is byte-for-byte what it was. It is REQUIRED for KindGateResponse,
	// which has no legacy records: without it the answer would apply and never
	// settle. A non-empty id is validated exactly as the durable boundary
	// validates it.
	//
	// KindCreate and KindRestore are on INPUT AND INTERRUPT's footing, not
	// gate_response's: optional. The requirement gate_response carries is a
	// last-resort guard for a kind whose whole purpose is settlement, and it is not
	// what makes a create settle — Host dispatches these two under an attempt, and
	// the authority that decides whether a dispatch is authorized is Host's
	// admission rather than this validator. A record with no attempt is a shape
	// that writes no disposition, which is a legacy shape and not a malformed one;
	// refusing it here would turn an old Host's create into a hard refusal instead
	// of the unsettled command it already is.
	AttemptID AttemptID
}

// Validate fails closed on any admitted record Harness cannot apply.
func (a Admitted) Validate() error {
	if err := a.CommandID.Validate(); err != nil {
		return err
	}
	if a.RuntimeCommandID.IsZero() {
		return &ValidationError{Field: "RuntimeCommandID", Reason: "zero"}
	}
	if !a.Kind.Valid() {
		return &ValidationError{Field: "Kind", Reason: "unknown kind " + strconv.Quote(string(a.Kind))}
	}
	if a.LeaseEpoch == 0 {
		return &ValidationError{Field: "LeaseEpoch", Reason: "zero"}
	}
	if a.AttemptID != "" {
		if err := a.AttemptID.Validate(); err != nil {
			return err
		}
	}
	if a.Kind == KindGateResponse && a.AttemptID == "" {
		// The empty-attempt arm exists for LEGACY input/interrupt records, and
		// gate_response has none. Accepting one here would apply the answer and
		// write no evidence — a command that sits applying forever.
		return &ValidationError{Field: "AttemptID", Reason: "gate_response requires a dispatch attempt"}
	}
	if a.Kind == KindInput && len(a.Blocks) == 0 {
		return &ValidationError{Field: "Blocks", Reason: "input carries no content"}
	}
	// KindCreate is the one kind whose payload is OPTIONAL: a create may carry the
	// session's first message or nothing at all, and both settle. Every other kind
	// but input forbids one, because blocks a kind cannot apply are dropped in
	// silence.
	if a.Kind != KindInput && a.Kind != KindCreate && len(a.Blocks) > 0 {
		return &ValidationError{Field: "Blocks", Reason: string(a.Kind) + " carries no payload"}
	}
	for i, b := range a.Blocks {
		if b == nil {
			return &ValidationError{Field: "Blocks", Reason: "nil block at index " + strconv.Itoa(i)}
		}
	}
	if a.Kind == KindGateResponse && a.GateResponse == nil {
		return &ValidationError{Field: "GateResponse", Reason: "gate_response carries no response"}
	}
	if a.Kind != KindGateResponse && a.GateResponse != nil {
		return &ValidationError{Field: "GateResponse", Reason: string(a.Kind) + " carries no gate response"}
	}
	if a.GateResponse != nil && a.GateResponse.GateID.IsZero() {
		return &ValidationError{Field: "GateResponse", Reason: "names no gate"}
	}
	return nil
}

// Application is the private durable correlation an applier writes BEFORE the
// effect: the public id, the one runtime id it maps to, and the lease epoch the
// application ran under. It is the whole content of the application prefix — no
// payload, no user content — because its only job is to answer "was this public
// command already applied, and under which runtime identity?".
//
// Known gap, recorded so it is a decision rather than an oversight: it carries no
// loop id. KindInput dispatches to the session's ACTIVE loop, and which loop that
// was is not recoverable from this record — neither recovery nor an audit can say
// where an admitted command landed. Nothing in the current contract needs it (the
// runtime id correlates the events, and the events carry the loop), but a
// per-loop-addressed admitted command would need this field, and adding it later
// changes the persisted body and therefore every existing record's fingerprint.
type Application struct {
	CommandID        CommandID `json:"command_id"`
	RuntimeCommandID uuid.UUID `json:"runtime_command_id"`
	LeaseEpoch       uint64    `json:"lease_epoch"`
	// Kind is the admitted command's kind. It is part of the correlation because
	// the released SessionStore reader correlates on it: a prefix whose kind
	// disagrees with the inbox record's resolves CONFLICTED, not applied. Omitting
	// it would make every application unmatchable by the counterparty.
	Kind Kind `json:"command_kind"`
}

// Application returns the durable correlation for this admitted record. It copies
// the identities rather than deriving new ones.
func (a Admitted) Application() Application {
	return Application{
		CommandID:        a.CommandID,
		RuntimeCommandID: a.RuntimeCommandID,
		LeaseEpoch:       a.LeaseEpoch,
		Kind:             a.Kind,
	}
}

// Validate fails closed on a correlation that cannot have been produced by a valid
// admitted record. It is the decode-side guard for a prefix read back from storage.
func (a Application) Validate() error {
	if err := a.CommandID.Validate(); err != nil {
		return err
	}
	if a.RuntimeCommandID.IsZero() {
		return &ValidationError{Field: "RuntimeCommandID", Reason: "zero"}
	}
	if a.LeaseEpoch == 0 {
		return &ValidationError{Field: "LeaseEpoch", Reason: "zero"}
	}
	if !a.Kind.Valid() {
		return &ValidationError{Field: "Kind", Reason: "unknown kind " + strconv.Quote(string(a.Kind))}
	}
	return nil
}

// Disposition is what an application resolved to. A duplicate delivery reports the
// ORIGINAL disposition — the same runtime id and the same prefix sequence the first
// delivery reported — with Duplicate set, so a caller can distinguish "applied by
// this call" from "already applied" without being able to confuse the two.
type Disposition struct {
	// CommandID echoes the public id this disposition answers for.
	CommandID CommandID
	// RuntimeCommandID is the durable runtime identity of the application.
	RuntimeCommandID uuid.UUID
	// PrefixSequence is the journal sequence of the application prefix. For a
	// duplicate it is the ORIGINAL append's sequence, never a new one.
	PrefixSequence uint64
	// Duplicate reports that this delivery applied nothing because the command was
	// already applied.
	Duplicate bool
	// Interrupted reports, for KindInterrupt, whether a running turn was cancelled.
	// It is the TRANSIENT outcome of an application, not part of the durable prefix,
	// so a duplicate delivery always reports false: the prefix records the
	// correlation, never the outcome. Read it only alongside Duplicate.
	Interrupted bool
}

// Applier is the SEGREGATED runtime-command capability. It is deliberately not a
// method on session.Session: almost every Session implementation — every test
// double, every adapter, the TUI's view — will never apply an admitted command,
// and widening the base contract would force all of them to grow a method they
// cannot honor. A caller obtains one through Provider.
type Applier interface {
	// ApplyRuntimeCommand durably records the application prefix and then applies
	// the command, returning the disposition. A duplicate delivery returns the
	// original disposition and applies nothing.
	//
	// A non-nil error does not license a retry. The prefix is written BEFORE the
	// effect, so an error raised by the effect leaves a durable prefix behind and
	// every later delivery deduplicates against it. Re-deliver and read
	// Disposition.Duplicate to learn what happened; do not assume an error means
	// nothing was recorded.
	//
	// A returned Disposition also does not mean the application will SETTLE. The
	// durable settlement correlation resolves an application by finding the public
	// event adjacent to its prefix, and adjacency is not guaranteed for any command
	// kind: another writer's record in that slot leaves the application unresolved
	// forever. Unresolved never licenses a rejection, so nothing is applied twice and
	// nothing is settled over — but a caller that BLOCKS on an application settling
	// blocks indefinitely. Treat the Disposition as the answer, not as a promise that
	// a later durable query will agree.
	ApplyRuntimeCommand(context.Context, Admitted) (Disposition, error)
}

// Provider is implemented by a session that MAY be able to apply admitted runtime
// commands. RuntimeCommands reports the capability: ok is false — with a nil
// Applier — for a session with no durable, deduplicating application-prefix log,
// which includes a headless/no-persistence session.
//
// The two-result form is the point, exactly as it is for the committed-public-event
// capability: a single-result form would hand back an applier that fails only after
// Host has already acknowledged the command as accepted.
type Provider interface {
	RuntimeCommands() (Applier, bool)
}

// ValidationError reports a malformed admitted record or correlation.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return "runtimecommand: invalid " + e.Field + ": " + e.Reason
}

// MappingConflictError reports that the public CommandID is already durable under a
// mapping this delivery may not be applied against. It covers two situations that
// must both fail closed, and the two are distinguishable — do not collapse them.
//
// A CONFLICT: the durable prefix was read and binds this public id to a DIFFERENT
// RuntimeCommandID. That is never a legitimate retry, because a retry of an admitted
// command carries the mapping Host allocated once. DurableRuntimeID is set.
//
// An UNREADABLE prefix: the durable frame at Sequence could not be read at all, so
// the mapping is UNKNOWN. DurableRuntimeID is then zero and Cause is the read
// failure. Reporting this as a duplicate would tell Host "already applied" about a
// command that may never have been applied.
//
// Error() distinguishes them. The unreadable arm must not name a mapping: printing
// an all-zero DurableRuntimeID as though it had been read is a message that
// contradicts its own Cause, and it sends a reader hunting for a runtime command
// that does not exist.
type MappingConflictError struct {
	CommandID CommandID
	// RuntimeCommandID is the id the offered delivery carried.
	RuntimeCommandID uuid.UUID
	// DurableRuntimeID is the id the durable application prefix holds. It is zero
	// when the prefix could not be read; see Cause.
	DurableRuntimeID uuid.UUID
	// Kind is the kind the offered delivery carried, and DurableKind the kind the
	// durable prefix holds. They are compared because the RELEASED reader compares
	// them: a prefix whose kind disagrees with the inbox record's resolves
	// CONFLICTED. Ignoring the kind here would have Harness report already-applied
	// for a shape the counterparty refuses.
	Kind        Kind
	DurableKind Kind
	// Sequence is the journal sequence of the durable prefix.
	Sequence uint64
	Cause    error
}

func (e *MappingConflictError) Error() string {
	prefix := "runtimecommand: public command " + strconv.Quote(string(e.CommandID))
	switch {
	case e.DurableRuntimeID.IsZero():
		return prefix +
			" collides with a durable application prefix at seq " + strconv.FormatUint(e.Sequence, 10) +
			" that could not be read, so its mapping is unknown; refusing the offered " +
			e.RuntimeCommandID.String()
	case e.DurableRuntimeID != e.RuntimeCommandID:
		return prefix +
			" is durably mapped to runtime command " + e.DurableRuntimeID.String() +
			" at seq " + strconv.FormatUint(e.Sequence, 10) +
			", not the offered " + e.RuntimeCommandID.String()
	default:
		return prefix +
			" is durably applied as kind " + strconv.Quote(string(e.DurableKind)) +
			" at seq " + strconv.FormatUint(e.Sequence, 10) +
			", not the offered " + strconv.Quote(string(e.Kind))
	}
}

func (e *MappingConflictError) Unwrap() error { return e.Cause }

// StaleLeaseEpochError reports an admitted record whose lease epoch is not the
// epoch the applier currently holds. Applying it would let a superseded owner's
// admission take effect under a lease it no longer holds.
type StaleLeaseEpochError struct {
	CommandID CommandID
	Admitted  uint64
	Current   uint64
}

func (e *StaleLeaseEpochError) Error() string {
	return "runtimecommand: command " + strconv.Quote(string(e.CommandID)) +
		" was admitted under lease epoch " + strconv.FormatUint(e.Admitted, 10) +
		", but the applier holds epoch " + strconv.FormatUint(e.Current, 10)
}

// LeaseLostError reports that the applier's session lease is no longer held, so no
// effect may be applied under it.
type LeaseLostError struct {
	CommandID CommandID
	Epoch     uint64
}

func (e *LeaseLostError) Error() string {
	return "runtimecommand: session lease at epoch " + strconv.FormatUint(e.Epoch, 10) +
		" is lost; refusing command " + strconv.Quote(string(e.CommandID))
}

// CapabilityUnavailableError reports that ApplyRuntimeCommand was called on a
// session that does not advertise the capability. A caller that went through
// Provider.RuntimeCommands never sees it; it exists so a caller that reached the
// method by a bare type assertion fails loudly instead of silently applying a
// command with no durable correlation.
type CapabilityUnavailableError struct{ CommandID CommandID }

func (e *CapabilityUnavailableError) Error() string {
	return "runtimecommand: this session has no durable application-prefix log; refusing command " +
		strconv.Quote(string(e.CommandID))
}
