package foreign_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

func registryBuilder(marker string) foreign.Builder {
	return func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		loop.Provenance,
		foreign.EventPublisher,
		loop.BoundDefinition,
		func() (uuid.UUID, error),
		*event.Factory,
	) (loop.Backend, string, error) {
		return nil, marker, nil
	}
}

func registryRestoredBuilder(marker error) foreign.RestoredBuilder {
	return func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
		loop.Provenance,
		foreign.EventPublisher,
		loop.BoundDefinition,
		func() (uuid.UUID, error),
		*event.Factory,
		foreign.RestoredForeign,
	) (loop.Backend, error) {
		return nil, marker
	}
}

func TestBuilderRegistry(t *testing.T) {
	t.Parallel()

	restoredMarker := errors.New("restored marker")
	live := registryBuilder("live")
	restored := registryRestoredBuilder(restoredMarker)

	tests := []struct {
		name              string
		profile           loop.RuntimeProfileName
		register          func(*foreign.BuilderRegistry) error
		wantLive          string
		wantRestored      error
		wantRegisterError func(*testing.T, error)
		wantLookupError   func(*testing.T, error)
	}{
		{
			name:    "happy path",
			profile: "acp-claude",
			register: func(r *foreign.BuilderRegistry) error {
				return r.Register("acp-claude", live, restored)
			},
			wantLive:     "live",
			wantRestored: restoredMarker,
		},
		{
			name:    "duplicate profile",
			profile: "acp-claude",
			register: func(r *foreign.BuilderRegistry) error {
				if err := r.Register("acp-claude", live, restored); err != nil {
					return err
				}
				return r.Register("acp-claude", registryBuilder("replacement"), registryRestoredBuilder(nil))
			},
			wantRegisterError: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("duplicate Register() error = nil")
				}
				if strings.Contains(err.Error(), "acp-claude") {
					t.Fatalf("duplicate Register() leaked profile: %q", err)
				}
			},
		},
		{
			name:    "empty profile",
			profile: "empty-profile",
			register: func(r *foreign.BuilderRegistry) error {
				return r.Register("", live, restored)
			},
			wantRegisterError: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("empty Register() error = nil")
				}
			},
		},
		{
			name:    "nil live builder",
			profile: "nil-live",
			register: func(r *foreign.BuilderRegistry) error {
				return r.Register("nil-live", nil, restored)
			},
			wantRegisterError: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("nil live Register() error = nil")
				}
			},
		},
		{
			name:    "nil restored builder",
			profile: "nil-restored",
			register: func(r *foreign.BuilderRegistry) error {
				return r.Register("nil-restored", live, nil)
			},
			wantRegisterError: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("nil restored Register() error = nil")
				}
			},
		},
		{
			name:    "unknown profile",
			profile: "unknown-secret-profile-token",
			register: func(r *foreign.BuilderRegistry) error {
				return r.Register("acp-claude", live, restored)
			},
			wantLookupError: func(t *testing.T, err error) {
				var unknown *foreign.UnknownProfileError
				if !errors.As(err, &unknown) {
					t.Fatalf("Builder() error = %v, want *UnknownProfileError", err)
				}
				if strings.Contains(err.Error(), "unknown-secret-profile-token") {
					t.Fatalf("unknown Builder() leaked profile: %q", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var registry foreign.BuilderRegistry
			if err := tt.register(&registry); tt.wantRegisterError != nil {
				tt.wantRegisterError(t, err)
				return
			} else if err != nil {
				t.Fatalf("Register() error = %v", err)
			}

			gotLive, gotRestored, err := registry.Builder(tt.profile)
			if tt.wantLookupError != nil {
				tt.wantLookupError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("Builder() error = %v", err)
			}
			if gotLive == nil || gotRestored == nil {
				t.Fatal("Builder() returned a nil builder pair")
			}

			_, gotSID, err := gotLive(context.Background(), uuid.UUID{}, uuid.UUID{}, loop.Provenance{}, nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("live builder error = %v", err)
			}
			if gotSID != tt.wantLive {
				t.Fatalf("live builder sid = %q, want %q", gotSID, tt.wantLive)
			}
			if _, err := gotRestored(context.Background(), uuid.UUID{}, uuid.UUID{}, loop.Provenance{}, nil, nil, nil, nil, foreign.RestoredForeign{}); !errors.Is(err, tt.wantRestored) {
				t.Fatalf("restored builder error = %v, want %v", err, tt.wantRestored)
			}
		})
	}
}

func TestRestoredForeignZeroValueRemainsBackwardCompatible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		seed foreign.RestoredForeign
	}{
		{name: "zero value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var received foreign.RestoredForeign
			restored := foreign.RestoredBuilder(func(
				context.Context,
				uuid.UUID,
				uuid.UUID,
				loop.Provenance,
				foreign.EventPublisher,
				loop.BoundDefinition,
				func() (uuid.UUID, error),
				*event.Factory,
				foreign.RestoredForeign,
			) (loop.Backend, error) {
				received = tt.seed
				return nil, nil
			})

			if _, err := restored(context.Background(), uuid.UUID{}, uuid.UUID{}, loop.Provenance{}, nil, nil, nil, nil, tt.seed); err != nil {
				t.Fatalf("zero-value restored builder error = %v", err)
			}
			if received.ForeignSID != "" || received.AgentSessionID != "" || received.TurnIndex != 0 || received.Msgs != nil {
				t.Fatalf("zero-value RestoredForeign changed across restore: %#v", received)
			}
		})
	}
}
