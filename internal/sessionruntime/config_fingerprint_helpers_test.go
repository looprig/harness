package sessionruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
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

// testFingerprintForDefinition lets focused restore tests model the composition
// root's topology identity: the immutable loop policy revision is fingerprinted,
// while raw structured-output descriptions and schemas never enter
// ConfigFingerprint. StructuredOutputRevision is included only when output is
// configured, preserving compatibility for definitions without this feature.
func testFingerprintForDefinition(definition loop.Definition) FingerprintProvider {
	policyRevision := definition.PolicyRevision()
	return func(bound loop.BoundDefinition) event.ConfigFingerprint {
		fingerprint := testFingerprintProvider(bound)
		structuredRevision := ""
		if _, configured := bound.OutputSchema(); configured {
			structuredRevision = inference.StructuredOutputRevision
		}
		fingerprint.TopologyRev = testFingerprintHash(policyRevision + "\x00" + structuredRevision)
		return fingerprint
	}
}

func testDefinitionPolicyRevision(definition loop.Definition, structuredRevision string) string {
	return testFingerprintHash(definition.PolicyRevision() + "\x00" + structuredRevision)
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

func TestSessionFingerprintHelpersRejectStructuredOutputPolicyDrift(t *testing.T) {
	output := inference.OutputSchema{
		Name:        "session_result",
		Description: "Return the session result containing policy-secret",
		Schema:      json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
		Strict:      true,
	}
	define := func(output *inference.OutputSchema) loop.Definition {
		options := []loop.Option{
			loop.WithName("agent"),
			loop.WithInference(&stubLLM{}, validModel("model-x")),
			loop.WithSystem("be helpful"),
		}
		if output != nil {
			options = append(options, loop.WithOutputSchema(*output))
		}
		return mustDefine(options...)
	}

	baseDefinition := define(&output)
	baseFingerprint := testFingerprintForDefinition(baseDefinition)(bindFingerprintDefinition(baseDefinition))
	if strings.Contains(fmt.Sprintf("%+v", baseFingerprint), "policy-secret") {
		t.Fatalf("fingerprint retained raw output policy: %+v", baseFingerprint)
	}
	if baseFingerprint.TopologyRev == "" {
		t.Fatal("helper fingerprint omitted loop policy revision")
	}
	if current, future := testDefinitionPolicyRevision(baseDefinition, inference.StructuredOutputRevision), testDefinitionPolicyRevision(baseDefinition, "structured-output/future"); current == future {
		t.Fatal("helper fingerprint ignored StructuredOutputRevision drift")
	}

	absentA, absentB := define(nil), define(nil)
	if left, right := testFingerprintForDefinition(absentA)(bindFingerprintDefinition(absentA)), testFingerprintForDefinition(absentB)(bindFingerprintDefinition(absentB)); !left.Equal(right) {
		t.Fatalf("equivalent absent output policies differ: %+v / %+v", left, right)
	}

	drifts := []struct {
		name   string
		mutate func(*inference.OutputSchema)
	}{
		{name: "name", mutate: func(value *inference.OutputSchema) { value.Name = "session_result_v2" }},
		{name: "description", mutate: func(value *inference.OutputSchema) { value.Description = "different description" }},
		{name: "schema", mutate: func(value *inference.OutputSchema) {
			value.Schema = json.RawMessage(`{"type":"object","properties":{"allowed":{"type":"boolean"}},"required":["allowed"],"additionalProperties":false}`)
		}},
		{name: "strict", mutate: func(value *inference.OutputSchema) { value.Strict = false }},
	}
	for _, drift := range drifts {
		t.Run(drift.name, func(t *testing.T) {
			store := newRestoreStore(t)
			originalLifecycle, err := newTestLifecycle(baseDefinition, store, WithLifecycleFingerprintProvider(testFingerprintForDefinition(baseDefinition)))
			if err != nil {
				t.Fatalf("NewTopologyLifecycle(original): %v", err)
			}
			original, err := originalLifecycle.NewSession(context.Background(), "")
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			sessionID := original.SessionID()
			if err := original.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}

			changedOutput := output.Clone()
			drift.mutate(&changedOutput)
			changedDefinition := define(&changedOutput)
			restoreLifecycle, err := newTestLifecycle(changedDefinition, store, WithLifecycleFingerprintProvider(testFingerprintForDefinition(changedDefinition)))
			if err != nil {
				t.Fatalf("NewTopologyLifecycle(restore): %v", err)
			}
			restored, err := restoreLifecycle.RestoreSession(context.Background(), sessionID)
			if restored != nil {
				t.Fatal("RestoreSession returned a session after output policy drift")
			}
			var mismatch *ConfigMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("RestoreSession error = %T %v, want ConfigMismatchError", err, err)
			}
		})
	}
}
