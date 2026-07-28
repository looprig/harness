package hustle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/tool"
)

const (
	maxEvidenceToolDescriptionBytes = 1 << 20
	maxEvidenceToolSchemaBytes      = 1 << 20
)

// BoundEvidenceTool is an immutable, fingerprinted evidence capability. Its
// model-facing metadata is frozen separately from the concrete execution tool
// so optional capabilities on that tool remain available to the runtime.
type BoundEvidenceTool struct{ state *boundEvidenceToolState }

type boundEvidenceToolState struct {
	tool              tool.InvokableTool
	info              tool.ToolInfo
	name              string
	descriptionSHA256 [sha256.Size]byte
	schemaSHA256      [sha256.Size]byte
	identitySHA256    [sha256.Size]byte
}

func (b BoundEvidenceTool) Name() string {
	if b.state == nil {
		return ""
	}
	return b.state.name
}

func (b BoundEvidenceTool) DescriptionSHA256() [sha256.Size]byte {
	if b.state == nil {
		return [sha256.Size]byte{}
	}
	return b.state.descriptionSHA256
}

func (b BoundEvidenceTool) SchemaSHA256() [sha256.Size]byte {
	if b.state == nil {
		return [sha256.Size]byte{}
	}
	return b.state.schemaSHA256
}

func (b BoundEvidenceTool) IdentitySHA256() [sha256.Size]byte {
	if b.state == nil {
		return [sha256.Size]byte{}
	}
	return b.state.identitySHA256
}

func (b BoundEvidenceTool) Tool() tool.InvokableTool {
	if b.state == nil {
		return nil
	}
	return b.state.tool
}

// Info returns a defensive copy of the exact metadata frozen at bind time.
// Execution uses Tool so optional capabilities on the concrete tool are
// preserved; runtimes must use this accessor for model-facing metadata.
func (b BoundEvidenceTool) Info() *tool.ToolInfo {
	if b.state == nil {
		return nil
	}
	clone := b.state.info
	clone.Schema = append(json.RawMessage(nil), b.state.info.Schema...)
	return &clone
}

func bindEvidenceTools(
	ctx context.Context,
	policy EvidenceToolPolicy,
	bindings Bindings,
) ([]BoundEvidenceTool, error) {
	if !evidencePolicyEnabled(policy) {
		return nil, nil
	}
	if bindings.SessionID.IsZero() || bindings.LoopID.IsZero() {
		return nil, invalidEvidenceBind(nil)
	}
	built := make([]tool.InvokableTool, 0)
	declared := make([]string, 0)
	for _, definition := range policy.Definitions {
		names := definition.ProducedToolNames()
		declared = append(declared, names...)
		toolBindings := tool.Bindings{
			SessionID: bindings.SessionID,
			LoopID:    bindings.LoopID,
		}
		if definition.Requirements()&tool.RequiresWorkspace != 0 {
			toolBindings.Workspace = bindings.Workspace
		}
		tools, err := definition.Build(ctx, toolBindings)
		if err != nil {
			return nil, invalidEvidenceBind(err)
		}
		built = append(built, tools...)
	}
	if len(built) != len(declared) {
		return nil, invalidEvidenceBind(nil)
	}
	result := make([]BoundEvidenceTool, len(built))
	seen := make(map[string]struct{}, len(built))
	for index, concrete := range built {
		if nilEvidenceTool(concrete) {
			return nil, invalidEvidenceBind(nil)
		}
		info, err := concrete.Info(ctx)
		if err != nil || info == nil {
			return nil, invalidEvidenceBind(err)
		}
		canonicalSchema, err := validateEvidenceToolInfo(*info)
		if err != nil {
			return nil, invalidEvidenceBind(err)
		}
		confirmed, err := concrete.Info(ctx)
		if err != nil || confirmed == nil {
			return nil, invalidEvidenceBind(err)
		}
		confirmedSchema, err := validateEvidenceToolInfo(*confirmed)
		if err != nil ||
			info.Name != confirmed.Name ||
			info.Desc != confirmed.Desc ||
			!bytes.Equal(canonicalSchema, confirmedSchema) {
			return nil, invalidEvidenceBind(err)
		}
		if info.Name != declared[index] || !canonicalEvidenceToolName(info.Name) {
			return nil, invalidEvidenceBind(nil)
		}
		if _, duplicate := seen[info.Name]; duplicate {
			return nil, invalidEvidenceBind(nil)
		}
		seen[info.Name] = struct{}{}
		frozenInfo := tool.ToolInfo{
			Name: info.Name, Desc: info.Desc,
			Schema: append(json.RawMessage(nil), canonicalSchema...),
		}
		descriptionDigest := sha256.Sum256([]byte(info.Desc))
		schemaDigest := sha256.Sum256(canonicalSchema)
		identity, err := digestBoundEvidenceTool(info.Name, info.Desc, canonicalSchema)
		if err != nil {
			return nil, invalidEvidenceBind(err)
		}
		result[index] = BoundEvidenceTool{state: &boundEvidenceToolState{
			tool: concrete, info: frozenInfo, name: info.Name,
			descriptionSHA256: descriptionDigest,
			schemaSHA256:      schemaDigest,
			identitySHA256:    identity,
		}}
	}
	return result, nil
}

func validateEvidenceToolInfo(info tool.ToolInfo) ([]byte, error) {
	if !canonicalEvidenceToolName(info.Name) ||
		!utf8.ValidString(info.Desc) ||
		info.Desc != strings.TrimSpace(info.Desc) ||
		info.Desc == "" ||
		strings.ContainsRune(info.Desc, '\x00') ||
		len(info.Desc) > maxEvidenceToolDescriptionBytes ||
		len(info.Schema) == 0 ||
		len(info.Schema) > maxEvidenceToolSchemaBytes ||
		!utf8.Valid(info.Schema) {
		return nil, invalidEvidenceBind(nil)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &object); err != nil || object == nil {
		return nil, invalidEvidenceBind(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, info.Schema); err != nil {
		return nil, err
	}
	return append([]byte(nil), compact.Bytes()...), nil
}

func digestBoundEvidenceTool(name, description string, schema []byte) ([sha256.Size]byte, error) {
	projection := struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
	}{Name: name, Description: description, Schema: schema}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func nilEvidenceTool(value tool.InvokableTool) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func invalidEvidenceBind(cause error) error {
	return &BindError{Kind: BindInvalidEvidenceTools, Cause: cause}
}
