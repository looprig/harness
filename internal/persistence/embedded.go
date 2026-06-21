// Package persistence is the CLI's composition-root adapter for the local durable
// journal: it starts an embedded, in-process NATS JetStream server (no TCP socket) over
// a persistent on-disk StoreDir and hands back a bound JetStreamContext the session
// journal writes through. It is the ONLY package that imports nats-server/v2/server —
// the embedded server is a composition concern, never a journal-package one (the journal
// takes a nats.JetStreamContext). One responsibility: own the embedded engine lifecycle
// (start, connect in-process, shut down cleanly).
package persistence

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const (
	// defaultDirName is the per-user urvi data directory under the home root. It matches
	// the existing ~/.urvi convention (where the CLI already writes urvi.log), so all
	// urvi state lives under one directory.
	defaultDirName = ".urvi"
	// jetstreamDirName is the subdirectory under the urvi data dir that holds the
	// embedded server's StoreDir (its file-backed streams + KV + object buckets).
	jetstreamDirName = "jetstream"
	// storeDirPerm is the StoreDir permission: owner-only (0700). The durable journal may
	// hold conversation content, so it is never group/world readable.
	storeDirPerm = 0o700
	// defaultSyncInterval is the embedded server's fsync cadence — the design's round-5
	// power-loss durability knob, set EXPLICITLY rather than left to the server default.
	// A conservative few seconds bounds the data lost on an OS crash / power loss to that
	// window without fsyncing on every append (which would dominate latency). It is a
	// deliberate durability/throughput trade-off for a local single-user CLI.
	defaultSyncInterval = 2 * time.Second
	// readyTimeout bounds how long Open waits for the embedded server to accept
	// connections before failing closed.
	readyTimeout = 10 * time.Second
)

// StoreDirError reports that the embedded server's StoreDir could not be resolved or
// created: an empty/unresolvable home, an empty data dir, a path that escapes the home
// root (traversal), or a mkdir failure. It fails closed — the engine never starts on an
// unconfined or unwritable StoreDir. Cause chains the underlying os error when present.
type StoreDirError struct {
	Path  string
	Cause error
}

func (e *StoreDirError) Error() string {
	msg := "persistence: invalid StoreDir"
	if e.Path != "" {
		msg += " " + e.Path
	}
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}
func (e *StoreDirError) Unwrap() error { return e.Cause }

// ServerStartError reports that the embedded JetStream server could not be created,
// became ready within the timeout, or could not be connected to in-process. It fails
// closed: without a live engine there is no durable journal.
type ServerStartError struct{ Cause error }

func (e *ServerStartError) Error() string {
	if e.Cause != nil {
		return "persistence: embedded server start failed: " + e.Cause.Error()
	}
	return "persistence: embedded server start failed"
}
func (e *ServerStartError) Unwrap() error { return e.Cause }

// errServerNotReady is the leaf cause when the embedded server does not accept
// connections within readyTimeout. A sentinel is permitted (no context fields).
var errServerNotReady = errors.New("persistence: embedded server not ready within timeout")

// EngineOptions configures the embedded engine. DataDir is the StoreDir (resolved +
// confined to the home root); SyncInterval is the explicit fsync cadence (the power-loss
// knob). A zero SyncInterval falls back to the conservative default.
type EngineOptions struct {
	DataDir      string
	SyncInterval time.Duration
}

// DefaultEngineOptions returns the production engine options: StoreDir at
// ~/.urvi/jetstream (overridable by $XDG_DATA_HOME → $XDG_DATA_HOME/urvi/jetstream) and
// the conservative explicit SyncInterval. It resolves the home/XDG root via os, failing
// closed (typed *StoreDirError) if neither is available.
func DefaultEngineOptions() (EngineOptions, error) {
	dir, err := defaultDataDir()
	if err != nil {
		return EngineOptions{}, err
	}
	return EngineOptions{DataDir: dir, SyncInterval: defaultSyncInterval}, nil
}

// defaultDataDir computes the default StoreDir. It honors $XDG_DATA_HOME when set
// ($XDG_DATA_HOME/urvi/jetstream); otherwise it falls back to ~/.urvi/jetstream, matching
// the existing ~/.urvi convention (urvi.log). It never returns an unconfined path: the
// XDG/home root is treated as the containment root.
func defaultDataDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "urvi", jetstreamDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &StoreDirError{Cause: err}
	}
	return filepath.Join(home, defaultDirName, jetstreamDirName), nil
}

