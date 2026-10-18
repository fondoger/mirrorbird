package desktop

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/netip"
	"net/url"
	"os"
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
)

// ViewState is a value-only controller snapshot posted to the UI thread.
type ViewState struct {
	DesktopStatus  string
	DesktopVersion string
	TunnelStatus   string
	TunnelURL      string
	LoginURL       string // Ephemeral Microsoft device verification URL.
	LoginCode      string // Ephemeral user code; never persisted.
	LoggingIn      bool
	Message        string
	Busy           bool
	Starting       bool // Sharing startup is cancelable even while Busy.
	StartHidden    bool // Only affects the first window show (e.g. --background).
	Connected      bool
	Sharing        bool
	LoggedIn       bool // Microsoft Dev Tunnel account, not Desktop authentication.
	AutoStart      bool
}

// Actions run on the UI thread and must return promptly. The controller owns
// asynchronous work and publishes its results through the update callback.
// Nil actions are supported and their corresponding controls are disabled.
type Actions struct {
	Refresh       func()
	LaunchDesktop func()
	Login         func()
	CancelLogin   func()
	Logout        func()
	StartSharing  func()
	StopSharing   func()
	SetAutoStart  func(bool)
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
	return runWithLifecycle(mygo.App, onQuit, func() error {
		home, err := userDataDirectory()
		if err != nil {
			return err
		}
		mygo.App.SetPath(mygo.PathUserData, home)
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
		icon := trayIcon()
		var err error
		d.tray, err = mygo.NewTray(mygo.TrayOptions{
			Icon:           icon,
			IconIsTemplate: runtime.GOOS == "darwin",
			ToolTip:        "MirrorBird",
		})
		if err != nil {
			readyErr = fmt.Errorf("create MirrorBird tray: %w", err)
			mygo.App.Quit()
			return
		}
		d.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "MirrorBird",
			Width:     640,
			Height:    780,
			MinWidth:  480,
			MinHeight: 480,
			Hidden:    true,
			StateKey:  "settings",
			Content:   ui.View(d.view),
		})
		_ = d.win.SetIcon(icon)
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

	// Only the last attempted prompt is retained, in memory, until login ends.
	loginOpenURL  string
	loginOpenCode string
	loginCanceled bool
}

func newDesktopUI(initial ViewState, actions Actions) *desktopUI {
	return &desktopUI{state: initial, actions: actions, version: Version()}
}

// applyState runs on the UI thread with a controller snapshot.
// Rendering and periodic snapshots must not repeatedly open the login page.
func (d *desktopUI) applyState(next ViewState) {
	samePrompt := d.state.LoggingIn && next.LoggingIn &&
		d.state.LoginURL == next.LoginURL && d.state.LoginCode == next.LoginCode
	if !samePrompt {
		d.notice = ""
	}
	d.state = next
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
		d.state.Connected && d.state.LoggedIn && d.actions.StartSharing != nil
}

func (d *desktopUI) view(c *ui.Context) {
	if d.hide != nil && (c.Shortcut(0, ui.KeyEscape) || c.Shortcut(ui.Cmd, ui.KeyW)) {
		d.hide()
	}
	if d.quit != nil {
		c.OnShortcut(ui.Cmd, ui.KeyQ, d.quit)
	}
	t := c.Theme()
	ui.Column(c).Fill().Children(func() {
		ui.Row(c).Padding(20, 24, 16, 24).Gap(12).Children(func() {
			ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
				ui.Text(c, "MirrorBird").FontSize(22).Bold()
				ui.Text(c, "在网页中继续电脑上的全部会话。").FontSize(12).TextColor(t.TextMuted)
			})
			if ui.Button(c, "收起").Label("收起到托盘").Disabled(d.hide == nil).Clicked() {
				d.hide()
			}
		})
		ui.Divider(c)
		ui.Scroll(c.Key("settings")).Grow(1).MinHeight(0).Padding(20, 24).Gap(20).Children(func() {
			d.connectionSection(c)
			ui.Divider(c)
			d.sharingSection(c)
			ui.Divider(c)
			d.generalSection(c)
		})
		ui.Divider(c)
		ui.Column(c).Padding(12, 24).Gap(4).Children(func() {
			message := d.state.Message
			if message == "" {
				message = "关闭窗口后仍在托盘运行；退出会停止共享。"
			}
			ui.Text(c.Key("message"), message).Selectable().FontSize(12).TextColor(t.TextMuted)
			if d.state.LoggingIn {
				if ui.Button(c, "取消登录").Disabled(d.actions.CancelLogin == nil).Clicked() {
					d.cancelLogin()
				}
			} else if d.state.Busy {
				ui.Text(c, "正在处理，请稍候…").FontSize(12).TextColor(t.TextMuted)
			}
			if d.notice != "" {
				ui.Text(c.Key("notice"), d.notice).Selectable().FontSize(12)
			}
		})
	})
}

