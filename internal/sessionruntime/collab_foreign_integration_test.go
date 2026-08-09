package sessionruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// TestCollabForeignIntegrationRawBrokerRoutesToForeignDeliveryHook exercises
// the complete in-process path while keeping transport deterministic: a raw
// broker frame is authenticated, dispatched through the scoped controller,
// and admitted by the foreign delivery hook before the categorical result is
// encoded back onto the broker connection. net.Pipe avoids requiring an
// AF_UNIX listener in the restricted test runner.
func TestCollabForeignIntegrationRawBrokerRoutesToForeignDeliveryHook(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	token := bytes.Repeat([]byte{0x73}, collabCapabilityBytes)
	b, p := newInMemoryCollabBroker(t, fixture.childID, fixture.controller, token)

	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	handled := make(chan error, 1)
	go func() { handled <- b.handle(server) }()

	if err := writeCollabFrame(client, token, collabCapabilityBytes); err != nil {
		t.Fatalf("write capability: %v", err)
	}
	request := []byte(`{"agent_id":"` + fixture.childID.String() + `","message":"raw broker steer","wait_for_response":false}`)
	if err := writeCollabFrame(client, request, collabMaxArgumentBytes); err != nil {
		t.Fatalf("write request: %v", err)
	}

	rawValue := <-fixture.child.Commands
	raw, ok := rawValue.(command.UserInput)
	if !ok {
		t.Fatalf("brokered child command = %T, want command.UserInput", rawValue)
	}
	if !raw.NoFold {
		t.Fatal("foreign brokered MessageAgent command has NoFold=false, want non-folding foreign hook path")
	}
	if raw.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
		t.Fatalf("brokered delivery phase = %q, want %q", raw.DelegateDeliveryPhase, command.DelegateDeliveryPhaseIntent)
	}
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(raw.CommandID, fixture.childID)); err != nil {
		t.Fatalf("foreign QueueFallback: %v", err)
	}
	raw.Accepted <- nil

	response, err := readCollabFrame(client, collabMaxFrameBytes)
	if err != nil {
		t.Fatalf("read broker response: %v", err)
	}
	var result collabWireResult
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatalf("decode broker response: %v", err)
	}
	if result.AgentID != fixture.childID.String() || result.DeliveryStatus != string(tool.DelegateDeliveryQueued) || result.ResponseStatus != "" {
		t.Fatalf("broker result = %+v, want child queued without response status", result)
	}

	select {
	case err := <-handled:
		if err != nil {
			t.Fatalf("broker handle: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("broker handler did not finish")
	}

	p.mu.Lock()
	callCount := p.rateCount
	p.mu.Unlock()
	if callCount != 1 {
		t.Fatalf("origin call count = %d, want one authenticated request", callCount)
	}
	fixture.commands.mu.Lock()
	records := fixture.commands.records
	fixture.commands.mu.Unlock()
	if len(records) != 2 {
		t.Fatalf("foreign delivery records = %d, want intent plus fallback", len(records))
	}
}

// TestCollabForeignIntegrationRawBrokerRoutesToNativeMessageAgent covers the
// same broker/controller boundary for a native target. Native admission remains
// foldable and returns its provisional status before the target terminal; no
// foreign capability or adapter identity is involved.
func TestCollabForeignIntegrationRawBrokerRoutesToNativeMessageAgent(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	token := bytes.Repeat([]byte{0x74}, collabCapabilityBytes)
	b, _ := newInMemoryCollabBroker(t, fixture.childID, fixture.controller, token)
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	handled := make(chan error, 1)
	go func() { handled <- b.handle(server) }()
	if err := writeCollabFrame(client, token, collabCapabilityBytes); err != nil {
		t.Fatalf("write capability: %v", err)
	}
	request := []byte(`{"agent_id":"` + fixture.childID.String() + `","message":"raw native steer","wait_for_response":false}`)
	if err := writeCollabFrame(client, request, collabMaxArgumentBytes); err != nil {
		t.Fatalf("write request: %v", err)
	}
	rawValue := <-fixture.child.Commands
	raw, ok := rawValue.(command.UserInput)
	if !ok {
		t.Fatalf("native brokered child command = %T, want command.UserInput", rawValue)
	}
	if raw.NoFold {
		t.Fatal("native brokered MessageAgent command has NoFold=true, want foldable native admission")
	}
	if raw.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
		t.Fatalf("native brokered delivery phase = %q, want %q", raw.DelegateDeliveryPhase, command.DelegateDeliveryPhaseIntent)
	}
	raw.Accepted <- nil
	response, err := readCollabFrame(client, collabMaxFrameBytes)
	if err != nil {
		t.Fatalf("read broker response: %v", err)
	}
	var result collabWireResult
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatalf("decode broker response: %v", err)
	}
	if result.AgentID != fixture.childID.String() || result.DeliveryStatus != string(tool.DelegateDeliveryAcceptedPending) {
		t.Fatalf("native broker result = %+v, want accepted_pending", result)
	}
	turnID := mustUUID()
	fixture.sub.feed(event.TurnStarted{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: raw.CommandID},
	}})
	fixture.sub.feed(event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
	}, Message: aiMessage("native broker answer")})
	select {
	case err := <-handled:
		if err != nil {
			t.Fatalf("broker handle: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("native broker handler did not finish")
	}
}

func TestCollabForeignIntegrationTwoRequestsResolveFromOneTerminal(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	subscriptions := make(chan *fakeSubscription, 2)
	fixture.session.delegateSubscribe = func(event.EventFilter) (event.Subscription, error) {
		sub := newFakeSubscription(8)
		subscriptions <- sub
		return sub, nil
	}

	type result struct {
		value tool.DelegateResult
		err   error
	}
	results := make(chan result, 2)
	start := func(message string) (*fakeSubscription, command.UserInput) {
		go func() {
			value, err := fixture.controller.Execute(context.Background(), tool.DelegateRequest{
				Operation:       tool.DelegateSend,
				AgentID:         fixture.childID,
				Message:         message,
				WaitForResponse: true,
			})
			results <- result{value: value, err: err}
		}()
		sub := <-subscriptions
		raw := <-fixture.child.Commands
		cmd, ok := raw.(command.UserInput)
		if !ok {
			t.Fatalf("foreign request command = %T, want command.UserInput", raw)
		}
		return sub, cmd
	}

	subA, cmdA := start("fold A")
	subB, cmdB := start("fold B")
	for _, cmd := range []command.UserInput{cmdA, cmdB} {
		if err := fixture.hook.Reserve(context.Background(), foreign.DeliveryReservation{LoopID: fixture.childID, RequestID: cmd.CommandID}); err != nil {
			t.Fatalf("Reserve(%v): %v", cmd.CommandID, err)
		}
	}
	cmdA.Accepted <- nil
	cmdB.Accepted <- nil
	turnID := mustUUID()
	fold := func(cmd command.UserInput) event.TurnFoldedInto {
		return event.TurnFoldedInto{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID},
			Cause:       identity.Cause{CommandID: cmd.CommandID},
		}}
	}
	foldA, foldB := fold(cmdA), fold(cmdB)
	fixture.hook.observeFold(foldA)
	fixture.hook.observeFold(foldB)
	for _, cmd := range []command.UserInput{cmdA, cmdB} {
		if err := fixture.hook.Resolve(context.Background(), foreign.DeliveryResolution{
			LoopID: fixture.childID, RequestID: cmd.CommandID, TurnID: turnID,
			State: foreign.DeliveryResolutionInjected,
		}); err != nil {
			t.Fatalf("Resolve(%v): %v", cmd.CommandID, err)
		}
	}

	// The two request-specific subscriptions observe the same turn coordinates;
	// each fold is authoritative for its own request and the single terminal is
	// therefore allowed to resolve both foreground waiters.
	for _, sub := range []*fakeSubscription{subA, subB} {
		sub.feed(foldA)
		sub.feed(foldB)
		sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{
			SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID,
		}}, Message: aiMessage("one terminal")})
	}
	for i := 0; i < 2; i++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("foreground request %d: %v", i, got.err)
			}
			if got.value.DeliveryStatus != tool.DelegateDeliveryInjected || got.value.ResponseStatus != tool.DelegateResponseCompleted || got.value.Response != "one terminal" {
				t.Fatalf("foreground result %d = %+v, want injected/completed/one terminal", i, got.value)
			}
		case <-time.After(time.Second):
			t.Fatalf("foreground request %d did not resolve", i)
		}
	}
}

