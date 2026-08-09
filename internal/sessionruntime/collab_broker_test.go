package sessionruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

func TestCollabBrokerCreatesPrivateEndpointAndDigestScopedCapability(t *testing.T) {
	if !collabPlatformSupported() {
		t.Skip("collaboration broker requires Unix-domain sockets")
	}

	sessionID := mustUUID()
	s := &Session{sessionID: sessionID, sessionCtx: context.Background()}
	broker, err := newCollabBroker(s)
	if err != nil {
		t.Skipf("Unix socket unavailable in this runner: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close(context.Background()) })

	endpoint := broker.Endpoint()
	if !filepath.IsAbs(endpoint) {
		t.Fatalf("broker endpoint = %q, want absolute path", endpoint)
	}
	info, err := os.Stat(endpoint)
	if err != nil {
		t.Fatalf("stat broker endpoint: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions = %#o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(endpoint))
	if err != nil {
		t.Fatalf("stat broker directory: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions = %#o, want 0700", dirInfo.Mode().Perm())
	}

	origin := mustUUID()
	descriptor, err := broker.Mint(origin, &recordingDelegateController{})
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	capability := descriptor.Capability()
	if len(capability) != collabCapabilityBytes {
		t.Fatalf("capability length = %d, want %d", len(capability), collabCapabilityBytes)
	}
	wantDigest := sha256.Sum256(capability)
	if !broker.HasRawCapability(capability) {
		t.Fatal("broker did not authenticate minted capability")
	}
	if broker.HasRawCapability(make([]byte, collabCapabilityBytes)) {
		t.Fatal("zero capability unexpectedly authenticated")
	}
	if got := broker.digestForTest(capability); got != wantDigest {
		t.Fatalf("capability digest = %x, want %x", got, wantDigest)
	}
	if got := descriptor.Endpoint(); got != endpoint {
		t.Fatalf("descriptor endpoint = %q, want %q", got, endpoint)
	}
}

func TestCollabBrokerRejectsStaleAndSymlinkSocketPaths(t *testing.T) {
	if !collabPlatformSupported() {
		t.Skip("collaboration broker requires Unix-domain sockets")
	}
	root := t.TempDir()
	stale := filepath.Join(root, "stale.sock")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newCollabBrokerAt(&Session{sessionID: mustUUID()}, root, filepath.Base(stale)); !errors.Is(err, errCollabBrokerStaleEndpoint) {
		t.Fatalf("stale endpoint error = %v, want %v", err, errCollabBrokerStaleEndpoint)
	}
	symlink := filepath.Join(root, "symlink.sock")
	if err := os.Symlink(filepath.Join(root, "missing"), symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := newCollabBrokerAt(&Session{sessionID: mustUUID()}, root, filepath.Base(symlink)); !errors.Is(err, errCollabBrokerStaleEndpoint) {
		t.Fatalf("symlink endpoint error = %v, want %v", err, errCollabBrokerStaleEndpoint)
	}
}

func TestCollabBrokerGoldenHandshakeAndRequestFraming(t *testing.T) {
	capability := bytes.Repeat([]byte{0x2a}, collabCapabilityBytes)
	request := []byte(`{"agent_id":"55555555-5555-4555-8555-555555555555","message":"hello","wait_for_response":true}`)
	var wire bytes.Buffer
	if err := writeCollabFrame(&wire, capability, collabCapabilityBytes); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	if err := writeCollabFrame(&wire, request, collabMaxArgumentBytes); err != nil {
		t.Fatalf("write request: %v", err)
	}
	want := append([]byte{0, 0, 0, collabCapabilityBytes}, capability...)
	requestHeader := make([]byte, 4)
	binary.BigEndian.PutUint32(requestHeader, uint32(len(request)))
	want = append(want, requestHeader...)
	want = append(want, request...)
	if !bytes.Equal(wire.Bytes(), want) {
		t.Fatalf("framing = %x, want %x", wire.Bytes(), want)
	}
	gotCapability, err := readCollabHandshake(&wire)
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	if !bytes.Equal(gotCapability, capability) {
		t.Fatalf("capability = %x, want %x", gotCapability, capability)
	}
	gotRequest, err := readCollabFrame(&wire, collabMaxArgumentBytes)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if !bytes.Equal(gotRequest, request) {
		t.Fatalf("request = %s, want %s", gotRequest, request)
	}
}

func TestCollabBrokerFramingWritesShortWritersFully(t *testing.T) {
	payload := []byte("short-writer-safe")
	var wire shortWriter
	if err := writeCollabFrame(&wire, payload, collabMaxFrameBytes); err != nil {
		t.Fatalf("writeCollabFrame() error = %v", err)
	}
	got, err := readCollabFrame(bytes.NewReader(wire.buf), collabMaxFrameBytes)
	if err != nil {
		t.Fatalf("readCollabFrame() error = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestCollabBrokerRequestValidationRejectsForgedIdentityAndBounds(t *testing.T) {
	valid := `{"agent_id":"55555555-5555-4555-8555-555555555555","message":"hello"}`
	for _, field := range []string{"session_id", "origin_loop_id", "request_id", "parent_tool_use_id", "correlation_id"} {
		raw := strings.TrimSuffix(valid, "}") + `,"` + field + `":"forged"}`
		if _, err := decodeCollabRequest([]byte(raw)); !errors.Is(err, errCollabBrokerInvalidRequest) {
			t.Errorf("field %q error = %v, want invalid request", field, err)
		}
	}
	oversized := `{"agent_id":"55555555-5555-4555-8555-555555555555","message":"` + strings.Repeat("x", collabMaxMessageBytes+1) + `"}`
	if _, err := decodeCollabRequest([]byte(oversized)); !errors.Is(err, errCollabBrokerInvalidRequest) {
		t.Fatalf("oversized request error = %v, want invalid request", err)
	}
	for _, raw := range []string{
		`{"agent_id":"55555555-5555-4555-8555-555555555555","message":"hello","wait_for_response":null}`,
		`{"agent_id":"55555555-5555-4555-8555-555555555555","message":"hello","timeout_seconds":null}`,
	} {
		if _, err := decodeCollabRequest([]byte(raw)); !errors.Is(err, errCollabBrokerInvalidRequest) {
			t.Errorf("null optional field request error = %v, want invalid request", err)
		}
	}
	got, err := decodeCollabRequest([]byte(valid))
	if err != nil || !got.waitForResponse || got.timeoutSeconds != nil {
		t.Fatalf("default request = %#v, error %v", got, err)
	}
}

func TestCollabBrokerRevokeCancelsPendingCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &collabBroker{
		ctx:         ctx,
		principals:  make(map[[32]byte]*collabPrincipal),
		byLoop:      make(map[uuid.UUID]*collabPrincipal),
		connections: make(map[net.Conn]*collabPrincipal),
		globalCalls: make(chan struct{}, collabMaxConcurrent),
	}
	loopID := mustUUID()
	p := &collabPrincipal{
		broker:      b,
		loopID:      loopID,
		controller:  &blockingDelegateController{},
		concurrent:  make(chan struct{}, collabMaxPerCapability),
		callCancels: make(map[net.Conn]context.CancelFunc),
		connections: make(map[net.Conn]struct{}),
	}
	b.byLoop[loopID] = p
	conn := &testConn{}
	callCtx, release, ok := b.beginCall(p, conn)
	if !ok {
		t.Fatal("beginCall rejected initial call")
	}
	b.mu.Lock()
	b.connections[conn] = p
	b.mu.Unlock()
	if err := b.Revoke(loopID); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	select {
	case <-callCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel pending call")
	}
	release()
}

func TestCollabBrokerBoundsConcurrentAndRateAdmission(t *testing.T) {
	b := &collabBroker{
		ctx:         context.Background(),
		globalCalls: make(chan struct{}, 1),
	}
	p := &collabPrincipal{
		broker:      b,
		controller:  &recordingDelegateController{},
		concurrent:  make(chan struct{}, 1),
		callCancels: make(map[net.Conn]context.CancelFunc),
		connections: make(map[net.Conn]struct{}),
	}
	firstCtx, firstRelease, ok := b.beginCall(p, &testConn{})
	if !ok || firstCtx == nil {
		t.Fatal("first call was not admitted")
	}
	if _, _, ok := b.beginCall(p, &testConn{}); ok {
		t.Fatal("second concurrent call admitted beyond per-broker bound")
	}
	firstRelease()
	b.globalCalls = make(chan struct{}, collabMaxConcurrent)
	p.concurrent = make(chan struct{}, collabMaxPerCapability)
	p.rateStart = time.Now()
	p.rateCount = collabRateLimit
	if _, _, ok := b.beginCall(p, &testConn{}); ok {
		t.Fatal("call admitted beyond per-capability rate limit")
	}
}

func TestCollabBrokerDescriptorIsPerOriginAndDefensivelyCopied(t *testing.T) {
	s := &Session{
		sessionID:            mustUUID(),
		foreignDeliveryHooks: make(map[uuid.UUID]*foreignDeliveryHook),
		collabBroker: &collabBroker{
			endpoint:    "/private/tmp/collab.sock",
			principals:  make(map[[32]byte]*collabPrincipal),
			byLoop:      make(map[uuid.UUID]*collabPrincipal),
			connections: make(map[net.Conn]*collabPrincipal),
		},
	}
	loopID := mustUUID()
	controller := &recordingDelegateController{}
	first, hook := s.foreignServicesForTrackedWithController(loopID, controller)
	second, sameHook := s.foreignServicesForTrackedWithController(loopID, controller)
	if hook != sameHook {
		t.Fatal("repeated construction did not retain the origin hook")
	}
	firstCapability := first.Broker.Capability()
	secondCapability := second.Broker.Capability()
	if !bytes.Equal(firstCapability, secondCapability) {
		t.Fatal("repeated construction rotated a live origin capability")
	}
	firstCapability[0] ^= 0xff
	if bytes.Equal(firstCapability, first.Broker.Capability()) {
		t.Fatal("descriptor capability accessor exposed mutable backing")
	}
}

func TestCollabBrokerResultGoldenJSONOmitsInternalIdentity(t *testing.T) {
	agentID := mustUUID()
	requestID := mustUUID()
	result, err := encodeCollabResult(tool.DelegateResult{
		AgentID:        agentID,
		Name:           "worker",
		State:          tool.AgentStateIdle,
		DeliveryStatus: tool.DelegateDeliveryInjected,
		ResponseStatus: tool.DelegateResponseCompleted,
		Response:       "done",
		CorrelationID:  requestID,
	})
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	want := `{"agent_id":"` + agentID.String() + `","name":"worker","state":"idle","delivery_status":"injected","response_status":"completed","response":"done"}`
	if string(result) != want {
		t.Fatalf("result JSON = %s, want %s", result, want)
	}
	if bytes.Contains(result, []byte(requestID.String())) || bytes.Contains(result, []byte("session_id")) {
		t.Fatalf("result leaked internal identity: %s", result)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["response_status"]; !ok {
		t.Fatal("result omitted response_status")
	}
}

func TestCollabBrokerAuthenticatesEachCapabilityToItsController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var peerChecks int
	b := &collabBroker{
		ctx:         ctx,
		principals:  make(map[[32]byte]*collabPrincipal),
		byLoop:      make(map[uuid.UUID]*collabPrincipal),
		connections: make(map[net.Conn]*collabPrincipal),
		globalCalls: make(chan struct{}, collabMaxConcurrent),
		peerUID: func(net.Conn) (uint32, bool) {
			peerChecks++
			return collabUID(), true
		},
	}
	controller := &recordingDelegateController{}
	token := bytes.Repeat([]byte{0x44}, collabCapabilityBytes)
	digest := sha256.Sum256(token)
	loopID := mustUUID()
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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	done := make(chan struct{})
	go func() { b.handle(server); close(done) }()
	if err := writeCollabFrame(client, token, collabCapabilityBytes); err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"agent_id":"55555555-5555-4555-8555-555555555555","message":"hello","wait_for_response":false}`)
	if err := writeCollabFrame(client, request, collabMaxArgumentBytes); err != nil {
		t.Fatal(err)
	}
	response, err := readCollabFrame(client, collabMaxFrameBytes)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var got collabWireResult
	if err := json.Unmarshal(response, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "worker" {
		t.Fatalf("response = %#v, want worker controller result", got)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broker handler did not finish")
	}
	controller.mu.Lock()
	called := controller.calls
	controller.mu.Unlock()
	if called != 1 {
		t.Fatalf("controller calls = %d, want 1", called)
	}
	if peerChecks != 1 {
		t.Fatalf("peer credential checks = %d, want 1", peerChecks)
	}
}

func TestCollabBrokerDeadlineSetupFailureIsBoundedAndFixed(t *testing.T) {
	const secret = "deadline-failure-secret-token"
	conn := &deadlineFailureConn{admissionProbeConn: admissionProbeConn{secret: secret}}
	b := newCollabAdmissionTestBroker(func(net.Conn) (uint32, bool) {
		return collabUID(), true
	})

	done := make(chan error, 1)
	go func() { done <- b.handle(conn) }()
	select {
	case err := <-done:
		if !errors.Is(err, errCollabBrokerProtocol) {
			t.Fatalf("handle() error = %v, want fixed protocol error", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("handle() error leaked secret: %v", err)
		}
		if conn.readCalls != 0 {
			t.Fatalf("handler read %d bytes before rejecting deadline setup", conn.readCalls)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not terminate after deadline setup failure")
	}
}

func TestCollabBrokerRejectsPeerCredentialFailureAndWrongUIDBeforeTokenAuth(t *testing.T) {
	const secret = "peer-admission-secret-token"
	tests := []struct {
		name string
		peer func(net.Conn) (uint32, bool)
	}{
		{
			name: "credential syscall failure",
			peer: func(net.Conn) (uint32, bool) { return 0, false },
		},
		{
			name: "wrong uid",
			peer: func(net.Conn) (uint32, bool) { return collabUID() + 1, true },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &admissionProbeConn{secret: secret}
			b := newCollabAdmissionTestBroker(tt.peer)

			done := make(chan error, 1)
			go func() { done <- b.handle(conn) }()
			select {
			case err := <-done:
				if !errors.Is(err, errCollabBrokerAuthentication) {
					t.Fatalf("handle() error = %v, want fixed authentication error", err)
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("handle() error leaked secret: %v", err)
				}
				if conn.readCalls != 0 {
					t.Fatalf("handler read token before peer rejection: %d reads", conn.readCalls)
				}
			case <-time.After(time.Second):
				t.Fatal("handler did not terminate after peer rejection")
			}
		})
	}
}

func TestCollabBrokerCloseReturnsAtDeadlineForNonCooperativeController(t *testing.T) {
	if !collabPlatformSupported() {
		t.Skip("collaboration broker requires Unix-domain sockets")
	}
	s := &Session{sessionID: mustUUID(), sessionCtx: context.Background()}
	b, err := newCollabBroker(s)
	if err != nil {
		t.Skipf("Unix socket unavailable in this runner: %v", err)
	}
	controller := newNonCooperativeDelegateController()
	t.Cleanup(func() {
		controller.release()
		_ = b.Close(context.Background())
	})
	descriptor, err := b.Mint(mustUUID(), controller)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	conn, err := net.Dial("unix", descriptor.Endpoint())
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	defer conn.Close()
	if err := writeCollabFrame(conn, descriptor.Capability(), collabCapabilityBytes); err != nil {
		t.Fatalf("write capability: %v", err)
	}
	request := []byte(`{"agent_id":"55555555-5555-4555-8555-555555555555","message":"noncooperative","wait_for_response":false}`)
	if err := writeCollabFrame(conn, request, collabMaxArgumentBytes); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case <-controller.entered:
	case <-time.After(time.Second):
		t.Fatal("controller did not receive admitted call")
	}

	const releaseDelay = 500 * time.Millisecond
	time.AfterFunc(releaseDelay, controller.release)
	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = b.Close(closeCtx)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want context deadline", err)
	}
	if elapsed >= releaseDelay/2 {
		t.Fatalf("Close() waited for non-cooperative controller: %v", elapsed)
	}
	if _, statErr := os.Stat(descriptor.Endpoint()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("broker endpoint stat = %v, want removed before handler release", statErr)
	}

	controller.release()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
	defer cleanupCancel()
	if err := b.Close(cleanupCtx); err != nil {
		t.Fatalf("Close() after controller release: %v", err)
	}
}

func newCollabAdmissionTestBroker(peerUID func(net.Conn) (uint32, bool)) *collabBroker {
	return &collabBroker{
		ctx:         context.Background(),
		principals:  make(map[[32]byte]*collabPrincipal),
		byLoop:      make(map[uuid.UUID]*collabPrincipal),
		connections: make(map[net.Conn]*collabPrincipal),
		globalCalls: make(chan struct{}, collabMaxConcurrent),
		peerUID:     peerUID,
	}
}

func FuzzDecodeCollabRequest(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"agent_id":"55555555-5555-4555-8555-555555555555","message":"hello"}`),
		[]byte(`{"agent_id":null,"message":"hello"}`),
		[]byte(`not-json`),
		[]byte{0xff, 0xfe, 0xfd},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		request, err := decodeCollabRequest(raw)
		if err != nil {
			return
		}
		if request.agentID.IsZero() || strings.TrimSpace(request.message) == "" {
			t.Fatalf("successful decode produced invalid request: %#v", request)
		}
	})
}

type blockingDelegateController struct{}

func (*blockingDelegateController) Execute(ctx context.Context, _ tool.DelegateRequest) (tool.DelegateResult, error) {
	<-ctx.Done()
	return tool.DelegateResult{}, ctx.Err()
}

type nonCooperativeDelegateController struct {
	entered   chan struct{}
	releaseCh chan struct{}
	once      sync.Once
}

func newNonCooperativeDelegateController() *nonCooperativeDelegateController {
	return &nonCooperativeDelegateController{entered: make(chan struct{}), releaseCh: make(chan struct{})}
}

func (c *nonCooperativeDelegateController) Execute(context.Context, tool.DelegateRequest) (tool.DelegateResult, error) {
	close(c.entered)
	<-c.releaseCh
	return tool.DelegateResult{AgentID: mustUUID(), Name: "worker", State: tool.AgentStateIdle}, nil
}

func (c *nonCooperativeDelegateController) release() {
	c.once.Do(func() { close(c.releaseCh) })
}

type testConn struct{}

func (*testConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (*testConn) Write(p []byte) (int, error)      { return len(p), nil }
func (*testConn) Close() error                     { return nil }
func (*testConn) LocalAddr() net.Addr              { return testAddr("local") }
func (*testConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (*testConn) SetDeadline(time.Time) error      { return nil }
func (*testConn) SetReadDeadline(time.Time) error  { return nil }
func (*testConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

type admissionProbeConn struct {
	testConn
	secret    string
	readCalls int
}

func (c *admissionProbeConn) Read([]byte) (int, error) {
	c.readCalls++
	return 0, errors.New(c.secret)
}

type deadlineFailureConn struct {
	admissionProbeConn
}

func (c *deadlineFailureConn) SetReadDeadline(time.Time) error {
	return errors.New(c.secret)
}

type shortWriter struct{ buf []byte }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

type recordingDelegateController struct {
	mu    sync.Mutex
	calls int
}

func (c *recordingDelegateController) Execute(context.Context, tool.DelegateRequest) (tool.DelegateResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return tool.DelegateResult{AgentID: mustUUID(), Name: "worker", State: tool.AgentStateIdle}, nil
}
