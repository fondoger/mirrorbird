package app

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"mirrorbird/internal/desktop"
	"mirrorbird/internal/diagnostics"
)

const (
	operationTimeout = 45 * time.Second
	startupTimeout   = 2 * time.Minute
	loginTimeout     = 5 * time.Minute
	shutdownTimeout  = 5 * time.Second
)

type operation struct {
	ctx    context.Context
	cancel context.CancelFunc
	epoch  uint64
	began  time.Time
}

type hostRun struct {
	listener net.Listener
	http     httpService
	served   chan struct{}
	watchEnd chan struct{}
	stopOnce sync.Once
}

func (h *hostRun) stopWatching() { h.stopOnce.Do(func() { close(h.watchEnd) }) }

type controller struct {
	// mu guards short in-memory transitions, including the final enable and
	// immediate revoke. No process, RPC, setting write or shutdown runs under it.
	mu sync.Mutex
	// serial orders all blocking lifecycle work. Stop does NOT wait for it to
	// revoke access or cancel a pending operation.
	serial sync.Mutex
	wg     sync.WaitGroup
	root   context.Context
	cancel context.CancelFunc
	done   chan struct{}

	load        func() (*resources, error)
	resources   *resources
	update      func(desktop.ViewState) // must be nonblocking, like Window.Update
	state       desktop.ViewState
	initialized bool
	closing     bool
	stopping    bool
	epoch       uint64
	operation   *operation
	host        *hostRun

	// Only serial's owner accesses tunnelOwned and the resources' config.
	tunnelOwned      bool
	pollEvery        time.Duration
	probingDesktop   bool
	lastDesktopProbe time.Time
}

// Pure construction: no goroutines, files, attachment probes, or processes.
func newController(options Options, load func() (*resources, error)) *controller {
	ctx, cancel := context.WithCancel(context.Background())
	return &controller{
		root: ctx, cancel: cancel, done: make(chan struct{}), load: load,
		pollEvery: time.Second,
		state: desktop.ViewState{
			StartHidden:   options.Background,
			DesktopStatus: "尚未连接 Desktop", TunnelStatus: "共享已关闭",
			Message: "正在准备 MirrorBird；共享默认关闭。",
		},
	}
}

func (c *controller) actions() desktop.Actions {
	return desktop.Actions{
		Refresh: c.refresh, LaunchDesktop: c.launchDesktop, RestartDesktop: c.restartDesktop,
		Login: c.login, CancelLogin: c.cancelLogin, Logout: c.logout,
		StartSharing: c.startSharing, StopSharing: c.stopSharing,
		SetAutoStart: c.setAutoStart,
	}
}

func (c *controller) snapshot() desktop.ViewState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Emitting while holding mu preserves publication order across Stop, worker
// completion, and the timer. The production callback only posts a copied value
// to the UI thread; it never calls back into the controller.
func (c *controller) publishLocked() {
	if c.update != nil && !c.closing {
		c.update(c.state)
	}
}

func (c *controller) ready(update func(desktop.ViewState)) {
	c.mu.Lock()
	if c.initialized || c.closing {
		c.mu.Unlock()
		return
	}
	c.initialized, c.update = true, update
	op := c.beginLocked(false, operationTimeout, "正在读取设置和检查本机连接；共享仍关闭。")
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer op.cancel()
		c.serial.Lock()
		defer c.serial.Unlock()
		c.mu.Lock()
		closing := c.closing
		c.mu.Unlock()
		if closing {
			return
		}
		r, err := c.load()
		c.mu.Lock()
		if c.closing {
			c.mu.Unlock()
			if r != nil {
				r.api.SetSharing(false)
				r.sessions.Close()
			}
			return
		}
		if err != nil {
			c.state.Message = err.Error()
			if c.operation == op {
				c.operation = nil
				c.state.Busy = false
			}
			c.publishLocked()
			c.mu.Unlock()
			return
		}
		c.resources = r
		r.api.SetSharing(false)
		r.sessions.ClearProjections()
		c.state.AutoStart = r.config.AutoStart
		c.sampleLocked()
		c.wg.Add(1)
		go c.poll()
		current := c.currentLocked(op)
		c.publishLocked()
		c.mu.Unlock()
		if current {
			c.finish(op, c.refreshWork(op.ctx, r, op))
		} else {
			c.finish(op, "")
		}
	}()
}

