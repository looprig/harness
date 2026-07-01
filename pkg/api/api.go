// Package api is the agent-agnostic HTTP runner surface. It defines the narrow
// Agent interface the runner drives, the Factory the consumer supplies to build
// (or resume) a session's agent, and the server Config. It owns the HTTP
// surface, the many-session map, SSE, and gate routing — never agent policy,
// composition, persistence, or credentials, all of which the consumer owns via
// the Factory. It deliberately does NOT import pkg/session or pkg/loop: the
// consumer wires the concrete Agent implementation at its own composition root.
package api

import (
	"context"
	"net"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// Agent is the narrow surface the HTTP runner drives (Interface Segregation: a
// subset of tui.Agent minus the TUI-only AcceptsImages/ReplayBacklog). Existing
// agents satisfy it structurally.
type Agent interface {
	Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
	PrimaryLoopID() uuid.UUID
	Subscribe(filter event.EventFilter) (event.Subscription, error)
	Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error
	Deny(ctx context.Context, loopID, callID uuid.UUID) error
	ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error
	Interrupt(ctx context.Context) (bool, error)
	Close(ctx context.Context) error
	ExportSource(ctx context.Context) (transcript.RecordSource, transcript.SystemPromptResolver, error)
}

// AgentRequest tells the Factory which session to build and whether to create or
// resume it. The runner mints (or, for resume, is given) the SessionID.
type AgentRequest struct {
	SessionID uuid.UUID
	Resume    bool
}

// Factory builds a driven Agent for one session. The consumer owns the agent's
// composition, policy, and PERSISTENCE (session.New WithSessionID for create;
// Restore for resume). pkg/api owns the HTTP surface, the many-session map, SSE,
// and gate routing — never policy or credentials.
type Factory func(ctx context.Context, req AgentRequest) (Agent, error)

// Config configures the HTTP server. Zero value binds to loopback.
type Config struct {
	// Addr is the listen address. Empty => "127.0.0.1:0" (loopback, ephemeral
	// port). Binding to a non-loopback host requires AllowPublic=true.
	Addr string
	// AllowPublic must be set explicitly to bind a non-loopback interface; plain
	// v1 endpoints expose autonomous execution, so public exposure is opt-in.
	AllowPublic                            bool
	ReadTimeout, WriteTimeout, IdleTimeout time.Duration
	MaxHeaderBytes                         int
	MaxBodyBytes                           int64
}

// PublicBindError reports that cfg.Addr resolves to a non-loopback interface
// while cfg.AllowPublic is false. It is the loopback-default guard's fail-secure
// refusal: because plain v1 endpoints expose autonomous execution, binding a
// public interface must be an explicit opt-in.
type PublicBindError struct{ Addr string }

func (e PublicBindError) Error() string {
	return "api: refusing to bind non-loopback address " + e.Addr + " without AllowPublic"
}

// InvalidAddrError reports that cfg.Addr is malformed (e.g. missing port) and
// could not be parsed by net.SplitHostPort. The guard fails secure and binds
// nothing. Cause is the underlying parse error, exposed via Unwrap.
type InvalidAddrError struct {
	Addr  string
	Cause error
}

func (e InvalidAddrError) Error() string {
	return "api: invalid listen address " + e.Addr + ": " + e.Cause.Error()
}

func (e InvalidAddrError) Unwrap() error { return e.Cause }

// resolveListenAddr returns the concrete TCP address to bind for cfg, enforcing
// the loopback-default policy: an empty Addr becomes loopback+ephemeral; a
// non-loopback host is rejected unless cfg.AllowPublic is set. It is fail-secure:
// any host it cannot prove to be loopback requires the explicit opt-in, and any
// error returns an empty address so a caller never binds on failure.
func resolveListenAddr(cfg Config) (string, error) {
	if cfg.Addr == "" {
		return "127.0.0.1:0", nil
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return "", InvalidAddrError{Addr: cfg.Addr, Cause: err}
	}
	if isLoopbackHost(host) {
		return cfg.Addr, nil
	}
	if cfg.AllowPublic {
		return cfg.Addr, nil
	}
	return "", PublicBindError{Addr: cfg.Addr}
}

// isLoopbackHost reports whether host is provably loopback. It treats the
// literal "localhost", 127.0.0.0/8, and ::1 as loopback. An empty host (as in
// ":8080") binds all interfaces and is NOT loopback; any host that does not
// parse as a loopback IP and is not the "localhost" literal is treated as
// non-loopback (fail-secure).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
