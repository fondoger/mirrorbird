package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mirrorbird/internal/desktop"
	"mirrorbird/internal/settings"
	"mirrorbird/internal/tunnel"
)

// Every OS/process boundary is fake. Tests do not call Run, Launch, Apply,
// Connect on a real attachment, net.Listen, or an installed CLI.
type fakeSession struct {
	mu         sync.Mutex
	connected  bool
	connects   int
	refreshes  int
	closes     int
	clears     int
	refreshFn  func(context.Context) error
	connectErr error
	outdated   bool
}

func (s *fakeSession) Connect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connects++
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.connectErr != nil {
		return s.connectErr
	}
	s.connected = true
	return nil
}
func (s *fakeSession) Connected() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.connected }
func (s *fakeSession) EnginePID() int  { return 12345 }
func (s *fakeSession) AttachmentOutdated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected && s.outdated
}
func (s *fakeSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	s.connected = false
}
func (s *fakeSession) Refresh(ctx context.Context) error {
	s.mu.Lock()
	s.refreshes++
	fn := s.refreshFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return ctx.Err()
}
func (s *fakeSession) ClearProjections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clears++
}
func (s *fakeSession) clearCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clears
}

type fakeTunnel struct {
	mu          sync.Mutex
	account     tunnel.Account
	state       tunnel.State
	done        chan struct{}
	accounts    int
	starts      int
	stops       int
	logins      int
	logouts     int
	accountFn   func(context.Context) (tunnel.Account, error)
	startFn     func(context.Context, tunnel.Options) (tunnel.State, error)
	loginFn     func(context.Context) error
	challengeFn func(context.Context, func(tunnel.LoginChallenge)) error
	stopFn      func() error
	options     tunnel.Options
	log         *eventLog
}

func (t *fakeTunnel) Account(ctx context.Context) (tunnel.Account, error) {
	t.mu.Lock()
	t.accounts++
	fn, account := t.accountFn, t.account
	t.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return account, ctx.Err()
}
func (t *fakeTunnel) LoginWithProgress(ctx context.Context, notify func(tunnel.LoginChallenge)) error {
	t.mu.Lock()
	t.logins++
	fn, challengeFn := t.loginFn, t.challengeFn
	t.mu.Unlock()
	if challengeFn != nil {
		if err := challengeFn(ctx, notify); err != nil {
			return err
		}
	}
	if fn != nil {
		if err := fn(ctx); err != nil {
			return err
		}
	}
	t.mu.Lock()
	t.account.LoggedIn = true
	t.mu.Unlock()
	return nil
}
func (t *fakeTunnel) Logout(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logouts++
	t.account.LoggedIn = false
	return ctx.Err()
}
func (t *fakeTunnel) Start(ctx context.Context, options tunnel.Options) (tunnel.State, error) {
	t.log.add("start")
	t.mu.Lock()
	t.starts++
	t.options = options
	fn := t.startFn
	t.mu.Unlock()
	state := tunnel.State{Running: true, TunnelID: "owned.usw2", URL: "https://owned-43123.usw2.devtunnels.ms", Port: options.Port}
	var err error
	if fn != nil {
		state, err = fn(ctx, options)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		t.state = state
		t.done = make(chan struct{})
	}
	return state, err
}
func (t *fakeTunnel) Stop() error {
	t.log.add("stop")
	t.mu.Lock()
	t.stops++
	t.state.Running, t.state.URL = false, ""
	if t.done != nil && !channelClosed(t.done) {
		close(t.done)
	}
	fn := t.stopFn
	t.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return nil
}
func (t *fakeTunnel) State() tunnel.State { t.mu.Lock(); defer t.mu.Unlock(); return t.state }
func (t *fakeTunnel) Done() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return t.done
}
func (t *fakeTunnel) exit() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.Running, t.state.URL = false, ""
	if !channelClosed(t.done) {
		close(t.done)
	}
}

