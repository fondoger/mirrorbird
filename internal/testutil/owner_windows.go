//go:build windows

package testutil

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TempDir returns a temporary directory owned by the process token's user.
func TempDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	SetCurrentUserOwner(t, path)
	return path
}

// SetCurrentUserOwner sets only the owner of a test-owned file or directory.
// Elevated Windows tokens may default to BUILTIN\Administrators as the owner
// of new objects, independently of their parent's owner and inherited DACL.
// Call this before removing WRITE_OWNER from a fixture's DACL.
func SetCurrentUserOwner(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatalf("set test fixture owner for %q: %v", path, err)
	}
}
