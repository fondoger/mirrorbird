package desktop

import (
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestSettingsPreviewAddressCannotOpenBeforeVerification(t *testing.T) {
	copied, opened := "", 0
	d := newDesktopUI(ViewState{Starting: true, Busy: true, Connected: true, LoggedIn: true,
		PendingTunnelURL: "https://owned-43123.usw2.devtunnels.ms", TunnelStatus: "隧道已建立，正在验证连接"}, Actions{})
	d.copyText = func(text string) { copied = text }
	d.openURL = func(string) error { opened++; return nil }
	tt := ui.NewTester(d.view, 640, 700)
	if !tt.HasText("隧道已建立，正在验证连接") {
		t.Fatal(tt.Texts())
	}
	if err := tt.Click("复制地址"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("打开地址"); err != nil {
		t.Fatal(err)
	}
	if copied != d.state.PendingTunnelURL || opened != 0 {
		t.Fatal("unverified preview opened or could not be copied")
	}
	d.applyState(ViewState{})
	tt.Frame()
	if tt.HasText(copied) {
		t.Fatal("canceled address remained visible")
	}
}

func TestSettingsAutoStartAndLogsRemainFunctional(t *testing.T) {
	var values []bool
	logs := 0
	d := newDesktopUI(ViewState{}, Actions{SetAutoStart: func(enabled bool) { values = append(values, enabled) }})
	d.openLogs = func() error { logs++; return nil }
	tt := ui.NewTester(d.view, 640, 700)
	if err := tt.Click("开机启动开关"); err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || !values[0] {
		t.Fatal("autostart did not send the new value", values)
	}
	d.applyState(ViewState{Busy: true, Starting: true})
	tt.Frame()
	if err := tt.Click("查看日志"); err != nil {
		t.Fatal(err)
	}
	if logs != 1 {
		t.Fatal("logs were unavailable while startup was busy")
	}
}

func TestLogOpenResultIsVisible(t *testing.T) {
	for _, failed := range []bool{false, true} {
		d := newDesktopUI(ViewState{}, Actions{})
		d.openLogs = func() error {
			if failed {
				return errors.New("shell open failed")
			}
			return nil
		}
		tt := ui.NewTester(d.versionSection, 480, 120)
		if err := tt.Click("查看日志"); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		message := "已打开日志文件夹"
		if failed {
			message = "无法打开日志文件夹，请检查目录权限。"
			if d.notice != message {
				t.Fatal("open failure was not retained in settings")
			}
		}
		rect, ok := tt.Find(message)
		if !ok || rect.Y < 0 || rect.Y+rect.H > 120 {
			t.Fatal("open result was not visible", message, rect, tt.Texts())
		}
	}
}

func TestSettingsLogoutAndEscapeRequireConfirmation(t *testing.T) {
	logouts, hidden := 0, 0
	d := newDesktopUI(ViewState{Connected: true, LoggedIn: true, Sharing: true},
		Actions{Logout: func() { logouts++ }})
	d.hide = func() { hidden++ }
	tt := ui.NewTester(d.view, 640, 700)
	if err := tt.Click("退出 Dev Tunnel 登录"); err != nil {
		t.Fatal(err)
	}
	if logouts != 0 || !tt.HasText("退出 Microsoft 登录？") {
		t.Fatal("logout bypassed confirmation")
	}
	tt.Key(0, ui.KeyEscape)
	if logouts != 0 || hidden != 0 || d.confirmLogout {
		t.Fatal("Escape should dismiss the dialog, not hide the window or log out")
	}
	if err := tt.Click("退出 Dev Tunnel 登录"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("确认退出登录"); err != nil {
		t.Fatal(err)
	}
	if logouts != 1 {
		t.Fatal("confirmed logout was not dispatched")
	}
}

func TestNativeDesignLayouts(t *testing.T) {
	ready := ViewState{Connected: true, LoggedIn: true, Sharing: true, TunnelURL: "https://mirrorbird-demo-43123.usw2.devtunnels.ms"}
	longAddress := ready
	longAddress.TunnelURL = "https://" + strings.Repeat("a", 48) + "-43123.usw2.devtunnels.ms"
	for _, tc := range []struct {
		name  string
		width int
		state ViewState
	}{
		{"ready", 640, ready}, {"compact", 480, ready},
		{"short", 480, ready}, {"long-address", 480, longAddress},
		{"confirm-stop", 640, ready}, {"confirm-logout", 640, ready},
		{"off", 640, ViewState{}},
		{"verifying", 640, ViewState{Connected: true, LoggedIn: true, Busy: true, Starting: true,
			PendingTunnelURL: ready.TunnelURL, TunnelStatus: "隧道已建立，正在验证连接"}},
		{"login", 640, ViewState{Busy: true, LoggingIn: true, LoginURL: "https://login.microsoft.com/device", LoginCode: "TEST12345"}},
		{"restart", 640, ViewState{Connected: true, LoggedIn: true, DesktopRunning: true, AttachmentOutdated: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDesktopUI(tc.state, Actions{
				StartSharing: func() {}, StopSharing: func() {}, Refresh: func() {}, LaunchDesktop: func() {},
				RestartDesktop: func() {}, Login: func() {}, Logout: func() {}, CancelLogin: func() {}, SetAutoStart: func(bool) {},
			})
			d.copyText, d.openURL = func(string) {}, func(string) error { return nil }
			d.openLogs, d.checkForUpdates = func() error { return nil }, func() {}
			d.confirmRestart = tc.name == "restart"
			d.confirmStop, d.confirmLogout = tc.name == "confirm-stop", tc.name == "confirm-logout"
			height := 700
			if tc.name == "short" {
				height = 480
			}
			tt := ui.NewTester(d.view, tc.width, height)
			tt.SetTitleBar(ui.TitleBar{Height: 48, Right: 144})
			tt.Frame()
			if tc.name != "login" && tc.name != "restart" && !d.confirmStop && !d.confirmLogout {
				var previous float32
				for _, label := range []string{"设置", "远程访问", "Desktop", "Microsoft Dev Tunnel", "Windows 登录时启动"} {
					rect, ok := tt.Find(label)
					if !ok || rect.Y < previous || rect.X < 0 || rect.X+rect.W > float32(tc.width) {
						t.Fatal("wrong settings order or horizontal overflow", label, rect)
					}
					previous = rect.Y
				}
				rect, ok := tt.Find("查看日志")
				if !ok || rect.Y < 0 || rect.Y+rect.H > float32(height) {
					t.Fatal("logs must remain visible at the minimum window height", rect)
				}
			}
			if dir := os.Getenv("MIRRORBIRD_NATIVE_ARTIFACTS"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				f, err := os.Create(filepath.Join(dir, "native-"+tc.name+".png"))
				if err != nil {
					t.Fatal(err)
				}
				if err := png.Encode(f, tt.Image()); err != nil {
					_ = f.Close()
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
