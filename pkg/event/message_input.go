package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/identity"
)

// MessageInput describes attribution and presentation on an assembled USER
// message. Its original blocks are Message.Blocks[Prefix : len-Suffix]. A nil
// value means no attribution or presentation and preserves old event bytes.
// Older harness releases silently drop this field; see the one-way upgrade note.
type MessageInput struct {
	Principal *sessionwire.Principal      `json:"principal,omitzero"`
	Metadata  sessionwire.MessageMetadata `json:"metadata,omitempty"`
	Prefix    int                         `json:"prefix,omitzero"`
	Suffix    int                         `json:"suffix,omitzero"`
}

type messageInputWire MessageInput

// UnmarshalJSON refuses unknown members and trailing data, even though the
// surrounding legacy event decoder accepts additive top-level members.
func (m *MessageInput) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire messageInputWire
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data after input")
	}
	*m = MessageInput(wire)
	return nil
}

// UserBlocks returns the user's own blocks from an assembled message. Invalid
// counts return nil; event validation normally refuses them before publication.
func UserBlocks(msg *content.UserMessage, in *MessageInput) []content.Block {
	if msg == nil {
		return nil
	}
	if in == nil {
		return msg.Blocks
	}
	end := len(msg.Blocks) - in.Suffix
	if in.Prefix < 0 || in.Suffix < 0 || in.Prefix > end {
		return nil
	}
	return msg.Blocks[in.Prefix:end]
}

func validateMessageInput(name EventName, agency identity.Agency, msg *content.UserMessage, in *MessageInput) error {
	if in == nil {
		return nil
	}
	switch {
	case agency != identity.AgencyUser,
		in.Principal == nil && len(in.Metadata) == 0 && in.Prefix == 0 && in.Suffix == 0,
		in.Prefix < 0 || in.Suffix < 0,
		msg == nil || in.Prefix+in.Suffix > len(msg.Blocks),
		in.Principal != nil && in.Principal.Validate() != nil,
		len(in.Metadata) > 0 && in.Metadata.Validate() != nil:
		return &InvalidEventError{Event: name, Field: FieldInput, Rule: RuleInvalid}
	}
	return nil
}
