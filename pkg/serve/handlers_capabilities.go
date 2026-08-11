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

// fullFeatures is the capability set a complete server (live + control + read)
// advertises, in the canonical contract order. readOnlyFeatures (read_server.go) is
// the honest subset for a handler with no live plane.
var fullFeatures = []string{featureJournal, featureLiveSSE, featureEphemeralSSE, featureGateResponse}

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
// document (SPEC §6). It reads no request state — the document is fixed at
// construction from the feature set this server actually serves, so a read-only
// server honestly advertises less than a full one. The Features order is part of
// the contract.
func (s *readServer) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, capabilities{
		Protocol: protocolName,
		Version:  protocolVersion,
		Features: s.features,
	})
}
