package persistence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	sessionRootDirName = "sessions"
	// xdgAppDirName is the app directory used beneath XDG_DATA_HOME. Unlike the home
	// fallback it is not a dot directory, matching the XDG base-directory convention.
	xdgAppDirName = "looprig"
)

var (
	errEmptySessionStoreRoot  = errors.New("empty session store root")
	errInvalidSessionID       = errors.New("invalid session ID")
	errSessionPathEscapesRoot = errors.New("session path escapes sessions root")
	errSessionStoreSymlink    = errors.New("session store path contains a symlink")
	errSessionStoreNotDir     = errors.New("session store path is not a directory")
)

// SessionStoreOperation identifies the session-store action that failed.
type SessionStoreOperation string

const (
	// SessionStoreOpen creates and validates the private sessions root.
	SessionStoreOpen SessionStoreOperation = "open session store root"
	// SessionStoreResolve resolves a session ID to its confined directory.
	SessionStoreResolve SessionStoreOperation = "resolve session directory"
	// SessionStoreCreate creates a private session directory.
	SessionStoreCreate SessionStoreOperation = "create session directory"
)

// SessionStoreError reports a confined session-store operation failure. Path is always a
// derived local filesystem path; it never contains session content or credentials.
type SessionStoreError struct {
	Operation SessionStoreOperation
	Path      string
	Cause     error
}

func (e *SessionStoreError) Error() string {
	message := "persistence: session store"
	if e.Operation != "" {
		message += " " + string(e.Operation)
	}
	if e.Path != "" {
		message += " " + e.Path
	}
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

// Unwrap returns the underlying filesystem or validation error.
func (e *SessionStoreError) Unwrap() error { return e.Cause }

// SessionStoreRoot confines session directories beneath one private, user-local root.
// The fields remain unexported so callers can resolve only typed UUID session IDs.
type SessionStoreRoot struct {
	appDir      string
	sessionsDir string
}

// OpenSessionStoreRoot creates and validates the private directory used to contain local
// session directories. It honours XDG_DATA_HOME when set, otherwise it falls back to the
// user's home directory and the existing ~/.looprig convention.
func OpenSessionStoreRoot() (*SessionStoreRoot, error) {
	home, homeErr := os.UserHomeDir()
	xdg := os.Getenv("XDG_DATA_HOME")
	if strings.TrimSpace(xdg) == "" && homeErr != nil {
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Cause: homeErr}
	}

	root, appDirName, err := sessionDataRootFrom(xdg, home)
	if err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Cause: err}
	}
	return openSessionStoreRootAt(root, appDirName)
}

// openSessionStoreRootAt creates and validates <root>/<appDirName>/sessions, normalising
// owner-only modes and rejecting any symlink in the components the feature controls. It is
// the testable seam beneath OpenSessionStoreRoot.
func openSessionStoreRootAt(root, appDirName string) (*SessionStoreRoot, error) {
	if err := ensureRootDirectory(root); err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Path: root, Cause: err}
	}

	appDir, err := confinedChild(root, appDirName)
	if err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Path: root, Cause: err}
	}
	if err := ensurePrivateDirectory(appDir); err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Path: appDir, Cause: err}
	}

	sessionsDir, err := confinedChild(appDir, sessionRootDirName)
	if err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Path: appDir, Cause: err}
	}
	if err := ensurePrivateDirectory(sessionsDir); err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Path: sessionsDir, Cause: err}
	}

	return &SessionStoreRoot{appDir: appDir, sessionsDir: sessionsDir}, nil
}

// SessionDir returns the directory for a non-zero session ID. It never accepts a caller
// supplied path component: UUID.String produces the canonical directory name.
func (r *SessionStoreRoot) SessionDir(id uuid.UUID) (string, error) {
	if id == uuid.Nil {
		return "", &SessionStoreError{Operation: SessionStoreResolve, Cause: errInvalidSessionID}
	}
	if err := r.validate(); err != nil {
		return "", &SessionStoreError{Operation: SessionStoreResolve, Cause: err}
	}

	dir, err := confinedChild(r.sessionsDir, id.String())
	if err != nil {
		return "", &SessionStoreError{Operation: SessionStoreResolve, Path: r.sessionsDir, Cause: err}
	}
	if err := validateExistingDirectory(dir); err != nil {
		return "", &SessionStoreError{Operation: SessionStoreResolve, Path: dir, Cause: err}
	}
	return dir, nil
}