type fakeAPI struct {
	mu        sync.Mutex
	sharing   bool
	enables   int
	disables  int
	origin    string
	enableErr error
	log       *eventLog
}

func (a *fakeAPI) EnableSharing(origin string) error {
	a.log.add("enable")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.enableErr != nil {
		return a.enableErr
	}
	a.enables++
	a.sharing, a.origin = true, origin
	return nil
}
func (a *fakeAPI) SetSharing(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sharing = enabled
	if !enabled {
		a.disables++
	}
}
func (a *fakeAPI) Sharing() bool         { a.mu.Lock(); defer a.mu.Unlock(); return a.sharing }
func (a *fakeAPI) Handler() http.Handler { return http.NotFoundHandler() }

type fakeStore struct {
	mu      sync.Mutex
	configs []settings.Config
	err     error
	saveFn  func(settings.Config) error
	log     *eventLog
}

func (s *fakeStore) Save(config settings.Config) error {
	s.log.add("save")
	s.mu.Lock()
	s.configs = append(s.configs, config)
	fn, err := s.saveFn, s.err
	s.mu.Unlock()
	if fn != nil {
		return fn(config)
	}
	return err
}

type fakeListener struct {
	mu     sync.Mutex
	addr   *net.TCPAddr
	closed bool
}

func (l *fakeListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l *fakeListener) Close() error              { l.mu.Lock(); l.closed = true; l.mu.Unlock(); return nil }
func (l *fakeListener) Addr() net.Addr            { return l.addr }

type fakeHTTP struct {
	mu          sync.Mutex
	done        chan struct{}
	once        sync.Once
	shutdowns   int
	closes      int
	deadline    bool
	shutdownErr error
}

func (s *fakeHTTP) Serve(net.Listener) error { <-s.done; return http.ErrServerClosed }
func (s *fakeHTTP) finish()                  { s.once.Do(func() { close(s.done) }) }
func (s *fakeHTTP) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shutdowns++
	_, s.deadline = ctx.Deadline()
	err := s.shutdownErr
	s.mu.Unlock()
	s.finish()
	return err
}
func (s *fakeHTTP) Close() error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	s.finish()
	return nil
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(s string) { l.mu.Lock(); l.events = append(l.events, s); l.mu.Unlock() }

type fixture struct {
	c        *controller
	r        *resources
	session  *fakeSession
	tunnel   *fakeTunnel
	api      *fakeAPI
	store    *fakeStore
	listener *fakeListener
	http     *fakeHTTP
	log      *eventLog
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	log := &eventLog{}
	f := &fixture{
		session: &fakeSession{connected: true},
		tunnel:  &fakeTunnel{account: tunnel.Account{LoggedIn: true}, log: log},
		api:     &fakeAPI{log: log}, store: &fakeStore{log: log},
		listener: &fakeListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 43123}},
		http:     &fakeHTTP{done: make(chan struct{})}, log: log,
	}
	f.r = &resources{
		home: t.TempDir(), config: settings.Config{Schema: 1, Port: 43123},
		store: f.store, sessions: f.session, tunnel: f.tunnel, api: f.api,
		listen: func(ctx context.Context, network, address string) (net.Listener, error) {
			if network != "tcp4" || address != "127.0.0.1:43123" {
				t.Errorf("unsafe listen %q %q", network, address)
			}
			log.add("listen")
			return f.listener, ctx.Err()
		},
		newHTTP: func(http.Handler) httpService { return f.http },
		resolve: func(context.Context, desktop.Installation) (desktop.Installation, error) {
			t.Error("unexpected installation detection")
			return desktop.Installation{}, errors.New("unexpected")
		},
		launch: func(desktop.Installation, string, string) error {
			t.Error("unexpected launch")
			return errors.New("unexpected")
		},
		restart: func(context.Context, desktop.Installation, string, string) error {
			t.Error("unexpected Desktop restart")
			return errors.New("unexpected restart")
		},
		running:    func(context.Context) (bool, error) { return false, nil },
		executable: func() (string, error) { return filepath.Join(t.TempDir(), "mirrorbird.exe"), nil },
		autoStart: func(bool) (func() error, error) {
			t.Error("unexpected autorun write")
			return nil, errors.New("unexpected")
		},
	}
	f.c = newController(Options{}, func() (*resources, error) { return f.r, nil })
	f.c.pollEvery = time.Hour
	t.Cleanup(f.c.close)
	f.c.ready(func(desktop.ViewState) {})
	waitIdle(t, f.c)
	return f
}