func (d *desktopUI) generalSection(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Gap(8).Children(func() {
		ui.Text(c, "通用").FontSize(15).Bold()
		if ui.Checkbox(c.Key("auto-start"), &d.state.AutoStart, "登录 Windows 后启动 MirrorBird").
			Disabled(d.state.Busy || d.actions.SetAutoStart == nil).Changed() {
			d.actions.SetAutoStart(d.state.AutoStart)
		}
		ui.Text(c, "只启动 MirrorBird，不会自动开启共享。").FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "MirrorBird 版本 "+d.version).FontSize(12).TextColor(t.TextMuted)
			if ui.Button(c, "检查更新").Disabled(d.checkForUpdates == nil).Clicked() {
				d.checkForUpdates()
			}
		})
	})
}

func (d *desktopUI) connectionSection(c *ui.Context) {
	s, a, t := &d.state, d.actions, c.Theme()
	ui.Column(c).Gap(10).Children(func() {
		ui.Text(c, "桌面连接").FontSize(15).Bold()
		status := s.DesktopStatus
		if status == "" {
			status = "尚未连接"
			if s.Connected {
				status = "已连接"
			}
		}
		ui.Text(c, status).Selectable()
		if s.DesktopVersion != "" {
			ui.Text(c, "Desktop 版本 "+s.DesktopVersion).FontSize(12).TextColor(t.TextMuted)
		}
		account := "Dev Tunnel · Microsoft 账号未登录"
		if s.LoggedIn {
			account = "Dev Tunnel · Microsoft 账号已登录"
		}
		ui.Text(c, account).FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Wrap().Gap(8).Children(func() {
			if !s.Connected {
				if ui.Button(c, "启动 Desktop").Disabled(s.Busy || a.LaunchDesktop == nil).Clicked() {
					a.LaunchDesktop()
				}
			}
			if ui.Button(c, "刷新连接").Disabled(s.Busy || a.Refresh == nil).Clicked() {
				a.Refresh()
			}
			if s.LoggingIn {
				ui.Text(c, "等待 Microsoft 登录确认").FontSize(12).TextColor(t.TextMuted)
			} else if s.LoggedIn {
				if ui.Button(c, "退出 Dev Tunnel 登录").Disabled(s.Busy || a.Logout == nil).Clicked() {
					a.Logout()
				}
			} else if ui.Button(c, "登录 Dev Tunnel").Disabled(s.Busy || a.Login == nil).Clicked() {
				a.Login()
			}
		})
		if s.LoggingIn {
			ui.Column(c.Key("device-login")).Gap(8).Children(func() {
				if !d.hasLoginChallenge() {
					ui.Text(c, "正在向 Microsoft 获取验证码，获取后会自动打开登录页。其他设置暂不可用，可在下方取消登录。").FontSize(12).TextColor(t.TextMuted)
				} else {
					ui.Text(c, "在 Microsoft 登录页输入下面的验证码：").FontSize(12)
					ui.Text(c.Key("login-code"), s.LoginCode).FontSize(22).Bold().Selectable()
					ui.Row(c).Wrap().Gap(8).Children(func() {
						if ui.Button(c, "打开登录页").Disabled(d.openURL == nil || !allowedLoginURL(s.LoginURL)).Clicked() {
							d.openLoginPage()
						}
						if ui.Button(c, "复制验证码").Disabled(d.copyText == nil).Clicked() {
							d.copyText(s.LoginCode)
							d.notice = "已复制验证码。仅在你自己发起的 Microsoft 登录页使用。"
						}
					})
					ui.Text(c, "若浏览器未出现，请点击“打开登录页”，或手动访问 https://login.microsoft.com/device 并复制验证码。完成验证后会自动更新；验证码不保存到配置或日志。").FontSize(12).TextColor(t.TextMuted)
				}
			})
		}
		if !s.Connected {
			ui.Text(c, "通过 MirrorBird 启动 Desktop 后手动刷新；不会关闭现有 Desktop。").FontSize(12).TextColor(t.TextMuted)
		}
		if !s.LoggedIn {
			ui.Text(c, "Dev Tunnel 使用 Microsoft 账号独立登录，不要求 Desktop 已连接。").FontSize(12).TextColor(t.TextMuted)
		}
	})
}

