package catalogreader

import (
	"testing"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/sessionstore"
	harnesssessionwire "github.com/looprig/harness/pkg/sessionwire"
)

func compatibilityUUID(seed byte) uuid.UUID {
	var id uuid.UUID
	for index := range id {
		id[index] = seed
	}
	return id
}

func TestLegacyProjectionAvailabilityBoundary(t *testing.T) {
	t.Parallel()

	sid := compatibilityUUID(0x71)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	authority := harnesssessionwire.ReadAuthority{TenantID: "tenant-a", AgentID: "fixture-agent"}
	scope := harnesssessionwire.ReadScope{
		TenantID: authority.TenantID, SessionID: coresessionwire.SessionID(sid.String()),
		AgentID: authority.AgentID, Residency: coresessionwire.SessionResidencyCold,
	}

	t.Run("summary classifies only documented omissions", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			meta sessionstore.SessionMeta
			want coreProjectionAbsence
		}{
			{name: "created and active timestamps", meta: sessionstore.SessionMeta{SessionID: sid, State: sessionstore.StateIdle, CreatedAt: now.Add(-time.Hour), LastActiveAt: now}, want: coreProjectionPresent},
			{name: "last active only", meta: sessionstore.SessionMeta{SessionID: sid, State: sessionstore.StateIdle, LastActiveAt: now}, want: coreProjectionPresent},
			{name: "created only lacks legacy activity", meta: sessionstore.SessionMeta{SessionID: sid, State: sessionstore.StateIdle, CreatedAt: now}, want: coreProjectionMissingActivity},
			{name: "legacy state only with activity", meta: sessionstore.SessionMeta{SessionID: sid, LastActiveAt: now}, want: coreProjectionMissingState},
			{name: "neither timestamp", meta: sessionstore.SessionMeta{SessionID: sid, State: sessionstore.StateIdle}, want: coreProjectionMissingActivity},
			{name: "state and activity", meta: sessionstore.SessionMeta{SessionID: sid}, want: coreProjectionMissingState | coreProjectionMissingActivity},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got, err := projectLegacySummary(authority, test.meta)
				if err != nil {
					t.Fatalf("projectLegacySummary() error = %v", err)
				}
				if got.absence != test.want {
					t.Errorf("projection absence = %d, want %d", got.absence, test.want)
				}
			})
		}
	})

	t.Run("status zero activity remains Core representable", func(t *testing.T) {
		t.Parallel()
		got, err := projectLegacyStatus(scope, sessionstore.SessionMeta{SessionID: sid, State: sessionstore.StateIdle})
		if err != nil {
			t.Fatalf("projectLegacyStatus() error = %v", err)
		}
		if got.absence != coreProjectionPresent {
			t.Errorf("projection absence = %d, want present", got.absence)
		}
	})

	t.Run("status legacy state is explicitly unavailable", func(t *testing.T) {
		t.Parallel()
		got, err := projectLegacyStatus(scope, sessionstore.SessionMeta{SessionID: sid})
		if err != nil {
			t.Fatalf("projectLegacyStatus() error = %v", err)
		}
		if got.absence != coreProjectionMissingState {
			t.Errorf("projection absence = %d, want missing state", got.absence)
		}
	})

	t.Run("invalid source shapes never become unavailable projections", func(t *testing.T) {
		t.Parallel()
		tests := []sessionstore.SessionMeta{
			{SessionID: sid, State: sessionstore.SessionState("future_state")},
			{State: sessionstore.StateIdle},
		}
		for _, meta := range tests {
			if _, err := projectLegacySummary(authority, meta); err == nil {
				t.Errorf("projectLegacySummary(%+v) error = nil, want fail-closed", meta)
			}
		}
		other := compatibilityUUID(0x72)
		if _, err := projectLegacyStatus(scope, sessionstore.SessionMeta{SessionID: other}); err == nil {
			t.Fatal("projectLegacyStatus(cross-session) error = nil, want fail-closed")
		}
	})
}