func waitFor(t *testing.T, description string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}
func waitIdle(t *testing.T, c *controller) {
	t.Helper()
	waitFor(t, "idle", func() bool { return !c.snapshot().Busy })
}
func receive(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
func (f *fixture) start(t *testing.T) {
	t.Helper()
	f.c.startSharing()
	waitIdle(t, f.c)
	if !f.api.Sharing() {
		t.Fatalf("sharing did not start: %+v", f.c.snapshot())
	}
}

func TestConstructionAndSecondInstanceHaveNoSideEffects(t *testing.T) {
	loads := 0
	load := func() (*resources, error) { loads++; return nil, errors.New("must not be reached") }
	c := newController(Options{Background: true}, load)
	if loads != 0 || !c.snapshot().StartHidden {
		t.Fatal("construction performed work or lost background")
	}
	c.close()
	if loads != 0 {
		t.Fatal("close initialized resources")
	}
	var actions desktop.Actions
	err := run(Options{Background: true}, func(initial desktop.ViewState, a desktop.Actions, ready func(func(desktop.ViewState)), quit func()) error {
		if !initial.StartHidden || initial.Sharing {
			t.Fatal("bad initial state")
		}
		actions = a
		// This is desktop.Run's second-instance behavior: no ready and no quit.
		a.Refresh()
		a.StartSharing()
		a.Login()
		return nil
	}, load)
	if err != nil || loads != 0 {
		t.Fatalf("second instance initialized resources: loads=%d err=%v", loads, err)
	}
	actions.Refresh() // Escaped callbacks after close remain inert.
	if loads != 0 {
		t.Fatal("late callback initialized resources")
	}
}

func TestStartWithoutSessionSelectionUsesOwnExactListener(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.tunnel.mu.Lock()
	starts := f.tunnel.starts
	f.tunnel.mu.Unlock()
	if starts != 1 {
		t.Fatal("remote service must start without selecting any session")
	}
	f.log.mu.Lock()
	events := strings.Join(f.log.events, ",")
	f.log.mu.Unlock()
	if events != "listen,start,save,enable" {
		t.Fatalf("wrong startup order: %s", events)
	}
	f.store.mu.Lock()
	saved := f.store.configs
	f.store.mu.Unlock()
	if len(saved) != 1 || saved[0].TunnelID != "owned.usw2" {
		t.Fatalf("unverified or missing persisted ID: %+v", saved)
	}
}

func TestBusyStopCancelsPendingStartAndLateSuccessCannotEnable(t *testing.T) {
	f := newFixture(t)
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.tunnel.startFn = func(ctx context.Context, options tunnel.Options) (tunnel.State, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release // Deliberately returns a stale success after cancellation.
		return tunnel.State{Running: true, TunnelID: "late.usw2", URL: "https://late.devtunnels.ms", Port: options.Port}, nil
	}
	f.c.startSharing()
	receive(t, entered, "pending Start")
	before := f.c.snapshot()
	if !before.Starting || !before.Busy {
		t.Fatal("missing starting/busy snapshot")
	}
	returned := make(chan struct{})
	go func() { f.c.stopSharing(); close(returned) }()
	receive(t, returned, "prompt Stop return")
	receive(t, canceled, "Start context canceled")
	if f.api.Sharing() || f.session.clearCount() < 2 || f.c.snapshot().Starting {
		t.Fatal("Stop did not revoke immediately")
	}
	once.Do(func() { close(release) })
	waitIdle(t, f.c)
	if f.api.Sharing() || f.tunnel.State().Running {
		t.Fatal("late success re-enabled stopped sharing")
	}
}

func TestPendingStartLateCompletionStaysClosed(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.tunnel.startFn = func(ctx context.Context, options tunnel.Options) (tunnel.State, error) {
		close(entered)
		<-release
		return tunnel.State{Running: true, TunnelID: "late.usw2", URL: "https://late.devtunnels.ms", Port: options.Port}, nil
	}
	f.c.startSharing()
	receive(t, entered, "Start")
	f.c.stopSharing()
	once.Do(func() { close(release) })
	waitIdle(t, f.c)
	f.api.mu.Lock()
	enables := f.api.enables
	f.api.mu.Unlock()
	f.store.mu.Lock()
	saves := len(f.store.configs)
	f.store.mu.Unlock()
	if enables != 0 || saves != 0 || f.api.Sharing() || f.tunnel.State().Running {
		t.Fatal("late success resurrected or persisted sharing")
	}
	f.session.mu.Lock()
	closes := f.session.closes
	f.session.mu.Unlock()
	if closes != 0 {
		t.Fatal("Stop closed local attachment")
	}
}

func TestHostExitDisablesAccessClearsProjectionsAndCleansListener(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	beforeClears := f.session.clearCount()
	f.tunnel.exit()
	waitFor(t, "fail-closed host exit", func() bool { return !f.api.Sharing() && !f.c.snapshot().Busy })
	if f.session.clearCount() <= beforeClears || f.c.snapshot().TunnelURL != "" {
		t.Fatal("host exit retained stale projection lifetime or URL")
	}
	f.listener.mu.Lock()
	closed := f.listener.closed
	f.listener.mu.Unlock()
	if !closed {
		t.Fatal("own listener not closed")
	}
}

func TestServerExitDuringBlockedRefreshFailsClosedImmediately(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	entered, canceled := make(chan struct{}), make(chan struct{})
	f.session.mu.Lock()
	f.session.refreshFn = func(ctx context.Context) error { close(entered); <-ctx.Done(); close(canceled); return ctx.Err() }
	f.session.mu.Unlock()
	f.c.refresh()
	receive(t, entered, "refresh")
	f.http.finish()
	receive(t, canceled, "HTTP failure canceled blocked refresh")
	waitFor(t, "server fail closed", func() bool { return !f.api.Sharing() && !f.c.snapshot().Busy })
	if f.session.clearCount() < 2 || f.tunnel.State().Running {
		t.Fatal("server exit kept access/host")
	}
}

func TestStopAndQuitAreIdempotentAndCloseOnlyLocalAttachmentOnQuit(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.c.stopSharing()
	f.c.stopSharing()
	waitIdle(t, f.c)
	f.session.mu.Lock()
	closes := f.session.closes
	f.session.mu.Unlock()
	if closes != 0 {
		t.Fatal("Stop closed attachment")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(f.c.close)
	}
	wg.Wait()
	f.session.mu.Lock()
	closes = f.session.closes
	f.session.mu.Unlock()
	f.tunnel.mu.Lock()
	stops := f.tunnel.stops
	f.tunnel.mu.Unlock()
	if closes != 1 || stops != 1 {
		t.Fatalf("non-idempotent close: session=%d tunnel=%d", closes, stops)
	}
}

func TestRefreshDoesNotReconnectConnectedAttachment(t *testing.T) {
	f := newFixture(t)
	f.c.refresh()
	waitIdle(t, f.c)
	f.session.mu.Lock()
	connects, refreshes, clears := f.session.connects, f.session.refreshes, f.session.clears
	f.session.mu.Unlock()
	if connects != 0 || refreshes != 2 || clears != 1 {
		t.Fatalf("refresh replaced attachment/cache: connects=%d refreshes=%d clears=%d", connects, refreshes, clears)
	}
}

func TestLoginWithoutDesktopAndStopCancellation(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.connected = false
	f.session.mu.Unlock()
	entered, canceled := make(chan struct{}), make(chan struct{})
	f.tunnel.loginFn = func(ctx context.Context) error { close(entered); <-ctx.Done(); close(canceled); return ctx.Err() }
	f.c.login()
	receive(t, entered, "independent Dev Tunnel login")
	f.c.stopSharing()
	receive(t, canceled, "login canceled")
	waitIdle(t, f.c)
	f.session.mu.Lock()
	connects := f.session.connects
	f.session.mu.Unlock()
	if connects != 0 {
		t.Fatal("login connected Desktop")
	}
}

func TestListenerFailureDoesNotStartOrReuseTunnel(t *testing.T) {
	f := newFixture(t)
	binds := 0
	f.r.listen = func(context.Context, string, string) (net.Listener, error) {
		binds++
		return nil, errors.New("address already in use")
	}
	f.c.startSharing()
	waitIdle(t, f.c)
	f.tunnel.mu.Lock()
	starts, stops := f.tunnel.starts, f.tunnel.stops
	f.tunnel.mu.Unlock()
	if binds != 1 || starts != 0 || stops != 0 || !strings.Contains(f.c.snapshot().Message, "43123") {
		t.Fatalf("bad listener failure handling: %d %d %d", binds, starts, stops)
	}
	if !f.session.Connected() {
		t.Fatal("recoverable start failure disconnected Desktop")
	}
}

func TestPersistFailureUnwindsAndDoesNotEnable(t *testing.T) {
	f := newFixture(t)
	f.store.err = errors.New("permission denied")
	f.c.startSharing()
	waitIdle(t, f.c)
	f.api.mu.Lock()
	enables := f.api.enables
	f.api.mu.Unlock()
	if enables != 0 || f.tunnel.State().Running || f.r.config.TunnelID != "" {
		t.Fatal("save failure kept remote access or changed config")
	}
	if !f.session.Connected() {
		t.Fatal("start save failure disconnected Desktop")
	}
}

func TestStopDuringIDSaveCannotEnableAfterSave(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.store.saveFn = func(settings.Config) error { close(entered); <-release; return nil }
	f.c.startSharing()
	receive(t, entered, "ID persistence")
	f.c.stopSharing()
	once.Do(func() { close(release) })
	waitIdle(t, f.c)
	f.api.mu.Lock()
	enables := f.api.enables
	f.api.mu.Unlock()
	if enables != 0 || f.tunnel.State().Running {
		t.Fatal("late save enabled sharing")
	}
}

func TestInvalidSettingsAreNotOverwritten(t *testing.T) {
	for _, original := range []string{`{"schema":999,"port":43123,"userData":"do not overwrite"}`, `null`, `[]`, `"not a config"`, "", `{"port":0}`} {
		t.Run(original, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "settings.json")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := openResources(home)
			if err == nil || !strings.Contains(err.Error(), "settings.json") {
				t.Fatalf("missing useful settings error: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != original {
				t.Fatal("invalid settings overwritten")
			}
		})
	}
}

