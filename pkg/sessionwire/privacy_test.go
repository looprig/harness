package sessionwire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	model "github.com/looprig/inference/model"
)

// credentialBaseURL is a model gateway URL carrying a credential in every place
// an operator has been seen to put one: userinfo, a query parameter, a fragment,
// and a path token. No part of it may reach a public body.
const credentialBaseURL = "https://user:secret@gw.example/v1/tok-path?key=abc#frag"

var baseURLMarkers = []string{"user", "secret", "gw.example", "abc", "tok-path", "frag", `"base_url"`}

// physicalWorkspace is a Host filesystem path in both encodings the durable
// workspace_root field takes: a placement fingerprint and a caller-supplied root.
const (
	physicalPlacementRoot = "exclusive:/private/var/folders/host-7/session-workspaces/a273eac4"
	physicalCallerRoot    = "/Users/operator/src/private-repo"
)

var workspaceMarkers = []string{"/private/var", "host-7", "session-workspaces", "/Users/operator", "private-repo", "exclusive:"}

func privacyRuntime() event.ModelRuntime {
	return event.ModelRuntime{
		Key:       model.ModelKey{Provider: "gateway", Model: "model-a"},
		Limits:    model.ContextLimits{WindowTokens: 1000},
		Effort:    model.EffortLow,
		APIFormat: model.APIFormat("openai-chat"),
		BaseURL:   credentialBaseURL,
	}
}

func assertNoMarkers(t *testing.T, body []byte, markers []string) {
	t.Helper()
	for _, marker := range markers {
		if bytes.Contains(body, []byte(marker)) {
			t.Errorf("public body leaked %q: %s", marker, body)
		}
	}
}

func TestProjectLoopRuntimeOmitsBaseURL(t *testing.T) {
	t.Parallel()
	sid, lid := testUUID(1), testUUID(2)
	header := event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: testUUID(3)}
	runtime := privacyRuntime()
	tests := []struct {
		name  string
		value event.Event
	}{
		{"LoopStarted", event.LoopStarted{Header: header, Runtime: runtime}},
		{"LoopInferenceChanged", event.LoopInferenceChanged{Header: header, Runtime: runtime}},
		{"LoopModeChanged", event.LoopModeChanged{Header: header, Mode: "plan", Runtime: runtime}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Project("tenant-a", "public-session", test.value)
			if err != nil {
				t.Fatalf("Project() error = %v", err)
			}
			assertNoMarkers(t, got.Body, baseURLMarkers)
			// The public identity of the runtime survives: provider, model and
			// transport format remain for presentation.
			for _, marker := range []string{`"type":"` + test.name + `"`, `"Provider":"gateway"`, `"Model":"model-a"`, `"api_format":"openai-chat"`} {
				if !bytes.Contains(got.Body, []byte(marker)) {
					t.Errorf("body missing %s: %s", marker, got.Body)
				}
			}
			// The native (private, replay) encoding is untouched: restore still
			// reads the declared transport from the runtime body.
			native, err := event.MarshalEvent(test.value)
			if err != nil {
				t.Fatalf("MarshalEvent() error = %v", err)
			}
			if !bytes.Contains(native, []byte(`"base_url":"`)) {
				t.Errorf("native body lost base_url; restore depends on it: %s", native)
			}
		})
	}
}

