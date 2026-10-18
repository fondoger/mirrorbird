package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"mirrorbird/internal/attachment"
)

// lifecycleTestApp models MyGo's ordered OnQuit listeners. Taking the lock
// registers its release, and relaunch happens only after every listener returns.
// No native app, lock, updater, network call, or process is started by these tests.
type lifecycleTestApp struct {
	takeLock   bool
	beforeLock func()
	callbacks  []func()
	released   bool
	relaunched bool
}

func (a *lifecycleTestApp) OnQuit(fn func()) func() {
	index := len(a.callbacks)
	a.callbacks = append(a.callbacks, fn)
	return func() { a.callbacks[index] = nil }
}

func (a *lifecycleTestApp) RequestSingleInstanceLock() bool {
	if a.beforeLock != nil {
		a.beforeLock()
	}
	if a.takeLock {
		a.OnQuit(func() { a.released = true })
	}
	return a.takeLock
}

func (a *lifecycleTestApp) finish(relaunch bool) {
	for _, fn := range slices.Clone(a.callbacks) {
		if fn != nil {
			fn()
		}
	}
	a.relaunched = relaunch
}

func TestRunLifecycle(t *testing.T) {
	initErr := errors.New("native initialization failed")
	for _, tc := range []struct {
		name               string
		acquired, emitQuit bool
		err                error
	}{
		{"ordinary quit", true, true, nil},
		{"second instance", false, false, nil},
		{"initialization failure", true, false, initErr},
		{"ready failure with quit", true, true, initErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &lifecycleTestApp{takeLock: tc.acquired}
			cleanups, runs := 0, 0
			a.beforeLock = func() {
				if len(a.callbacks) != 1 {
					t.Fatal("cleanup must be registered before requesting the lock")
				}
				a.callbacks[0]() // Not acquired yet: do not clean up or consume Once.
				if cleanups != 0 {
					t.Fatal("cleanup ran before acquiring the lock")
				}
			}
			err := runWithLifecycle(a, func() {
				cleanups++
				if a.released || a.relaunched {
					t.Fatal("cleanup ran after lock release or relaunch")
				}
			}, func() error {
				runs++
				if tc.emitQuit {
					a.finish(false)
					if cleanups != 1 || !a.released {
						t.Fatal("OnQuit did not clean up synchronously before returning from Run")
					}
				}
				return tc.err
			})
			wantCalls := 0
			if tc.acquired {
				wantCalls = 1
			}
			if !errors.Is(err, tc.err) || runs != wantCalls || cleanups != wantCalls {
				t.Fatalf("error=%v runs=%d cleanups=%d want=%d", err, runs, cleanups, wantCalls)
			}
			if a.callbacks[0] != nil {
				t.Fatal("cleanup listener not removed after Run")
			}
		})
	}
}

func TestRunLifecycleBlockingCleanupHoldsLockAndRelaunch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &lifecycleTestApp{takeLock: true}
		unblock, runDone := make(chan struct{}), make(chan struct{})
		cleanups := 0
		go func() {
			_ = runWithLifecycle(a, func() {
				cleanups++
				<-unblock
			}, func() error {
				a.finish(true)   // Like MyGo, Run cannot return until OnQuit finishes.
				a.callbacks[0]() // A repeated callback and the deferred fallback are no-ops.
				return nil
			})
			close(runDone)
		}()
		synctest.Wait()
		if a.released || a.relaunched || cleanups != 1 {
			t.Fatal("lock released, app relaunched, or cleanup duplicated while blocked")
		}
		select {
		case <-runDone:
			t.Fatal("Run returned before cleanup finished")
		default:
		}
		close(unblock)
		synctest.Wait()
		select {
		case <-runDone:
		default:
			t.Fatal("cleanup did not unblock Run")
		}
		if !a.released || !a.relaunched || cleanups != 1 || a.callbacks[0] != nil {
			t.Fatal("cleanup must finish once, then release the lock and allow relaunch")
		}
	})
}

func TestRunLifecycleNilCleanup(t *testing.T) {
	a := &lifecycleTestApp{takeLock: true}
	if err := runWithLifecycle(a, nil, func() error { a.finish(false); return nil }); err != nil || !a.released {
		t.Fatal("nil cleanup prevented ordinary exit")
	}
}

