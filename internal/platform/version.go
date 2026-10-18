package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	versionOutputLimit = 4096
	versionTimeout     = 5 * time.Second
)

// ExecutableVersion runs a trusted, explicitly selected executable with the
// single argument --version, without a shell or PATH lookup. The executable
// path must be absolute. Helper windows are hidden on Windows.
//
// It returns the first nonempty output line after trimming whitespace. Combined
// stdout/stderr is limited to 4 KiB; overflow aborts the probe rather than
// returning a truncated version. Runtime is limited to five seconds (or ctx's
// earlier deadline), with at most one additional second to close inherited I/O
// pipes. Errors never include captured stdout or stderr.
//
// Like exec.CommandContext, cancellation kills the direct executable, not an
// arbitrary process tree. This is a version probe for trusted executables, not
// a sandbox for untrusted programs.
func ExecutableVersion(ctx context.Context, path string) (string, error) {
	if ctx == nil {
		return "", errors.New("executable version: nil context")
	}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return "", errors.New("executable version requires an absolute path without NUL")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	probeCtx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	output := &versionCapture{cancel: cancel}
	cmd := exec.CommandContext(probeCtx, path, "--version")
	HideWindow(cmd)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	text, overflow := output.result()
	if overflow {
		return "", fmt.Errorf("executable version output exceeds %d bytes", versionOutputLimit)
	}
	if ctxErr := probeCtx.Err(); ctxErr != nil {
		return "", fmt.Errorf("executable version: %w", ctxErr)
	}
	if err != nil {
		return "", fmt.Errorf("execute version query: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("executable returned no version")
	}
	if !utf8.ValidString(text) {
		return "", errors.New("executable returned a non-UTF-8 version")
	}
	line, _, _ := strings.Cut(text, "\n")
	line = strings.TrimSpace(line)
	for _, r := range line {
		if unicode.IsControl(r) {
			return "", errors.New("executable version contains control characters")
		}
	}
	return line, nil
}

// Always acknowledge the entire write, but never retain more than the limit.
// Cancel the executable on overflow. The lock also makes this safe for separate
// stdout/stderr copying goroutines, independent of exec.Cmd's writer sharing.
// Reading the result after Cmd.Run never exposes the buffer itself.
type versionCapture struct {
	mu       sync.Mutex
	data     []byte
	overflow bool
	cancel   context.CancelFunc
}

func (w *versionCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	remaining := versionOutputLimit - len(w.data)
	n := len(p)
	if n > remaining {
		n = remaining
		w.overflow = true
	}
	w.data = append(w.data, p[:n]...)
	overflow := w.overflow
	w.mu.Unlock()
	if overflow && w.cancel != nil {
		w.cancel()
	}
	return len(p), nil
}

func (w *versionCapture) result() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.data), w.overflow
}
