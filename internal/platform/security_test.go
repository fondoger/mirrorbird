package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentUserIDStable(t *testing.T) {
	first, err := CurrentUserID()
	if err != nil || first == "" {
		t.Fatalf("CurrentUserID = %q, %v", first, err)
	}
	second, err := CurrentUserID()
	if err != nil || second != first {
		t.Fatalf("CurrentUserID changed: %q, %v", second, err)
	}
}

func TestSecureDirectoryValidation(t *testing.T) {
	for _, path := range []string{"", "relative", ".", "C:relative", filepath.Join(t.TempDir(), "bad\x00path")} {
		if err := SecureDirectory(path); err == nil {
			t.Fatalf("invalid private directory path accepted: %q", path)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := SecureDirectory(missing); err == nil {
		t.Fatal("missing directory accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SecureDirectory(file); err == nil {
		t.Fatal("regular file accepted as directory")
	}
}

func TestSecureDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("directory symlink creation not available: %v", err)
	}
	if err := SecureDirectory(link); err == nil {
		t.Fatal("symlink accepted as private directory")
	}
	if err := SecureDirectory(link + string(filepath.Separator)); err == nil {
		t.Fatal("symlink with trailing separator accepted as private directory")
	}
}