func TestCollabForeignIntegrationObserverTimeoutDoesNotCancelTarget(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	zero := 0
	resultCh := make(chan struct {
		value tool.DelegateResult
		err   error
	}, 1)
	go func() {
		value, err := fixture.controller.Execute(context.Background(), tool.DelegateRequest{
			Operation:       tool.DelegateSend,
			AgentID:         fixture.childID,
			Message:         "observer timeout",
			WaitForResponse: true,
			TimeoutSeconds:  &zero,
		})
		resultCh <- struct {
			value tool.DelegateResult
			err   error
		}{value: value, err: err}
	}()
	raw := (<-fixture.child.Commands).(command.UserInput)
	raw.Accepted <- nil
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("observer timeout Execute: %v", got.err)
		}
		if got.value.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || got.value.ResponseStatus != tool.DelegateResponseTimedOut {
			t.Fatalf("observer timeout result = %+v, want accepted_pending/timed_out", got.value)
		}
	case <-time.After(time.Second):
		t.Fatal("observer timeout Execute did not return")
	}

	// The caller's response clock ended, but the accepted request remains owned
	// by the target. Complete it through the normal foreign fallback path and
	// prove no retraction/interrupt command was emitted in either journal or
	// actor inbox.
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(raw.CommandID, fixture.childID)); err != nil {
		t.Fatalf("QueueFallback after observer timeout: %v", err)
	}
	turnID := mustUUID()
	fixture.sub.feed(foreignTurnEvent(fixture, raw.CommandID, turnID, false))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{
		SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID,
	}}, Message: aiMessage("fallback answer")})
	for _, record := range fixture.commands.snapshot() {
		switch record.Command().(type) {
		case command.CancelDelegateRequest, command.CancelQueuedInput, command.Interrupt:
			t.Fatalf("observer timeout persisted target cancellation %T", record.Command())
		}
	}
	select {
	case extra := <-fixture.child.Commands:
		switch extra.(type) {
		case command.CancelDelegateRequest, command.CancelQueuedInput, command.Interrupt:
			t.Fatalf("observer timeout dispatched target cancellation %T", extra)
		default:
			t.Fatalf("unexpected post-timeout target command %T", extra)
		}
	default:
	}
}

