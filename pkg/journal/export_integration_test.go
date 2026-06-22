//go:build integration

package journal

import (
	"context"

	"github.com/nats-io/nats.go"
)

// PublishFunc is the test-visible alias of the journal's unexported publish seam. The
// integration tests (package journal_test) construct closures of this shape to drive
// the ambiguous-ack resolve branches without the production seam ever leaking into the
// public API.
type PublishFunc = func(ctx context.Context, msg *nats.Msg) (*nats.PubAck, error)

// SwapPublish is a test-only hook (compiled only under -tags integration) over the
// journal's unexported publish seam. With a non-nil fn it installs fn as the seam and
// returns the seam it replaced (so a test can capture and fall through to the real
// fenced publish). With a nil fn it does NOT replace anything — it just returns the
// current seam, so a test can capture the real default before overriding. It panics if
// j is not the concrete *streamJournal, since only that type has the seam.
func SwapPublish(j SessionJournal, fn PublishFunc) PublishFunc {
	sj, ok := j.(*streamJournal)
	if !ok {
		panic("journal: SwapPublish on a non-*streamJournal SessionJournal")
	}
	prev := sj.publish
	if fn != nil {
		sj.publish = fn
	}
	return prev
}
