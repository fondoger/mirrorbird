// Package app connects the native settings/tray UI to MirrorBird's local
// services. Construction is inert; only the single-instance UI's onReady
// callback is allowed to read settings, discover an attachment, or run a CLI.
package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/autostart"
	"mirrorbird/internal/desktop"
	"mirrorbird/internal/server"
	"mirrorbird/internal/session"
	"mirrorbird/internal/settings"
	"mirrorbird/internal/tunnel"
	"mirrorbird/web"
)

// Options describes launch behavior. Background never starts a tunnel or signs in.
type Options struct {
	Background bool
}

// Run must run on the main goroutine, just like desktop.Run.
func Run(options Options) error {
	return run(options, desktop.Run, loadResources)
}

type uiRunner func(desktop.ViewState, desktop.Actions, func(func(desktop.ViewState)), func()) error

func run(options Options, ui uiRunner, load func() (*resources, error)) error {
	c := newController(options, load)
	// desktop.Run obtains its single-instance lock before onReady. In particular,
	// do not move loadResources/settings.Open above this boundary.
	err := ui(c.snapshot(), c.actions(), c.ready, c.close)
	c.close() // Also covers a UI failure before onReady; close is idempotent.
	return err
}

type sessionService interface {
	Connect(context.Context) error
	Connected() bool
	EnginePID() int
	AttachmentOutdated() bool
	Close()
	Refresh(context.Context) error
	ClearProjections()
}

type tunnelService interface {
	Account(context.Context) (tunnel.Account, error)
	LoginWithProgress(context.Context, func(tunnel.LoginChallenge)) error
	Logout(context.Context) error
	StartWithProgress(context.Context, tunnel.Options, func(tunnel.StartupProgress)) (tunnel.State, error)
	Stop() error
	State() tunnel.State
	Done() <-chan struct{}
}

type sharingAPI interface {
	EnableSharing(string) error
	SetSharing(bool)
	Sharing() bool
	Handler() http.Handler
}

type configStore interface {
	Save(settings.Config) error
}

type httpService interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

// All process, listener, and persistent-setting boundaries are replaceable in
// unit tests. Tests never have to start the native UI or an installed CLI.
type resources struct {
	home       string
	config     settings.Config // accessed only while controller.serial is held
	store      configStore
	sessions   sessionService
	tunnel     tunnelService
	api        sharingAPI
	listen     func(context.Context, string, string) (net.Listener, error)
	newHTTP    func(http.Handler) httpService
	resolve    func(context.Context, desktop.Installation) (desktop.Installation, error)
	launch     func(desktop.Installation, string, string) error
	restart    func(context.Context, desktop.Installation, string, string) error
	running    func(context.Context) (bool, error)
	executable func() (string, error)
	autoStart  func(bool) (func() error, error)
}

func loadResources() (*resources, error) {
	home, err := attachment.Home()
	if err != nil {
		return nil, fmt.Errorf("无法确定 MirrorBird 设置目录。请检查 MIRRORBIRD_HOME 是否为绝对路径，然后重启。")
	}
	return openResources(home)
}

func openResources(home string) (*resources, error) {
	store, config, err := settings.Open(home)
	if err != nil {
		// A malformed file is never replaced by defaults. Do not include its raw
		// contents or arbitrary parser errors in a status message.
		return nil, fmt.Errorf("无法读取设置 %s。请检查文件格式、版本、端口和绝对程序路径；修复或备份该文件后重启。原设置未被覆盖。", filepath.Join(home, "settings.json"))
	}
	sessions := session.New(home)
	api := server.New(sessions, web.Assets())
	return &resources{
		home: home, config: config, store: store, sessions: sessions,
		tunnel: tunnel.New(config.TunnelExecutable), api: api,
		listen: (&net.ListenConfig{}).Listen, newHTTP: newHTTPServer,
		resolve: desktop.ResolveInstallation, launch: desktop.Launch, executable: os.Executable,
		restart: desktop.Restart, running: desktop.Running,
		autoStart: func(enabled bool) (func() error, error) {
			executable, err := os.Executable()
			if err != nil {
				return nil, err
			}
			return autostart.Apply(executable, enabled)
		},
	}, nil
}

func newHTTPServer(handler http.Handler) httpService {
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 * 1024,
		// SSE sets a deadline for each write. A global WriteTimeout would sever
		// healthy long-lived streams. JSON body bytes are bounded by the API.
		WriteTimeout: 0,
		// net/http diagnostics can contain attacker-supplied request bytes.
		// Surface a fixed local-server failure message through the controller.
		ErrorLog: log.New(io.Discard, "", 0),
	}
}
