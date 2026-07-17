//go:build migration

package foreignloop

import "github.com/looprig/core/content"

// DecodeTranscriptForMigration exposes the legacy decoder only to migration
// parity tests. It must not become a permanent public API.
func DecodeTranscriptForMigration(path string) ([]content.AgenticMessages, error) {
	return decodeTranscriptTail(path, 0)
}
