package sessionruntime

import (
	"context"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

type priorityTestBackend struct {
	ordinary chan command.Command
	priority chan command.Command
	done     chan struct{}
}

func newPriorityTestBackend() *priorityTestBackend {
	return &priorityTestBackend{ordinary: make(chan command.Command, 1), priority: make(chan command.Command, 1), done: make(chan struct{})}
}

func (b *priorityTestBackend) CommandSink() chan<- command.Command         { return b.ordinary }
func (b *priorityTestBackend) PriorityCommandSink() chan<- command.Command { return b.priority }
func (b *priorityTestBackend) DoneChan() <-chan struct{}                   { return b.done }
func (b *priorityTestBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

type ordinaryTestBackend struct {
	ordinary chan command.Command
	done     chan struct{}
}

func (b *ordinaryTestBackend) CommandSink() chan<- command.Command { return b.ordinary }
func (b *ordinaryTestBackend) DoneChan() <-chan struct{}           { return b.done }
func (b *ordinaryTestBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

func TestCommandSinkForRoutesOnlyNativePriorityControls(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		backend      func() (*priorityTestBackend, loop.Backend)
		command      command.Command
		wantPriority bool
	}{
		{
			name: "interrupt uses priority capability",
			backend: func() (*priorityTestBackend, loop.Backend) {
				value := newPriorityTestBackend()
				return value, value
			},
			command:      command.Interrupt{Header: command.Header{CommandID: uuid.UUID{1}}, Ack: make(chan bool, 1)},
			wantPriority: true,
		},
		{
			name: "shutdown uses priority capability",
			backend: func() (*priorityTestBackend, loop.Backend) {
				value := newPriorityTestBackend()
				return value, value
			},
			command:      command.Shutdown{Header: command.Header{CommandID: uuid.UUID{2}}, Ack: make(chan error, 1)},
			wantPriority: true,
		},
		{
			name: "ordinary command stays on fifo sink",
			backend: func() (*priorityTestBackend, loop.Backend) {
				value := newPriorityTestBackend()
				return value, value
			},
			command: command.UserInput{Header: command.Header{CommandID: uuid.UUID{3}}},
		},
		{
			name: "backend without capability falls back",
			backend: func() (*priorityTestBackend, loop.Backend) {
				value := newPriorityTestBackend()
				return value, &ordinaryTestBackend{ordinary: value.ordinary, done: value.done}
			},
			command: command.Interrupt{Header: command.Header{CommandID: uuid.UUID{4}}, Ack: make(chan bool, 1)},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			channels, backend := tt.backend()
			commandSinkFor(backend, tt.command) <- tt.command
			if got := len(channels.priority) == 1; got != tt.wantPriority {
				t.Errorf("priority delivery = %v, want %v", got, tt.wantPriority)
			}
			if got := len(channels.ordinary) == 1; got == tt.wantPriority {
				t.Errorf("ordinary delivery = %v, want %v", got, !tt.wantPriority)
			}
		})
	}
}