func (d *desktopUI) sharingSection(c *ui.Context) {
	s, a, t := &d.state, d.actions, c.Theme()
	ui.Column(c).Gap(10).Children(func() {
		ui.Text(c, "远程共享").FontSize(15).Bold()
		status := s.TunnelStatus
		if status == "" {
			status = "共享已关闭"
			if s.Sharing {
				status = "共享已开启"
			}
		}
		ui.Text(c, status).Selectable()
		ui.Row(c).Wrap().Gap(8).Children(func() {
			if s.Sharing || s.Starting || s.Busy {
				if ui.Button(c, "停止共享").Disabled(!d.canStopSharing()).Clicked() {
					a.StopSharing()
				}
			} else if ui.PrimaryButton(c, "开启共享").Disabled(!d.canStartSharing()).Clicked() {
				a.StartSharing()
			}
		})
		if !s.Sharing {
			if s.Starting {
				ui.Text(c, "正在验证私有 tunnel，尚未允许远程访问。可随时停止。").FontSize(12).TextColor(t.TextMuted)
			} else {
				ui.Text(c, "连接 Desktop 并登录 Dev Tunnel 后，开启远程服务即可在网页访问全部会话。无需选择或配对。").FontSize(12).TextColor(t.TextMuted)
			}
			return
		}
		if !s.Connected {
			ui.Text(c, "Desktop 已断开，网页暂时无法访问会话。你仍可停止远程服务。").FontSize(12).TextColor(t.TextMuted)
		}
		if s.TunnelURL != "" {
			d.linkField(c, "访问地址", &s.TunnelURL, "复制地址", "打开地址")
		} else {
			ui.Text(c, "正在等待访问地址；若长时间未出现，请停止共享后重试。").FontSize(12).TextColor(t.TextMuted)
		}
		ui.Text(c, "网页自动连接，可自行选择全部会话。访问仍需通过 Microsoft 私有 Tunnel 验证。").FontSize(12).TextColor(t.TextMuted)
	})
}

func (d *desktopUI) linkField(c *ui.Context, label string, value *string, copyLabel, openLabel string) {
	ui.Column(c.Key(label)).Gap(6).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(c.Theme().TextMuted)
		ui.TextInput(c.Key("url"), value).ReadOnly(true).Label(label).FontSize(12).FillWidth()
		ui.Row(c).Wrap().Gap(8).Children(func() {
			if ui.Button(c, copyLabel).Disabled(d.copyText == nil).Clicked() {
				d.copyText(*value)
				d.notice = "已复制" + label + "。"
			}
			if ui.Button(c, openLabel).Disabled(d.openURL == nil || !allowedExternalURL(*value)).Clicked() {
				d.openLink(*value)
			}
		})
	})
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

// trayIcon is original artwork: two rounded blue wing strokes, drawn directly
// as a small transparent PNG. No font, downloaded asset, or image dependency.
func trayIcon() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	ink := color.NRGBA{R: 57, G: 121, B: 201, A: 255}
	if runtime.GOOS == "darwin" {
		ink = color.NRGBA{A: 255}
	}
	segments := [][4]float64{
		{5, 14, 13, 20}, {13, 20, 27, 6},
		{7, 23, 14, 27}, {14, 27, 25, 16},
	}
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			covered := 0
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					px, py := float64(x)+(float64(sx)+0.5)/4, float64(y)+(float64(sy)+0.5)/4
					for _, s := range segments {
						dx, dy := s[2]-s[0], s[3]-s[1]
						along := max(0.0, min(1.0, ((px-s[0])*dx+(py-s[1])*dy)/(dx*dx+dy*dy)))
						xd, yd := px-s[0]-along*dx, py-s[1]-along*dy
						if xd*xd+yd*yd <= 1.6*1.6 {
							covered++
							break
						}
					}
				}
			}
			pixel := ink
			pixel.A = uint8(covered * 255 / 16)
			img.SetNRGBA(x, y, pixel)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}
