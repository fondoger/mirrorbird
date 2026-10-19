package desktop

import "github.com/egoist/mygo/ui"

var settingsTheme = func() *ui.Theme {
	t := *ui.LightTheme()
	t.Background, t.Surface = ui.Hex("#ffffff"), ui.Hex("#ffffff")
	t.SurfaceHover, t.SurfacePressed = ui.Hex("#edf0f5"), ui.Hex("#e5e9f0")
	t.Text, t.TextMuted = ui.Hex("#20242c"), ui.Hex("#646b78")
	t.Border = ui.Hex("#e5e7ec")
	t.Accent, t.AccentHover, t.AccentPressed = ui.Hex("#2165d6"), ui.Hex("#174da9"), ui.Hex("#123d87")
	t.Success, t.Danger = ui.Hex("#247147"), ui.Hex("#ae3438")
	t.Focus, t.Selection = t.Accent, ui.Hex("#eaf1ff")
	t.Font, t.FontSize, t.Radius = "Segoe UI", 13, 8
	return &t
}()

func settingsButton(c *ui.Context, label string) ui.Element {
	return ui.Button(c, label).FontSize(13).FontWeight(550).MinHeight(44).Padding(8, 16).Radius(8)
}

func quietButton(c *ui.Context, label string) ui.Element {
	b := settingsButton(c, label).BorderWidth(0).Background(ui.RGBA(0, 0, 0, 0)).TextColor(c.Theme().TextMuted)
	if b.Hovered() {
		b.Background(c.Theme().SurfaceHover).TextColor(c.Theme().Text)
	}
	return b
}

func primaryButton(c *ui.Context, label string) ui.Element {
	return ui.PrimaryButton(c, label).FontSize(13).FontWeight(550).MinHeight(44).Padding(8, 16).Radius(8)
}

func (d *desktopUI) view(c *ui.Context) {
	c.SetTheme(settingsTheme)
	modal := d.state.LoggingIn || d.confirmRestart || d.confirmStop || d.confirmLogout
	if d.hide != nil && ((!modal && c.Shortcut(0, ui.KeyEscape)) || c.Shortcut(ui.Cmd, ui.KeyW)) {
		d.hide()
	}
	if d.quit != nil {
		c.OnShortcut(ui.Cmd, ui.KeyQ, d.quit)
	}
	ui.Column(c).Fill().Background(c.Theme().Background).Children(func() {
		bar := c.TitleBar()
		ui.Row(c.Key("titlebar")).Height(max(48, bar.Height)).Shrink(0).
			Padding(0, max(16, bar.Right), 0, max(16, bar.Left)).
			Gap(8).AlignItems(ui.Center).Background(ui.Hex("#fbfcfd")).DragWindow().Children(func() {
			if d.logo != nil {
				ui.Image(c, d.logo).Width(24).Height(24)
			}
			ui.Text(c, "MirrorBird").FontSize(13).FontWeight(600)
		})
		ui.Divider(c)
		ui.Scroll(c.Key("settings")).Grow(1).MinHeight(0).Padding(24, 32, 16, 32).Children(func() {
			ui.Text(c, "设置").FontSize(22).FontWeight(600).Padding(0, 0, 24, 0)
			d.sharingSection(c)
			d.connectionSection(c)
			d.generalSection(c)
			if d.state.Message != "" && !d.state.Starting && !d.state.LoggingIn {
				ui.Text(c.Key("message"), d.state.Message).FontSize(12).LineHeight(1.6).TextColor(c.Theme().TextMuted).Selectable()
			}
			if d.notice != "" && !d.state.LoggingIn {
				ui.Text(c.Key("notice"), d.notice).Padding(8, 0, 0, 0).FontSize(12).TextColor(c.Theme().Danger).Selectable()
			}
			if d.logProblem != nil {
				if problem := d.logProblem(); problem != "" {
					ui.Text(c, problem).Padding(8, 0, 0, 0).FontSize(12).TextColor(c.Theme().Danger)
				}
			}
		})
		ui.Divider(c)
		d.versionSection(c)
	})
	d.confirmationDialog(c)
	d.loginDialog(c)
}

