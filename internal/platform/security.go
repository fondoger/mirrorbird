package platform

import (
	"errors"
	"path/filepath"
	"strings"
)

// SecureDirectory restricts an existing, app-owned directory to the current
// user. The path must be absolute, and its ancestors must already be trusted
// (for example, the user's profile directory). Call os.MkdirAll first, then
// SecureDirectory BEFORE creating any capability files or private children.
// Never continue writing secrets if it fails.
//
// On Windows it requires the current user to own the directory and installs a
// protected DACL with a single inheritable full-access grant to that user. It
// does not follow a final-component reparse point or take ownership of another
// user's directory. It does not remove explicit access grants on
// pre-existing children; it is not a recursive repair of an untrusted tree.
// On other platforms it sets mode 0700 and rejects a final-component symlink.
func SecureDirectory(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return errors.New("secure directory requires an absolute path without NUL")
	}
	return secureDirectory(filepath.Clean(path))
}