func TestAutoStartSavesOnlyAfterSuccessAndRollsBack(t *testing.T) {
	f := newFixture(t)
	var calls, rollbacks int
	f.r.autoStart = func(enabled bool) (func() error, error) {
		calls++
		if !enabled {
			t.Error("unexpected disable")
		}
		return func() error { rollbacks++; return nil }, nil
	}
	f.store.err = errors.New("cannot save")
	f.c.setAutoStart(true)
	waitIdle(t, f.c)
	if calls != 1 || rollbacks != 1 || f.c.snapshot().AutoStart || f.r.config.AutoStart {
		t.Fatal("autorun rollback/config mismatch")
	}
	f.store.err = nil
	f.r.autoStart = func(bool) (func() error, error) { return nil, errors.New("registry denied") }
	f.c.setAutoStart(true)
	waitIdle(t, f.c)
	f.store.mu.Lock()
	saves := len(f.store.configs)
	f.store.mu.Unlock()
	if saves != 1 {
		t.Fatal("saved setting after failed registry update")
	}
}

func TestConfiguredLaunchUsesBundledSiblingShim(t *testing.T) {
	f := newFixture(t)
	f.session.mu.Lock()
	f.session.connected = false
	f.session.mu.Unlock()
	base := t.TempDir()
	f.r.config.DesktopExecutable = filepath.Join(base, "custom", "ChatGPT.exe")
	f.r.config.CodexExecutable = filepath.Join(base, "engine", "codex.exe")
	f.r.resolve = func(_ context.Context, selected desktop.Installation) (desktop.Installation, error) {
		return selected, nil
	}
	f.r.executable = func() (string, error) { return filepath.Join(base, "Mirror Bird", "mirrorbird.exe"), nil }
	var got desktop.Installation
	var home, shim string
	f.r.launch = func(i desktop.Installation, h, s string) error { got, home, shim = i, h, s; return nil }
	f.c.launchDesktop()
	waitIdle(t, f.c)
	if got.Desktop != f.r.config.DesktopExecutable || got.Codex != f.r.config.CodexExecutable || home != f.r.home || shim != filepath.Join(base, "Mirror Bird", "mirrorbird-codex.exe") {
		t.Fatalf("wrong launch: %+v %s %s", got, home, shim)
	}
}