func TestUpdaterOptionsKeepNativeDefaultsAndExplainRestart(t *testing.T) {
	opts := updaterOptions()
	if opts.Language != "zh-Hans" || opts.Interval != 0 || opts.DisableAutomaticChecks {
		t.Fatal("updater must use Simplified Chinese and its default daily automatic checks")
	}
	format := opts.Strings[opts.Language].ReadyMessage
	if !strings.Contains(format, "%[1]s") || !strings.Contains(format, "%[2]s") {
		t.Fatal("ReadyMessage must use indexed app-name and version placeholders")
	}
	message := fmt.Sprintf(format, "MirrorBird", "1.2.3")
	for _, want := range []string{"MirrorBird 1.2.3", "重启 MirrorBird 会停止共享", "重新启动 Desktop 以加载新组件"} {
		if !strings.Contains(message, want) || strings.Contains(message, "%!") {
			t.Fatalf("incorrect restart message: %q", message)
		}
	}
}

func TestUserDataDirectoryRespectsIsolatedHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv(attachment.EnvHome, home)
	got, err := userDataDirectory()
	if err != nil || got != home {
		t.Fatalf("home=%q error=%v", got, err)
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatalf("updater directory not created: %v", err)
	}
	t.Setenv(attachment.EnvHome, "relative-home")
	if _, err := userDataDirectory(); err == nil {
		t.Fatal("invalid home silently fell back to the real profile")
	}
}

func TestSettingsAndTrayCheckUpdates(t *testing.T) {
	previous := mygo.App.Version()
	t.Cleanup(func() { mygo.App.SetVersion(previous) })
	for _, version := range []string{"", "1.2.3"} {
		mygo.App.SetVersion(version)
		d := newDesktopUI(ViewState{Busy: true, Sharing: true}, Actions{})
		calls := 0
		d.checkForUpdates = func() { calls++ }
		tt := ui.NewTester(d.view, 640, 900)
		wantVersion := version
		if wantVersion == "" {
			wantVersion = "dev"
		}
		if !tt.HasText("MirrorBird 版本 " + wantVersion) {
			t.Fatalf("settings version not from MyGo: %q", tt.Texts())
		}
		if err := tt.Click("检查更新"); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range d.trayMenuItems() {
			if item.Label == "检查更新" {
				found = true
				if item.Disabled || item.Click == nil {
					t.Fatal("tray update check unavailable while sharing")
				}
				item.Click(item, nil)
			}
		}
		if !found || calls != 2 {
			t.Fatalf("settings/tray did not dispatch update checks: found=%v calls=%d", found, calls)
		}
	}
}

func TestNilUpdateCheckDisabled(t *testing.T) {
	d := newDesktopUI(ViewState{}, Actions{})
	tt := ui.NewTester(d.generalSection, 640, 240)
	if err := tt.Click("检查更新"); err != nil {
		t.Fatal(err)
	}
	for _, item := range d.trayMenuItems() {
		if item.Label == "检查更新" {
			if !item.Disabled {
				t.Fatal("nil update check enabled in tray")
			}
			item.Click(item, nil) // Validate the nil callback at the boundary too.
			return
		}
	}
	t.Fatal("missing tray update item")
}

// MyGo's Tester renders and dispatches input without a window, tray, shell,
// clipboard, process, or App.Run. System-boundary callbacks below are fakes.
func TestDevTunnelLoginDoesNotRequireDesktop(t *testing.T) {
	logins := 0
	d := newDesktopUI(ViewState{}, Actions{Login: func() { logins++ }})
	tt := ui.NewTester(d.connectionSection, 640, 340)
	if !tt.HasText("Dev Tunnel · Microsoft 账号未登录") {
		t.Fatalf("wrong account copy: %q", tt.Texts())
	}
	if err := tt.Click("登录 Dev Tunnel"); err != nil {
		t.Fatal(err)
	}
	if logins != 1 {
		t.Fatal("login incorrectly gated on Desktop")
	}
	d.state.Busy = true
	tt.Frame()
	if err := tt.Click("登录 Dev Tunnel"); err != nil {
		t.Fatal(err)
	}
	if logins != 1 {
		t.Fatal("busy login dispatched duplicate work")
	}
}

func TestStopAvailableWhileStartingAndBusy(t *testing.T) {
	for _, state := range []ViewState{{Starting: true, Busy: true}, {Sharing: true, Busy: true}, {Busy: true}} {
		stops := 0
		d := newDesktopUI(state, Actions{StopSharing: func() { stops++ }})
		if !d.canStopSharing() {
			t.Fatal("native tray gate disabled Stop")
		}
		tt := ui.NewTester(d.sharingSection, 640, 300)
		if err := tt.Click("停止共享"); err != nil {
			t.Fatal(err)
		}
		if stops != 1 {
			t.Fatal("busy Stop did not dispatch")
		}
	}
	d := newDesktopUI(ViewState{Starting: true, Busy: true}, Actions{})
	if d.canStopSharing() {
		t.Fatal("nil Stop action enabled")
	}
}

