package serve

import "net/http"

// Static capability-discovery constants (SPEC §6). protocolName and protocolVersion
// identify the wire contract; the feature strings name the optional planes a client
// may rely on. These are compile-time constants — the document is the same for every
// request and never depends on server state, auth, or tenancy.
const (
	protocolName    = "looprig.serve"
	protocolVersion = 1

	featureJournal      = "journal"
	featureLiveSSE      = "live_sse"
	featureEphemeralSSE = "ephemeral_sse"
	featureGateResponse = "gate_response"
)

// capabilities is the typed discovery document returned by GET /v1/capabilities.
// It is pure capability advertisement — not health, auth, or tenancy — so a client
// can negotiate the protocol version and learn which optional planes this server
// supports before opening a session. The Features order is part of the contract.
type capabilities struct {
	Protocol string   `json:"protocol"`
	Version  int      `json:"version"`
	Features []string `json:"features"`
}

// handleCapabilities serves GET /v1/capabilities: the static protocol-discovery
// document (SPEC §6). It reads no request state and touches no server dependency —
// it always emits the same 200 JSON body naming the protocol, its version, and the
// supported feature planes in their canonical order.
func (s *server[S, O]) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, capabilities{
		Protocol: protocolName,
		Version:  protocolVersion,
		Features: []string{featureJournal, featureLiveSSE, featureEphemeralSSE, featureGateResponse},
	})
}