func (c *controller) beginLocked(starting bool, timeout time.Duration, message string) *operation {
	ctx, cancel := context.WithTimeout(c.root, timeout)
	op := &operation{ctx: ctx, cancel: cancel, epoch: c.epoch, began: time.Now()}
	diagnostics.Event("operation.started", diagnostics.Bool("sharing_startup", starting), diagnostics.Int("epoch", int64(c.epoch)))
	c.operation = op
	c.state.Busy, c.state.Starting = true, starting
	if starting {
		c.state.PendingTunnelURL = ""
		c.state.TunnelStatus = "正在验证私有 Dev Tunnel；共享仍关闭"
	}
	c.state.Message = message
	c.publishLocked()
	return op
}

type work func(context.Context, *resources, *operation) string

func (c *controller) submit(starting bool, timeout time.Duration, message string, fn work) {
	c.mu.Lock()
	if c.closing || c.stopping || c.operation != nil || c.resources == nil || (starting && c.state.Sharing) {
		c.mu.Unlock()
		return
	}
	op := c.beginLocked(starting, timeout, message)
	r := c.resources
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer op.cancel()
		c.serial.Lock()
		defer c.serial.Unlock()
		if c.current(op) {
			c.finish(op, fn(op.ctx, r, op))
		} else {
			c.finish(op, "")
		}
	}()
}

func (c *controller) currentLocked(op *operation) bool {
	return !c.closing && !c.stopping && c.operation == op && c.epoch == op.epoch && op.ctx.Err() == nil
}

func (c *controller) current(op *operation) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentLocked(op)
}

func (c *controller) finish(op *operation, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.operation != op || c.epoch != op.epoch {
		return // A canceled/late completion must not replace Stop's snapshot.
	}
	c.operation = nil
	c.state.Busy, c.state.Starting = false, false
	c.state.PendingTunnelURL = ""
	c.clearLoginLocked()
	if !c.state.Sharing {
		c.state.TunnelStatus = "共享已关闭"
	}
	if message == "" && op.ctx.Err() != nil {
		message = "操作已取消或超时；请检查本机状态后再重试。"
	}
	if message != "" {
		c.state.Message = message
	}
	c.sampleLocked()
	diagnostics.Event("operation.finished", diagnostics.Int("duration_ms", time.Since(op.began).Milliseconds()),
		diagnostics.Bool("desktop_connected", c.state.Connected), diagnostics.Bool("sharing", c.state.Sharing))
	c.publishLocked()
}

func (c *controller) sampleLocked() {
	r := c.resources
	if r == nil {
		return
	}
	connected := r.sessions.Connected()
	if c.state.Connected != connected {
		diagnostics.Event("desktop.connection_changed", diagnostics.Bool("connected", connected))
	}
	c.state.Connected = connected
	c.state.AttachmentOutdated = r.sessions.AttachmentOutdated()
	c.state.DesktopStatus = "Desktop 未连接；请手动刷新连接。"
	if c.state.Connected {
		c.state.DesktopRunning = true
		c.state.DesktopStatus = fmt.Sprintf("已连接 Desktop · Codex PID %d", r.sessions.EnginePID())
		if c.state.AttachmentOutdated {
			c.state.DesktopStatus = "Desktop 仍在运行旧版连接组件，需要重启 Desktop 接入新版。"
		}
	} else if c.state.DesktopRunning {
		c.state.DesktopStatus = "ChatGPT Desktop 已运行，但未通过 MirrorBird 的 shim 接入。"
	}
}

// Polling never reconnects, reads history, launches Desktop, or restarts a
// tunnel. A throttled, read-only process probe runs outside the controller lock.
func (c *controller) poll() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-c.root.Done():
			return
		case <-ticker.C:
			c.pollOnce()
		}
	}
}