func (d *desktopUI) sharingSection(c *ui.Context) {
	s, t := d.state, c.Theme()
	status, color := "未开启", t.TextMuted
	switch {
	case s.Starting:
		status = s.TunnelStatus
	case s.Sharing:
		status, color = "已开启", t.Success
	case s.AttachmentOutdated:
		status = "请先重启 Desktop 接入新版组件"
	case !s.Connected:
		status = "请先连接 Desktop"
	case !s.LoggedIn:
		status = "请先登录 Microsoft"
	}
	ui.Column(c.Key("remote-panel")).Border(1, t.Border).Radius(8).Clip().Children(func() {
		ui.Row(c).Padding(16).Gap(16).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).MinWidth(0).Gap(3).Children(func() {
				ui.Text(c, "远程访问").FontSize(15).FontWeight(550)
				ui.Text(c.Key("remote-status"), status).FontSize(13).TextColor(color)
			})
			on := s.Sharing || s.Starting
			disabled := !d.canStartSharing()
			if on {
				disabled = !d.canStopSharing()
			}
			if ui.Switch(c.Key("remote-switch"), &on).Label("远程访问开关").Disabled(disabled).Changed() {
				if s.Starting {
					d.actions.StopSharing()
				} else if s.Sharing {
					d.confirmStop = true
				} else if d.canStartSharing() {
					d.actions.StartSharing()
				}
			}
		})
		address := ""
		if s.Sharing {
			address = s.TunnelURL
		} else if s.Starting {
			address = s.PendingTunnelURL
		}
		if address != "" && allowedExternalURL(address) {
			ui.Divider(c)
			ui.Column(c.Key("address-area")).Padding(16).Gap(8).Background(ui.Hex("#f6f7f9")).Children(func() {
				ui.Text(c, "访问地址").FontSize(12).TextColor(t.TextMuted)
				ui.Text(c.Key("access-url"), address).Font("Consolas").FontSize(13).LineHeight(1.7).Selectable()
				if !s.Sharing {
					ui.Text(c, "验证完成后开放访问。现在可以复制地址。").FontSize(12).TextColor(t.TextMuted)
				}
				ui.Row(c).Justify(ui.End).Gap(8).Padding(4, 0, 0, 0).Children(func() {
					if primaryButton(c, "复制地址").Disabled(d.copyText == nil).Clicked() {
						d.copyText(address)
						c.Toast("地址已复制")
					}
					if settingsButton(c, "打开").Label("打开地址").
						Disabled(!s.Sharing || d.openURL == nil).Clicked() {
						d.openLink(address)
					}
				})
			})
		}
		if s.Busy && !s.Sharing && !s.Starting && !s.LoggingIn {
			ui.Row(c).Padding(0, 16, 8, 16).Justify(ui.End).Children(func() {
				if quietButton(c, "取消操作").Disabled(!d.canStopSharing()).Clicked() {
					d.actions.StopSharing()
				}
			})
		}
	})
}