func TestCollabForeignIntegrationStolenTokenDeniedAcrossOrigins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID := mustUUID()
	parentA, parentB := mustUUID(), mustUUID()
	childA, childB := mustUUID(), mustUUID()
	childBoundA := bindCfg(engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("a")}}, loop.EngineForeignClaude, "a"), sessionID, childA)
	childBoundB := bindCfg(engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("b")}}, loop.EngineForeignClaude, "b"), sessionID, childB)
	s := &Session{
		sessionID:  sessionID,
		sessionCtx: ctx,
		loops: map[uuid.UUID]*loopHandle{
			parentA: {id: parentA},
			parentB: {id: parentB},
			childA:  {id: childA, bound: childBoundA, backend: &channelBackend{Commands: make(chan command.Command, 2), Done: make(chan struct{})}, parent: loop.Provenance{LoopID: parentA}, agentName: "a"},
			childB:  {id: childB, bound: childBoundB, backend: &channelBackend{Commands: make(chan command.Command, 2), Done: make(chan struct{})}, parent: loop.Provenance{LoopID: parentB}, agentName: "b"},
		},
	}
	manager := newDelegationManager(Topology{})
	manager.attach(s)
	spyA := &collabCountingController{target: &scopedController{manager: manager, parentLoopID: parentA, style: loop.DelegationManaged}}
	spyB := &collabCountingController{target: &scopedController{manager: manager, parentLoopID: parentB, style: loop.DelegationManaged}}
	tokenA := bytes.Repeat([]byte{0xa1}, collabCapabilityBytes)
	tokenB := bytes.Repeat([]byte{0xb2}, collabCapabilityBytes)
	b, _ := newInMemoryCollabBroker(t, parentA, spyA, tokenA)
	addInMemoryCollabPrincipal(b, parentB, spyB, tokenB)

	err := runInMemoryCollabRequest(t, b, tokenA, childB)
	if !errors.Is(err, errCollabBrokerProtocol) {
		t.Fatalf("origin A using token A for origin B child: %v, want fixed broker protocol error", err)
	}
	err = runInMemoryCollabRequest(t, b, tokenB, childA)
	if !errors.Is(err, errCollabBrokerProtocol) {
		t.Fatalf("origin B using token B for origin A child: %v, want fixed broker protocol error", err)
	}
	if got := spyA.callCount(); got != 1 {
		t.Fatalf("origin A controller calls = %d, want one denied scoped call", got)
	}
	if got := spyB.callCount(); got != 1 {
		t.Fatalf("origin B controller calls = %d, want one denied scoped call", got)
	}
	if b.HasRawCapability(tokenA) == false || b.HasRawCapability(tokenB) == false {
		t.Fatal("valid origin capabilities were not retained")
	}
	if b.HasRawCapability(bytes.Repeat([]byte{0xc3}, collabCapabilityBytes)) {
		t.Fatal("stolen/forged capability unexpectedly authenticated")
	}
}

