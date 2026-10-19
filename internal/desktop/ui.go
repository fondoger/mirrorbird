package desktop

import (
	_ "embed"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/updater"
	"github.com/egoist/mygo/plugins/updater/native"
	"github.com/egoist/mygo/ui"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/diagnostics"
)

// ViewState is a value-only controller snapshot posted to the UI thread.
type ViewState struct {
	DesktopStatus      string
	DesktopVersion     string
	TunnelStatus       string
	TunnelURL          string
	PendingTunnelURL   string // Authenticated address; access checks still pending.
	LoginURL           string // Ephemeral Microsoft device verification URL.
	LoginCode          string // Ephemeral user code; never persisted.
	LoggingIn          bool
	Message            string
	Busy               bool
	Starting           bool // Sharing startup is cancelable even while Busy.
	StartHidden        bool // Only affects the first window show (e.g. --background).
	Connected          bool
	DesktopRunning     bool
	AttachmentOutdated bool
	Sharing            bool
	LoggedIn           bool // Microsoft Dev Tunnel account, not Desktop authentication.
	AutoStart          bool
}

// Actions run on the UI thread and must return promptly. The controller owns
// asynchronous work and publishes its results through the update callback.
// Nil actions are supported and their corresponding controls are disabled.
type Actions struct {
	Refresh        func()
	LaunchDesktop  func()
	RestartDesktop func()
	Login          func()
	CancelLogin    func()
	Logout         func()
	StartSharing   func()
	StopSharing    func()
	SetAutoStart   func(bool)
}

// Version is shared by the settings window and CLI. MyGo supplies the packaged
// version; an unpackaged executable without version metadata is a dev build.
func Version() string {
	if version := mygo.App.Version(); version != "" {
		return version
	}
	return "dev"
}

// Run must be called on the main goroutine. Closing the window hides it; Quit
// calls onQuit once after the native event loop stops, before releasing the
// single-instance lock or relaunching. Initialization failures also clean up.
// onReady receives an update function safe to call from any goroutine. A second
// instance focuses the first one and returns without calling onReady or onQuit.
func Run(initial ViewState, actions Actions, onReady func(update func(ViewState)), onQuit func()) error {
	mygo.App.SetName("MirrorBird")
	stopLogs := func() {}
	cleanup := func() {
		diagnostics.Event("app.stopping")
		if onQuit != nil {
			onQuit()
		}
		diagnostics.Event("app.stopped")
		stopLogs()
	}
	return runWithLifecycle(mygo.App, cleanup, func() error {
		home, err := userDataDirectory()
		if err != nil {
			return err
		}
		mygo.App.SetPath(mygo.PathUserData, home)
		stopLogs, err = diagnostics.Start(home)
		if err != nil {
			initial.Message = "本地诊断日志不可用，请检查设置目录权限。"
		}
		diagnostics.Event("app.started", diagnostics.String("version", Version()),
			diagnostics.String("os", runtime.GOOS), diagnostics.String("arch", runtime.GOARCH))
		// Only the first instance registers the plugin. Keep its daily automatic
		// checks and leave automatic downloads to the user's updater preference.
		mygo.Use(native.New(updaterOptions()))
		return runUI(initial, actions, onReady)
	})
}

// lifecycleApp is the narrow MyGo boundary exercised without starting a native
// app in tests. Run uses the same registration, acquired gate and fallback.
type lifecycleApp interface {
	OnQuit(func()) func()
	RequestSingleInstanceLock() bool
}

func runWithLifecycle(app lifecycleApp, onQuit func(), run func() error) error {
	var acquired atomic.Bool
	var once sync.Once
	cleanup := func() {
		if acquired.Load() {
			once.Do(func() {
				if onQuit != nil {
					onQuit()
				}
			})
		}
	}
	// MyGo fires OnQuit in registration order, then relaunches. Register before
	// RequestSingleInstanceLock adds its release callback, and wait for cleanup
	// synchronously so the replacement cannot start while sharing is still up.
	offQuit := app.OnQuit(cleanup)
	defer offQuit()
	defer cleanup() // App.Run may fail before it ever emits OnQuit.
	if !app.RequestSingleInstanceLock() {
		return nil
	}
	acquired.Store(true)
	return run()
}

// Keep native window state and updater preferences in the controller's home,
// including MIRRORBIRD_HOME overrides used for isolation.
func userDataDirectory() (string, error) {
	home, err := attachment.Home()
	if err != nil {
		return "", fmt.Errorf("resolve MirrorBird settings directory: %w", err)
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return "", fmt.Errorf("create MirrorBird settings directory: %w", err)
	}
	return home, nil
}

func updaterOptions() updater.Options {
	return updater.Options{
		Language: "zh-Hans",
		Strings: map[string]updater.Strings{
			"zh-Hans": {
				ReadyMessage: "%[1]s %[2]s 已准备就绪。重启 MirrorBird 会停止共享；请重新启动 Desktop 以加载新组件。",
			},
		},
	}
}