func (d *desktopUI) connectionSection(c *ui.Context) {
	s, a, t := d.state, d.actions, c.Theme()
	ui.Column(c).Padding(16, 0, 0, 0).Children(func() {
		ui.Row(c.Key("desktop-row")).MinHeight(80).Padding(16, 0).Gap(16).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).MinWidth(0).Gap(3).Children(func() {
				ui.Text(c, "Desktop").FontSize(15).FontWeight(550)
				status, color := "未连接", t.TextMuted
				switch {
				case s.AttachmentOutdated:
					status, color = "连接组件需要更新", t.Danger
				case s.Connected:
					status = "已连接"
				case s.DesktopRunning:
					status = "已运行，尚未接入"
				}
				if s.DesktopVersion != "" {
					status += " · " + s.DesktopVersion
				}
				ui.Text(c, status).FontSize(13).TextColor(color).SingleLine()
			})
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				if s.AttachmentOutdated || (!s.Connected && s.DesktopRunning) {
					if quietButton(c, "重启并接入").Label("重启 Desktop 并接入").TextColor(t.Accent).
						Disabled(s.Busy || a.RestartDesktop == nil).Clicked() {
						d.confirmRestart = true
					}
				} else if !s.Connected {
					if quietButton(c, "启动并连接").Label("启动 Desktop").TextColor(t.Accent).
						Disabled(s.Busy || a.LaunchDesktop == nil).Clicked() {
						a.LaunchDesktop()
					}
				}
				if quietButton(c, "刷新").Label("刷新连接").Disabled(s.Busy || a.Refresh == nil).Clicked() {
					a.Refresh()
				}
			})
		})
		ui.Divider(c)
		ui.Row(c.Key("account-row")).MinHeight(80).Padding(16, 0).Gap(16).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).MinWidth(0).Gap(3).Children(func() {
				ui.Text(c, "Microsoft Dev Tunnel").FontSize(15).FontWeight(550)
				account := "未登录"
				if s.LoggingIn {
					account = "等待登录确认"
				} else if s.LoggedIn {
					account = "已登录"
				}
				ui.Text(c, account).FontSize(13).TextColor(t.TextMuted)
			})
			if s.LoggedIn {
				if quietButton(c, "退出登录").Label("退出 Dev Tunnel 登录").Disabled(s.Busy || a.Logout == nil).Clicked() {
					d.confirmLogout = true
				}
			} else if quietButton(c, "登录").Label("登录 Dev Tunnel").TextColor(t.Accent).
				Disabled(s.Busy || a.Login == nil).Clicked() {
				a.Login()
			}
		})
		ui.Divider(c)
	})
}

func (d *desktopUI) generalSection(c *ui.Context) {
	ui.Row(c.Key("startup-row")).Padding(20, 0).Gap(16).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(3).Children(func() {
			ui.Text(c, "Windows 登录时启动").FontSize(15).FontWeight(550)
			ui.Text(c, "只启动 MirrorBird，不会自动开启共享").FontSize(12).TextColor(c.Theme().TextMuted)
		})
		if ui.Switch(c.Key("auto-start"), &d.state.AutoStart).Label("开机启动开关").
			Disabled(d.state.Busy || d.actions.SetAutoStart == nil).Changed() {
			d.actions.SetAutoStart(d.state.AutoStart)
		}
	})
}

func (d *desktopUI) versionSection(c *ui.Context) {
	ui.Row(c.Key("footer")).Padding(4, 24).Gap(4).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "MirrorBird 版本 "+d.version).FontSize(12).TextColor(c.Theme().TextMuted).Grow(1).MinWidth(0).SingleLine()
		if quietButton(c, "检查更新").Disabled(d.checkForUpdates == nil).Clicked() {
			d.checkForUpdates()
		}
		if quietButton(c, "查看日志").Disabled(d.openLogs == nil).Clicked() {
			if err := d.openLogs(); err != nil {
				d.notice = "无法打开日志文件夹，请检查目录权限。"
				c.Toast(d.notice)
			} else {
				c.Toast("已打开日志文件夹")
			}
		}
	})
}

func settingsDialog(c *ui.Context, open *bool, title, closeLabel string, body, footer func()) {
	ui.DialogBase(c, open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.RGBA(35, 46, 66, .32)).Padding(16)
		panel.Width(440).MaxWidthPercent(100).MaxHeightPercent(100).Background(c.Theme().Background).
			Border(1, c.Theme().Border).Radius(12).Clip().Children(func() {
			ui.Row(c).Padding(12, 16, 4, 24).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, title).FontSize(18).FontWeight(600).Grow(1).MinWidth(0)
				if quietButton(c, "×").Label(closeLabel).Width(44).FontSize(20).Clicked() {
					*open = false
				}
			})
			ui.Scroll(c.Key("dialog-content")).MinHeight(0).Shrink(1).Padding(0, 24, 24, 24).Gap(12).Children(body)
			ui.Divider(c)
			ui.Row(c).Padding(16, 24).Justify(ui.End).Gap(8).Children(footer)
		})
	})
}

