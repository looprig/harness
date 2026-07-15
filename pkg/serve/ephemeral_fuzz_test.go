package serve

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
)

// FuzzEphemeralFrameDecode fuzzes the SSE wire-envelope decode path — the CLIENT's
// reconstruction side of the serve encoder. serve is write-only (it emits the two
// frame DTOs and never decodes them), so this target guards the contract a decoder
// depends on: arbitrary bytes fed at the frame boundary (a) never panic, (b) yield a
// TYPED error on malformed input, and (c) never reconstruct a persistable/Ephemeral
// event from a durable envelope.
//
// It exercises all three legs of the wire:
//  1. ephemeralFrame — a structural JSON decode; a decoded Delta is further
//     interpreted (json.Valid) to prove no follow-on panic.
//  2. enduringFrame — a structural decode whose inner "event" raw is the durable
//     envelope; that raw is fed to event.UnmarshalEvent under the full invariant.
//  3. event.UnmarshalEvent(rawFuzzBytes) directly — the envelope decoder is also
//     driven with the bare fuzz input, not only well-framed bodies.
//
// pkg/event already has FuzzDecodeEvent for UnmarshalEvent alone; this does NOT
// duplicate it — it adds the serve-level frame DTO decode (the two unexported wire
// shapes) that lives only in this package, and re-asserts the no-Ephemeral-
// reconstruction invariant at the frame boundary.
func FuzzEphemeralFrameDecode(f *testing.F) {
	// Golden seeds: the exact JSON bodies the P2-7 fixtures emit after "data: ",
	// read from the .sse goldens so the corpus stays in lockstep with the encoder.
	f.Add(fixtureDataPayload(f, "enduring_frame.sse"))
	f.Add(fixtureDataPayload(f, "ephemeral_token_delta.sse"))
	f.Add([]byte(`{"v":1,"kind":"compaction_started","header":{"event_id":"88888888-8888-8888-8888-888888888888"},"delta":{"attempt_id":"66666666-6666-6666-6666-666666666666","reason":1,"basis":{"revision":3,"through_event_id":"77777777-7777-7777-7777-777777777777"}}}`))

	// A real durable envelope (event.MarshalEvent of an Enduring TurnDone): the exact
	// bytes that ride in an enduringFrame's inner "event" field.
	if raw, err := event.MarshalEvent(event.TurnDone{TurnIndex: 1}); err == nil {
		f.Add(raw)
	}

	// Malformed / adversarial samples.
	for _, s := range []string{
		"",
		"{",
		"null",
		"{}",
		"not json",
		`{"v":"notint"}`,
		`{"kind":123}`,
		`{"v":1,"event":"not-an-object"}`,
		`{"v":1,"event":{"type":"NotReal","v":1}}`,
		`{"v":1,"kind":"token_delta","delta":{`,
		`{"v":1,"kind":"token_delta","header":{"session_id":123}}`,
		`{"type":"TokenDelta","v":1}`, // an Ephemeral type name in a durable envelope
		`[1,2,3]`,
		`{"v":1,"event":{"type":"StepDone","messages":[{"role":"tool"}]}}`,
	} {
		f.Add([]byte(s))
	}
	// Non-UTF8 bytes.
	f.Add([]byte{0xff, 0xfe, 0x00, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Leg 1: ephemeralFrame structural decode — no panic; a decoded Delta must be
		// further-interpretable without panicking.
		var ef ephemeralFrame
		if err := json.Unmarshal(data, &ef); err == nil {
			if len(ef.Delta) > 0 {
				// json.Valid never panics; interpreting the delta bytes as JSON is the
				// decoder's next step and must be safe on any accepted frame.
				_ = json.Valid(ef.Delta)
				var probe json.RawMessage
				_ = json.Unmarshal(ef.Delta, &probe)
			}
		}

		// Leg 2: enduringFrame structural decode — no panic; a non-empty inner "event"
		// raw is the durable envelope and must satisfy the reconstruction invariant.
		var enf enduringFrame
		if err := json.Unmarshal(data, &enf); err == nil {
			if len(enf.Event) > 0 {
				assertReconstruct(t, enf.Event)
			}
		}

		// Leg 3: feed the raw fuzz bytes straight into the envelope decoder.
		assertReconstruct(t, data)
	})
}

// assertReconstruct drives event.UnmarshalEvent over raw and enforces the wire
// invariant: no panic; on error a TYPED codec error (never an untyped error, never a
// non-nil event alongside an error); on success a NON-Ephemeral event — a durable
// envelope can never reconstruct an Ephemeral-class event (the marshaler fails closed
// on Ephemeral, so the decoder must never produce one from persisted bytes).
func assertReconstruct(t *testing.T, raw []byte) {
	t.Helper()
	ev, err := event.UnmarshalEvent(raw)
	if err != nil {
		if ev != nil {
			t.Errorf("UnmarshalEvent returned both an event (%#v) and an error (%v)", ev, err)
		}
		if !isTypedEventError(err) {
			t.Errorf("UnmarshalEvent returned an untyped error %T: %v for input %q", err, err, raw)
		}
		return
	}
	if ev == nil {
		t.Errorf("UnmarshalEvent returned nil event with nil error for input %q", raw)
		return
	}
	if ev.Class() == event.Ephemeral {
		t.Errorf("INVARIANT BREACH: durable envelope reconstructed an Ephemeral event %T from %q", ev, raw)
	}
}

// isTypedEventError reports whether err is one of the event codec's concrete error
// types (or a delegated content/tool codec error). It mirrors the set proven
// exhaustive by pkg/event's own FuzzDecodeEvent, so any error UnmarshalEvent returns
// across the fuzz input space is a typed, errors.As-inspectable value — never a bare
// errors.New/fmt.Errorf leaked across the untrusted boundary.
func isTypedEventError(err error) bool {
	var (
		unknownEvent *event.UnknownEventTypeError
		decode       *event.EventDecodeError
		limit        *event.EventLimitError
		invalid      *event.InvalidEventError
		unknownRole  *event.UnknownMessageRoleError
		blockDecode  *content.BlockDecodeError
		blockLimit   *content.BlockLimitError
		unknownBlock *content.UnknownBlockTypeError
		reqDecode    *tool.PermissionRequestDecodeError
		reqLimit     *tool.PermissionRequestLimitError
		reqUnknown   *tool.UnknownPermissionRequestError
	)
	return errors.As(err, &unknownEvent) ||
		errors.As(err, &decode) ||
		errors.As(err, &limit) ||
		errors.As(err, &invalid) ||
		errors.As(err, &unknownRole) ||
		errors.As(err, &blockDecode) ||
		errors.As(err, &blockLimit) ||
		errors.As(err, &unknownBlock) ||
		errors.As(err, &reqDecode) ||
		errors.As(err, &reqLimit) ||
		errors.As(err, &reqUnknown)
}

// fixtureDataPayload reads a golden .sse fixture and returns the JSON body after the
// "data: " prefix — the exact bytes a client parses out of the SSE frame. It fails
// the seed phase loudly if the golden is missing or malformed so the corpus can never
// silently drift from the encoder.
func fixtureDataPayload(f *testing.F, name string) []byte {
	f.Helper()
	raw, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "fixtures", name)))
	if err != nil {
		f.Fatalf("read fixture %s: %v", name, err)
	}
	const marker = "data: "
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(line, []byte(marker)) {
			return bytes.TrimPrefix(line, []byte(marker))
		}
	}
	f.Fatalf("fixture %s has no %q line", name, marker)
	return nil
}
