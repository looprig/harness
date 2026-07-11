package loopruntime

import (
	"testing"

	"github.com/looprig/harness/pkg/command"
)

// compile-time: *Loop must satisfy Backend.
var _ Backend = (*Loop)(nil)

func TestLoopAccessorsExposeChannels(t *testing.T) {
	t.Parallel()
	l := &Loop{Commands: make(chan command.Command), Done: make(chan struct{})}
	if l.CommandSink() == nil {
		t.Fatal("CommandSink() returned nil")
	}
	if l.DoneChan() == nil {
		t.Fatal("DoneChan() returned nil")
	}
}
