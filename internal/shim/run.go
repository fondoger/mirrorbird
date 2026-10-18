package shim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/bridge"
	"mirrorbird/internal/platform"
)

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	real := os.Getenv(attachment.EnvRealCodex)
	if real == "" || !filepath.IsAbs(real) {
		fmt.Fprintln(stderr, "MirrorBird: MIRRORBIRD_REAL_CODEX must name the original absolute Codex executable")
		return 2
	}
	realInfo, err := os.Stat(real)
	if err != nil {
		fmt.Fprintf(stderr, "MirrorBird: %v\n", platform.LaunchError("backend executable availability check", err))
		return 2
	}
	if realInfo.IsDir() {
		fmt.Fprintln(stderr, "MirrorBird: backend executable availability check failed: configured path is a directory, not an executable")
		return 2
	}
	self, _ := os.Executable()
	selfInfo, _ := os.Stat(self)
	if selfInfo != nil && os.SameFile(selfInfo, realInfo) {
		fmt.Fprintln(stderr, "MirrorBird: refusing recursive Codex proxy configuration")
		return 2
	}
	// The job handle lives until process exit; Desktop may hard-kill this shim.
	if _, err := platform.ContainCurrentProcess(); err != nil {
		fmt.Fprintf(stderr, "MirrorBird: %v\n", platform.LaunchError("backend process containment", err))
		return 2
	}
	cmd := exec.Command(real, args...)
	cmd.Env = backendEnvironment(os.Environ(), real)
	platform.HideWindow(cmd)
	if !IsStdioServer(args) {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		if err := startBackend(cmd, stderr); err != nil {
			return exitCode(err)
		}
		return exitCode(cmd.Wait())
	}
	home, err := attachment.Home()
	if err != nil {
		fmt.Fprintln(stderr, "MirrorBird: invalid application data directory")
		return 2
	}
	upstream, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(stderr, "MirrorBird: cannot open backend input")
		return 2
	}
	downstream, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(stderr, "MirrorBird: cannot open backend output")
		return 2
	}
	cmd.Stderr = stderr
	if err = startBackend(cmd, stderr); err != nil {
		return 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := bridge.New(upstream, stdout)
	defer mux.Close(io.EOF)
	userID, userErr := platform.CurrentUserID()
	metadata, metadataErr := attachment.NewMetadata(userID, real, cmd.Process.Pid)
	if userErr == nil && metadataErr == nil {
		listener, listenErr := platform.ListenPipe(metadata.Pipe)
		if listenErr == nil {
			defer listener.Close()
			path, saveErr := attachment.Save(home, metadata)
			if saveErr == nil {
				defer os.Remove(path)
				host := &attachment.Host{Mux: mux, Metadata: metadata}
				go func() { _ = host.Serve(ctx, listener) }()
			} else {
				_ = listener.Close()
				fmt.Fprintf(stderr, "MirrorBird: private attachment metadata unavailable; Desktop remains connected: %v\n", saveErr)
			}
		} else {
			fmt.Fprintln(stderr, "MirrorBird: local attachment unavailable; Desktop remains connected")
		}
	} else {
		fmt.Fprintln(stderr, "MirrorBird: local identity unavailable; Desktop remains connected")
	}
	inputDone := make(chan error, 1)
	outputDone := make(chan error, 1)
	go func() {
		err := bridge.ReadFrames(stdin, bridge.MaxFrameBytes, mux.FromDesktop)
		_ = upstream.Close()
		inputDone <- err
	}()
	go func() { outputDone <- bridge.ReadFrames(downstream, bridge.MaxFrameBytes, mux.FromServer) }()
	select {
	case err = <-outputDone:
		if err != nil {
			_ = cmd.Process.Kill()
		}
	case <-inputDone:
		select {
		case err = <-outputDone:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			err = <-outputDone
		}
	}
	mux.Close(io.EOF)
	waitErr := cmd.Wait()
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(stderr, "MirrorBird: backend protocol stream ended unexpectedly")
		return 1
	}
	return exitCode(waitErr)
}

// Keep launch diagnostics on stderr, never on the backend's JSON stdout. Do not
// format cmd itself: its arguments and environment can contain credentials.
func startBackend(cmd *exec.Cmd, stderr io.Writer) error {
	err := platform.LaunchError("backend launch (Start)", cmd.Start())
	if err != nil {
		fmt.Fprintf(stderr, "MirrorBird: %v\n", err)
	}
	return err
}

func backendEnvironment(env []string, real string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "CODEX_CLI_PATH") || strings.HasPrefix(strings.ToUpper(name), "MIRRORBIRD_") {
			continue
		}
		out = append(out, entry)
	}
	// Child tools must find the original CLI, not recursively invoke this shim.
	return append(out, "CODEX_CLI_PATH="+real)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		return exit.ExitCode()
	}
	return 1
}
