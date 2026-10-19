package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"mirrorbird/internal/diagnostics"
	"mirrorbird/internal/server"
	"mirrorbird/internal/tunnel"
)

func (c *controller) startSharing() {
	c.submit(true, startupTimeout, "正在检查私有 Dev Tunnel；尚未允许远程访问，可随时停止。", c.startWork)
}

func (c *controller) startWork(ctx context.Context, r *resources, op *operation) string {
	if !r.sessions.Connected() {
		return "请先连接通过 MirrorBird 启动的 Desktop，然后再开启共享。"
	}
	if r.sessions.AttachmentOutdated() {
		return "Desktop 仍在运行旧版 shim，请先确认重启 Desktop 并接入新版，再开启共享。"
	}
	if !c.current(op) {
		return ""
	}
	if r.tunnel.State().Running {
		return "已有 Dev Tunnel host 正在运行；请先停止它，不会接管或终止不属于此次启动的 host。"
	}
	// No wildcard, port zero, fallback port, or reuse of another process's
	// listener. The tunnel is not touched until this exact local bind succeeds.
	listener, err := r.listen(ctx, "tcp4", server.PortAddress(r.config.Port))
	if err != nil {
		diagnostics.Event("http.bind_failed", diagnostics.ErrorFields(err)...)
		return fmt.Sprintf("无法绑定 127.0.0.1:%d；端口可能已占用。请关闭占用程序或修改 %s 的 port 后重启。未启动 tunnel。", r.config.Port, filepath.Join(r.home, "settings.json"))
	}
	host := &hostRun{listener: listener, http: r.newHTTP(r.api.Handler()), served: make(chan struct{}), watchEnd: make(chan struct{})}
	// Validate even an injected listener before publishing it to the manager.
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) || addr.Port != r.config.Port {
		diagnostics.Event("http.listener_rejected")
		_ = listener.Close()
		return "本机监听地址与配置不一致；已拒绝开启共享。"
	}
	c.mu.Lock()
	if !c.currentLocked(op) {
		c.mu.Unlock()
		_ = listener.Close()
		return ""
	}
	c.host = host
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		err := host.http.Serve(listener)
		fields := append(diagnostics.ErrorFields(err), diagnostics.Bool("expected",
			errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)))
		diagnostics.Event("http.listener_exit", fields...)
		close(host.served)
		c.hostFailed(host, "本机 HTTP 服务已停止，远程访问已关闭。请检查端口后手动重试。")
	}()
	active := false
	defer func() {
		if !active {
			c.mu.Lock()
			if c.host == host {
				c.host = nil
			}
			c.mu.Unlock()
			c.cleanup(r, host)
		}
	}()

	if !c.current(op) {
		return ""
	}
	// Mark ownership before calling Start: failure/cancellation may have acquired
	// a host that must be reaped. The injected manager belongs only to this app.
	c.tunnelOwned = true
	accountChecked := false
	started, err := r.tunnel.StartWithProgress(ctx, tunnel.Options{TunnelID: r.config.TunnelID, Port: r.config.Port},
		func(progress tunnel.StartupProgress) {
			accountChecked = accountChecked || progress.Stage != tunnel.CheckingAccount
			c.sharingProgress(op, progress)
		})
	if err != nil {
		if !accountChecked || errors.Is(err, tunnel.ErrNotLoggedIn) {
			c.setLoggedIn(op, false)
		}
		return tunnelMessage("无法开启私有 Dev Tunnel", err)
	}
	done := r.tunnel.Done()
	state := r.tunnel.State() // A host may exit between successful Start and Done.
	origin, err := checkedOrigin(state, r.config.Port)
	if err != nil || started.TunnelID != state.TunnelID || channelClosed(done) || !c.current(op) {
		return "Dev Tunnel 未保持就绪或启动已取消；共享仍关闭，请手动重试。"
	}
	c.mu.Lock()
	if !c.currentLocked(op) {
		c.mu.Unlock()
		return ""
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		select {
		case <-done:
			c.hostFailed(host, "Dev Tunnel host 已退出，远程访问已关闭。请检查账号与网络后手动开启。")
		case <-host.watchEnd:
		case <-c.root.Done():
		}
	}()

	// Persist only the ID returned by a verified, running host. Save failure is
	// not permission to silently lose the reusable tunnel; unwind and say why.
	if state.TunnelID != r.config.TunnelID {
		config := r.config
		config.TunnelID = state.TunnelID
		if err := r.store.Save(config); err != nil {
			return fmt.Sprintf("已停止共享：无法将已验证的 Tunnel ID 写入 %s。请检查目录权限；已有配置未替换。", filepath.Join(r.home, "settings.json"))
		}
		r.config = config
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// This lock also guards Stop's revoke, so no late startup response can turn
	// sharing back on. Server/service locks never call back into this controller.
	live := r.tunnel.State()
	if !c.currentLocked(op) || c.host != host || channelClosed(done) || channelClosed(host.served) ||
		!live.Running || live.URL != state.URL || live.TunnelID != state.TunnelID || live.Port != r.config.Port ||
		!r.sessions.Connected() || r.sessions.AttachmentOutdated() {
		return "启动期间连接已变化；远程服务仍关闭，请重新检查后开启。"
	}
	if err := r.api.EnableSharing(origin); err != nil {
		return "Dev Tunnel 没有提供有效的 HTTPS 地址；共享仍关闭。"
	}
	c.state.Sharing = true
	c.state.TunnelURL = origin
	c.state.PendingTunnelURL = ""
	c.state.TunnelStatus = "私有 Dev Tunnel 正在共享"
	diagnostics.Event("sharing.enabled")
	active = true
	return "远程服务已开启。通过 Microsoft 验证后，打开网页即可查看和选择全部会话，无需配对。"
}

func (c *controller) sharingProgress(op *operation, progress tunnel.StartupProgress) {
	status := ""
	switch progress.Stage {
	case tunnel.CheckingAccount:
		status = "正在检查 Microsoft 登录"
	case tunnel.CreatingTunnel:
		status = "正在创建私有隧道"
	case tunnel.CheckingTunnel:
		status = "正在检查隧道配置"
	case tunnel.StartingHost:
		status = "正在连接转发服务"
	case tunnel.CheckingAddress, tunnel.CheckingAccess:
		status = "隧道已建立，正在验证连接"
	default:
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentLocked(op) || !c.state.Starting {
		return
	}
	if progress.Stage != tunnel.CheckingAccount {
		c.state.LoggedIn = true
	}
	if progress.Stage == tunnel.CheckingAccess && progress.URL != "" {
		if origin, err := checkedHTTPSOrigin(progress.URL); err == nil {
			c.state.PendingTunnelURL = origin
		}
	}
	c.state.TunnelStatus = status
	c.state.Message = status + "；尚未开放远程访问，可随时取消。"
	c.publishLocked()
}

func checkedOrigin(state tunnel.State, port int) (string, error) {
	if !state.Running || state.TunnelID == "" || state.Port != port {
		return "", fmt.Errorf("tunnel is not ready")
	}
	return checkedHTTPSOrigin(state.URL)
}

func checkedHTTPSOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.ContainsAny(raw, "\\\r\n\t ") || strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("invalid forwarding origin")
	}
	if raw := u.Port(); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			return "", fmt.Errorf("invalid forwarding port")
		}
	}
	return "https://" + u.Host, nil
}

func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return true // An unobservable host is not a verified running lifecycle.
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
