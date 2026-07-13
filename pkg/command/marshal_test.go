package command

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// seededUUID builds a deterministic non-zero uuid from a single seed byte so the
// marshalled output is stable across runs (mirrors the event codec's helper).
func seededUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// fullHeader is a representative, fully-populated command Header used by the
// round-trip table. Cause is populated (including a non-default Agency) so the
// nested correlation metadata is exercised by the round-trip.
func fullHeader() Header {
	return Header{
		CommandID: seededUUID(0x11),
		Cause: identity.Cause{
			Coordinates:     identity.Coordinates{SessionID: seededUUID(0x22), LoopID: seededUUID(0x33)},
			CommandID:       seededUUID(0x44),
			EventID:         seededUUID(0x55),
			ToolExecutionID: seededUUID(0x66),
			Agency:          identity.AgencyUser,
		},
		Agency: identity.AgencyUser,
	}
}

// sampleBlocks is a representative content slice exercising the block-delegation
// path (the content codec tags each block by type).
func sampleBlocks(text string) []content.Block {
	return []content.Block{
		&content.TextBlock{Text: text},
		&content.ToolUseBlock{ID: "tu-1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
	}
}

// TestMarshalCommandRoundTrip is the exhaustive fidelity table: one instance of
// EVERY concrete command type round-trips through MarshalCommand/UnmarshalCommand
// deep-equal to the original (modulo the transient ack channels, which are
// json:"-" and therefore nil after unmarshal). UserInput/SubagentResult blocks
// survive via the content codec.
func TestMarshalCommandRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cmd  Command
	}{
		{"UserInput delegate route", UserInput{Header: fullHeader(), Blocks: sampleBlocks("hello"), NoFold: true, TargetLoopID: seededUUID(0x44)}},
		{"UserInput nil blocks", UserInput{Header: fullHeader()}},
		{"SubagentResult", SubagentResult{
			Header:      fullHeader(),
			Coordinates: identity.Coordinates{SessionID: seededUUID(0x22), LoopID: seededUUID(0x33)},
			Blocks:      sampleBlocks("subagent output"),
		}},
		{"ApproveToolCall", ApproveToolCall{
			Header:    fullHeader(),
			GateRoute: GateRoute{Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, ToolExecutionID: seededUUID(0x77)},
			Scope:     tool.ScopeWorkspace,
		}},
		{"DenyToolCall", DenyToolCall{
			Header:    fullHeader(),
			GateRoute: GateRoute{Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, ToolExecutionID: seededUUID(0x77)},
		}},
		{"ProvideUserInput", ProvideUserInput{
			Header:    fullHeader(),
			GateRoute: GateRoute{Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, ToolExecutionID: seededUUID(0x77)},
			Answer:    "the answer",
		}},
		{"CancelQueuedInput", CancelQueuedInput{
			Header:          fullHeader(),
			Coordinates:     identity.Coordinates{SessionID: seededUUID(0x22), LoopID: seededUUID(0x33)},
			TargetCommandID: seededUUID(0x88),
		}},
		{"CancelDelegateRequest", CancelDelegateRequest{
			Header:          fullHeader(),
			Coordinates:     identity.Coordinates{SessionID: seededUUID(0x22), LoopID: seededUUID(0x33)},
			TargetCommandID: seededUUID(0x88),
		}},
		{"Interrupt", Interrupt{Header: fullHeader()}},
		{"Shutdown", Shutdown{Header: fullHeader()}},
		{"SetSecurityCeiling", SetSecurityCeiling{Header: fullHeader(), Level: 2}},
		{"SetSecurityCeiling zero", SetSecurityCeiling{Header: fullHeader()}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalCommand(tt.cmd)
			if err != nil {
				t.Fatalf("MarshalCommand(%s) error = %v", tt.name, err)
			}
			got, err := UnmarshalCommand(data)
			if err != nil {
				t.Fatalf("UnmarshalCommand(%s) error = %v\nwire: %s", tt.name, err, data)
			}
			if !reflect.DeepEqual(got, tt.cmd) {
				t.Errorf("round-trip(%s) mismatch:\n got = %#v\nwant = %#v\nwire: %s", tt.name, got, tt.cmd, data)
			}
		})
	}
}

