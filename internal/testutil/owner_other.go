//go:build !windows

package testutil

import "testing"

func TempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// SetCurrentUserOwner is unnecessary on non-Windows systems: test-created
// objects are already owned by the current user.
func SetCurrentUserOwner(t *testing.T, _ string) {
	t.Helper()
}