func TestHTTPServerHasBodyHeaderBoundsWithoutSSEWriteTimeout(t *testing.T) {
	s := newHTTPServer(http.NotFoundHandler()).(*http.Server)
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes != 16*1024 || s.WriteTimeout != 0 {
		t.Fatalf("unsafe HTTP limits: %+v", s)
	}
}

func TestShutdownUsesDeadlineAndForceClosesAfterError(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.http.shutdownErr = context.DeadlineExceeded
	f.c.stopSharing()
	waitIdle(t, f.c)
	f.http.mu.Lock()
	shutdowns, closes, deadline := f.http.shutdowns, f.http.closes, f.http.deadline
	f.http.mu.Unlock()
	if shutdowns != 1 || closes != 1 || !deadline {
		t.Fatalf("shutdown not bounded: %d %d %v", shutdowns, closes, deadline)
	}
}

func TestCheckedOriginRejectsUnverifiedState(t *testing.T) {
	for _, raw := range []string{"http://example.test", "https://example.test/path", "https://a:b@example.test", "https://example.test/?token=x", "https://example.test/#secret", "https://example.test:", "https://example.test:65536", "https://example.test\\evil"} {
		t.Run(raw, func(t *testing.T) {
			_, err := checkedOrigin(tunnel.State{Running: true, TunnelID: "id", Port: 43123, URL: raw}, 43123)
			if err == nil {
				t.Fatalf("unsafe origin accepted: %s", raw)
			}
		})
	}
	good := tunnel.State{Running: true, TunnelID: "id", Port: 43123, URL: "https://example.test/"}
	if origin, err := checkedOrigin(good, 43123); err != nil || origin != "https://example.test" {
		t.Fatalf("canonical origin: %s %v", origin, err)
	}
	good.Running = false
	if _, err := checkedOrigin(good, 43123); err == nil {
		t.Fatal("stopped tunnel accepted")
	}
}

func TestPublishedSnapshotsDoNotAliasControllerState(t *testing.T) {
	f := newFixture(t)
	f.c.mu.Lock()
	var published desktop.ViewState
	f.c.update = func(s desktop.ViewState) { published = s }
	f.c.sampleLocked()
	f.c.publishLocked()
	f.c.mu.Unlock()
	published.DesktopStatus = "mutated caller snapshot"
	if f.c.snapshot().DesktopStatus == published.DesktopStatus {
		t.Fatal("published value aliases state")
	}
}

func TestNoRawCLIOrBackendErrorIsPublished(t *testing.T) {
	message := tunnelMessage("检查失败", fmt.Errorf("secret-code-sentinel"))
	if strings.Contains(message, "secret-code-sentinel") {
		t.Fatal("raw error leaked")
	}
}