func runUI(initial ViewState, actions Actions, onReady func(update func(ViewState))) error {
	d := newDesktopUI(initial, actions)
	d.copyText = mygo.Clipboard.WriteText
	d.openURL = mygo.Shell.OpenExternal
	d.quit = mygo.App.Quit
	d.checkForUpdates = updater.CheckForUpdates
	d.logProblem = diagnostics.Problem
	d.openLogs = func() error {
		path := diagnostics.Path()
		if path == "" {
			return errors.New("diagnostics unavailable")
		}
		return mygo.Shell.OpenPath(filepath.Dir(path))
	}
	var stopped atomic.Bool
	defer stopped.Store(true)
	var readyErr error

	offBeforeQuit := mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		d.quitting = true
		stopped.Store(true)
	})
	defer offBeforeQuit()
	offWillQuit := mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		if d.tray != nil {
			d.tray.Destroy()
		}
	})
	defer offWillQuit()
	offSecond := mygo.App.OnSecondInstance(func([]string, string) { d.show() })
	defer offSecond()
	offActivate := mygo.App.OnActivate(func(bool) { d.show() })
	defer offActivate()

	mygo.App.WhenReady(func() {
		var err error
		d.tray, err = mygo.NewTray(mygo.TrayOptions{
			Icon:    appIcon,
			ToolTip: "MirrorBird",
		})
		if err != nil {
			readyErr = fmt.Errorf("create MirrorBird tray: %w", err)
			mygo.App.Quit()
			return
		}
		d.win = mygo.NewWindow(mygo.WindowOptions{
			Title:           "MirrorBird",
			Width:           640,
			Height:          700,
			MinWidth:        480,
			MinHeight:       480,
			TitleBarStyle:   mygo.TitleBarHidden,
			TitleBarHeight:  48,
			BackgroundColor: "#ffffff",
			Hidden:          true,
			StateKey:        "settings",
			Content:         ui.View(d.view),
		})
		if err := d.win.SetIcon(appIcon); err != nil {
			readyErr = fmt.Errorf("set MirrorBird window icon: %w", err)
			mygo.App.Quit()
			return
		}
		d.hide = d.win.Hide
		d.win.OnClose(func(e *mygo.CloseEvent) {
			if !d.quitting {
				e.PreventDefault()
				d.win.Hide()
			}
		})
		d.installTrayMenu()
		if !initial.StartHidden {
			d.show()
		}
		d.applyState(d.state)
		if onReady != nil {
			onReady(func(next ViewState) {
				if stopped.Load() {
					return
				}
				d.win.Update(func() {
					if !stopped.Load() {
						d.applyState(next)
					}
				})
			})
		}
	})
	if err := mygo.App.Run(); err != nil {
		return err
	}
	return readyErr
}

// Only the native UI thread accesses desktopUI. Keeping the system adapters
// here, rather than in the view, also lets ui.NewTester exercise it headlessly.
type desktopUI struct {
	state           ViewState
	actions         Actions
	win             *mygo.Window
	tray            *mygo.Tray
	stopItem        *mygo.MenuItem
	quitting        bool
	notice          string
	copyText        func(string)
	openURL         func(string) error
	hide            func()
	quit            func()
	version         string
	checkForUpdates func()
	openLogs        func() error
	logProblem      func() string
	logo            *ui.Bitmap

	// Only the last attempted prompt is retained, in memory, until login ends.
	loginOpenURL   string
	loginOpenCode  string
	loginCanceled  bool
	confirmRestart bool
	confirmStop    bool
	confirmLogout  bool
}

func newDesktopUI(initial ViewState, actions Actions) *desktopUI {
	d := &desktopUI{state: initial, actions: actions, version: Version()}
	var err error
	d.logo, err = ui.DecodeBitmap(appIcon)
	if err != nil {
		d.notice = "无法读取应用图标。"
	}
	return d
}

// applyState runs on the UI thread with a controller snapshot.
// Rendering and periodic snapshots must not repeatedly open the login page.
func (d *desktopUI) applyState(next ViewState) {
	samePrompt := d.state.LoggingIn && next.LoggingIn &&
		d.state.LoginURL == next.LoginURL && d.state.LoginCode == next.LoginCode
	if !samePrompt && (d.state.LoggingIn || next.LoggingIn || d.state.Message != next.Message) {
		d.notice = ""
	}
	d.state = next
	if next.Busy || (!next.AttachmentOutdated && (next.Connected || !next.DesktopRunning)) {
		d.confirmRestart = false
	}
	if !next.Sharing {
		d.confirmStop = false
	}
	if next.Busy || !next.LoggedIn {
		d.confirmLogout = false
	}
	if !next.LoggingIn {
		d.loginOpenURL, d.loginOpenCode = "", ""
		d.loginCanceled = false
	}
	if d.loginCanceled || !next.LoggingIn {
		// A challenge queued before Cancel must stay hidden until the controller
		// acknowledges the end of that attempt with LoggingIn == false.
		d.state.LoggingIn = false
		d.state.LoginURL, d.state.LoginCode = "", ""
	} else if d.hasLoginChallenge() && (d.loginOpenURL != next.LoginURL || d.loginOpenCode != next.LoginCode) {
		d.openLoginPage()
	}
	d.updateTray()
}

