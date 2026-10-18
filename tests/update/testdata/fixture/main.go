// A disposable app/helper used only by tests/update. It never imports MirrorBird
// production code, creates a visible window, or starts Desktop or a model.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/egoist/mygo"
)

func main() {
	control := flag.String("control", "", "test controller's loopback address")
	state := flag.String("state", "", "disposable user data directory")
	role := flag.String("role", "app", "app or helper")
	native := flag.Bool("native", false, "run the real MyGo event loop")
	flag.Parse()
	// Also bounds a relaunched process that the controller fails to accept.
	time.AfterFunc(90*time.Second, func() { os.Exit(2) })
	conn, err := net.DialTimeout("tcp", *control, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()
	for _, name := range []mygo.PathName{mygo.PathUserData, mygo.PathCache, mygo.PathLogs} {
		mygo.App.SetPath(name, *state)
	}
	mygo.App.SetMenu(nil)

	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	report := func(fields map[string]any) bool {
		fields["pid"], fields["version"] = os.Getpid(), mygo.App.Version()
		fields["role"], fields["ready"] = *role, mygo.App.IsReady()
		fields["exe"], _ = os.Executable()
		fields["cwd"], _ = os.Getwd()
		fields["enabled"], fields["dev"] = mygo.Updater.Enabled(), mygo.IsDev()
		return enc.Encode(fields) == nil
	}
	serve := func() {
		if !report(map[string]any{"event": "ready"}) {
			return
		}
		for {
			var command string
			if dec.Decode(&command) != nil {
				return
			}
			fields := map[string]any{"event": command}
			switch command {
			case "ping":
			case "check", "install":
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				up, err := mygo.Updater.Check(ctx)
				fields["checked"] = err == nil
				fields["upToDate"] = err == nil && up == nil
				if err == nil && up != nil {
					fields["update"] = up.Version
					if command == "install" {
						err = up.Install(ctx, func(n, total int64) {
							fields["downloaded"], fields["total"] = n, total
						})
						fields["installed"] = err == nil
					}
				}
				cancel()
				if err != nil {
					fields["error"] = err.Error()
				}
			case "relaunch":
				if !*native {
					fields["error"] = "relaunch needs the native event loop"
					break
				}
				if report(fields) {
					mygo.App.Relaunch()
				}
				return
			default:
				fields["error"] = "unknown command"
			}
			if !report(fields) {
				return
			}
		}
	}
	if !*native {
		serve()
		return
	}
	// No Window/NewWindow, tray, webview, or updater plugin is needed. MyGo's
	// Windows backend uses just its hidden application HWND and native loop.
	mygo.App.WhenReady(func() {
		go func() {
			serve()
			// Relaunch is posted before this Quit; its quit sequence wins.
			mygo.App.Quit()
		}()
	})
	if err := mygo.App.Run(); err != nil {
		report(map[string]any{"event": "init-error", "error": err.Error()})
		fmt.Fprintln(os.Stderr, filepath.Base(os.Args[0]), err)
		os.Exit(1)
	}
}
