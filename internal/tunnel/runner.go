package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mirrorbird/internal/platform"
)

// commandRunner is deliberately small: tests never need an account, network,
// shell, or installed CLI. Start must stop its own child when ctx is cancelled;
// Wait must reap that child, and must be called exactly once.
type commandRunner interface {
	Resolve(string) (string, error)
	Run(context.Context, string, ...string) ([]byte, error)
	Start(context.Context, string, io.Writer, ...string) (hostProcess, error)
}

type hostProcess interface {
	Wait() error
}

type execRunner struct{}

const maxOutput = 1 << 20

var errOutputLimit = errors.New("CLI output exceeded safety limit")

// Only a locally installed Dev Tunnels executable is accepted, not a command
// line, script, relative path, or another program selected by a remote caller.
// As with any executable setting, the local user must trust the installed file.
func validateExecutable(executable string) error {
	if executable == "" || executable == "devtunnel" || (runtime.GOOS == "windows" && strings.EqualFold(executable, "devtunnel.exe")) {
		return nil
	}
	if !filepath.IsAbs(executable) || strings.HasPrefix(executable, `\\`) || strings.HasPrefix(executable, "//") || strings.TrimSpace(executable) != executable || strings.ContainsAny(executable, "\x00\r\n\"") {
		return ErrExecutable
	}
	name := filepath.Base(executable)
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(name, "devtunnel.exe") {
			return ErrExecutable
		}
	} else if name != "devtunnel" {
		return ErrExecutable
	}
	return nil
}

func (execRunner) Resolve(executable string) (string, error) {
	if err := validateExecutable(executable); err != nil {
		return "", err
	}
	if executable == "" {
		executable = "devtunnel"
	}
	path, err := exec.LookPath(executable)
	if err != nil || !filepath.IsAbs(path) {
		return "", ErrExecutable
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil || validateExecutable(path) != nil {
		return "", ErrExecutable
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", ErrExecutable
	}
	return path, nil
}

func newCommand(ctx context.Context, executable string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = childEnvironment(os.Environ())
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	platform.HideWindow(cmd)
	return cmd
}

// Inherit only OS/account/browser/proxy essentials. In particular, unrelated
// app credentials, tunnel service overrides, and runtime injection variables
// (DOTNET_STARTUP_HOOKS, LD_PRELOAD, NODE_OPTIONS, etc.) never reach the CLI.
func childEnvironment(parent []string) []string {
	allowed := map[string]bool{
		"SYSTEMROOT": true, "WINDIR": true, "SYSTEMDRIVE": true,
		"PATH": true, "PATHEXT": true, "COMSPEC": true,
		"USERPROFILE": true, "HOME": true, "HOMEDRIVE": true, "HOMEPATH": true,
		"LOCALAPPDATA": true, "APPDATA": true, "PROGRAMDATA": true,
		"PROGRAMFILES": true, "PROGRAMFILES(X86)": true, "PROGRAMW6432": true,
		"TEMP": true, "TMP": true, "TMPDIR": true,
		"DISPLAY": true, "WAYLAND_DISPLAY": true, "XDG_RUNTIME_DIR": true,
		"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "DBUS_SESSION_BUS_ADDRESS": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true,
	}
	env := make([]string, 0, len(allowed)+2)
	for _, item := range parent {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[strings.ToUpper(key)] {
			env = append(env, item)
		}
	}
	return append(env, "DOTNET_CLI_UI_LANGUAGE=en-US", "NO_COLOR=1")
}

type limitedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxOutput - b.Len()
	if n > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	// Drain excess output rather than blocking the process or retaining secrets.
	return n, nil
}

func (execRunner) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	cmd := newCommand(ctx, executable, args...)
	var output limitedOutput
	cmd.Stdout = &output
	err := cmd.Run()
	if output.overflow {
		return nil, errOutputLimit
	}
	return output.Bytes(), err
}

func (execRunner) Start(ctx context.Context, executable string, stdout io.Writer, args ...string) (hostProcess, error) {
	cmd := newCommand(ctx, executable, args...)
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// Raw CLI output and error strings may contain credentials or device codes.
// Only a fixed operation label, context cause, or numeric exit code escapes.
func commandError(action string, ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("Dev Tunnels %s: %w", action, ctx.Err())
	}
	if errors.Is(err, errOutputLimit) {
		return fmt.Errorf("%w: %s output exceeded safety limit", ErrCommand, action)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%w: %s exited with code %d; check the CLI locally", ErrCommand, action, exit.ExitCode())
	}
	return fmt.Errorf("%w: %s; check the CLI locally", ErrCommand, action)
}