func (d *desktopUI) cancelLogin() {
	if !d.state.LoggingIn || d.actions.CancelLogin == nil {
		return
	}
	// Suppress already-posted challenges immediately, before calling the controller.
	d.loginCanceled = true
	d.loginOpenURL, d.loginOpenCode = "", ""
	d.state.LoggingIn = false
	d.state.LoginURL, d.state.LoginCode = "", ""
	d.notice = ""
	d.actions.CancelLogin()
}

func (d *desktopUI) show() {
	if d.win == nil || d.quitting {
		return
	}
	d.win.Restore()
	d.win.Show()
	d.win.Focus()
}

func (d *desktopUI) trayMenuItems() []*mygo.MenuItem {
	d.stopItem = &mygo.MenuItem{
		Label: "停止共享",
		Click: func(*mygo.MenuItem, *mygo.Window) {
			if d.canStopSharing() {
				d.actions.StopSharing()
			}
		},
	}
	return []*mygo.MenuItem{
		{Label: "打开 MirrorBird", Click: func(*mygo.MenuItem, *mygo.Window) { d.show() }},
		d.stopItem,
		mygo.Separator(),
		{Label: "检查更新", Disabled: d.checkForUpdates == nil, Click: func(*mygo.MenuItem, *mygo.Window) {
			if d.checkForUpdates != nil {
				d.checkForUpdates()
			}
		}},
		{Label: "退出 MirrorBird", Role: mygo.RoleQuit},
	}
}

func (d *desktopUI) installTrayMenu() {
	menu := mygo.NewMenu(d.trayMenuItems())
	if runtime.GOOS == "linux" {
		d.tray.SetMenu(menu)
	} else {
		d.tray.OnClick(d.show)
		d.tray.OnRightClick(func() {
			d.tray.SetMenu(menu)
			defer d.tray.SetMenu(nil)
			d.tray.PopUpMenu()
		})
	}
	d.updateTray()
}

func (d *desktopUI) updateTray() {
	if d.stopItem != nil {
		d.stopItem.SetEnabled(d.canStopSharing())
	}
	if d.tray != nil {
		status := "共享已关闭"
		if d.state.Sharing {
			status = "正在共享"
		} else if d.state.Starting {
			status = "正在开启共享"
		}
		d.tray.SetToolTip("MirrorBird · " + status)
	}
}

func (d *desktopUI) canStopSharing() bool {
	return (d.state.Sharing || d.state.Starting || d.state.Busy) && d.actions.StopSharing != nil
}

func (d *desktopUI) canStartSharing() bool {
	return !d.state.Busy && !d.state.Starting && !d.state.Sharing &&
		d.state.Connected && !d.state.AttachmentOutdated && d.state.LoggedIn && d.actions.StartSharing != nil
}

func allowedLoginURL(raw string) bool {
	return raw == "https://login.microsoft.com/device" || raw == "https://microsoft.com/devicelogin" || raw == "https://www.microsoft.com/devicelogin"
}

func (d *desktopUI) hasLoginChallenge() bool {
	if !d.state.LoggingIn || d.loginCanceled || !allowedLoginURL(d.state.LoginURL) {
		return false
	}
	// The controller receives bounded ASCII codes from the parser. Check again
	// before any shell action so a partial or invalid snapshot cannot launch it.
	code := d.state.LoginCode
	if len(code) < 6 || len(code) > 16 {
		return false
	}
	for i := range len(code) {
		c := code[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (d *desktopUI) openLoginPage() {
	if !d.hasLoginChallenge() {
		return
	}
	// Mark before calling the shell, including failures. Only an explicit click
	// may retry this prompt; never include its code in the URL or shell error.
	d.loginOpenURL, d.loginOpenCode = d.state.LoginURL, d.state.LoginCode
	if d.openURL == nil || d.openURL(d.state.LoginURL) != nil {
		d.notice = "无法打开浏览器。可点击“打开登录页”重试，或手动访问 https://login.microsoft.com/device，再点击“复制验证码”并在该页输入。"
		return
	}
	d.notice = ""
}

func (d *desktopUI) openLink(raw string) {
	// Validate again at the system boundary, not only when enabling a button.
	if d.openURL == nil || !allowedExternalURL(raw) {
		d.notice = "无法打开此地址：仅支持 HTTPS 或本机 HTTP 地址。"
		return
	}
	if err := d.openURL(raw); err != nil {
		// Shell errors can repeat their arguments; keep the fallback concise.
		d.notice = "无法打开浏览器，请复制链接后手动打开。"
	}
}

func allowedExternalURL(raw string) bool {
	if raw == "" || strings.ContainsRune(raw, '\\') || strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.Zone() == "" && ip.Unmap().IsLoopback()
}

// Generated from web/public/favicon.svg by tools/generate-icons.mjs.
//
//go:embed app-icon.png
var appIcon []byte
