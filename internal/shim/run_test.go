package shim

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/platform"
)

func TestRunUnavailableBackendDiagnostics(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.exe")
	_, statErr := os.Stat(missing)
	if statErr == nil {
		t.Fatal("test executable must not exist")
	}
	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{"missing", missing, platform.LaunchError("backend executable availability check", statErr).Error()},
		{"directory", dir, "backend executable availability check failed: configured path is a directory, not an executable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(attachment.EnvRealCodex, test.path)
			t.Setenv("MIRRORBIRD_TEST_CREDENTIAL", "private-env-value")
			var stdout, stderr bytes.Buffer
			// Both paths return before containment or exec. Never call Run with a
			// valid executable in this test process: containment is irreversible.
			code := Run([]string{"app-server", "-c", "api_key=private-arg-value"}, strings.NewReader(""), &stdout, &stderr)
			if code != 2 {
				t.Fatalf("Run returned %d; want 2", code)
			}
			if stdout.Len() != 0 {
				t.Fatal("availability diagnostic polluted protocol stdout")
			}
			if got, want := stderr.String(), "MirrorBird: "+test.want+"\n"; got != want {
				t.Fatalf("stderr = %q; want %q", got, want)
			}
			if strings.Contains(stderr.String(), "private-") {
				t.Fatal("availability diagnostic leaked arguments or environment")
			}
		})
	}
}

func TestStartBackendDiagnostics(t *testing.T) {
	for _, args := range [][]string{
		{"app-server", "-c", "api_key=private-arg-value"},
		{"exec", "private-arg-value"},
	} {
		for _, code := range []syscall.Errno{2, 5, 193, 225, 226} {
			t.Run(fmt.Sprintf("%s/%d", args[0], code), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				pathErr := &os.PathError{Op: "fork/exec", Path: `C:\Program Files\WindowsApps\example\codex.exe`, Err: code}
				// Cmd.Err makes Start return before any process is created. This
				// exercises the same helper for server and passthrough launches
				// without launching Codex or containing the test runner.
				cmd := &exec.Cmd{
					Path:   pathErr.Path,
					Args:   append([]string{pathErr.Path}, args...),
					Env:    []string{"TEST_CREDENTIAL=private-env-value"},
					Stdout: &stdout,
					Stderr: &stderr,
					Err:    fmt.Errorf("process creation: %w", pathErr),
				}
				err := startBackend(cmd, &stderr)
				if !errors.Is(err, code) {
					t.Fatalf("start error lost errno %d: %v", code, err)
				}
				var gotPathErr *os.PathError
				if !errors.As(err, &gotPathErr) || gotPathErr != pathErr {
					t.Fatal("start error lost original path error")
				}
				if cmd.Process != nil {
					t.Fatal("fake failure unexpectedly started a process")
				}
				if stdout.Len() != 0 {
					t.Fatal("start diagnostic polluted protocol stdout")
				}
				if got, want := stderr.String(), "MirrorBird: "+platform.LaunchError("backend launch (Start)", cmd.Err).Error()+"\n"; got != want {
					t.Fatalf("stderr = %q; want %q", got, want)
				}
				if strings.Contains(stderr.String(), "private-") || strings.Contains(stderr.String(), "TEST_CREDENTIAL") {
					t.Fatal("start diagnostic leaked arguments or environment")
				}
				if got := exitCode(err); got != 1 {
					t.Fatalf("passthrough start exit code changed to %d; want 1", got)
				}
			})
		}
	}
}
