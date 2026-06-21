// Package journal will persist agent session events to a durable JetStream
// journal. This file is a temporary dependency anchor: it blank-imports the
// NATS client and embedded server so `go mod tidy` records them as direct
// dependencies before the first real importer lands (Task 0.2 onward). Delete
// this file once journal code imports these packages directly.
package journal

import (
	// Blank-imported to anchor the NATS dependencies; see package doc.
	_ "github.com/nats-io/nats-server/v2/server"
	_ "github.com/nats-io/nats.go"
)
