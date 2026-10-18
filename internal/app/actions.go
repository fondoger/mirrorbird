package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"mirrorbird/internal/desktop"
	"mirrorbird/internal/session"
	"mirrorbird/internal/tunnel"
)

func (c *controller) refresh() {
	c.submit(false, operationTimeout, "正在刷新 Desktop 连接和 Dev Tunnel 账号状态。", c.refreshWork)
}

func (c *controller) refreshWork(ctx context.Context, r *resources, op *operation) string {
	messages := []string{}
	if !r.sessions.Connected() {
		// Stop the old remote-service lifetime before replacing its attachment,
		// even when the status timer has not observed the disconnect yet.
		c.mu.Lock()
		if c.currentLocked(op) && c.state.Sharing {
			c.stopLocked("Desktop 已断开，远程服务已停止。请再次刷新连接后重新开启。")
			c.mu.Unlock()
			return ""
		}
		c.mu.Unlock()
		if err := r.sessions.Connect(ctx); err != nil {
			messages = append(messages, "尚未连接 Desktop：请通过 MirrorBird 启动 Desktop，再手动刷新。不会强制关闭正在运行的 Desktop。")
			running, probeErr := r.running(ctx)
			c.mu.Lock()
			if c.currentLocked(op) {
				c.lastDesktopProbe = time.Now()
				if probeErr == nil {
					c.state.DesktopRunning = running
				}
			}
			c.mu.Unlock()
			if probeErr != nil {
				messages = append(messages, "无法检查 Desktop 是否在运行，请刷新重试。")
			} else if running {
				messages = append(messages, "检测到 Desktop 已运行但未接入 shim；可确认重启并接入。")
			}
		}
	}
	if ctx.Err() != nil || !c.current(op) {
		return ""
	}
	if r.sessions.Connected() {
		// Catalogue reads belong to the web client. Only a broken event ordering
		// requires replacing this attachment, after stopping its remote lifetime.
		err := r.sessions.Refresh(ctx)
		if errors.Is(err, session.ErrResyncRequired) {
			c.mu.Lock()
			if !c.currentLocked(op) {
				c.mu.Unlock()
				return ""
			}
			if c.state.Sharing {
				c.stopLocked("会话事件需要重新同步，远程服务已停止。再次点击“刷新连接”即可重新连接同一个 Desktop。")
				c.mu.Unlock()
				return ""
			}
			c.mu.Unlock()
			err = r.sessions.Connect(ctx)
			if err == nil {
				err = r.sessions.Refresh(ctx)
			}
		}
		if err != nil {
			messages = append(messages, "无法刷新 Desktop 连接，请在 Desktop 检查状态后重试。")
		} else if r.sessions.AttachmentOutdated() {
			messages = append(messages, "Desktop 使用旧版连接组件，请点击“重启 Desktop 并接入”。仅重启 MirrorBird 不会更新已运行的 shim。")
		} else {
			messages = append(messages, "Desktop 连接已刷新。会话可直接在网页选择。")
		}
	}
	if ctx.Err() != nil || !c.current(op) {
		return ""
	}
	account, err := r.tunnel.Account(ctx)
	c.setLoggedIn(op, err == nil && account.LoggedIn)
	if err != nil {
		messages = append(messages, tunnelMessage("无法检查 Dev Tunnel 账号", err))
	} else if !account.LoggedIn {
		messages = append(messages, "Dev Tunnel 未登录，请单独使用 Microsoft 账号登录后再开启共享。")
	} else {
		messages = append(messages, "Dev Tunnel 账号已登录。")
	}
	c.mu.Lock()
	if c.currentLocked(op) && c.state.Sharing && (err != nil || !account.LoggedIn || !r.sessions.Connected()) {
		c.stopLocked("无法确认 Desktop 或 Dev Tunnel 账号仍然有效，远程服务已停止。请检查后手动开启。")
	}
	c.mu.Unlock()
	return strings.Join(messages, " ")
}

func (c *controller) setLoggedIn(op *operation, loggedIn bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentLocked(op) {
		c.state.LoggedIn = loggedIn
	}
}