func TestStartRequiresOnlyConnectionAndMicrosoftLogin(t *testing.T) {
	starts := 0
	d := newDesktopUI(ViewState{Connected: true, LoggedIn: true}, Actions{StartSharing: func() { starts++ }})
	tt := ui.NewTester(d.sharingSection, 640, 300)
	if err := tt.Click("开启共享"); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatal("Start must not require selected sessions or paired devices")
	}
	d.state.Connected = false
	tt.Frame()
	if err := tt.Click("开启共享"); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatal("disconnected Start permitted")
	}
	d.state.Connected = true
	d.state.LoggedIn = false
	tt.Frame()
	if err := tt.Click("开启共享"); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatal("logged-out Start permitted")
	}
}

func TestSettingsExposeAddressWithoutPairingOrSessionSelection(t *testing.T) {
	var opened, copied string
	d := newDesktopUI(ViewState{Connected: true, LoggedIn: true, Sharing: true, TunnelURL: "https://owned.devtunnels.ms"}, Actions{})
	d.openURL = func(raw string) error { opened = raw; return nil }
	d.copyText = func(raw string) { copied = raw }
	tt := ui.NewTester(d.view, 640, 780)
	for _, label := range []string{"配对新设备", "配对链接", "共享会话", "已配对设备"} {
		if tt.HasText(label) {
			t.Fatalf("obsolete control remains: %s", label)
		}
	}
	if err := tt.Click("打开地址"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("复制地址"); err != nil {
		t.Fatal(err)
	}
	if opened != d.state.TunnelURL || copied != opened {
		t.Fatal("normal access address unavailable")
	}
}

func TestEscapeHidesAndQuitShortcutUsesFakeCallback(t *testing.T) {
	hidden, quit := 0, 0
	d := newDesktopUI(ViewState{}, Actions{})
	d.hide = func() { hidden++ }
	d.quit = func() { quit++ }
	tt := ui.NewTester(d.view, 640, 780)
	tt.Key(0, ui.KeyEscape)
	tt.Key(ui.Cmd, ui.KeyQ)
	if hidden != 1 || quit != 1 {
		t.Fatalf("hide=%d quit=%d", hidden, quit)
	}
}

func TestExternalURLValidationAndErrorDoNotExposeSecret(t *testing.T) {
	for _, raw := range []string{"javascript:alert(1)", "file:///C:/private", "http://example.test", "https://user:pass@example.test", "https://example.test\\evil", "https://example.test:65536"} {
		if allowedExternalURL(raw) {
			t.Errorf("unsafe URL accepted: %q", raw)
		}
	}
	for _, raw := range []string{"https://owned.devtunnels.ms", "http://127.0.0.1:43123", "http://localhost:43123"} {
		if !allowedExternalURL(raw) {
			t.Errorf("valid URL rejected: %q", raw)
		}
	}
	d := newDesktopUI(ViewState{}, Actions{})
	calls := 0
	d.openURL = func(raw string) error { calls++; return errors.New(raw) }
	d.openLink("javascript:alert(1)")
	if calls != 0 {
		t.Fatal("unsafe URL reached shell")
	}
	d.openLink("https://owned.devtunnels.ms")
	if calls != 1 || d.notice != "无法打开浏览器，请复制链接后手动打开。" {
		t.Fatal("open error leaked link")
	}
}

func TestDeviceLoginChallengeAutoOpensOnceAndActionsStayAvailable(t *testing.T) {
	canceled, copied := 0, ""
	var opened []string
	prompt := ViewState{Busy: true, LoggingIn: true, LoginURL: "https://login.microsoft.com/device", LoginCode: "TEST12345"}
	d := newDesktopUI(ViewState{Busy: true, LoggingIn: true}, Actions{CancelLogin: func() { canceled++ }})
	d.openURL = func(raw string) error { opened = append(opened, raw); return nil }
	d.copyText = func(text string) { copied = text }
	d.applyState(prompt)
	tt := ui.NewTester(d.view, 640, 900)
	if !tt.HasText(prompt.LoginCode) {
		t.Fatalf("missing login challenge: %q", tt.Texts())
	}
	for range 3 {
		d.applyState(prompt)
		tt.Frame()
	}
	if len(opened) != 1 || opened[0] != prompt.LoginURL {
		t.Fatalf("automatic open calls: %q", opened)
	}
	if err := tt.Click("打开登录页"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("复制验证码"); err != nil {
		t.Fatal(err)
	}
	d.applyState(prompt)
	tt.Frame()
	if err := tt.Click("取消登录"); err != nil {
		t.Fatal(err)
	}
	if canceled != 1 || len(opened) != 2 || opened[1] != prompt.LoginURL || copied != prompt.LoginCode {
		t.Fatal("busy login controls did not dispatch or repeated snapshot reopened page")
	}
	// Even an already-queued challenge cannot reopen the page after the click.
	d.applyState(prompt)
	d.openLoginPage()
	tt.Frame()
	if len(opened) != 2 || tt.HasText(prompt.LoginCode) || d.loginOpenURL != "" || d.loginOpenCode != "" {
		t.Fatal("canceled challenge remained actionable or retained its attempt")
	}
	d.applyState(ViewState{}) // Cancellation acknowledged; the next login is new.
	d.applyState(prompt)
	d.applyState(prompt)
	if len(opened) != 3 {
		t.Fatal("cancel prevented a new login from reopening a reused challenge")
	}
}

func TestDeviceLoginAutoOpensOnlyLiteralAllowedPage(t *testing.T) {
	for _, raw := range []string{"https://login.microsoft.com/device", "https://microsoft.com/devicelogin", "https://www.microsoft.com/devicelogin"} {
		t.Run(raw, func(t *testing.T) {
			prompt := ViewState{Busy: true, LoggingIn: true, LoginURL: raw, LoginCode: "TEST12345"}
			d := newDesktopUI(prompt, Actions{})
			calls := 0
			d.openURL = func(url string) error {
				calls++
				if calls != 1 || url != raw || strings.Contains(url, prompt.LoginCode) {
					t.Fatal("duplicate open or nonliteral verification URL")
				}
				if d.loginOpenURL != raw || d.loginOpenCode != prompt.LoginCode {
					t.Fatal("attempt was not marked before calling shell")
				}
				d.applyState(prompt) // A reentrant/repetitive update must not retry.
				return nil
			}
			d.applyState(d.state) // Initial snapshot is applied after adapters are ready.
			prompt.Message = "periodic controller status"
			d.applyState(prompt)
			if calls != 1 {
				t.Fatalf("shell calls = %d", calls)
			}
		})
	}
}

func TestDeviceLoginAutoOpenRejectsPartialInvalidAndInactivePrompts(t *testing.T) {
	valid := ViewState{Busy: true, LoggingIn: true, LoginURL: "https://login.microsoft.com/device", LoginCode: "TEST12345"}
	cases := []struct {
		name  string
		state ViewState
	}{
		{"waiting", ViewState{Busy: true, LoggingIn: true}},
		{"url without code", ViewState{Busy: true, LoggingIn: true, LoginURL: valid.LoginURL}},
		{"code without url", ViewState{Busy: true, LoggingIn: true, LoginCode: valid.LoginCode}},
		{"ended with stale prompt", ViewState{LoginURL: valid.LoginURL, LoginCode: valid.LoginCode}},
	}
	for _, code := range []string{"     ", "SHORT", "0123456789ABCDEFG", "TEST\n1234", "TEST-1234", "验证码12345"} {
		s := valid
		s.LoginCode = code
		cases = append(cases, struct {
			name  string
			state ViewState
		}{"invalid code " + code, s})
	}
	for _, raw := range []string{
		"https://attacker.example/device", "http://login.microsoft.com/device",
		"https://login.microsoft.com.attacker.example/device", "https://login.microsoft.com@attacker.example/device",
		"https://login.microsoft.com:443/device", "https://login.microsoft.com/%64evice",
		"https://login.microsoft.com/device/", "https://login.microsoft.com/device?code=TEST12345",
		"https://login.microsoft.com/device#TEST12345", "https://login.microsoft.com/device ",
		"javascript:alert(1)", "file:///C:/private",
	} {
		s := valid
		s.LoginURL = raw
		cases = append(cases, struct {
			name  string
			state ViewState
		}{"invalid url " + raw, s})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDesktopUI(ViewState{}, Actions{})
			calls := 0
			d.openURL = func(string) error { calls++; return nil }
			d.applyState(tc.state)
			d.applyState(tc.state)
			d.openLoginPage() // Validate at the shell boundary, not just in auto-open.
			if calls != 0 {
				t.Fatal("invalid or inactive challenge reached shell")
			}
			d.applyState(valid)
			if calls != 1 {
				t.Fatal("partial prompt suppressed subsequent valid challenge")
			}
		})
	}
}

