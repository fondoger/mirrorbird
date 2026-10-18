//go:build !windows

package platform

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestUnsupportedOperationsFailClosed(t *testing.T) {
	if listener, err := ListenPipe("mirrorbird-test"); !errors.Is(err, ErrUnsupported) || listener != nil {
		t.Fatalf("ListenPipe = %v, %v", listener, err)
	}
	if conn, err := DialPipe(context.Background(), "mirrorbird-test"); !errors.Is(err, ErrUnsupported) || conn != nil {
		t.Fatalf("DialPipe = %v, %v", conn, err)
	}
	if cleanup, err := ContainCurrentProcess(); !errors.Is(err, ErrUnsupported) || cleanup != nil {
		t.Fatalf("ContainCurrentProcess did not fail closed: %v", err)
	}
}

func TestSecureDirectoryMode(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SecureDirectory(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o", info.Mode().Perm())
	}
}
