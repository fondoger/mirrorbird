package platform

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
)

// LaunchError labels a failed launch stage and preserves err for errors.Is/As.
// On Windows it includes the Win32 code and distinguishes access denial from
// explicit antivirus detection/deletion. It returns nil when err is nil.
// Use fixed stage labels and errors without arguments, environment values, or
// captured output; the diagnostic may include an executable path and is local,
// not protocol output.
func LaunchError(stage string, err error) error {
	if err == nil {
		return nil
	}
	var code syscall.Errno
	if runtime.GOOS != "windows" || !errors.As(err, &code) {
		return fmt.Errorf("%s failed: %w", stage, err)
	}
	var cause string
	switch code {
	case 2:
		cause = "ERROR_FILE_NOT_FOUND; file not found"
	case 3:
		cause = "ERROR_PATH_NOT_FOUND; path not found"
	case 5:
		cause = "ERROR_ACCESS_DENIED; access denied"
	case 193:
		cause = "ERROR_BAD_EXE_FORMAT; not a valid Windows executable"
	case 225:
		cause = "ERROR_VIRUS_INFECTED; Windows reports antivirus detection"
	case 226:
		cause = "ERROR_VIRUS_DELETED; Windows reports antivirus deletion of the file"
	}
	if cause == "" {
		return fmt.Errorf("%s failed (Win32 %d): %w", stage, code, err)
	}
	return fmt.Errorf("%s failed (Win32 %d: %s): %w", stage, code, cause, err)
}
