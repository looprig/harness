package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
)

// sessionEvidenceAccessStub is a minimal gate.EvidenceAccessEvaluator test
// double: sessionruntime never evaluates it itself, it only forwards it into
// hustleruntime.RuntimeConfig.Evidence at construction.
type sessionEvidenceAccessStub struct{}

func (sessionEvidenceAccessStub) AccessFor(tool.Requirement) (uint8, error) {
	return gate.AccessAllow, nil
}

// sessionEvidenceContainmentStub is a minimal gate.EvidenceContainmentVerifier
// test double, mirroring sessionEvidenceAccessStub's role.
type sessionEvidenceContainmentStub struct{}

func (sessionEvidenceContainmentStub) VerifyEvidenceContainment(context.Context, gate.EvidenceContainmentPolicy, tool.Request) error {
	return nil
}

// newHustleBindableSession builds the minimal *Session collaborator set
// bindSessionHustles needs (factory, hub, sessionCtx) plus the fields under
// test, mirroring TestBindSessionHustlesIsSingleTransactionalConstruction's
// construction shape.
func newHustleBindableSession(t *testing.T, definitions []hustle.Definition, workspaceRoot string) *Session {
	t.Helper()
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	factory := event.NewFactory(uuid.New, time.Now)
	return &Session{
		sessionID: sessionID, sessionCtx: ctx, sessionCancel: cancel,
		factory: factory, hub: hub.New(sessionID, hub.WithFactory(factory)),
		hustleDefinitions: append([]hustle.Definition(nil), definitions...),
		hustleLimits:      testHustleLimits(), loops: make(map[uuid.UUID]*loopHandle),
		wsRoot: workspaceRoot,
	}
}

// TestBindSessionHustlesWiresEvidenceRuntimeFromSessionWorkspaceAndOption
// proves the fix for the confirmed Task 23 hard blocker: a session whose
// hustles need evidence tools no longer fails 100% of the time at
// construction. It proves newHustleController auto-derives ReadWorkspace
// from the session's own s.wsRoot (never asking the consumer for it) and
// forwards the consumer-supplied Access/Containment/AllowedKinds installed
// by withPermissionReviewEvidence, together sufficient for construction to
// succeed.
func TestBindSessionHustlesWiresEvidenceRuntimeFromSessionWorkspaceAndOption(t *testing.T) {
	t.Parallel()
	definition := sessionEvidenceDefinition(t, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&sessionEvidenceTool{infos: []*tool.ToolInfo{{
			Name: "workspace-status", Desc: "read workspace status",
			Schema: []byte(`{"type":"object","properties":{},"additionalProperties":false}`),
		}}}}, nil
	})
	s := newHustleBindableSession(t, []hustle.Definition{definition}, "/managed/workspace")
	withPermissionReviewEvidence(
		sessionEvidenceAccessStub{}, sessionEvidenceContainmentStub{}, []string{"filesystem.read"},
	)(s)

	if err := s.bindSessionHustles(); err != nil {
		t.Fatalf("bindSessionHustles() error = %v, want evidence runtime wired from session workspace + option", err)
	}
	if s.hustleController == nil {
		t.Fatal("bindSessionHustles did not construct the controller")
	}
}

// TestBindSessionHustlesFailsClosedWhenEvidenceNeededButNeverConfigured
// proves the fail-closed half of the same fix: when a registered hustle
// needs evidence tools but withPermissionReviewEvidence was never applied
// (the pre-fix default for every session), construction still fails —
// exactly the hard blocker's symptom — rather than silently proceeding with
// a permissive default access/containment pair.
func TestBindSessionHustlesFailsClosedWhenEvidenceNeededButNeverConfigured(t *testing.T) {
	t.Parallel()
	definition := sessionEvidenceDefinition(t, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&sessionEvidenceTool{infos: []*tool.ToolInfo{{
			Name: "workspace-status", Desc: "read workspace status",
			Schema: []byte(`{"type":"object","properties":{},"additionalProperties":false}`),
		}}}}, nil
	})
	s := newHustleBindableSession(t, []hustle.Definition{definition}, "/managed/workspace")

	err := s.bindSessionHustles()
	var construction *HustleConstructionError
	if !errors.As(err, &construction) || construction.Reason != HustleConstructionRuntimeFailed {
		t.Fatalf("bindSessionHustles() error = %T %v, want HustleConstructionRuntimeFailed", err, err)
	}
	var configErr *hustleruntime.ConfigError
	if !errors.As(err, &configErr) || configErr.Field != "runtime.evidence" {
		t.Fatalf("bindSessionHustles() cause = %v, want runtime.evidence missing-collaborator", err)
	}
	if s.hustleController != nil {
		t.Fatal("failed construction retained a partial controller")
	}
}
