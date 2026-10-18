package app

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mirrorbird/internal/desktop"
	"mirrorbird/internal/settings"
	"mirrorbird/internal/tunnel"
)

func TestQuitCancelsPendingStartAndWaitsForOnlyOwnedCleanup(t *testing.T) {
	f := newFixture(t)
	entered, canceled := make(chan struct{}), make(chan struct{})
	f.tunnel.startFn = func(ctx context.Context, _ tunnel.Options) (tunnel.State, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return tunnel.State{}, ctx.Err()
	}
	f.c.startSharing()
	receive(t, entered, "pending Start")
	closed := make(chan struct{})
	go func() { f.c.close(); close(closed) }()
	receive(t, canceled, "Quit cancellation")
	receive(t, closed, "Quit completion")
	f.session.mu.Lock()
	closes := f.session.closes
	f.session.mu.Unlock()
	if closes != 1 || f.api.Sharing() || f.session.clearCount() < 2 {
		t.Fatal("Quit did not close the local attachment exactly once")
	}
}

func TestInitialLoadAndBackgroundNeverRestoreSharing(t *testing.T) {
	f := newFixture(t)
	// Initialization clears old local cache lifetimes without restoring remote
	// access or touching registry state.
	f.c.close()
	f.session.mu.Lock()
	f.session.connected = true
	f.session.mu.Unlock()
	f.r.config.AutoStart = true
	loads := 0
	c := newController(Options{Background: true}, func() (*resources, error) { loads++; return f.r, nil })
	c.pollEvery = time.Hour
	defer c.close()
	c.ready(func(desktop.ViewState) {})
	waitIdle(t, c)
	state := c.snapshot()
	f.tunnel.mu.Lock()
	starts, logins := f.tunnel.starts, f.tunnel.logins
	f.tunnel.mu.Unlock()
	if loads != 1 || !state.StartHidden || !state.AutoStart || state.Sharing || f.session.clearCount() < 2 || starts != 0 || logins != 0 {
		t.Fatalf("unsafe background initial state: %+v", state)
	}
}

func TestReadyLoadErrorIsPublishedAndNotRetriedByActions(t *testing.T) {
	loads := 0
	c := newController(Options{}, func() (*resources, error) {
		loads++
		return nil, errors.New("settings.json is invalid; fix it and restart")
	})
	defer c.close()
	c.ready(func(desktop.ViewState) {})
	waitIdle(t, c)
	c.refresh()
	c.login()
	c.setAutoStart(true)
	if loads != 1 || !strings.Contains(c.snapshot().Message, "settings.json") || c.snapshot().Sharing {
		t.Fatal("invalid settings initialized services or lost error")
	}
}

func TestStopWhileSettingsLoadPendingPreventsAutomaticProbes(t *testing.T) {
	f := newFixture(t)
	f.c.close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.session.mu.Lock()
	beforeRefresh := f.session.refreshes
	f.session.mu.Unlock()
	f.tunnel.mu.Lock()
	beforeAccount := f.tunnel.accounts
	f.tunnel.mu.Unlock()
	c := newController(Options{}, func() (*resources, error) { close(entered); <-release; return f.r, nil })
	c.pollEvery = time.Hour
	defer func() { once.Do(func() { close(release) }); c.close() }()
	c.ready(func(desktop.ViewState) {})
	receive(t, entered, "settings load")
	c.stopSharing()
	once.Do(func() { close(release) })
	waitIdle(t, c)
	f.session.mu.Lock()
	refreshes := f.session.refreshes
	f.session.mu.Unlock()
	f.tunnel.mu.Lock()
	accounts := f.tunnel.accounts
	f.tunnel.mu.Unlock()
	if refreshes != beforeRefresh || accounts != beforeAccount {
		t.Fatal("canceled initialization still probed dependencies")
	}
}