func TestDeviceLoginOpenFailureKeepsFallbackAndAllowsManualRetry(t *testing.T) {
	prompt := ViewState{Busy: true, LoggingIn: true, LoginURL: "https://login.microsoft.com/device", LoginCode: "TEST12345"}
	d := newDesktopUI(ViewState{}, Actions{})
	calls, copied, fail := 0, "", true
	d.openURL = func(string) error {
		calls++
		if fail {
			return errors.New("private shell diagnostic " + prompt.LoginCode)
		}
		return nil
	}
	d.copyText = func(text string) { copied = text }
	d.applyState(prompt)
	notice := d.notice
	for _, want := range []string{"无法打开浏览器", "打开登录页", "复制验证码", "https://login.microsoft.com/device"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("missing fallback %q: %q", want, notice)
		}
	}
	if strings.Contains(notice, prompt.LoginCode) || strings.Contains(notice, "private shell diagnostic") {
		t.Fatal("shell error or code leaked into notice")
	}
	tt := ui.NewTester(d.view, 640, 900)
	for range 3 {
		d.applyState(prompt)
		tt.Frame()
	}
	if calls != 1 || d.notice != notice || !tt.HasText(notice) {
		t.Fatal("periodic snapshot retried shell or erased fallback")
	}
	if err := tt.Click("打开登录页"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || d.notice != notice {
		t.Fatal("manual retry after failure unavailable")
	}
	if err := tt.Click("复制验证码"); err != nil {
		t.Fatal(err)
	}
	fail = false
	if err := tt.Click("打开登录页"); err != nil {
		t.Fatal(err)
	}
	d.applyState(prompt)
	if calls != 3 || copied != prompt.LoginCode || d.notice != "" {
		t.Fatal("successful manual retry did not clear fallback or repeated auto-open")
	}
}