func TestProjectSessionStartedPublishesLogicalWorkspaceRoot(t *testing.T) {
	t.Parallel()
	sid := testUUID(9)
	logical := "/sessions/" + sid.String() + "/workspace"
	for _, physical := range []string{physicalPlacementRoot, physicalCallerRoot} {
		t.Run(physical, func(t *testing.T) {
			t.Parallel()
			manifest := event.ConfigManifest{SchemaVersion: event.ManifestSchemaVersion, ModelID: "model-a", WorkspaceRoot: physical}
			ev := event.SessionStarted{
				Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: testUUID(10)},
				Config:   event.ConfigFingerprint{ModelID: "model-a", WorkspaceRoot: physical},
				Manifest: manifest,
			}
			got, err := Project("tenant-a", "public-session", ev)
			if err != nil {
				t.Fatalf("Project() error = %v", err)
			}
			assertNoMarkers(t, got.Body, workspaceMarkers)
			var body struct {
				Config struct {
					WorkspaceRoot string `json:"workspace_root"`
				} `json:"config"`
				Manifest struct {
					WorkspaceRoot string `json:"workspace_root"`
					ModelID       string `json:"model_id"`
				} `json:"manifest"`
			}
			if err := json.Unmarshal(got.Body, &body); err != nil {
				t.Fatalf("decode public body: %v", err)
			}
			if body.Config.WorkspaceRoot != logical || body.Manifest.WorkspaceRoot != logical {
				t.Errorf("public workspace_root = (config %q, manifest %q), want logical root %q", body.Config.WorkspaceRoot, body.Manifest.WorkspaceRoot, logical)
			}
			if body.Manifest.ModelID != "model-a" {
				t.Errorf("manifest model_id = %q, want preserved", body.Manifest.ModelID)
			}
			native, err := event.MarshalEvent(ev)
			if err != nil {
				t.Fatalf("MarshalEvent() error = %v", err)
			}
			if !bytes.Contains(native, []byte(physical)) {
				t.Errorf("native body lost the physical root restore compares: %s", native)
			}
		})
	}
}

func TestProjectSessionStartedWithoutWorkspaceRootStaysAbsent(t *testing.T) {
	t.Parallel()
	ev := event.SessionStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: testUUID(11)}, EventID: testUUID(12)},
		Config: event.ConfigFingerprint{ModelID: "model-a"},
	}
	got, err := Project("tenant-a", "public-session", ev)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if bytes.Contains(got.Body, []byte("workspace_root")) {
		t.Errorf("an absent root must stay absent, not become a logical root: %s", got.Body)
	}
	if bytes.Contains(got.Body, []byte(`"manifest"`)) {
		t.Errorf("a zero manifest must stay omitted: %s", got.Body)
	}
}

func TestProjectConfigurationAdoptedRedactsWorkspacePaths(t *testing.T) {
	t.Parallel()
	sid := testUUID(13)
	logical := "/sessions/" + sid.String() + "/workspace"
	manifest := event.ConfigManifest{SchemaVersion: event.ManifestSchemaVersion, ModelID: "model-a", WorkspaceRoot: physicalCallerRoot}
	ev := event.ConfigurationAdopted{
		Header:             event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: testUUID(14)},
		Epoch:              2,
		AdoptedFingerprint: manifest.Fingerprint(),
		Manifest:           manifest,
		Drift: []event.DriftChange{
			{Category: event.DriftWorkspace, Old: physicalPlacementRoot, New: physicalCallerRoot, Severity: event.DriftWarn},
			{Category: event.DriftModel, Old: "model-z", New: "model-a", Severity: event.DriftInfo},
		},
		Source: event.DecisionSourcePolicy,
	}
	got, err := Project("tenant-a", "public-session", ev)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	assertNoMarkers(t, got.Body, workspaceMarkers)
	var body struct {
		Manifest struct {
			WorkspaceRoot string `json:"workspace_root"`
		} `json:"manifest"`
		Drift []event.DriftChange `json:"drift"`
	}
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("decode public body: %v", err)
	}
	if body.Manifest.WorkspaceRoot != logical {
		t.Errorf("manifest workspace_root = %q, want %q", body.Manifest.WorkspaceRoot, logical)
	}
	want := []event.DriftChange{
		{Category: event.DriftWorkspace, Severity: event.DriftWarn},
		{Category: event.DriftModel, Old: "model-z", New: "model-a", Severity: event.DriftInfo},
	}
	if len(body.Drift) != len(want) {
		t.Fatalf("drift = %+v, want %+v", body.Drift, want)
	}
	for i := range want {
		if body.Drift[i] != want[i] {
			t.Errorf("drift[%d] = %+v, want %+v", i, body.Drift[i], want[i])
		}
	}
}

