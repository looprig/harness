package sessionstore

import (
	"context"
	"strconv"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	durablestore "github.com/looprig/sessionstore"
)

// This file is the SETTLEMENT EVIDENCE BOUNDARY, from the journal store's side.
//
// A disposition command's settlement lives in an ORCHESTRATION store that is a
// different store, on a different backend, from the one holding the session's
// journal. That separation is the design, not an accident of deployment: the
// orchestration store holds the catalog, inbox and residency, and the bound journal
// is wherever the session's immutable binding says it is. Settlement therefore never
// reads a journal itself — it hands its configured reader the immutable binding and
// asks.
//
// WHAT A READER IS FOR. The released store's own journal reader answers only for a
// session in its OWN keyspace, which is the case where the journal and the
// orchestration rows share a store. That is not this program's composition, and it
// cannot be: a disposition session's catalog requires the released store's
// multi-tenant layout, while this package addresses its journals on the legacy
// single-tenant layout as "sessions/<uuid>". Pointing one store at both is the
// configuration TestOrchestrationStoreMustNotShareTheHarnessBackend refuses.
//
// WHY THE ADDRESSING BELONGS HERE. "How to address my journal" is this package's
// contract: the tenant it was opened with, and a session named by the UUID's
// canonical rendering. Nothing else can state that rule without restating it, and a
// restatement is a second authority free to drift. A reader needs only the released
// sessionstore package, so Host or Factory COULD write one without importing
// Harness — that variant matters for a Factory-side reconciler settling with no
// resident Host — but it would be carrying this package's addressing rule in
// someone else's source.

// DispositionBindingError reports that this Store may not answer a settlement
// evidence request: the request names another tenant, or its binding does not
// address a session this Store can resolve.
//
// It is a refusal and never an empty answer. The released reader contract is
// explicit that a reader must NEVER report a missing or unreadable record as an
// empty DispositionEvidence, because absence is not a disposition — the zero value
// settles nothing, and a reader that returned one would be handing the verifier a
// silence it could mistake for an answer.
type DispositionBindingError struct {
	Field            string
	TenantID         coresessionwire.TenantID
	RuntimeSessionID string
	Cause            error
}

func (e *DispositionBindingError) Error() string {
	return "sessionstore: this store cannot resolve the settlement evidence request: invalid " +
		e.Field + " (tenant " + strconv.Quote(string(e.TenantID)) +
		", runtime session " + strconv.Quote(e.RuntimeSessionID) + ")"
}

func (e *DispositionBindingError) Unwrap() error { return e.Cause }

// ReadDispositionEvidence resolves the request's immutable binding to a session in
// THIS Store and reports the committed disposition it holds. It satisfies the
// released sessionstore.DispositionEvidenceReader, so an orchestration store is
// configured with it through WithDispositionEvidence.
//
// THE ROUTING IS THE BINDING'S, NOT THE REQUEST'S SessionID. req.SessionID is the
// ORCHESTRATION store's identity for the session — the opaque one Factory admitted
// it under, which is not a UUID by contract — and it is deliberately not used to
// address anything here. The journal is addressed by Binding.RuntimeSessionID, which
// is the rig's UUID, under the tenant this Store was opened with. That indirection is
// the whole point of the binding: the two identity spaces never have to agree.
//
// THREE REFUSALS, and each is a refusal rather than a quieter answer:
//
//   - ANOTHER TENANT. A Store files one tenant's sessions for its whole life. A
//     request naming a different tenant is a request about a keyspace this Store
//     cannot see, and answering it from this one would be answering the wrong
//     question confidently.
//   - A BINDING THAT IS NOT IN THE DISPOSITION PROTOCOL. A legacy binding names the
//     released single-store protocol, whose outcomes are not dispositions at all.
//   - A RuntimeSessionID THAT IS NOT A UUID. This package's journals are named from
//     the UUID's canonical rendering and nothing else; a binding naming anything else
//     addresses no journal here.
//
// WHAT IT DOES NOT CHECK, stated because it is an obligation that moves rather than
// disappears: it does not validate Binding.StorageBindingID. A Store has no registry
// of which storage bindings it serves, so it cannot tell "my binding" from another's
// — it can only answer for the keyspace it owns. SELECTING which reader serves which
// StorageBindingID is therefore the composition root's job, and a deployment with two
// journal stores needs a multiplexer keyed on that field in front of these. See
// TestReaderDoesNotRouteOnStorageBindingID, which pins the consequence so the
// obligation cannot be quietly assumed to live here.
func (s *Store) ReadDispositionEvidence(
	ctx context.Context, req durablestore.DispositionEvidenceRequest,
) (durablestore.DispositionEvidence, error) {
	if req.TenantID != s.opts.TenantID {
		return durablestore.DispositionEvidence{}, &DispositionBindingError{
			Field: "TenantID", TenantID: req.TenantID, RuntimeSessionID: req.Binding.RuntimeSessionID,
		}
	}
	if req.Binding.ProtocolMode != durablestore.ProtocolModeDisposition {
		return durablestore.DispositionEvidence{}, &DispositionBindingError{
			Field: "Binding.ProtocolMode", TenantID: req.TenantID, RuntimeSessionID: req.Binding.RuntimeSessionID,
		}
	}
	id, err := uuid.Parse(req.Binding.RuntimeSessionID)
	if err != nil {
		return durablestore.DispositionEvidence{}, &DispositionBindingError{
			Field: "Binding.RuntimeSessionID", TenantID: req.TenantID,
			RuntimeSessionID: req.Binding.RuntimeSessionID, Cause: err,
		}
	}
	if id.IsZero() {
		return durablestore.DispositionEvidence{}, &DispositionBindingError{
			Field: "Binding.RuntimeSessionID", TenantID: req.TenantID,
			RuntimeSessionID: req.Binding.RuntimeSessionID,
		}
	}
	// The delegated request is re-addressed, not forwarded: the session named here is
	// THIS store's, and every other member is carried through unchanged because the
	// orchestration store derived them from its own immutable record and this reader
	// may not reinterpret one.
	local := req
	local.SessionID = harnessSessionID(id)
	return s.durable.ReadDispositionEvidence(ctx, local)
}

var _ durablestore.DispositionEvidenceReader = (*Store)(nil)
