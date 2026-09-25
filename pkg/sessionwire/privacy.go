package sessionwire

import (
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// privacy.go holds public-body redactions: Host-only model endpoints and
// workspace paths, gate audit payloads, and message metadata.
//
// Both redactions act on the PUBLIC projection only. The native replay body the
// journal stores beside it (the envelope's runtime slot) is unchanged, so restore,
// config-fingerprint comparison and drift assessment read exactly what they always
// read, and a journal written before this redaction replays identically. A public
// body already written keeps what was written: this projects new events, it does not
// rewrite history. (A page re-projected from native records through
// ProjectJournalPage is redacted, because it is a new projection.)
//
// The public ConfigManifest is therefore a PRESENTATION of the manifest, not the
// manifest: with its workspace_root replaced it no longer hashes to the
// adopted_fingerprint beside it. Fingerprints are authenticated only against the
// native body, which is the only copy restore reads.

// logicalWorkspacePrefix and publicWorkspaceRoot mirror the runtime's model-visible
// workspace identity (internal/sessionruntime logicalWorkspaceRoot): a function of the
// session id alone, stable across Hosts. A test in internal/sessionruntime pins that
// the two derivations agree.
const logicalWorkspacePrefix = "/sessions/"

func publicWorkspaceRoot(sessionID uuid.UUID) string {
	if sessionID.IsZero() {
		return ""
	}
	return logicalWorkspacePrefix + sessionID.String() + "/workspace"
}

// redactPublicBody applies the per-type public redactions to an event's native
// encoding. It returns encoded unchanged for a type that carries nothing to redact.
func redactPublicBody(ev event.Event, encoded []byte) ([]byte, error) {
	switch value := ev.(type) {
	case event.TurnStarted, event.TurnFoldedInto, event.InputCancelled:
		// The public message remains exactly what the model saw, and the
		// principal/boundary counts remain visible. Custom metadata is audit-only.
		if !hasMessageMetadata(value) {
			return encoded, nil
		}
		return editObject(encoded, func(fields map[string]json.RawMessage) error {
			if err := editMember(fields, "input", func(input map[string]json.RawMessage) error {
				delete(input, "metadata")
				return nil
			}); err != nil {
				return err
			}
			if string(fields["input"]) == "{}" {
				delete(fields, "input")
			}
			return nil
		})
	case event.GateResolved:
		// GateResolved's runtime audit may contain raw form answers. The public
		// record retains the gate/action/source correlation but never the audit.
		return editObject(encoded, func(fields map[string]json.RawMessage) error {
			delete(fields, "audit")
			return nil
		})
	case event.LoopStarted, event.LoopInferenceChanged, event.LoopModeChanged:
		// The model endpoint is Host configuration: it names internal gateway
		// topology and may embed a credential in its userinfo, query, fragment or
		// path, none of which can be proven safe by inspection. The runtime's public
		// identity is its provider and model (key) plus api_format; the endpoint is
		// omitted outright rather than sanitised.
		return editObject(encoded, func(fields map[string]json.RawMessage) error {
			return editMember(fields, "runtime", func(runtime map[string]json.RawMessage) error {
				delete(runtime, "base_url")
				return nil
			})
		})
	case event.SessionStarted:
		logical := publicWorkspaceRoot(value.SessionID)
		return editObject(encoded, func(fields map[string]json.RawMessage) error {
			if err := editMember(fields, "config", replaceWorkspaceRoot(logical)); err != nil {
				return err
			}
			return editMember(fields, "manifest", replaceWorkspaceRoot(logical))
		})
	case event.ConfigurationAdopted:
		logical := publicWorkspaceRoot(value.SessionID)
		return editObject(encoded, func(fields map[string]json.RawMessage) error {
			if err := editMember(fields, "manifest", replaceWorkspaceRoot(logical)); err != nil {
				return err
			}
			return redactWorkspaceDrift(fields)
		})
	default:
		return encoded, nil
	}
}

func hasMessageMetadata(value event.Event) bool {
	var input *event.MessageInput
	switch typed := value.(type) {
	case event.TurnStarted:
		input = typed.Input
	case event.TurnFoldedInto:
		input = typed.Input
	case event.InputCancelled:
		input = typed.Input
	}
	return input != nil && len(input.Metadata) > 0
}

// replaceWorkspaceRoot swaps a present workspace_root for the session's logical
// root. An absent root stays absent; a root with no derivable logical form is
// dropped rather than published.
func replaceWorkspaceRoot(logical string) func(map[string]json.RawMessage) error {
	return func(object map[string]json.RawMessage) error {
		if _, ok := object["workspace_root"]; !ok {
			return nil
		}
		if logical == "" {
			delete(object, "workspace_root")
			return nil
		}
		encoded, err := json.Marshal(logical)
		if err != nil {
			return err
		}
		object["workspace_root"] = encoded
		return nil
	}
}

// redactWorkspaceDrift drops old/new from every workspace-category drift change:
// both are physical roots, and two logical roots of one session would be equal, so
// the category and severity are all a viewer can be told.
func redactWorkspaceDrift(fields map[string]json.RawMessage) error {
	raw, ok := fields["drift"]
	if !ok {
		return nil
	}
	var changes []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &changes); err != nil {
		return err
	}
	workspace, err := json.Marshal(event.DriftWorkspace)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if string(change["category"]) == string(workspace) {
			delete(change, "old")
			delete(change, "new")
		}
	}
	encoded, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	fields["drift"] = encoded
	return nil
}

func editObject(encoded []byte, edit func(map[string]json.RawMessage) error) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	if err := edit(fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// editMember applies edit to the object at fields[name]; an absent member is left
// absent.
func editMember(fields map[string]json.RawMessage, name string, edit func(map[string]json.RawMessage) error) error {
	raw, ok := fields[name]
	if !ok {
		return nil
	}
	edited, err := editObject(raw, edit)
	if err != nil {
		return err
	}
	fields[name] = edited
	return nil
}
