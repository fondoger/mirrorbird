package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrDesktopMissing = errors.New("ChatGPT Desktop was not found")
	ErrBackendMissing = errors.New("the selected Desktop backend is unavailable")
	ErrStagedBackend  = errors.New("Desktop's matching staged backend is missing or incomplete; open ChatGPT normally once to let it prepare its runtime")
)

// These are Desktop's native Windows runtime group, in its content-key order.
// The key only locates a directory: every file still needs a complete hash match.
var runtimeFiles = [...]string{
	"codex.exe",
	"codex-code-mode-host.exe",
	"codex-windows-sandbox-setup.exe",
	"codex-command-runner.exe",
}

type runtimeFile struct {
	name string
	size int64
	hash [sha256.Size]byte
}

// ResolveInstallation fills only missing explicit paths, always relative to the
// selected Desktop. Discovery never executes a backend or a version probe.
func ResolveInstallation(ctx context.Context, selected Installation) (Installation, error) {
	return resolveInstallation(ctx, selected, os.Getenv("LOCALAPPDATA"), discoverDesktop)
}

func resolveInstallation(ctx context.Context, selected Installation, localAppData string, discover func(context.Context) (Installation, error)) (Installation, error) {
	if err := ctx.Err(); err != nil {
		return Installation{}, err
	}
	if selected.Desktop == "" {
		found, err := discover(ctx)
		if err != nil {
			return Installation{}, err
		}
		selected.Desktop, selected.Version = found.Desktop, found.Version
	}
	if err := regularExecutable(selected.Desktop); err != nil {
		return Installation{}, fmt.Errorf("%w: select a valid absolute ChatGPT executable", ErrDesktopMissing)
	}
	if selected.Codex != "" {
		if err := regularExecutable(selected.Codex); err != nil {
			return Installation{}, fmt.Errorf("%w: check the configured codexExecutable", ErrBackendMissing)
		}
		if err := ctx.Err(); err != nil {
			return Installation{}, err
		}
		selected.BackendSource = "configured"
		return selected, nil
	}

	resourceRoot := filepath.Join(filepath.Dir(selected.Desktop), "resources")
	bundled := filepath.Join(resourceRoot, "codex.exe")
	if err := regularExecutable(bundled); err != nil {
		return Installation{}, fmt.Errorf("%w: no codex.exe in this Desktop's resources", ErrBackendMissing)
	}
	if !insideWindowsApps(bundled) {
		if err := ctx.Err(); err != nil {
			return Installation{}, err
		}
		selected.Codex, selected.BackendSource = bundled, "bundled"
		return selected, nil
	}
	if localAppData == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Installation{}, fmt.Errorf("%w: cannot locate the current user's runtime directory", ErrStagedBackend)
		}
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	if !filepath.IsAbs(localAppData) {
		return Installation{}, fmt.Errorf("%w: LOCALAPPDATA must be absolute", ErrStagedBackend)
	}
	backend, err := matchingStagedBackend(ctx, resourceRoot, filepath.Join(localAppData, "OpenAI", "Codex", "bin"))
	if err != nil {
		return Installation{}, err
	}
	selected.Codex, selected.BackendSource = backend, "desktop-staged"
	return selected, nil
}

func regularExecutable(path string) error {
	if !filepath.IsAbs(path) || strings.TrimSpace(path) != path || strings.ContainsAny(path, "\x00\r\n\"") || !strings.EqualFold(filepath.Ext(path), ".exe") {
		return errors.New("invalid executable path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular executable")
	}
	return nil
}

func insideWindowsApps(path string) bool {
	for _, segment := range strings.FieldsFunc(filepath.Clean(path), func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(segment, "WindowsApps") {
			return true
		}
	}
	return false
}

func matchingStagedBackend(ctx context.Context, resourceRoot, stageRoot string) (string, error) {
	group := make([]runtimeFile, 0, len(runtimeFiles))
	for _, name := range runtimeFiles {
		entry, err := runtimeFingerprint(ctx, filepath.Join(resourceRoot, name), name)
		if err != nil {
			return "", stagedError(ctx, "cannot read the selected Desktop runtime group")
		}
		group = append(group, entry)
	}
	directory := filepath.Join(stageRoot, runtimeKey(group))
	// Do not create or copy a runtime to work around a launch denial. Only use
	// the exact existing group prepared by Desktop's own installation resolver.
	for _, expected := range group {
		path := filepath.Join(directory, expected.name)
		if !containedRegularFile(stageRoot, path) {
			return "", stagedError(ctx, "the expected runtime is absent or redirects outside its directory")
		}
		actual, err := runtimeFingerprint(ctx, path, expected.name)
		if err != nil || actual.size != expected.size || actual.hash != expected.hash {
			return "", stagedError(ctx, "the existing runtime does not match this Desktop installation")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return filepath.Join(directory, "codex.exe"), nil
}

func stagedError(ctx context.Context, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrStagedBackend, reason)
}

func containedRegularFile(root, path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.Mode().IsRegular()
}

func runtimeFingerprint(ctx context.Context, path, name string) (runtimeFile, error) {
	if err := ctx.Err(); err != nil {
		return runtimeFile{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return runtimeFile{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > 1<<30 {
		return runtimeFile{}, errors.New("invalid runtime file")
	}
	h := sha256.New()
	reader := &contextReader{ctx: ctx, reader: io.LimitReader(file, before.Size()+1)}
	written, err := io.Copy(h, reader)
	if err != nil {
		return runtimeFile{}, err
	}
	after, err := file.Stat()
	if err != nil || written != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return runtimeFile{}, errors.New("runtime file changed during verification")
	}
	entry := runtimeFile{name: name, size: written}
	copy(entry.hash[:], h.Sum(nil))
	return entry, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func runtimeKey(group []runtimeFile) string {
	h := sha256.New()
	for _, entry := range group {
		_, _ = io.WriteString(h, entry.name)
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, hex.EncodeToString(entry.hash[:]))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