type collabCountingController struct {
	target tool.DelegateController
	mu     sync.Mutex
	calls  int
}

func (c *collabCountingController) Execute(ctx context.Context, req tool.DelegateRequest) (tool.DelegateResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.target.Execute(ctx, req)
}

func (c *collabCountingController) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func addInMemoryCollabPrincipal(b *collabBroker, loopID uuid.UUID, controller tool.DelegateController, token []byte) *collabPrincipal {
	digest := sha256.Sum256(token)
	p := &collabPrincipal{
		broker:      b,
		loopID:      loopID,
		digest:      digest,
		controller:  controller,
		concurrent:  make(chan struct{}, collabMaxPerCapability),
		callCancels: make(map[net.Conn]context.CancelFunc),
		connections: make(map[net.Conn]struct{}),
	}
	b.mu.Lock()
	b.principals[digest] = p
	b.byLoop[loopID] = p
	b.mu.Unlock()
	return p
}

func runInMemoryCollabRequest(t *testing.T, b *collabBroker, token []byte, agentID uuid.UUID) error {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close()
	handled := make(chan error, 1)
	go func() { handled <- b.handle(server) }()
	if err := writeCollabFrame(client, token, collabCapabilityBytes); err != nil {
		return err
	}
	request := []byte(`{"agent_id":"` + agentID.String() + `","message":"cross-origin","wait_for_response":false}`)
	if err := writeCollabFrame(client, request, collabMaxArgumentBytes); err != nil {
		return err
	}
	select {
	case err := <-handled:
		return err
	case <-time.After(time.Second):
		t.Fatal("cross-origin broker handler did not finish")
		return nil
	}
}

func newInMemoryCollabBroker(t *testing.T, loopID uuid.UUID, controller tool.DelegateController, token []byte) (*collabBroker, *collabPrincipal) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := &collabBroker{
		ctx:         ctx,
		principals:  make(map[[32]byte]*collabPrincipal),
		byLoop:      make(map[uuid.UUID]*collabPrincipal),
		connections: make(map[net.Conn]*collabPrincipal),
		globalCalls: make(chan struct{}, collabMaxConcurrent),
		peerUID: func(net.Conn) (uint32, bool) {
			return collabUID(), true
		},
	}
	digest := sha256.Sum256(token)
	p := &collabPrincipal{
		broker:      b,
		loopID:      loopID,
		digest:      digest,
		controller:  controller,
		concurrent:  make(chan struct{}, collabMaxPerCapability),
		callCancels: make(map[net.Conn]context.CancelFunc),
		connections: make(map[net.Conn]struct{}),
	}
	b.principals[digest] = p
	b.byLoop[loopID] = p
	return b, p
}