// TestMarshalCommandTransientChannelsNotSerialized proves the Interrupt/Shutdown
// ack channels (json:"-") never reach the wire and are nil after unmarshal: the
// live reply channel has no durable representation, so a restored control command
// carries a nil Ack. The header still round-trips.
func TestMarshalCommandTransientChannelsNotSerialized(t *testing.T) {
	t.Parallel()

	interruptAck := make(chan bool, 1)
	shutdownAck := make(chan error, 1)
	cancelAck := make(chan DelegateCancelResult, 1)

	tests := []struct {
		name string
		cmd  Command
	}{
		{"Interrupt with live ack", Interrupt{Header: fullHeader(), Ack: interruptAck}},
		{"Shutdown with live ack", Shutdown{Header: fullHeader(), Ack: shutdownAck}},
		{"CancelDelegateRequest with live ack", CancelDelegateRequest{Header: fullHeader(), Coordinates: identity.Coordinates{SessionID: seededUUID(0x22), LoopID: seededUUID(0x33)}, TargetCommandID: seededUUID(0x88), Ack: cancelAck}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalCommand(tt.cmd)
			if err != nil {
				t.Fatalf("MarshalCommand(%s) error = %v", tt.name, err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(data, &keys); err != nil {
				t.Fatalf("unmarshal to key map: %v", err)
			}
			for _, absent := range []string{"ack", "Ack"} {
				if _, ok := keys[absent]; ok {
					t.Errorf("%s wire output unexpectedly carries key %q\nwire: %s", tt.name, absent, data)
				}
			}
			got, err := UnmarshalCommand(data)
			if err != nil {
				t.Fatalf("UnmarshalCommand(%s) error = %v\nwire: %s", tt.name, err, data)
			}
			switch c := got.(type) {
			case Interrupt:
				if c.Ack != nil {
					t.Errorf("restored Interrupt.Ack = %v, want nil", c.Ack)
				}
			case Shutdown:
				if c.Ack != nil {
					t.Errorf("restored Shutdown.Ack = %v, want nil", c.Ack)
				}
			case CancelDelegateRequest:
				if c.Ack != nil {
					t.Errorf("restored CancelDelegateRequest.Ack = %v, want nil", c.Ack)
				}
			default:
				t.Fatalf("restored %T, want Interrupt or Shutdown", got)
			}
		})
	}
}

// TestMarshalCommandEnvelopeKeys asserts the wire envelope carries the stable
// type discriminator and schema version as top-level sibling keys, and that they
// match the package's CommandName naming source — the journal reader depends on
// these keys.
func TestMarshalCommandEnvelopeKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cmd     Command
		wantTag CommandName
	}{
		{"UserInput", UserInput{Header: fullHeader()}, CommandUserInput},
		{"SubagentResult", SubagentResult{Header: fullHeader()}, CommandSubagentResult},
		{"ApproveToolCall", ApproveToolCall{Header: fullHeader()}, CommandApproveToolCall},
		{"DenyToolCall", DenyToolCall{Header: fullHeader()}, CommandDenyToolCall},
		{"ProvideUserInput", ProvideUserInput{Header: fullHeader()}, CommandProvideUserInput},
		{"CancelQueuedInput", CancelQueuedInput{Header: fullHeader()}, CommandCancelQueuedInput},
		{"CancelDelegateRequest", CancelDelegateRequest{Header: fullHeader()}, CommandCancelDelegateRequest},
		{"Interrupt", Interrupt{Header: fullHeader()}, CommandInterrupt},
		{"Shutdown", Shutdown{Header: fullHeader()}, CommandShutdown},
		{"SetSecurityCeiling", SetSecurityCeiling{Header: fullHeader()}, CommandSetSecurityCeiling},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalCommand(tt.cmd)
			if err != nil {
				t.Fatalf("MarshalCommand(%s) error = %v", tt.name, err)
			}
			var env struct {
				Type CommandName `json:"type"`
				V    int         `json:"v"`
			}
			if err := json.Unmarshal(data, &env); err != nil {
				t.Fatalf("unmarshal envelope: %v\nwire: %s", err, data)
			}
			if env.Type != tt.wantTag {
				t.Errorf("envelope type = %q, want %q\nwire: %s", env.Type, tt.wantTag, data)
			}
			if env.V != schemaVersion {
				t.Errorf("envelope v = %d, want %d\nwire: %s", env.V, schemaVersion, data)
			}
		})
	}
}

// wantCommandTypes is the count of concrete command types the codec MUST cover.
// It is the drift-guard tripwire: adding a new command to the sealed union
// without codec coverage changes the live count derived from classifyCommand and
// fails TestMarshalCommandCoversEveryType. A missed command type is an
// unpersistable intent-log record = silent restore data loss, which this guard
// forbids.
const wantCommandTypes = 10