func TestPollingUpdatesConnectionWithoutNetworkOrReconnect(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.tunnel.mu.Lock()
	accounts := f.tunnel.accounts
	f.tunnel.mu.Unlock()
	f.c.pollOnce()
	state := f.c.snapshot()
	if !state.Connected || !strings.Contains(state.DesktopStatus, "12345") {
		t.Fatalf("poll did not update connection status: %+v", state)
	}
	f.session.mu.Lock()
	f.session.connected = false
	connects := f.session.connects
	f.session.mu.Unlock()
	f.c.pollOnce()
	waitIdle(t, f.c)
	f.c.pollOnce()
	f.session.mu.Lock()
	afterConnects := f.session.connects
	f.session.mu.Unlock()
	f.tunnel.mu.Lock()
	afterAccounts := f.tunnel.accounts
	f.tunnel.mu.Unlock()
	if afterConnects != connects || afterAccounts != accounts || f.api.Sharing() || f.session.clearCount() < 2 {
		t.Fatal("poll reconnected, made a CLI call, or kept access")
	}
}

func TestManualRefreshReconnectsOnlyAfterFailClosedTransition(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.session.mu.Lock()
	f.session.connected = false
	f.session.mu.Unlock()
	f.c.refresh()
	waitIdle(t, f.c)
	if f.api.Sharing() {
		t.Fatal("refresh retained sharing across attachment replacement")
	}
	f.c.refresh()
	waitIdle(t, f.c)
	f.session.mu.Lock()
	connects := f.session.connects
	f.session.mu.Unlock()
	if connects != 1 || f.api.Sharing() {
		t.Fatalf("reconnect policy: connects=%d sharing=%v", connects, f.api.Sharing())
	}
}

func TestLogoutRevokesBeforeWaitingForHostAndLeavesDesktopConnected(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.tunnel.stopFn = func() error { close(entered); <-release; return nil }
	f.c.logout()
	receive(t, entered, "host shutdown")
	if f.api.Sharing() || f.session.clearCount() < 2 {
		t.Fatal("logout waited for process before revocation")
	}
	once.Do(func() { close(release) })
	waitIdle(t, f.c)
	f.tunnel.mu.Lock()
	logouts := f.tunnel.logouts
	f.tunnel.mu.Unlock()
	if logouts != 1 || f.c.snapshot().LoggedIn || !f.session.Connected() {
		t.Fatal("logout did not target Dev Tunnel only")
	}
}

func TestHostExitBeforeEnableIsDetectedEvenWhileIDSaveBlocks(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.store.saveFn = func(settings.Config) error { close(entered); <-release; return nil }
	f.c.startSharing()
	receive(t, entered, "ID save")
	f.tunnel.exit()
	waitFor(t, "immediate host failure transition", func() bool { return !f.c.snapshot().Starting })
	if f.api.Sharing() || f.session.clearCount() < 2 {
		t.Fatal("host failure retained authority while settings write blocked")
	}
	once.Do(func() { close(release) })
	waitIdle(t, f.c)
	f.api.mu.Lock()
	enables := f.api.enables
	f.api.mu.Unlock()
	if enables != 0 {
		t.Fatal("late save enabled after host exit")
	}
}

func TestWrongListenerAndUnverifiedTunnelAreClosedWithoutEnable(t *testing.T) {
	for _, mode := range []string{"wrong-interface", "wrong-port", "not-running", "non-https", "api-refuses"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			switch mode {
			case "wrong-interface":
				f.listener.addr.IP = net.IPv4zero
			case "wrong-port":
				f.listener.addr.Port = 9999
			case "not-running":
				f.tunnel.startFn = func(_ context.Context, o tunnel.Options) (tunnel.State, error) {
					return tunnel.State{TunnelID: "id", Port: o.Port}, nil
				}
			case "non-https":
				f.tunnel.startFn = func(_ context.Context, o tunnel.Options) (tunnel.State, error) {
					return tunnel.State{Running: true, TunnelID: "id", Port: o.Port, URL: "http://public.test"}, nil
				}
			case "api-refuses":
				f.api.enableErr = errors.New("rejected")
			}
			f.c.startSharing()
			waitIdle(t, f.c)
			f.listener.mu.Lock()
			closed := f.listener.closed
			f.listener.mu.Unlock()
			if f.api.Sharing() || f.tunnel.State().Running || !closed {
				t.Fatal("unsafe host/listener survived")
			}
		})
	}
}

