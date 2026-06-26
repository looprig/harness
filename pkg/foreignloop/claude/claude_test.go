package claude

import (
	"context"
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/foreignloop"
)

// Agent must satisfy the foreign-agent port.
var _ foreignloop.ForeignAgent = (*Agent)(nil)

func TestAgentSpawnErrorPaths(t *testing.T) {
	t.Parallel()
	const sid = "11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name       string
		agent      *Agent
		wantConfig bool
		wantSpawn  bool
	}{
		{
			name:       "empty exec path fails closed with config error",
			agent:      &Agent{},
			wantConfig: true,
		},
		{
			name:      "bogus exec path surfaces a spawn error",
			agent:     &Agent{ExecPath: "/nonexistent/claude-binary-xyz-not-here", Model: "small"},
			wantSpawn: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			turn := foreignloop.ForeignTurn{ForeignSID: sid, StartNew: true}
			s, err := tt.agent.Spawn(context.Background(), turn)
			if err == nil {
				if s != nil {
					_ = s.Close()
				}
				t.Fatalf("Spawn() err = nil, want error")
			}
			if tt.wantConfig {
				var ce *SpawnConfigError
				if !errors.As(err, &ce) {
					t.Fatalf("Spawn() err = %v, want *SpawnConfigError", err)
				}
			}
			if tt.wantSpawn {
				var se *foreignloop.SpawnError
				if !errors.As(err, &se) {
					t.Fatalf("Spawn() err = %v, want *foreignloop.SpawnError", err)
				}
			}
		})
	}
}