// TestPublicBodiesCarryNoEndpointOrHostPath is the public-body denylist: every
// public event that can carry a model endpoint or a Host path, built with a
// credential-bearing endpoint and a physical path, projects to a body naming
// neither.
func TestPublicBodiesCarryNoEndpointOrHostPath(t *testing.T) {
	t.Parallel()
	sid, lid := testUUID(20), testUUID(21)
	loopHeader := event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: testUUID(22)}
	sessionHeader := event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: testUUID(23)}
	manifest := event.ConfigManifest{SchemaVersion: event.ManifestSchemaVersion, WorkspaceRoot: physicalPlacementRoot}
	values := []event.Event{
		event.LoopStarted{Header: loopHeader, Runtime: privacyRuntime()},
		event.LoopInferenceChanged{Header: loopHeader, Runtime: privacyRuntime()},
		event.LoopModeChanged{Header: loopHeader, Runtime: privacyRuntime()},
		event.SessionStarted{Header: sessionHeader, Config: event.ConfigFingerprint{WorkspaceRoot: physicalPlacementRoot}, Manifest: manifest},
		event.ConfigurationAdopted{
			Header: sessionHeader, Epoch: 3, AdoptedFingerprint: manifest.Fingerprint(), Manifest: manifest,
			Source: event.DecisionSourceOperator,
			Drift:  []event.DriftChange{{Category: event.DriftWorkspace, Old: physicalCallerRoot, New: physicalPlacementRoot, Severity: event.DriftWarn}},
		},
	}
	markers := append(append([]string{}, baseURLMarkers...), workspaceMarkers...)
	for _, value := range values {
		got, err := Project("tenant-a", "public-session", value)
		if err != nil {
			t.Fatalf("Project(%T) error = %v", value, err)
		}
		for _, marker := range markers {
			if strings.Contains(string(got.Body), marker) {
				t.Errorf("Project(%T) leaked %q: %s", value, marker, got.Body)
			}
		}
	}
}

// TestLogicalWorkspaceRootIgnoresZeroSession pins that a session with no identity
// derives no logical root (mirrors the runtime's derivation).
func TestLogicalWorkspaceRootIgnoresZeroSession(t *testing.T) {
	t.Parallel()
	if got := publicWorkspaceRoot(uuid.UUID{}); got != "" {
		t.Errorf("publicWorkspaceRoot(zero) = %q, want empty", got)
	}
	sid := testUUID(30)
	if got, want := publicWorkspaceRoot(sid), "/sessions/"+sid.String()+"/workspace"; got != want {
		t.Errorf("publicWorkspaceRoot = %q, want %q", got, want)
	}
}

// TestReplaceWorkspaceRootDropsRootWithNoLogicalForm pins the fail-closed arm: a
// root that has no derivable logical form is dropped, never published as-is.
// (Unreachable through Project today — every session-scoped event validates a
// non-zero session id — which is exactly why it is pinned directly.)
func TestReplaceWorkspaceRootDropsRootWithNoLogicalForm(t *testing.T) {
	t.Parallel()
	object := map[string]json.RawMessage{"workspace_root": json.RawMessage(`"/Users/operator/src"`), "model_id": json.RawMessage(`"m"`)}
	if err := replaceWorkspaceRoot("")(object); err != nil {
		t.Fatalf("replaceWorkspaceRoot: %v", err)
	}
	if _, ok := object["workspace_root"]; ok {
		t.Errorf("workspace_root kept with no logical root: %s", object["workspace_root"])
	}
	if string(object["model_id"]) != `"m"` {
		t.Errorf("unrelated member disturbed: %v", object)
	}
}