func (c *controller) pollOnce() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.resources == nil {
		return
	}
	r := c.resources
	if c.state.Sharing && (!r.sessions.Connected() || !r.tunnel.State().Running) {
		diagnostics.Event("sharing.connection_lost", diagnostics.Bool("desktop_connected", r.sessions.Connected()),
			diagnostics.Bool("tunnel_running", r.tunnel.State().Running))
		c.stopLocked("连接已断开，远程服务已停止。请手动刷新连接后重新开启。")
	} else {
		c.sampleLocked()
		if !c.state.Connected && !c.state.Busy && !c.probingDesktop && time.Since(c.lastDesktopProbe) >= 10*time.Second {
			c.probingDesktop = true
			c.lastDesktopProbe = time.Now()
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				running, err := r.running(c.root)
				c.mu.Lock()
				defer c.mu.Unlock()
				c.probingDesktop = false
				if c.closing || c.state.Busy || r.sessions.Connected() {
					return
				}
				if err != nil {
					c.state.Message = "无法检查 Desktop 是否在运行；请点击“刷新连接”重试。"
				} else {
					c.state.DesktopRunning = running
				}
				c.sampleLocked()
				c.publishLocked()
			}()
		}
		c.publishLocked()
	}
}

// Disable admission first. ClearProjections cancels cache lifetimes in memory;
// cleanup must not delay Stop behind a Desktop RPC. Already transmitted commands
// may complete and cannot be undone by stopping remote access.
func (c *controller) interruptLocked(message string) {
	if c.resources != nil {
		c.resources.api.SetSharing(false)
	}
	c.epoch++
	if c.operation != nil {
		c.operation.cancel()
		c.operation = nil
	}
	c.state.Sharing, c.state.Starting = false, false
	c.state.TunnelURL = ""
	c.state.PendingTunnelURL = ""
	c.state.TunnelStatus = "共享已关闭"
	c.clearLoginLocked()
	c.state.Message = message
	if c.resources != nil {
		c.resources.sessions.ClearProjections()
	}
	if c.host != nil {
		c.host.stopWatching()
	}
}

func (c *controller) stopSharing() {
	diagnostics.Event("sharing.stop_requested")
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked("远程服务已停止；已提交的桌面操作可能继续完成。")
}

func (c *controller) stopLocked(message string) {
	if c.closing || c.stopping {
		return
	}
	c.interruptLocked(message)
	c.stopping, c.state.Busy = true, true
	c.publishLocked()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.serial.Lock()
		defer c.serial.Unlock()
		c.mu.Lock()
		r, host := c.resources, c.host
		c.host = nil
		c.mu.Unlock()
		c.cleanup(r, host)
		c.mu.Lock()
		c.stopping = false
		if !c.closing {
			c.state.Busy = false
			c.sampleLocked()
			c.publishLocked()
		}
		c.mu.Unlock()
	}()
}

func (c *controller) hostFailed(host *hostRun, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host == host {
		diagnostics.Event("sharing.host_failed", diagnostics.Bool("http_stopped", channelClosed(host.served)))
		c.stopLocked(message)
	}
}

// cleanup runs under serial. Only listeners and the one host this controller
// acquired are stopped. No process-name termination or session.Close on Stop.
func (c *controller) cleanup(r *resources, host *hostRun) {
	if host != nil {
		host.stopWatching()
	}
	if r != nil && c.tunnelOwned {
		_ = r.tunnel.Stop() // The manager cancels and reaps only its own child.
		c.tunnelOwned = false
	}
	if host != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		if err := host.http.Shutdown(ctx); err != nil {
			_ = host.http.Close()
		}
		cancel()
		_ = host.listener.Close()
	}
}

// close is called outside the native event loop. Concurrent callers share one
// cleanup and wait; calling it for a second instance does not initialize work.
func (c *controller) close() {
	c.mu.Lock()
	if c.closing {
		done := c.done
		c.mu.Unlock()
		<-done
		return
	}
	c.closing = true
	c.interruptLocked("MirrorBird 正在退出。")
	c.cancel()
	c.mu.Unlock()

	c.serial.Lock()
	c.mu.Lock()
	r, host := c.resources, c.host
	c.host = nil
	c.mu.Unlock()
	c.cleanup(r, host)
	if r != nil {
		// This closes the local attachment only, never Desktop's original engine.
		r.sessions.Close()
	}
	c.serial.Unlock()
	c.wg.Wait()
	close(c.done)
}