func TestDeviceLoginAutoOpenResetsOnEndAndNewChallenge(t *testing.T) {
	prompt := ViewState{Busy: true, LoggingIn: true, LoginURL: "https://login.microsoft.com/device", LoginCode: "TEST12345"}
	d := newDesktopUI(ViewState{}, Actions{})
	calls := 0
	d.openURL = func(string) error { calls++; return nil }
	d.applyState(prompt)
	d.applyState(ViewState{Busy: true, LoggingIn: true})
	d.applyState(prompt)
	if calls != 1 {
		t.Fatal("intermediate partial update reset the current attempt")
	}
	prompt.LoginCode = "NEXT54321"
	d.applyState(prompt)
	d.applyState(prompt)
	if calls != 2 {
		t.Fatal("new challenge did not open exactly once")
	}
	d.applyState(ViewState{LoggedIn: true})
	if d.loginOpenURL != "" || d.loginOpenCode != "" || d.state.LoginCode != "" || d.state.LoginURL != "" {
		t.Fatal("completed login retained prompt")
	}
	d.applyState(prompt)
	d.applyState(prompt)
	if calls != 3 {
		t.Fatal("new login with reused code was suppressed")
	}
}

func TestCancelBeforeDeviceChallengeSuppressesQueuedPromptAndResets(t *testing.T) {
	calls, canceled := 0, 0
	d := newDesktopUI(ViewState{Busy: true, LoggingIn: true}, Actions{CancelLogin: func() { canceled++ }})
	d.openURL = func(string) error { calls++; return nil }
	tt := ui.NewTester(d.view, 640, 900)
	if !strings.Contains(strings.Join(tt.Texts(), " "), "获取后会自动打开登录页") {
		t.Fatal("waiting copy does not explain automatic page opening")
	}
	if calls != 0 {
		t.Fatal("browser opened before challenge existed")
	}
	if err := tt.Click("取消登录"); err != nil {
		t.Fatal(err)
	}
	prompt := ViewState{Busy: true, LoggingIn: true, LoginURL: "https://login.microsoft.com/device", LoginCode: "TEST12345"}
	d.applyState(prompt)
	d.applyState(prompt)
	d.openLoginPage()
	if canceled != 1 || calls != 0 || d.state.LoginCode != "" || d.state.LoginURL != "" {
		t.Fatal("queued challenge opened or restored canceled prompt")
	}
	d.applyState(ViewState{Busy: true}) // Controller acknowledges cancellation.
	d.applyState(prompt)                // A new attempt may reuse the test code.
	d.applyState(prompt)
	if calls != 1 || d.loginCanceled {
		t.Fatal("cancel suppression survived into the next login")
	}
}

func TestInitialSnapshotIsIndependentValue(t *testing.T) {
	s := ViewState{DesktopStatus: "before", StartHidden: true}
	d := newDesktopUI(s, Actions{})
	s.DesktopStatus = "after"
	if d.state.DesktopStatus != "before" || !d.state.StartHidden {
		t.Fatal("initial snapshot aliases caller")
	}
}
