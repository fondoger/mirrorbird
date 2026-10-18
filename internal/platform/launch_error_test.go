package platform

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestLaunchErrorNilAndPlainError(t *testing.T) {
	if err := LaunchError("desktop launch (Start)", nil); err != nil {
		t.Fatalf("successful launch returned %v", err)
	}
	cause := errors.New("unavailable")
	err := LaunchError("desktop launch (Start)", cause)
	if got, want := err.Error(), "desktop launch (Start) failed: unavailable"; got != want {
		t.Fatalf("diagnostic = %q; want %q", got, want)
	}
	if !errors.Is(err, cause) || errors.Unwrap(err) != cause {
		t.Fatal("launch error lost its original cause")
	}
}

func TestLaunchErrorWin32CausesAndWrapping(t *testing.T) {
	for _, test := range []struct {
		code  syscall.Errno
		cause string
	}{
		{2, "ERROR_FILE_NOT_FOUND; file not found"},
		{3, "ERROR_PATH_NOT_FOUND; path not found"},
		{5, "ERROR_ACCESS_DENIED; access denied"},
		{193, "ERROR_BAD_EXE_FORMAT; not a valid Windows executable"},
		{225, "ERROR_VIRUS_INFECTED; Windows reports antivirus detection"},
		{226, "ERROR_VIRUS_DELETED; Windows reports antivirus deletion of the file"},
		{32, ""}, // Even an unmapped Win32 error must retain its numeric code.
	} {
		t.Run(fmt.Sprint(uint32(test.code)), func(t *testing.T) {
			pathErr := &os.PathError{Op: "fork/exec", Path: `C:\Program Files\WindowsApps\example\codex.exe`, Err: test.code}
			cause := fmt.Errorf("original launch: %w", pathErr)
			err := LaunchError("backend launch (Start)", cause)
			want := "backend launch (Start) failed"
			if runtime.GOOS == "windows" {
				if test.cause == "" {
					want += fmt.Sprintf(" (Win32 %d)", test.code)
				} else {
					want += fmt.Sprintf(" (Win32 %d: %s)", test.code, test.cause)
				}
			}
			want += ": " + cause.Error()
			if got := err.Error(); got != want {
				t.Fatalf("diagnostic = %q; want %q", got, want)
			}
			if errors.Unwrap(err) != cause || !errors.Is(err, test.code) {
				t.Fatal("launch error lost its wrapped cause or errno")
			}
			var code syscall.Errno
			var gotPathErr *os.PathError
			if !errors.As(err, &code) || code != test.code || !errors.As(err, &gotPathErr) || gotPathErr != pathErr {
				t.Fatal("original typed errors cannot be recovered")
			}
			if test.code == 5 {
				text := strings.ToLower(err.Error())
				if strings.Contains(text, "virus") || strings.Contains(text, "malware") {
					t.Fatal("access denied was misdiagnosed as antivirus detection")
				}
			}
		})
	}
}