func TestStartDeclinesMissingAccountWithoutListener(t *testing.T) {
	for _, failed := range []bool{false, true} {
		f := newFixture(t)
		f.tunnel.mu.Lock()
		f.tunnel.account.LoggedIn = false
		if failed {
			f.tunnel.accountFn = func(context.Context) (tunnel.Account, error) { return tunnel.Account{}, tunnel.ErrExecutable }
		}
		f.tunnel.mu.Unlock()
		f.c.startSharing()
		waitIdle(t, f.c)
		f.log.mu.Lock()
		count := len(f.log.events)
		f.log.mu.Unlock()
		if count != 0 || f.api.Sharing() || f.c.snapshot().LoggedIn {
			t.Fatal("start touched listener without confirmed account")
		}
	}
}

func TestLoginSuccessDoesNotStartSharing(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.connected = false
	f.session.mu.Unlock()
	f.c.login()
	waitIdle(t, f.c)
	if !f.c.snapshot().LoggedIn || f.api.Sharing() || f.session.Connected() {
		t.Fatal("login unexpectedly connected or shared")
	}
}

func TestAutoStartSuccessDisableAndCanceledRollback(t *testing.T) {
	f := newFixture(t)
	var requests []bool
	rollbacks := 0
	f.r.autoStart = func(enabled bool) (func() error, error) {
		requests = append(requests, enabled)
		return func() error { rollbacks++; return nil }, nil
	}
	f.c.setAutoStart(true)
	waitIdle(t, f.c)
	if !f.c.snapshot().AutoStart || !f.r.config.AutoStart {
		t.Fatal("successful autorun not saved")
	}
	f.c.setAutoStart(false)
	waitIdle(t, f.c)
	if f.c.snapshot().AutoStart || f.r.config.AutoStart || len(requests) != 2 || rollbacks != 0 {
		t.Fatal("autorun disable mismatch")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.r.autoStart = func(bool) (func() error, error) {
		close(entered)
		<-release
		return func() error { rollbacks++; return nil }, nil
	}
	f.c.setAutoStart(true)
	receive(t, entered, "fake registry action")
	f.c.stopSharing()
	once.Do(func() { close(release) })
	waitIdle(t, f.c)
	if rollbacks != 1 || f.r.config.AutoStart || f.c.snapshot().AutoStart {
		t.Fatal("canceled registry mutation was persisted")
	}
}

func TestLaunchResolvesBackendForSelectedDesktopAndDoesNotReconnect(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.connected = false
	f.session.mu.Unlock()
	base := t.TempDir()
	configured := filepath.Join(base, "custom", "ChatGPT.exe")
	found := desktop.Installation{Desktop: configured, Codex: filepath.Join(base, "custom", "resources", "codex.exe"), Version: "1.2.3"}
	f.r.config.DesktopExecutable = configured
	f.r.resolve = func(_ context.Context, selected desktop.Installation) (desktop.Installation, error) {
		if selected.Desktop != configured || selected.Codex != "" {
			t.Errorf("resolver lost selected installation: %+v", selected)
		}
		return found, nil
	}
	var launched desktop.Installation
	f.r.launch = func(i desktop.Installation, _, _ string) error { launched = i; return nil }
	f.c.launchDesktop()
	waitIdle(t, f.c)
	if launched.Desktop != configured || launched.Codex != found.Codex || f.c.snapshot().DesktopVersion != "1.2.3" || f.session.Connected() {
		t.Fatalf("configured path overwritten or automatic connect: %+v", launched)
	}
}

func TestOldHostFailureCannotStopReplacementSharing(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.c.mu.Lock()
	old := f.c.host
	f.c.mu.Unlock()
	f.c.stopSharing()
	waitIdle(t, f.c)
	f.listener = &fakeListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 43123}}
	f.http = &fakeHTTP{done: make(chan struct{})}
	f.start(t)
	f.c.hostFailed(old, "stale failure")
	if !f.api.Sharing() || !f.c.snapshot().Sharing {
		t.Fatal("stale host monitor revoked replacement lifecycle")
	}
}