func (d *desktopUI) confirmationDialog(c *ui.Context) {
	if !d.confirmStop && !d.confirmLogout && !d.confirmRestart {
		return
	}
	title, description, accept, cancel := "", "", "", "取消"
	var action func()
	blocked := false
	switch {
	case d.confirmRestart:
		title, accept, cancel = "重启 ChatGPT Desktop？", "确认重启", "取消重启"
		description = "这会完全退出 ChatGPT Desktop、中断任务并停止共享。请先保存内容。若未正常退出，将结束此安装的后台和托盘进程，未保存内容可能丢失。"
		action, blocked = d.actions.RestartDesktop, d.state.Busy
	case d.confirmStop:
		title, accept = "停止远程访问？", "停止访问"
		description = "手机将无法访问，Desktop 任务不受影响。"
		action, blocked = d.actions.StopSharing, !d.canStopSharing()
	case d.confirmLogout:
		title, accept = "退出 Microsoft 登录？", "退出登录"
		description = "远程访问将停止，Desktop 任务不受影响。再次开启远程访问前需要重新登录。"
		action, blocked = d.actions.Logout, d.state.Busy || !d.state.LoggedIn
	}
	open := true
	acceptLabel := accept
	if d.confirmLogout {
		acceptLabel = "确认退出登录"
	}
	settingsDialog(c.Key("confirmation"), &open, title, "关闭确认框", func() {
		ui.Text(c, description).FontSize(14).LineHeight(1.75).TextColor(c.Theme().TextMuted)
	}, func() {
		if settingsButton(c, "取消").Label(cancel).Clicked() {
			open = false
		}
		if primaryButton(c, accept).Label(acceptLabel).Disabled(blocked || action == nil).Clicked() {
			open = false
			action()
		}
	})
	if !open {
		d.confirmStop, d.confirmLogout, d.confirmRestart = false, false, false
	}
}

func (d *desktopUI) loginDialog(c *ui.Context) {
	if !d.state.LoggingIn {
		return
	}
	open := true
	settingsDialog(c.Key("device-login"), &open, "登录 Microsoft", "关闭登录窗口", func() {
		if !d.hasLoginChallenge() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c).Width(16).Height(16)
				ui.Text(c, "正在获取验证码，随后会自动打开登录页。").FontSize(14).TextColor(c.Theme().TextMuted)
			})
			return
		}
		ui.Text(c, "1. 复制验证码").FontSize(14).Padding(8, 0, 0, 0)
		ui.Row(c).Padding(8, 12, 8, 16).Gap(12).Border(1, c.Theme().Border).Radius(8).
			Background(ui.Hex("#f6f7f9")).AlignItems(ui.Center).Children(func() {
			ui.Text(c.Key("login-code"), d.state.LoginCode).Font("Consolas").FontSize(24).Grow(1).MinWidth(0).Selectable()
			if quietButton(c, "复制").Label("复制验证码").Disabled(d.copyText == nil).Clicked() {
				d.copyText(d.state.LoginCode)
				c.Toast("验证码已复制")
			}
		})
		ui.Text(c, "2. 在登录页输入验证码").FontSize(14).Padding(4, 0, 0, 0)
		if settingsButton(c, "打开登录页").FillWidth().Disabled(d.openURL == nil).Clicked() {
			d.openLoginPage()
		}
		ui.Text(c, "完成验证后会自动更新。若浏览器未打开，可手动访问 https://login.microsoft.com/device。验证码不会保存到日志。").
			FontSize(12).LineHeight(1.6).TextColor(c.Theme().TextMuted).Selectable()
		if d.notice != "" {
			ui.Text(c, d.notice).FontSize(12).TextColor(c.Theme().Danger).Selectable()
		}
	}, func() {
		if settingsButton(c, "取消").Label("取消登录").Disabled(d.actions.CancelLogin == nil).Clicked() {
			open = false
		}
		primaryButton(c, "等待登录确认").Disabled(true)
	})
	if !open {
		d.cancelLogin()
	}
}