// unionInstances is one zero-valued instance of EVERY concrete command type. The
// drift guard asserts the codec handles each, so a new union member is forced
// through the guard.
func unionInstances() []Command {
	return []Command{
		UserInput{}, SubagentResult{},
		ApproveToolCall{}, DenyToolCall{}, ProvideUserInput{},
		CancelQueuedInput{}, CancelDelegateRequest{}, Interrupt{}, Shutdown{},
		SetSecurityCeiling{},
	}
}

// TestMarshalCommandCoversEveryType is the drift guard. It derives the command
// set from classifyCommand (the single sealed-union enumeration, which reuses the
// CommandName naming source) and asserts: (a) every type classifies and marshals
// without an unknown-type error (the codec has an arm for it), and (b) the count
// equals wantCommandTypes. Adding a new command type without a codec arm makes
// MarshalCommand return a *UnknownCommandTypeError here (or drops the count),
// failing the build's tests.
func TestMarshalCommandCoversEveryType(t *testing.T) {
	t.Parallel()

	var covered int
	for _, c := range unionInstances() {
		name, ok := classifyCommand(c)
		if !ok {
			t.Fatalf("classifyCommand(%T) ok = false; the drift guard's union list is stale", c)
		}
		covered++
		// A zero-valued instance need not pass post-decode validation, but it must
		// NOT fail with an unknown-type error: that would mean the codec has no arm.
		_, err := MarshalCommand(c)
		var unknown *UnknownCommandTypeError
		if errors.As(err, &unknown) {
			t.Errorf("MarshalCommand(%s) returned UnknownCommandTypeError; the codec is missing an arm for this command type", name)
		}
	}
	if covered != wantCommandTypes {
		t.Errorf("command type count = %d, want %d; update wantCommandTypes AND add codec coverage for the new command type", covered, wantCommandTypes)
	}
}

// foreignCommand is a Command implementor declared OUTSIDE the codec's union, used
// to prove MarshalCommand fails closed (UnknownCommandTypeError) on a type it does
// not recognize rather than emitting a lossy record. (The Command interface is
// sealed by isCommand(), but a same-package test can still implement it.)
type foreignCommand struct{ Header }

func (foreignCommand) isCommand() {}

// TestMarshalCommandRejectsForeignType proves MarshalCommand fails closed on a
// command type outside its sealed union.
func TestMarshalCommandRejectsForeignType(t *testing.T) {
	t.Parallel()
	data, err := MarshalCommand(foreignCommand{Header: fullHeader()})
	if data != nil {
		t.Errorf("MarshalCommand(foreign) returned non-nil bytes: %s", data)
	}
	var unknown *UnknownCommandTypeError
	if !errors.As(err, &unknown) {
		t.Fatalf("MarshalCommand(foreign) error = %T (%v), want *UnknownCommandTypeError", err, err)
	}
}

// TestUnmarshalCommandRejectsMalformed proves the untrusted-restore boundary fails
// closed on bad input with typed errors: nil/empty/garbage/non-object → typed
// decode error; a valid envelope with an unknown "type" → *UnknownCommandTypeError;
// a missing "type" → *UnknownCommandTypeError (empty tag has no concrete type).
func TestUnmarshalCommandRejectsMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		data        []byte
		wantUnknown bool // expect *UnknownCommandTypeError specifically
	}{
		{name: "nil bytes", data: nil},
		{name: "empty bytes", data: []byte{}},
		{name: "garbage", data: []byte("not json at all")},
		{name: "json array not object", data: []byte("[1,2,3]")},
		{name: "json null", data: []byte("null")},
		{name: "missing type tag", data: []byte(`{"v":1,"command_id":"x"}`), wantUnknown: true},
		{name: "unknown type tag", data: []byte(`{"type":"NotARealCommand","v":1}`), wantUnknown: true},
		{name: "known type malformed payload", data: []byte(`{"type":"ApproveToolCall","scope":"not a number"}`)},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := UnmarshalCommand(tt.data)
			if err == nil {
				t.Fatalf("UnmarshalCommand(%q) error = nil, want a typed error; got %#v", tt.data, got)
			}
			if got != nil {
				t.Errorf("UnmarshalCommand(%q) returned non-nil command on error: %#v", tt.data, got)
			}
			if tt.wantUnknown {
				var unknown *UnknownCommandTypeError
				if !errors.As(err, &unknown) {
					t.Errorf("UnmarshalCommand(%q) error = %T (%v), want *UnknownCommandTypeError", tt.data, err, err)
				}
			}
		})
	}
}

