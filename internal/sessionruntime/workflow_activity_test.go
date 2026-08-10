package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/tool"
)

func validWorkflowActivityMetadata(sessionID uuid.UUID) tool.WorkflowActivityMetadata {
	return tool.WorkflowActivityMetadata{
		EventID:         uuid.UUID{11},
		SessionID:       sessionID,
		RunID:           uuid.UUID{12},
		WorkflowName:    "source_document_extract",
		WorkflowVersion: "v1",
		Kind:            string(event.WorkflowActivityRunStarted),
		Status:          string(event.WorkflowRunStatusRunning),
		TotalVertices:   2,
		OccurredAt:      time.Date(2026, time.August, 9, 14, 0, 0, 0, time.UTC),
		Message:         "workflow started",
	}
}

func TestWorkflowActivityPublisherRejectsInvalidDependencies(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{1}
	h := hub.New(sessionID)
	if _, err := newCheckedWorkflowActivityPublisher(uuid.UUID{}, h, nil); err == nil {
		t.Fatal("zero session id constructor error = nil")
	}
	if _, err := newCheckedWorkflowActivityPublisher(sessionID, nil, nil); err == nil {
		t.Fatal("nil hub constructor error = nil")
	}
}

func TestWorkflowActivityPublisherPublishesValidatedSessionEvent(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{21}
	app := &dedupAppender{}
	h := hub.New(sessionID, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)
	defer sub.Close()

	now := time.Date(2026, time.August, 9, 15, 0, 0, 0, time.UTC)
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}
	metadata := validWorkflowActivityMetadata(sessionID)
	if err := pub.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("PublishWorkflowActivity() = %v", err)
	}

	delivered := recvProcessEvent(t, sub)
	got, ok := delivered.(event.WorkflowActivity)
	if !ok {
		t.Fatalf("published event = %T, want WorkflowActivity", delivered)
	}
	if got.EventID != metadata.EventID || got.RunID != metadata.RunID || got.WorkflowName != metadata.WorkflowName || got.Kind != event.WorkflowActivityKind(metadata.Kind) {
		t.Fatalf("published activity = %#v, want metadata-preserving event", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
	if got.EventVisibility != event.Public || got.Scope() != event.ScopeSession || got.Class() != event.Enduring {
		t.Fatalf("published contract = visibility:%v scope:%v class:%v", got.EventVisibility, got.Scope(), got.Class())
	}
}

func TestWorkflowActivityPublisherRejectsForeignSessionAndInvalidBody(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{31}
	app := &dedupAppender{}
	h := hub.New(sessionID, hub.WithAppender(app))
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}

	foreign := validWorkflowActivityMetadata(uuid.UUID{32})
	var ownerErr *WorkflowActivityOwnerMismatchError
	if err := pub.PublishWorkflowActivity(context.Background(), foreign); !errors.As(err, &ownerErr) {
		t.Fatalf("foreign PublishWorkflowActivity() = %T %v, want owner mismatch", err, err)
	}
	if app.callCount() != 0 {
		t.Fatalf("appender calls after foreign publish = %d, want 0", app.callCount())
	}

	invalid := validWorkflowActivityMetadata(sessionID)
	invalid.RunID = uuid.UUID{}
	if err := pub.PublishWorkflowActivity(context.Background(), invalid); err == nil {
		t.Fatal("invalid PublishWorkflowActivity() error = nil")
	}
	if app.callCount() != 0 {
		t.Fatalf("appender calls after invalid publish = %d, want 0", app.callCount())
	}
}

func TestWorkflowActivityPublisherDeduplicatesLiveDelivery(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{41}
	app := &dedupAppender{decisions: []bool{true, false}}
	h := hub.New(sessionID, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)
	defer sub.Close()
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}
	metadata := validWorkflowActivityMetadata(sessionID)
	if err := pub.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("first PublishWorkflowActivity() = %v", err)
	}
	if _, ok := recvProcessEvent(t, sub).(event.WorkflowActivity); !ok {
		t.Fatal("first publication was not WorkflowActivity")
	}
	if err := pub.PublishWorkflowActivity(context.Background(), metadata); err != nil {
		t.Fatalf("duplicate PublishWorkflowActivity() = %v", err)
	}
	expectNoProcessEvent(t, sub)
	if got := app.callCount(); got != 2 {
		t.Fatalf("appender calls = %d, want 2", got)
	}
}

func TestWorkflowActivityPublisherFailsAfterSessionAbort(t *testing.T) {
	t.Parallel()

	sessionID := uuid.UUID{51}
	h := hub.New(sessionID)
	pub, err := newCheckedWorkflowActivityPublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedWorkflowActivityPublisher() = %v", err)
	}
	h.AbortSession(errors.New("session shutdown"))
	if err := pub.PublishWorkflowActivity(context.Background(), validWorkflowActivityMetadata(sessionID)); err == nil {
		t.Fatal("PublishWorkflowActivity() after session abort error = nil")
	}
}
