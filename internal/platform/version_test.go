package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const versionTestEnv = "MIRRORBIRD_PLATFORM_VERSION_TEST"

// Version probes run the test executable itself; no installed tool, shell, or
// download is required. The helper is gated by both an exact argument and env.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--version" && os.Getenv(versionTestEnv) != "" {
		switch os.Getenv(versionTestEnv) {
		case "ok":
			fmt.Fprint(os.Stdout, "\r\n  mirrorbird-test 1.2.3  \r\nsecond line\n")
		case "stderr":
			fmt.Fprintln(os.Stderr, "mirrorbird-test 1.2.3")
		case "empty":
			fmt.Fprint(os.Stdout, " \r\n\t")
		case "failure":
			fmt.Fprintln(os.Stdout, "private-version-probe-output")
			fmt.Fprintln(os.Stderr, "private-version-probe-error")
			os.Exit(19)
		case "overflow":
			fmt.Fprint(os.Stdout, strings.Repeat("x", versionOutputLimit+1))
		case "control":
			fmt.Fprintln(os.Stdout, "version\x1b[31m1.0")
		case "invalid-utf8":
			os.Stdout.Write([]byte{0xff})
		case "sleep":
			time.Sleep(30 * time.Second)
		default:
			os.Exit(20)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExecutableVersion(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"ok", "stderr"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(versionTestEnv, mode)
			version, err := ExecutableVersion(context.Background(), exe)
			if err != nil || version != "mirrorbird-test 1.2.3" {
				t.Fatalf("ExecutableVersion = %q, %v", version, err)
			}
		})
	}
	for _, mode := range []string{"empty", "failure", "overflow", "control", "invalid-utf8"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(versionTestEnv, mode)
			version, err := ExecutableVersion(context.Background(), exe)
			if err == nil || version != "" {
				t.Fatalf("invalid version returned %q, %v", version, err)
			}
			if strings.Contains(err.Error(), "private-version-probe") {
				t.Fatal("captured output leaked into error")
			}
		})
	}
}

func TestExecutableVersionPathWithSpacesAndMetacharacters(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Copy only our own test binary into a temporary directory, then prove
	// spaces, shell metacharacters, and Unicode are treated as a literal path.
	path := filepath.Join(t.TempDir(), "version helper ;& 雀.exe")
	source, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	t.Setenv(versionTestEnv, "ok")
	version, err := ExecutableVersion(context.Background(), path)
	if err != nil || version != "mirrorbird-test 1.2.3" {
		t.Fatalf("literal executable path failed: %q, %v", version, err)
	}
}

func TestExecutableVersionContextAndPath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "tool.exe", "./tool.exe", "C:tool.exe", exe + "\x00"} {
		if version, err := ExecutableVersion(context.Background(), path); err == nil || version != "" {
			t.Fatalf("invalid path accepted: %q", path)
		}
	}
	if _, err := ExecutableVersion(nil, exe); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExecutableVersion(ctx, exe); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query returned %v", err)
	}
	t.Setenv(versionTestEnv, "sleep")
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := ExecutableVersion(ctx, exe); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out query returned %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("version query did not honor its context promptly")
	}
	missing := filepath.Join(t.TempDir(), "missing.exe")
	if version, err := ExecutableVersion(context.Background(), missing); err == nil || version != "" {
		t.Fatal("missing executable did not return an error")
	}
}

func TestVersionCaptureBounded(t *testing.T) {
	var canceled atomic.Bool
	capture := &versionCapture{cancel: func() { canceled.Store(true) }}
	for _, size := range []int{versionOutputLimit, 1, 16 * versionOutputLimit} {
		payload := strings.Repeat("x", size)
		if n, err := capture.Write([]byte(payload)); n != len(payload) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
		text, overflow := capture.result()
		if len(text) > versionOutputLimit {
			t.Fatalf("capture exceeded limit: %d", len(text))
		}
		if size == versionOutputLimit && (overflow || canceled.Load()) {
			t.Fatal("an exact-limit write must not overflow")
		}
	}
	text, overflow := capture.result()
	if len(text) != versionOutputLimit || !overflow || !canceled.Load() {
		t.Fatal("oversized output was not bounded and canceled")
	}
}

func TestVersionCaptureConcurrent(t *testing.T) {
	capture := &versionCapture{}
	var writers sync.WaitGroup
	for range 16 {
		writers.Go(func() {
			for range 10 {
				capture.Write([]byte("abc"))
			}
		})
	}
	writers.Wait()
	text, overflow := capture.result()
	if overflow || len(text) != 16*10*3 {
		t.Fatal("concurrent writes lost output")
	}
}
