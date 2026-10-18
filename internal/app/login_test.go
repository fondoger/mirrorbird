package app

import (
	"context"
	"strings"
	"testing"

	"mirrorbird/internal/tunnel"
)

func TestDeviceLoginPublishesChallengeAndClearsAfterSuccess(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.tunnel.challengeFn = func(ctx context.Context, notify func(tunnel.LoginChallenge)) error {
		notify(tunnel.LoginChallenge{URL: "https://login.microsoft.com/device", Code: "TEST12345"})
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.c.login()
	receive(t, entered, "visible login challenge")
	state := f.c.snapshot()
	if !state.LoggingIn || !state.Busy || state.LoginCode != "TEST12345" || state.LoginURL != "https://login.microsoft.com/device" {
		t.Fatal(state)
	}
	if strings.Contains(state.Message, "TEST12345") {
		t.Fatal("challenge leaked to persistent/general status text")
	}
	for _, want := range []string{"自动打开登录页", "打开登录页", "复制验证码", "https://login.microsoft.com/device"} {
		if !strings.Contains(state.Message, want) {
			t.Fatalf("missing login guidance %q: %q", want, state.Message)
		}
	}
	close(release)
	waitIdle(t, f.c)
	state = f.c.snapshot()
	if state.LoggingIn || state.LoginCode != "" || state.LoginURL != "" || !state.LoggedIn {
		t.Fatal(state)
	}
}

func TestDeviceLoginWaitingCopyExplainsAutoOpenAndCancel(t *testing.T) {
	f := newFixture(t)
	entered := make(chan struct{})
	f.tunnel.challengeFn = func(ctx context.Context, _ func(tunnel.LoginChallenge)) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	f.c.login()
	receive(t, entered, "waiting for login challenge")
	state := f.c.snapshot()
	if !state.LoggingIn || !state.Busy || state.LoginCode != "" || state.LoginURL != "" {
		t.Fatal("waiting login exposed a prompt or was not cancelable")
	}
	for _, want := range []string{"获取后会自动打开登录页", "打开登录页", "取消登录"} {
		if !strings.Contains(state.Message, want) {
			t.Fatalf("missing waiting guidance %q: %q", want, state.Message)
		}
	}
	if strings.Contains(state.Message, "不会自动打开浏览器") {
		t.Fatal("waiting status retained obsolete manual-only guidance")
	}
	f.c.cancelLogin()
	waitIdle(t, f.c)
}

func TestCancelLoginPreservesDesktopConnectionAndIgnoresLateChallenge(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	beforeClears := f.session.clears
	f.session.mu.Unlock()
	entered, canceled := make(chan struct{}), make(chan struct{})
	f.tunnel.challengeFn = func(ctx context.Context, notify func(tunnel.LoginChallenge)) error {
		notify(tunnel.LoginChallenge{URL: "https://login.microsoft.com/device", Code: "TEST12345"})
		close(entered)
		<-ctx.Done()
		notify(tunnel.LoginChallenge{URL: "https://login.microsoft.com/device", Code: "LATE54321"})
		close(canceled)
		return ctx.Err()
	}
	f.c.login()
	receive(t, entered, "login start")
	f.c.cancelLogin()
	receive(t, canceled, "login cancellation")
	waitIdle(t, f.c)
	state := f.c.snapshot()
	if state.LoggingIn || state.LoginCode != "" || state.LoginURL != "" || state.Busy {
		t.Fatal(state)
	}
	f.session.mu.Lock()
	defer f.session.mu.Unlock()
	if !f.session.connected || f.session.clears != beforeClears {
		t.Fatal("login cancel altered Desktop connection or projections")
	}
}
