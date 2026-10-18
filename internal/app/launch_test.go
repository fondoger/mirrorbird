package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mirrorbird/internal/desktop"
	"mirrorbird/internal/platform"
)

func TestMissingStagedBackendExplainsRecoveryWithoutLaunching(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.connected = false
	f.session.mu.Unlock()
	f.r.resolve = func(context.Context, desktop.Installation) (desktop.Installation, error) {
		return desktop.Installation{}, desktop.ErrStagedBackend
	}
	f.r.launch = func(desktop.Installation, string, string) error {
		t.Fatal("launch ran after missing staged backend")
		return nil
	}
	f.c.launchDesktop()
	waitIdle(t, f.c)
	if message := f.c.snapshot().Message; !strings.Contains(message, "匹配") || !strings.Contains(message, "开始菜单") {
		t.Fatal(message)
	}
}

func TestStartupChecksOldShimWithoutOpeningWeb(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.outdated = true
	f.session.mu.Unlock()
	f.c.refresh()
	waitIdle(t, f.c)
	state := f.c.snapshot()
	if !state.AttachmentOutdated || !strings.Contains(state.DesktopStatus, "旧版") || !strings.Contains(state.Message, "重启") {
		t.Fatal(state)
	}
	f.c.startSharing()
	waitIdle(t, f.c)
	if f.api.Sharing() || f.tunnel.starts != 0 {
		t.Fatal("old shim was allowed to start sharing")
	}
}

func TestUnattachedDesktopIsDetectedWithoutRestartingIt(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.connected = false
	f.session.connectErr = errors.New("no shim")
	f.session.mu.Unlock()
	probes := 0
	f.r.running = func(context.Context) (bool, error) { probes++; return true, nil }
	f.c.refresh()
	waitIdle(t, f.c)
	state := f.c.snapshot()
	if probes != 1 || !state.DesktopRunning || state.Connected || !strings.Contains(state.DesktopStatus, "未通过") {
		t.Fatal(probes, state)
	}
}

func TestConfirmedRestartRevokesSharingAndReconnectsWithoutReenabling(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.outdated = true
	f.session.mu.Unlock()
	base := t.TempDir()
	f.r.resolve = func(context.Context, desktop.Installation) (desktop.Installation, error) {
		return desktop.Installation{Desktop: filepath.Join(base, "ChatGPT.exe"), Codex: filepath.Join(base, "codex.exe")}, nil
	}
	restarts := 0
	f.r.restart = func(_ context.Context, installation desktop.Installation, home, shim string) error {
		restarts++
		if f.api.Sharing() || f.session.Connected() || home != f.r.home || filepath.Base(shim) != "mirrorbird-codex.exe" {
			t.Error("restart began before old admission/attachment was revoked")
		}
		f.session.mu.Lock()
		f.session.outdated = false
		f.session.mu.Unlock()
		return nil
	}
	f.c.mu.Lock()
	f.c.sampleLocked()
	f.c.state.Sharing = true
	f.c.mu.Unlock()
	f.api.SetSharing(true)
	f.c.restartDesktop()
	waitIdle(t, f.c)
	state := f.c.snapshot()
	if restarts != 1 || !state.Connected || state.AttachmentOutdated || state.Sharing || f.api.Sharing() {
		t.Fatal(restarts, state)
	}
}

func TestHealthyConnectionIgnoresStaleRestartAction(t *testing.T) {
	f := newFixture(t)
	f.c.restartDesktop()
	waitIdle(t, f.c)
	if !f.session.Connected() {
		t.Fatal("a healthy Desktop was disconnected by a stale restart action")
	}
}
func TestLaunchFailureRetainsStageAndNativeCause(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.connected = false
	f.session.mu.Unlock()
	base := t.TempDir()
	f.r.resolve = func(context.Context, desktop.Installation) (desktop.Installation, error) {
		return desktop.Installation{Desktop: filepath.Join(base, "ChatGPT.exe"), Codex: filepath.Join(base, "codex.exe")}, nil
	}
	f.r.launch = func(desktop.Installation, string, string) error {
		return platform.LaunchError("launch Desktop", syscall.Errno(5))
	}
	f.c.launchDesktop()
	waitIdle(t, f.c)
	message := f.c.snapshot().Message
	if !strings.Contains(message, "launch Desktop") || !strings.Contains(message, "无法启动") {
		t.Fatal(message)
	}
}
