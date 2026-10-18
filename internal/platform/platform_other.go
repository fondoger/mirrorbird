//go:build !windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
)

func listenPipe(string) (net.Listener, error) {
	return nil, fmt.Errorf("Windows named pipes: %w", ErrUnsupported)
}

func dialPipe(context.Context, string) (net.Conn, error) {
	return nil, fmt.Errorf("Windows named pipes: %w", ErrUnsupported)
}

// CurrentUserID returns the numeric process UID on non-Windows systems.
func CurrentUserID() (string, error) {
	uid := os.Getuid()
	if uid < 0 {
		return "", fmt.Errorf("process UID: %w", ErrUnsupported)
	}
	return strconv.Itoa(uid), nil
}

// ContainCurrentProcess fails closed on systems without Windows Job Objects.
// It must not silently pretend an uncontained backend has lifetime protection.
func ContainCurrentProcess() (func(), error) {
	return nil, fmt.Errorf("Windows Job Object containment: %w", ErrUnsupported)
}

// HideWindow has no effect on non-Windows systems.
func HideWindow(*exec.Cmd) {}

func secureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect private directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private directory must be a directory, not a symlink")
	}
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open private directory: %w", err)
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened private directory: %w", err)
	}
	if !os.SameFile(info, opened) {
		return errors.New("private directory changed while opening")
	}
	if err := dir.Chmod(0o700); err != nil {
		return fmt.Errorf("restrict private directory access: %w", err)
	}
	return nil
}
