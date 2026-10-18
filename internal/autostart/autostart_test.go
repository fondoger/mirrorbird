package autostart

import (
	"errors"
	"path/filepath"
	"testing"
)

// No test calls Apply or touches a registry hive. All mutations below are to
// this in-memory fake, including preserving/rolling back an existing Run value.
type fakeStore struct {
	value    value
	readErr  error
	writeErr error
	writes   int
}

func (s *fakeStore) read() (value, error) { return s.value, s.readErr }
func (s *fakeStore) write(v value) error {
	s.writes++
	if s.writeErr != nil {
		return s.writeErr
	}
	s.value = v
	return nil
}

func TestCommandQuotesExecutableAndAddsOnlyBackground(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "Mirror Bird", "mirrorbird.exe")
	command, err := Command(executable)
	want := `"` + executable + `" --background`
	if err != nil || command != want {
		t.Fatalf("command=%q err=%v, want %q", command, err, want)
	}
}

func TestCommandRejectsNonAbsoluteOrInjectedPaths(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"", "mirrorbird.exe", "./mirrorbird.exe", filepath.Join(root, "mirrorbird.exe") + `" --evil`, filepath.Join(root, "bad\nname.exe"), filepath.Join(root, "mirrorbird.cmd"), " " + filepath.Join(root, "mirrorbird.exe")} {
		if _, err := Command(path); err == nil {
			t.Errorf("unsafe command accepted: %q", path)
		}
	}
}

func TestApplyAndRollbackPreserveOldRunValue(t *testing.T) {
	old := value{text: `%LOCALAPPDATA%\MirrorBird\old.exe --background`, exists: true, expand: true}
	s := &fakeStore{value: old}
	executable := filepath.Join(t.TempDir(), "mirrorbird.exe")
	undo, err := apply(s, executable, true)
	if err != nil {
		t.Fatal(err)
	}
	command, _ := Command(executable)
	if s.value.text != command || s.value.expand || !s.value.exists {
		t.Fatalf("wrong new value: %+v", s.value)
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	if s.value != old {
		t.Fatalf("old value not restored: %+v", s.value)
	}
}

func TestDisableOnlyDeletesOwnValueAndCanRollback(t *testing.T) {
	old := value{text: `"C:\MirrorBird\mirrorbird.exe" --background`, exists: true}
	s := &fakeStore{value: old}
	undo, err := apply(s, filepath.Join(t.TempDir(), "mirrorbird.exe"), false)
	if err != nil || s.value.exists {
		t.Fatalf("disable: %+v %v", s.value, err)
	}
	if err := undo(); err != nil || s.value != old {
		t.Fatalf("rollback: %+v %v", s.value, err)
	}
	if keyPath != `Software\Microsoft\Windows\CurrentVersion\Run` || valueName != "MirrorBird" {
		t.Fatal("wrong per-user autorun target")
	}
}

func TestRollbackDoesNotClobberLaterUserChange(t *testing.T) {
	s := &fakeStore{}
	undo, err := apply(s, filepath.Join(t.TempDir(), "mirrorbird.exe"), true)
	if err != nil {
		t.Fatal(err)
	}
	user := value{text: "user changed this", exists: true}
	s.value = user
	if err := undo(); err == nil || s.value != user {
		t.Fatal("rollback overwrote concurrent edit")
	}
}

func TestRegistryErrorsNeverProduceUndoOrOverwrite(t *testing.T) {
	for _, readFailure := range []bool{true, false} {
		t.Run(map[bool]string{true: "read", false: "write"}[readFailure], func(t *testing.T) {
			old := value{text: "old", exists: true}
			s := &fakeStore{value: old}
			if readFailure {
				s.readErr = errors.New("denied")
			} else {
				s.writeErr = errors.New("denied")
			}
			undo, err := apply(s, filepath.Join(t.TempDir(), "mirrorbird.exe"), true)
			if err == nil || undo != nil || s.value != old {
				t.Fatal("failed write changed value")
			}
		})
	}
}

func TestAlreadyDisabledMakesNoWrite(t *testing.T) {
	s := &fakeStore{}
	undo, err := apply(s, filepath.Join(t.TempDir(), "mirrorbird.exe"), false)
	if err != nil || s.writes != 0 {
		t.Fatal("no-op changed registry")
	}
	if err := undo(); err != nil || s.writes != 0 {
		t.Fatal("no-op undo changed registry")
	}
}
