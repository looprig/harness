package swe

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
)

// TestModelFactorySystemPrompt proves the ModelFactory bakes the caller's system
// prompt verbatim into the returned spec's System field (and carries the model's
// provider/model identity), so each agent's full prompt reaches its loop unchanged.
func TestModelFactorySystemPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		system string
	}{
		{name: "non-empty role prompt", system: "<identity/><role name=\"x\"/>"},
		{name: "empty prompt", system: ""},
		{name: "whitespace prompt", system: "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			factory := newModelFactory("test-key")
			spec := factory(tt.system)

			if spec.System != tt.system {
				t.Errorf("spec.System = %q, want %q", spec.System, tt.system)
			}
			if spec.Provider != model.Provider {
				t.Errorf("spec.Provider = %q, want %q", spec.Provider, model.Provider)
			}
			if spec.Model != model.Name {
				t.Errorf("spec.Model = %q, want %q", spec.Model, model.Name)
			}
			if spec.APIKey != "test-key" {
				t.Errorf("spec.APIKey = %q, want %q", spec.APIKey, "test-key")
			}
		})
	}
}

// TestReadAPIKeyMissing proves readAPIKey fails loud with a typed *MissingEnvError
// when the model's provider requires a key and none is set, and that an explicit
// key (set via t.Setenv) is read verbatim. The whitespace-only case is treated as
// missing — a boundary check, so the failure is loud at startup, not deferred.
func TestReadAPIKey(t *testing.T) {
	// Not parallel: mutates the process environment via t.Setenv.
	tests := []struct {
		name    string
		set     bool
		value   string
		want    string
		wantErr bool
	}{
		{name: "key present", set: true, value: "secret-key", want: "secret-key"},
		{name: "key unset", set: false, wantErr: true},
		{name: "key empty", set: true, value: "", wantErr: true},
		{name: "key whitespace only", set: true, value: "   ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(envAPIKey, tt.value)
			} else {
				t.Setenv(envAPIKey, "")
			}

			got, err := readAPIKey()
			if (err != nil) != tt.wantErr {
				t.Fatalf("readAPIKey() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var me *MissingEnvError
				if !errors.As(err, &me) {
					t.Fatalf("readAPIKey() error = %v, want *MissingEnvError", err)
				}
				if me.Var != envAPIKey {
					t.Errorf("MissingEnvError.Var = %q, want %q", me.Var, envAPIKey)
				}
				return
			}
			if got != tt.want {
				t.Errorf("readAPIKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildClientFailsLoudOnMissingKey proves buildClient (the env+provider
// boundary) refuses to build a client when the required key is absent, returning
// the typed *MissingEnvError and a nil client + nil factory (fail secure).
func TestBuildClientFailsLoudOnMissingKey(t *testing.T) {
	t.Setenv(envAPIKey, "")

	client, factory, err := buildClient()
	if client != nil {
		t.Errorf("buildClient() client = %v, want nil", client)
	}
	if factory != nil {
		t.Errorf("buildClient() factory = %v, want nil", factory)
	}
	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("buildClient() error = %v, want *MissingEnvError", err)
	}
}

// TestBuildClientHappy proves buildClient builds a non-nil client + factory when
// the key is present, and that the factory it returns bakes a system prompt in.
func TestBuildClientHappy(t *testing.T) {
	t.Setenv(envAPIKey, "secret-key")

	client, factory, err := buildClient()
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("buildClient() returned nil client")
	}
	if factory == nil {
		t.Fatal("buildClient() returned nil factory")
	}
	const sys = "<role/>"
	if spec := factory(sys); spec.System != sys {
		t.Errorf("factory(%q).System = %q, want %q", sys, spec.System, sys)
	}
}

// ensure llm is referenced even if a future refactor drops a direct use.
var _ = llm.ProviderChutes