// TestUnmarshalCommandEnforcesByteCap proves the decode side fails closed with a
// *CommandLimitError on input exceeding the envelope byte cap (the cap applies at
// the untrusted boundary only — the encode side is uncapped).
func TestUnmarshalCommandEnforcesByteCap(t *testing.T) {
	t.Parallel()
	oversized := make([]byte, maxCommandBytes+1)
	_, err := UnmarshalCommand(oversized)
	var limit *CommandLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("UnmarshalCommand(oversized) error = %T (%v), want *CommandLimitError", err, err)
	}
	if limit.Max != maxCommandBytes {
		t.Errorf("CommandLimitError.Max = %d, want %d", limit.Max, maxCommandBytes)
	}
}

// TestUnmarshalCommandValidatesAfterDecode proves a structurally-valid record that
// violates the ID fill matrix (here, a zero CommandID) is rejected by the
// post-decode ValidateCommand check rather than resurrected.
func TestUnmarshalCommandValidatesAfterDecode(t *testing.T) {
	t.Parallel()
	// A UserInput with no CommandID: structurally valid JSON, but ValidateCommand
	// requires CommandID on every command.
	data := []byte(`{"type":"UserInput","v":1}`)
	got, err := UnmarshalCommand(data)
	if got != nil {
		t.Errorf("UnmarshalCommand returned non-nil command on validation failure: %#v", got)
	}
	var ve *CommandValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("UnmarshalCommand error = %T (%v), want *CommandValidationError", err, err)
	}
	if ve.Field != FieldCommandID {
		t.Errorf("CommandValidationError.Field = %q, want %q", ve.Field, FieldCommandID)
	}
}

// FuzzDecodeCommand drives UnmarshalCommand over the untrusted-restore boundary:
// it must never panic and must always return either a valid command or a typed
// error (never an untyped error or a non-nil command with a non-nil error). Seeds
// include valid encodings of several command types plus assorted garbage.
func FuzzDecodeCommand(f *testing.F) {
	seeds := []Command{
		UserInput{Header: fullHeader(), Blocks: sampleBlocks("hi")},
		SubagentResult{Header: fullHeader(), Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, Blocks: sampleBlocks("out")},
		ApproveToolCall{Header: fullHeader(), GateRoute: GateRoute{Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, ToolExecutionID: seededUUID(0x77)}, Scope: tool.ScopeSession},
		DenyToolCall{Header: fullHeader(), GateRoute: GateRoute{Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, ToolExecutionID: seededUUID(0x77)}},
		ProvideUserInput{Header: fullHeader(), GateRoute: GateRoute{Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, ToolExecutionID: seededUUID(0x77)}, Answer: "a"},
		CancelQueuedInput{Header: fullHeader(), Coordinates: identity.Coordinates{SessionID: seededUUID(0x22), LoopID: seededUUID(0x33)}, TargetCommandID: seededUUID(0x88)},
		Interrupt{Header: fullHeader()},
		Shutdown{Header: fullHeader()},
	}
	for _, c := range seeds {
		if data, err := MarshalCommand(c); err == nil {
			f.Add(data)
		}
	}
	f.Add([]byte(""))
	f.Add([]byte("null"))
	f.Add([]byte("{}"))
	f.Add([]byte(`{"type":"UserInput"}`))
	f.Add([]byte(`{"type":"NotReal","v":1}`))
	f.Add([]byte(`{"type":"UserInput","blocks":[{"type":"text"}]}`))
	f.Add([]byte("not json"))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := UnmarshalCommand(data)
		if err != nil {
			if got != nil {
				t.Errorf("UnmarshalCommand returned both a command (%#v) and an error (%v)", got, err)
			}
			if !isTypedDecodeError(err) {
				t.Errorf("UnmarshalCommand returned an untyped error %T: %v", err, err)
			}
			return
		}
		if got == nil {
			t.Errorf("UnmarshalCommand returned nil command with nil error for input %q", data)
		}
	})
}

// grantFreeApproveWire is the byte-exact durable wire form of a fully-populated
// grant-FREE ApproveToolCall (Scope=ScopeWorkspace, no AcceptedGrants). Adding the
// AcceptedGrants field (json:"accepted_grants,omitempty") MUST leave this byte
// stream unchanged — an old journal record and a new grant-free command marshal
// identically. If this drifts, the omitempty was dropped or a field was renamed;
// fix the code, never silently repin (old journals carry the old bytes).
const grantFreeApproveWire = `{"agency":1,"cause":{"session_id":"22222222-2222-2222-2222-222222222222","loop_id":"33333333-3333-3333-3333-333333333333","command_id":"44444444-4444-4444-4444-444444444444","event_id":"55555555-5555-5555-5555-555555555555","tool_execution_id":"66666666-6666-6666-6666-666666666666","agency":1},"command_id":"11111111-1111-1111-1111-111111111111","loop_id":"33333333-3333-3333-3333-333333333333","scope":2,"tool_execution_id":"77777777-7777-7777-7777-777777777777","type":"ApproveToolCall","v":1}`