func (c *controller) login() {
	c.submit(false, loginTimeout, "正在向 Microsoft 获取设备验证码，获取后会自动打开登录页；若未打开可点击“打开登录页”重试，也可取消登录。", func(ctx context.Context, r *resources, op *operation) string {
		c.mu.Lock()
		if !c.currentLocked(op) {
			c.mu.Unlock()
			return ""
		}
		if c.host != nil {
			c.mu.Unlock()
			return "请先停止共享，再更改 Dev Tunnel 账号。"
		}
		c.state.LoggingIn = true
		c.state.LoginURL, c.state.LoginCode = "", ""
		c.publishLocked()
		c.mu.Unlock()
		// The CLI supplies a narrowly parsed challenge, never raw OAuth output.
		err := r.tunnel.LoginWithProgress(ctx, func(challenge tunnel.LoginChallenge) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if !c.currentLocked(op) || !c.state.LoggingIn {
				return
			}
			c.state.LoginURL, c.state.LoginCode = challenge.URL, challenge.Code
			c.state.Message = "已获取 Microsoft 验证码，正在尝试自动打开登录页。若未打开，请点击“打开登录页”或手动访问 https://login.microsoft.com/device，再复制验证码完成登录；也可取消。"
			c.publishLocked()
		})
		if err != nil {
			return tunnelMessage("Dev Tunnel 登录未完成", err)
		}
		if !c.current(op) {
			return ""
		}
		account, err := r.tunnel.Account(ctx)
		c.setLoggedIn(op, err == nil && account.LoggedIn)
		if err != nil {
			return tunnelMessage("无法确认 Dev Tunnel 登录状态", err)
		}
		if !account.LoggedIn {
			return "Dev Tunnel 仍未登录，请检查 Microsoft 账号后重试。"
		}
		return "已登录 Microsoft Dev Tunnel；共享仍需手动开启。"
	})
}

func (c *controller) clearLoginLocked() {
	c.state.LoggingIn = false
	c.state.LoginURL, c.state.LoginCode = "", ""
}

func (c *controller) cancelLogin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.state.LoggingIn || c.operation == nil || c.closing {
		return
	}
	c.operation.cancel()
	c.clearLoginLocked()
	c.state.Message = "正在取消登录并关闭登录进程；Desktop 连接和会话保持不变。"
	c.publishLocked()
}

func (c *controller) logout() {
	// A logout immediately revokes access, even though host reaping and the CLI
	// command remain serialized and asynchronous.
	c.mu.Lock()
	if c.closing || c.stopping || c.operation != nil || c.resources == nil {
		c.mu.Unlock()
		return
	}
	c.interruptLocked("共享已关闭，正在退出 Dev Tunnel 的 Microsoft 账号。")
	op := c.beginLocked(false, operationTimeout, c.state.Message)
	r, host := c.resources, c.host
	c.host = nil
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer op.cancel()
		c.serial.Lock()
		defer c.serial.Unlock()
		c.cleanup(r, host)
		if !c.current(op) {
			c.finish(op, "")
			return
		}
		if err := r.tunnel.Logout(op.ctx); err != nil {
			c.finish(op, tunnelMessage("Dev Tunnel 退出登录未完成", err))
			return
		}
		c.setLoggedIn(op, false)
		c.finish(op, "已退出 Microsoft Dev Tunnel 账号。Desktop 的登录和任务不受影响。")
	}()
}

func (c *controller) launchDesktop() {
	c.submit(false, operationTimeout, "正在检查 Desktop 安装；不会关闭现有 Desktop 或 Codex 进程。", func(ctx context.Context, r *resources, op *operation) string {
		return c.launchDesktopWork(ctx, r, op, false)
	})
}

