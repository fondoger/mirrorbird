package attachment

import (
	"os"
	"path/filepath"
	"testing"

	"mirrorbird/internal/testutil"
)

func TestMetadataRoundtrip(t *testing.T) {
	home := testutil.TempDir(t)
	// Save requires a user-owned attachments directory. New objects do not
	// inherit their parent's owner on elevated Windows runners.
	dir := filepath.Join(home, "attachments")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	testutil.SetCurrentUserOwner(t, dir)
	m, err := NewMetadata("test-user", "original.exe", 1234)
	if err != nil {
		t.Fatal(err)
	}
	path, err := Save(home, m)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(home, "attachments") {
		t.Fatal(path)
	}
	entries, err := List(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Capability != m.Capability || entries[0].EnginePID != 1234 {
		t.Fatalf("invalid metadata roundtrip")
	}
	if err := os.WriteFile(filepath.Join(home, "attachments", "invalid.json"), []byte(`{"schema":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err = List(home)
	if err != nil || len(entries) != 1 {
		t.Fatal("invalid metadata was not ignored")
	}
}

func TestMetadataRejectsForeignPipe(t *testing.T) {
	m, err := NewMetadata("test-user", "original.exe", 1234)
	if err != nil {
		t.Fatal(err)
	}
	m.Pipe = `\\remote\pipe\mirrorbird-test`
	if m.Validate() == nil {
		t.Fatal("remote pipe accepted")
	}
}

func TestHomeRejectsRelativeOverride(t *testing.T) {
	t.Setenv(EnvHome, "relative")
	if _, err := Home(); err == nil {
		t.Fatal("relative data home accepted")
	}
}
