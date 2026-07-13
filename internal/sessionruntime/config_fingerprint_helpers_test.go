package sessionruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
)

func newTestSession(ctx context.Context, definition loop.Definition, options ...Option) (*Session, error) {
	resolved := []Option{WithFingerprintProvider(testFingerprintProvider)}
	resolved = append(resolved, options...)
	return New(ctx, definition, resolved...)
}

func restoreTestSession(ctx context.Context, definition loop.Definition, id uuid.UUID, store *sessionstore.Store, options ...Option) (*Session, error) {
	resolved := []Option{WithFingerprintProvider(testFingerprintProvider)}
	resolved = append(resolved, options...)
	return RestoreTopology(ctx, singleDefinitionTopology(definition), id, store, resolved...)
}

func newTestLifecycle(definition loop.Definition, store *sessionstore.Store, options ...LifecycleOption) (*Lifecycle, error) {
	resolved := []LifecycleOption{WithLifecycleFingerprintProvider(testFingerprintProvider)}
	resolved = append(resolved, options...)
	return NewTopologyLifecycle(singleDefinitionTopology(definition), store, resolved...)
}

func singleDefinitionTopology(definition loop.Definition) Topology {
	return Topology{Definitions: []loop.Definition{definition}, Primers: []identity.AgentName{definition.Name()}, ActivePrimer: definition.Name()}
}

type testFingerprintFields struct {
	AgentKind                 string
	RuntimeSkills             bool
	WorkspaceRoot             string
	AdapterID                 string
	Posture                   string
	NativePermissionPolicyRev string
}

func testFingerprintProvider(definition loop.BoundDefinition) event.ConfigFingerprint {
	names := make([]string, 0, len(definition.Tools()))
	for _, candidate := range definition.Tools() {
		info, err := candidate.Info(context.Background())
		if err == nil && info != nil {
			names = append(names, info.Name)
		}
	}
	sort.Strings(names)
	return event.ConfigFingerprint{
		ModelID:         definition.Model().Name,
		SystemPromptRev: testFingerprintHash(definition.EffectiveSystem()),
		ToolPolicyRev:   testFingerprintHash(strings.Join(names, "\n")),
	}
}

func testFingerprintWith(definition loop.BoundDefinition, fields testFingerprintFields) event.ConfigFingerprint {
	fingerprint := testFingerprintProvider(definition)
	fingerprint.AgentKind = fields.AgentKind
	fingerprint.RuntimeSkills = fields.RuntimeSkills
	fingerprint.WorkspaceRoot = fields.WorkspaceRoot
	fingerprint.AgentAdapter = fields.AdapterID
	fingerprint.PermissionPosture = fields.Posture
	fingerprint.NativePermissionPolicyRev = fields.NativePermissionPolicyRev
	return fingerprint
}

func testFingerprintHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func fingerprintFromDefinition(definition loop.Definition) event.ConfigFingerprint {
	return testFingerprintProvider(bindFingerprintDefinition(definition))
}

func testFingerprintFromDefinitionWithFields(definition loop.Definition, fields testFingerprintFields) event.ConfigFingerprint {
	return testFingerprintWith(bindFingerprintDefinition(definition), fields)
}

func withTestFingerprintFields(fields testFingerprintFields) Option {
	return WithFingerprintProvider(func(definition loop.BoundDefinition) event.ConfigFingerprint {
		return testFingerprintWith(definition, fields)
	})
}

func bindFingerprintDefinition(definition loop.Definition) loop.BoundDefinition {
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: sessionID, LoopID: loopID})
	if err != nil {
		panic(err)
	}
	return bound
}