func (c *controller) launchDesktopWork(ctx context.Context, r *resources, op *operation, restart bool) string {
	if r.sessions.Connected() && !restart {
		return "Desktop 已连接，无需再次启动。"
	}
	selected := desktop.Installation{Desktop: r.config.DesktopExecutable, Codex: r.config.CodexExecutable}
	installation, err := r.resolve(ctx, selected)
	if err != nil {
		if errors.Is(err, desktop.ErrStagedBackend) {
			return "未找到与当前 Desktop 安装包完整匹配的用户目录后端。请从 Windows 开始菜单正常打开一次 ChatGPT，让它准备运行文件，再退出并重试；不会使用不同版本的独立 Codex。"
		}
		return fmt.Sprintf("无法定位 Desktop/Codex 安装（%s）。请检查安装，或在 %s 配置 desktopExecutable 和 codexExecutable 的绝对路径。", err, filepath.Join(r.home, "settings.json"))
	}
	executable, err := r.executable()
	if err != nil || !filepath.IsAbs(executable) {
		return "无法定位 MirrorBird 程序；请从完整安装目录重新运行。"
	}
	shim := filepath.Join(filepath.Dir(executable), "mirrorbird-codex.exe")
	if !filepath.IsAbs(installation.Desktop) || !filepath.IsAbs(installation.Codex) {
		return "请选择有效的 Desktop 和 Codex 绝对路径；不会从当前目录猜测或运行程序。"
	}
	if !c.current(op) {
		return ""
	}
	// Existing Launch verifies files and refuses an already-running Desktop;
	// its process-scoped CODEX_CLI_PATH is never written to user/global env.
	// Once admitted, Launch has a bounded own check but no context parameter.
	if restart {
		err = r.restart(ctx, installation, r.home, shim)
	} else {
		err = r.launch(installation, r.home, shim)
	}
	if err != nil {
		return fmt.Sprintf("无法启动 Desktop：%s。请检查所选程序及 Windows 安全中心的保护历史记录；不会自动重试。", err)
	}
	c.mu.Lock()
	if c.currentLocked(op) {
		c.state.DesktopVersion = installation.Version
	}
	c.mu.Unlock()
	if restart {
		deadline := time.Now().Add(12 * time.Second)
		for ctx.Err() == nil && c.current(op) && time.Now().Before(deadline) {
			if r.sessions.Connect(ctx) == nil && !r.sessions.AttachmentOutdated() {
				return "Desktop 已重启并接入新版 shim；共享仍关闭，请按需重新开启。"
			}
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return fmt.Sprintf("已请求启动 Desktop，后端：%s。等待 Desktop 就绪后点击“刷新连接”；不会自动开启共享。", installation.Codex)
}

func (c *controller) restartDesktop() {
	c.mu.Lock()
	if c.closing || c.stopping || c.operation != nil || c.resources == nil ||
		(!c.state.AttachmentOutdated && (c.state.Connected || !c.state.DesktopRunning)) {
		c.mu.Unlock()
		return
	}
	c.interruptLocked("共享已关闭，正在按你的确认重启 Desktop 并接入 shim。")
	op := c.beginLocked(false, operationTimeout, c.state.Message)
	r, host := c.resources, c.host
	c.host = nil
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer op.cancel()
		c.serial.Lock()
		defer c.serial.Unlock()
		c.cleanup(r, host)
		if !c.current(op) {
			c.finish(op, "")
			return
		}
		r.sessions.Close()
		c.finish(op, c.launchDesktopWork(op.ctx, r, op, true))
	}()
}

func (c *controller) setAutoStart(enabled bool) {
	c.submit(false, operationTimeout, "正在更新当前 Windows 用户的启动项。", func(ctx context.Context, r *resources, op *operation) string {
		if enabled == r.config.AutoStart {
			return "启动设置未改变。"
		}
		if !c.current(op) {
			return ""
		}
		rollback, err := r.autoStart(enabled)
		if err != nil {
			return "无法修改当前用户的 Windows 启动项，设置未保存。不会请求管理员权限。"
		}
		if !c.current(op) {
			if rollback != nil {
				_ = rollback()
			}
			return ""
		}
		config := r.config
		config.AutoStart = enabled
		if err := r.store.Save(config); err != nil {
			if rollback != nil {
				if undoErr := rollback(); undoErr != nil {
					return "无法保存设置，且启动项回滚失败。请检查 Windows 当前用户启动项中的 MirrorBird 和 settings.json；未改动其他启动项。"
				}
			}
			return "无法保存设置，已恢复原启动项。请检查 MirrorBird 设置目录权限后重试。"
		}
		r.config = config
		// A successful persistent write is a fact even when Stop arrived while
		// Save was in progress. Reflect it, but never overwrite Stop's message.
		c.mu.Lock()
		c.state.AutoStart = enabled
		c.mu.Unlock()
		if enabled {
			return "已设置登录 Windows 后启动到托盘；仍不会自动共享或启动 Desktop。"
		}
		return "已关闭当前 Windows 用户的 MirrorBird 开机启动。"
	})
}

// The manager's public sentinels convey useful guidance without ever displaying
// raw CLI output, browser auth codes, tokens, or arbitrary backend RPC errors.
func tunnelMessage(action string, err error) string {
	switch {
	case errors.Is(err, tunnel.ErrExecutable):
		return action + "：请先自行安装受信任的 Microsoft Dev Tunnels CLI，或在 settings.json 的 tunnelExecutable 指定 devtunnel.exe 绝对路径。"
	case errors.Is(err, tunnel.ErrNotLoggedIn):
		return action + "：请使用 Microsoft 账号登录 Dev Tunnel。"
	case errors.Is(err, tunnel.ErrLoginNoChallenge):
		return action + "：30 秒内未取得 Microsoft 验证码。请检查网络或代理后再试；登录已取消。"
	case errors.Is(err, tunnel.ErrLoginTimeout):
		return action + "：验证码等待超时，登录已取消。请重新获取验证码。"
	case errors.Is(err, tunnel.ErrUnverified), errors.Is(err, tunnel.ErrOptions):
		return action + "：无法确认 tunnel 属于当前账号且仅开放所选端口。请检查已保存的 tunnel 配置；不会自动放宽权限。"
	case errors.Is(err, context.Canceled):
		return action + "：操作已取消。"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, tunnel.ErrNotReady):
		return action + "：等待超时。请检查本机网络和账号后手动重试。"
	case errors.Is(err, tunnel.ErrHostExited):
		return action + "：host 已退出，共享未开启。"
	case errors.Is(err, tunnel.ErrCommand):
		return action + "：" + err.Error() + "。请检查 CLI、账号和网络后重试。"
	case errors.Is(err, tunnel.ErrBusy):
		return action + "：另一项 Dev Tunnel 操作尚未结束，请取消或等待完成。"
	default:
		return action + "。请在本机检查 Dev Tunnels CLI 的账号与网络状态，然后手动重试。"
	}
}