// goldenApprove is the fixed, fully-populated grant-FREE ApproveToolCall whose wire
// form grantFreeApproveWire pins.
func goldenApprove() ApproveToolCall {
	return ApproveToolCall{
		Header:    fullHeader(),
		GateRoute: GateRoute{Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, ToolExecutionID: seededUUID(0x77)},
		Scope:     tool.ScopeWorkspace,
	}
}

// TestApproveToolCall_GrantFreeByteIdentical proves a grant-free ApproveToolCall
// marshals byte-identically to its pre-AcceptedGrants wire form (the omitempty
// field is absent), so introducing pre-ask grants does not break durable restore
// of an existing intent-log record.
func TestApproveToolCall_GrantFreeByteIdentical(t *testing.T) {
	t.Parallel()
	data, err := MarshalCommand(goldenApprove())
	if err != nil {
		t.Fatalf("MarshalCommand: %v", err)
	}
	if string(data) != grantFreeApproveWire {
		t.Errorf("grant-free ApproveToolCall wire drifted:\n got = %s\nwant = %s", data, grantFreeApproveWire)
	}
	if strings.Contains(string(data), "accepted_grants") {
		t.Errorf("grant-free wire must not carry the accepted_grants key: %s", data)
	}
}

// TestApproveToolCall_WithGrantsRoundTrip proves an ApproveToolCall carrying
// AcceptedGrants round-trips deep-equal (the accepted tokens survive the durable
// codec), and the key appears on the wire when non-empty.
func TestApproveToolCall_WithGrantsRoundTrip(t *testing.T) {
	t.Parallel()
	orig := goldenApprove()
	orig.AcceptedGrants = []string{"tok-egress", "tok-fswrite"}
	data, err := MarshalCommand(orig)
	if err != nil {
		t.Fatalf("MarshalCommand: %v", err)
	}
	if !strings.Contains(string(data), "accepted_grants") {
		t.Errorf("with-grants wire must carry the accepted_grants key: %s", data)
	}
	got, err := UnmarshalCommand(data)
	if err != nil {
		t.Fatalf("UnmarshalCommand: %v\nwire: %s", err, data)
	}
	if !reflect.DeepEqual(got, orig) {
		t.Errorf("with-grants round-trip mismatch:\n got = %#v\nwant = %#v\nwire: %s", got, orig, data)
	}
}

// TestApproveToolCall_OldCommandDecodesNilGrants proves a pre-existing command with
// NO accepted_grants key decodes to a nil AcceptedGrants (backward compatibility:
// old journals never wrote the field).
func TestApproveToolCall_OldCommandDecodesNilGrants(t *testing.T) {
	t.Parallel()
	got, err := UnmarshalCommand([]byte(grantFreeApproveWire))
	if err != nil {
		t.Fatalf("UnmarshalCommand(old wire): %v", err)
	}
	atc, ok := got.(ApproveToolCall)
	if !ok {
		t.Fatalf("decoded %T, want ApproveToolCall", got)
	}
	if atc.AcceptedGrants != nil {
		t.Errorf("AcceptedGrants = %#v, want nil for an old command", atc.AcceptedGrants)
	}
}

// isTypedDecodeError reports whether err is one of the codec's concrete error
// types (or a delegated content codec / validation error), proving the parser
// never leaks an untyped error across the boundary.
func isTypedDecodeError(err error) bool {
	var (
		unknownCmd   *UnknownCommandTypeError
		decode       *CommandDecodeError
		limit        *CommandLimitError
		validation   *CommandValidationError
		blockDecode  *content.BlockDecodeError
		blockLimit   *content.BlockLimitError
		unknownBlock *content.UnknownBlockTypeError
	)
	return errors.As(err, &unknownCmd) ||
		errors.As(err, &decode) ||
		errors.As(err, &limit) ||
		errors.As(err, &validation) ||
		errors.As(err, &blockDecode) ||
		errors.As(err, &blockLimit) ||
		errors.As(err, &unknownBlock)
}
