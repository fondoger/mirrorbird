package app

import (
	"context"
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
