// Package present defines the Message Presenter contract. A product hook may
// prepend and append text to a user message, but may not edit the user's blocks.
// The session journals the rendering so restore and replay never re-render it.
// Machine-originated input is never presented.
//
// A presenter should be deterministic in its Input. A command redelivered
// before its durable application prefix may be presented again.
package present

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/runtimecommand"
)

const (
	// MaxFrameBlocks bounds Prefix and Suffix combined.
	MaxFrameBlocks = 8
	// MaxFrameTextBytes bounds their summed UTF-8 text bytes.
	MaxFrameTextBytes = 8192
)

// Input is the read-only context given to a presenter. Principal and Metadata
// are nil when absent. Blocks is a clone of the user's original blocks.
type Input struct {
	SessionID uuid.UUID
	LoopID    uuid.UUID
	AgentName identity.AgentName
	Kind      runtimecommand.Kind
	Principal *sessionwire.Principal
	Metadata  sessionwire.MessageMetadata
	Blocks    []content.Block
}

// Frame is the text placed before and after the user's own blocks.
type Frame struct {
	Prefix []content.Block
	Suffix []content.Block
}

// Empty reports whether this frame adds any blocks.
func (f Frame) Empty() bool { return len(f.Prefix) == 0 && len(f.Suffix) == 0 }

// Validate permits only nonempty text blocks and applies both frame limits.
func (f Frame) Validate() error {
	if n := len(f.Prefix) + len(f.Suffix); n > MaxFrameBlocks {
		return frameInvalid(fmt.Errorf("%d blocks exceeds %d", n, MaxFrameBlocks))
	}
	total := 0
	for _, part := range [][]content.Block{f.Prefix, f.Suffix} {
		for i, block := range part {
			text, ok := block.(*content.TextBlock)
			if !ok || text == nil {
				return frameInvalid(fmt.Errorf("block %d is %T, want *content.TextBlock", i, block))
			}
			if text.Text == "" {
				return frameInvalid(fmt.Errorf("block %d is empty", i))
			}
			if !utf8.ValidString(text.Text) {
				return frameInvalid(fmt.Errorf("block %d has invalid UTF-8", i))
			}
			total += len(text.Text)
		}
	}
	if total > MaxFrameTextBytes {
		return frameInvalid(fmt.Errorf("%d text bytes exceeds %d", total, MaxFrameTextBytes))
	}
	return nil
}

// Presenter frames one user message.
type Presenter interface {
	Present(context.Context, Input) (Frame, error)
}

// ErrorKind classifies a presentation failure.
type ErrorKind string

const (
	ErrorPresenterFailed ErrorKind = "presenter_failed"
	ErrorFrameInvalid    ErrorKind = "frame_invalid"
)

// Error is a typed presentation failure. Error deliberately prints only Kind:
// product errors may name a person and must not be logged by harness.
type Error struct {
	Kind  ErrorKind
	Cause error
}

func (e *Error) Error() string { return "present: " + string(e.Kind) }
func (e *Error) Unwrap() error { return e.Cause }

func frameInvalid(cause error) error { return &Error{Kind: ErrorFrameInvalid, Cause: cause} }

// Run invokes a presenter with a defensive copy and returns a validated,
// separately cloned frame. A presenter error or panic is ErrorPresenterFailed.
func Run(ctx context.Context, p Presenter, in Input) (frame Frame, err error) {
	in.Blocks = content.CloneBlocks(in.Blocks)
	if in.Principal != nil {
		principal := *in.Principal
		in.Principal = &principal
	}
	if in.Metadata != nil {
		copyOfMetadata := make(sessionwire.MessageMetadata, len(in.Metadata))
		for key, value := range in.Metadata {
			copyOfMetadata[key] = value
		}
		in.Metadata = copyOfMetadata
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			frame, err = Frame{}, &Error{Kind: ErrorPresenterFailed, Cause: fmt.Errorf("presenter panic: %v", recovered)}
		}
	}()
	got, presentErr := p.Present(ctx, in)
	if presentErr != nil {
		return Frame{}, &Error{Kind: ErrorPresenterFailed, Cause: presentErr}
	}
	if err := got.Validate(); err != nil {
		return Frame{}, err
	}
	return Frame{Prefix: content.CloneBlocks(got.Prefix), Suffix: content.CloneBlocks(got.Suffix)}, nil
}
