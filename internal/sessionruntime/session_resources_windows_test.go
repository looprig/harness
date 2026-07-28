//go:build windows

package sessionruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/looprig/core/uuid"
	"golang.org/x/sys/windows"
)

func TestWindowsResourceStorageNewAndRestoreUseOwnerOnlySecurityDescriptors(t *testing.T) {
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	root := filepath.Join(t.TempDir(), "resources")
	resolve := func(context.Context, uuid.UUID) (string, string, error) {
		return root, "owner", nil
	}

	created, err := resolveSessionResources(context.Background(), id, resolve, "", false)
	if err != nil {
		t.Fatalf("new resolveSessionResources() error = %v", err)
	}
	if created.storageRoot != root {
		t.Fatalf("new storage root = %q, want %q", created.storageRoot, root)
	}
	assertWindowsOwnerOnlyPath(t, root)
	assertWindowsOwnerOnlyPath(t, filepath.Join(root, sessionResourceAnchorName))

	restored, err := resolveSessionResources(context.Background(), id, resolve, "", true)
	if err != nil {
		t.Fatalf("restore resolveSessionResources() error = %v", err)
	}
	if restored.storageRoot != root {
		t.Fatalf("restore storage root = %q, want %q", restored.storageRoot, root)
	}
	assertWindowsOwnerOnlyPath(t, root)
	assertWindowsOwnerOnlyPath(t, filepath.Join(root, sessionResourceAnchorName))
}

func TestWindowsResourceStorageRejectsCaseAlias(t *testing.T) {
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	base := t.TempDir()
	workspace := filepath.Join(base, "Workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatalf("Mkdir(workspace) error = %v", err)
	}

	_, err = resolveSessionResources(
		context.Background(),
		id,
		func(context.Context, uuid.UUID) (string, string, error) {
			return filepath.Join(base, "wORKSPACE"), "owner", nil
		},
		workspace,
		false,
	)
	var storageErr *SessionResourceStorageError
	if !errors.As(err, &storageErr) || storageErr.Kind != SessionResourceStorageWorkspaceOverlap {
		t.Fatalf("resolveSessionResources() error = %T %v, want workspace_overlap", err, err)
	}
}

func assertWindowsOwnerOnlyPath(t *testing.T, path string) {
	t.Helper()

	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() error = %v", err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%q) error = %v", path, err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatalf("Owner(%q) error = %v", path, err)
	}
	if owner == nil || !owner.Equals(tokenUser.User.Sid) {
		t.Fatalf("owner(%q) = %v, want current token user %v", path, owner, tokenUser.User.Sid)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("Control(%q) error = %v", path, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL(%q) is not protected", path)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("DACL(%q) error = %v", path, err)
	}
	if dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("DACL(%q) ACE count = %v, want exactly one", path, dacl)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatalf("GetAce(%q) error = %v", path, err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		t.Fatalf("DACL(%q) ACE type = %d, want allow", path, ace.Header.AceType)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !sid.Equals(tokenUser.User.Sid) {
		t.Fatalf("DACL(%q) trustee = %v, want current token user %v", path, sid, tokenUser.User.Sid)
	}
	if ace.Mask&windows.GENERIC_ALL == 0 {
		t.Fatalf("DACL(%q) mask = %#x, want GENERIC_ALL", path, ace.Mask)
	}
}