// resolveStoreDir cleans dataDir and verifies it stays within root (the home/XDG
// containment root) — a fail-secure guard against a traversal-crafted DataDir escaping
// the user's own tree. It returns the cleaned, confined absolute-or-relative path (the
// same form root is in) or a typed *StoreDirError. It does NOT create the directory
// (Open does, after this check).
func resolveStoreDir(root, dataDir string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", &StoreDirError{Path: dataDir, Cause: errors.New("persistence: empty home root")}
	}
	if strings.TrimSpace(dataDir) == "" {
		return "", &StoreDirError{Cause: errors.New("persistence: empty data dir")}
	}
	cleanRoot := filepath.Clean(root)
	clean := filepath.Clean(dataDir)
	// Confinement: the cleaned path must be the root itself or live beneath it. A
	// rel-based check defeats ".." traversal that string-prefix checks miss.
	rel, err := filepath.Rel(cleanRoot, clean)
	if err != nil {
		return "", &StoreDirError{Path: clean, Cause: err}
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &StoreDirError{Path: clean, Cause: errors.New("persistence: StoreDir escapes the home root")}
	}
	return clean, nil
}

// Engine owns the embedded JetStream server, the in-process client connection, and the
// bound JetStreamContext. It is the composition-root handle the CLI builds once at
// startup and Closes once at shutdown. It is NOT safe for concurrent Close, but the CLI
// closes it exactly once on exit.
type Engine struct {
	srv *server.Server
	nc  *nats.Conn
	js  nats.JetStreamContext
}

// Open starts an embedded JetStream server on a confined StoreDir, connects to it
// in-process (no TCP), and returns a live Engine. The StoreDir is created 0700 if absent
// and is verified to stay within the home/XDG root (fail-secure against traversal). The
// SyncInterval is set explicitly on the server options (the power-loss knob); a zero
// value falls back to the conservative default. On any failure the partially-started
// server is shut down before returning the typed error, so Open never leaks a server.
func Open(opts EngineOptions) (*Engine, error) {
	root, err := containmentRoot()
	if err != nil {
		return nil, err
	}
	storeDir, err := resolveStoreDir(root, opts.DataDir)
	if err != nil {
		return nil, err
	}
	// #nosec G301 -- StoreDir is owner-only by design (may hold conversation content).
	if err := os.MkdirAll(storeDir, storeDirPerm); err != nil {
		return nil, &StoreDirError{Path: storeDir, Cause: err}
	}

	sync := opts.SyncInterval
	if sync <= 0 {
		sync = defaultSyncInterval
	}

	srv, err := server.NewServer(&server.Options{
		JetStream:    true,
		StoreDir:     storeDir,
		DontListen:   true, // in-process only — no TCP socket
		SyncInterval: sync, // explicit power-loss durability knob (design round 5)
		NoSigs:       true, // the CLI owns signal handling; the server must not install handlers
		NoLog:        true, // server logs would corrupt the TUI's stdout/scrollback
	})
	if err != nil {
		return nil, &ServerStartError{Cause: err}
	}
	go srv.Start()
	if !srv.ReadyForConnections(readyTimeout) {
		srv.Shutdown()
		return nil, &ServerStartError{Cause: errServerNotReady}
	}

	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		srv.Shutdown()
		return nil, &ServerStartError{Cause: err}
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		srv.Shutdown()
		return nil, &ServerStartError{Cause: err}
	}
	return &Engine{srv: srv, nc: nc, js: js}, nil
}

// containmentRoot is the directory the StoreDir must stay within: $XDG_DATA_HOME when
// set, else the user's home directory. It mirrors defaultDataDir's root selection so the
// confinement check matches the default path.
func containmentRoot() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return xdg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &StoreDirError{Cause: err}
	}
	return home, nil
}

// JetStream returns the bound JetStreamContext the session journal writes through. It is
// valid until Close.
func (e *Engine) JetStream() nats.JetStreamContext { return e.js }

// Close drains the client connection and shuts the embedded server down cleanly,
// flushing JetStream state to the StoreDir. It is best-effort and safe to call once at
// CLI exit; a drain error is returned but the server is always shut down.
func (e *Engine) Close() error {
	var err error
	if e.nc != nil {
		// Drain flushes pending publishes + unsubscribes before closing, so an in-flight
		// append is not lost on a clean exit. It closes the conn when done.
		err = e.nc.Drain()
	}
	if e.srv != nil {
		e.srv.Shutdown()
		e.srv.WaitForShutdown()
	}
	return err
}
