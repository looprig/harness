package codex

import (
	"reflect"
	"testing"
)

func TestWhitelistEnvIncludesOnlyAllowedParentKeysPlusSortedCredentials(t *testing.T) {
	t.Parallel()
	parent := []string{
		"SECRET_TOKEN=drop",
		"TERM=xterm-256color",
		"PATH=/usr/bin",
		"MALFORMED",
		"HOME=/home/runner",
	}
	cfg := SpecConfig{
		ExecPath: "/usr/local/bin/codex",
		Cwd:      "/work/repo",
		EnvAllow: []string{"PATH", "HOME"},
		Credential: map[string]string{
			"ZZZ_TOKEN":     "last",
			"CODEX_API_KEY": "first",
		},
	}

	spec, err := NewSpec(parent, cfg)
	if err != nil {
		t.Fatalf("NewSpec() error = %v", err)
	}
	want := []string{
		"CODEX_API_KEY=first",
		"HOME=/home/runner",
		"PATH=/usr/bin",
		"ZZZ_TOKEN=last",
	}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Fatalf("spec.Env = %v, want %v", spec.Env, want)
	}
}
