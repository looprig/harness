package tool

import "context"

// SessionResource is session-owned state shared by tool definitions. Activate
// late-binds live session services after construction and restore planning;
// Shutdown releases the resource during session teardown.
type SessionResource interface {
	Activate(context.Context, SessionResourceServices) error
	Shutdown(context.Context) error
}

// SessionResourceRegistry atomically resolves one session-owned resource by
// key. The factory receives a private storage directory reserved for that key.
type SessionResourceRegistry interface {
	GetOrCreate(context.Context, string, func(string) (SessionResource, error)) (SessionResource, error)
}

// SessionResourceServices is the late-bound service set supplied to every
// session resource after the live session has been constructed.
type SessionResourceServices struct{}

// ProcessBinding contains the session-scoped capabilities supplied to process
// tool definitions.
type ProcessBinding struct {
	Registry SessionResourceRegistry
}
