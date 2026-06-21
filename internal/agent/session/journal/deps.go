// Package journal persists agent session events to a durable JetStream journal.
// This file is a temporary dependency anchor for the embedded server only: the
// nats.go client is now imported directly by the production journal/nats.go, but
// the embedded server (nats-server/v2) is imported solely by the integration test
// harness until the composition root wires it in (Phase 10). The blank import keeps
// `go mod tidy` recording the server as a direct dependency until then. Delete this
// file once production code imports the server package directly.
package journal

import (
	// Blank-imported to anchor the embedded-server dependency; see package doc.
	_ "github.com/nats-io/nats-server/v2/server"
)