// CreateSessionDir creates or validates a private directory for a non-zero session ID.
func (r *SessionStoreRoot) CreateSessionDir(id uuid.UUID) (string, error) {
	dir, err := r.SessionDir(id)
	if err != nil {
		return "", &SessionStoreError{Operation: SessionStoreCreate, Path: sessionIDPath(r, id), Cause: err}
	}
	if err := ensurePrivateDirectory(dir); err != nil {
		return "", &SessionStoreError{Operation: SessionStoreCreate, Path: dir, Cause: err}
	}
	return dir, nil
}

// sessionDataRootFrom resolves the absolute data root and app directory name from the
// supplied XDG_DATA_HOME and home values. XDG_DATA_HOME takes precedence and uses the
// plain "looprig" directory; the home fallback uses the existing ~/.looprig convention.
func sessionDataRootFrom(xdgDataHome, home string) (string, string, error) {
	if strings.TrimSpace(xdgDataHome) != "" {
		return cleanAbsolutePath(xdgDataHome, xdgAppDirName)
	}
	if strings.TrimSpace(home) == "" {
		return "", "", errEmptySessionStoreRoot
	}
	return cleanAbsolutePath(home, defaultDirName)
}

func cleanAbsolutePath(root, appDirName string) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", errEmptySessionStoreRoot
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", "", err
	}
	return absRoot, appDirName, nil
}

func (r *SessionStoreRoot) validate() error {
	if r == nil || r.appDir == "" || r.sessionsDir == "" {
		return errEmptySessionStoreRoot
	}
	expectedSessions, err := confinedChild(r.appDir, sessionRootDirName)
	if err != nil {
		return err
	}
	if filepath.Clean(r.sessionsDir) != expectedSessions {
		return errSessionPathEscapesRoot
	}
	if err := validateExistingDirectory(r.appDir); err != nil {
		return err
	}
	return validateExistingDirectory(r.sessionsDir)
}

func confinedChild(root, child string) (string, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(child) == "" {
		return "", errEmptySessionStoreRoot
	}
	if filepath.Base(child) != child || child == "." || child == string(filepath.Separator) {
		return "", errSessionPathEscapesRoot
	}

	cleanRoot := filepath.Clean(root)
	path := filepath.Join(cleanRoot, child)
	rel, err := filepath.Rel(cleanRoot, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errSessionPathEscapesRoot
	}
	return path, nil
}

// ensureRootDirectory validates the data root, creating only the components below the
// nearest existing ancestor. It rejects any symlink or non-directory it must traverse or
// create, but never chmods or rejects an already-existing real directory's mode, because
// the data root belongs to the user, not the feature.
func ensureRootDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errSessionStoreSymlink
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %s", errSessionStoreNotDir, path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	parent := filepath.Dir(path)
	if parent == path {
		return errEmptySessionStoreRoot
	}
	if err := ensureRootDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, storeDirPerm); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Mkdir(path, storeDirPerm); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(path)
		if err != nil {
			return err
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errSessionStoreSymlink
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s", errSessionStoreNotDir, path)
	}
	if err := os.Chmod(path, storeDirPerm); err != nil {
		return err
	}
	return nil
}

func validateExistingDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errSessionStoreSymlink
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s", errSessionStoreNotDir, path)
	}
	return nil
}

func sessionIDPath(r *SessionStoreRoot, id uuid.UUID) string {
	if r == nil || r.sessionsDir == "" || id == uuid.Nil {
		return ""
	}
	return filepath.Join(r.sessionsDir, id.String())
}
