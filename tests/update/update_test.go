package update_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var files = []string{"mirrorbird.exe", "mirrorbird-codex.exe", "mirrorbird-diagnose.exe"}

// TestMyGoUpdate verifies signed installation, live helpers, and native relaunch using disposable executables and a loopback feed.
func TestMyGoUpdate(t *testing.T) {
	if runtime.GOOS != "windows" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		t.Skip("native Windows arm64/amd64 executable replacement test")
	}
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, wrongKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var archive []byte
	var badSignature atomic.Bool
	var checks, downloads atomic.Int32
	// The archive/signatures are set before any child is started; subtests
	// are sequential. Only the switch and request counters change in flight.
	var signature, wrongSignature string
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/update.json", func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		sig := signature
		if badSignature.Load() {
			sig = wrongSignature
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": "1.1.0", "url": server.URL + "/app.tar.gz",
			"size": len(archive), "signature": sig,
		})
	})
	mux.HandleFunc("/app.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	})
	builds := t.TempDir()
	old := buildFixture(t, filepath.Join(builds, "old.exe"), "1.0.0", server.URL, pub)
	new := buildFixture(t, filepath.Join(builds, "new.exe"), "1.1.0", server.URL, pub)
	if bytes.Equal(old, new) {
		t.Fatal("version injection produced identical executables")
	}
	archive = makeArchive(t, new)
	digest := sha256.Sum256(archive)
	// MyGo signs the SHA-256 digest of the .tar.gz, not the JSON or raw tar.
	signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, digest[:]))
	wrongSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(wrongKey, digest[:]))

	for _, tc := range []struct {
		name   string
		bad    bool
		native bool
	}{
		{name: "reject_wrong_signing_key", bad: true},
		{name: "install_three_files_with_live_helper"},
		{name: "native_relaunch_with_live_helper", native: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badSignature.Store(tc.bad)
			beforeChecks, beforeDownloads := checks.Load(), downloads.Load()
			h := newHarness(t, old)
			helper := h.start(files[1], "helper", false)
			app := h.start(files[0], "app", tc.native)
			if app.info.Event == "init-error" && tc.native {
				// Install is separately covered without a native event loop.
				t.Skipf("native initialization unavailable; GUI Relaunch still needs validation: %s", app.info.Error)
			}
			assertVersion(t, helper.info, "1.0.0", false)
			assertVersion(t, app.info, "1.0.0", tc.native)
			initial := app.request(t, "check")
			if !initial.Checked || initial.Update != "1.1.0" || initial.Error != "" {
				t.Fatalf("public Check: %+v", initial)
			}
			result := app.request(t, "install")
			if !result.Checked || result.Update != "1.1.0" || result.Downloaded != int64(len(archive)) || result.Total != int64(len(archive)) {
				t.Fatalf("Check/Install or download progress: %+v", result)
			}
			assertVersion(t, result, "1.0.0", tc.native) // running old app is unaffected
			if checks.Load()-beforeChecks != 2 || downloads.Load()-beforeDownloads != 1 {
				t.Fatalf("want two feed requests and one archive download, got %d/%d", checks.Load()-beforeChecks, downloads.Load()-beforeDownloads)
			}
			if tc.bad {
				if result.Installed || !strings.Contains(result.Error, "not signed with the app's key") {
					t.Fatalf("wrong-key signature was not rejected: %+v", result)
				}
				h.assertFiles(old)
				assertLive(t, helper, "1.0.0")
				t.Logf("real Check succeeded; Install rejected wrong Ed25519 key; all three SHA-256 hashes unchanged; helper PID %d still answers", helper.info.PID)
				return
			}
			if !result.Installed || result.Error != "" {
				t.Fatalf("public Install: %+v", result)
			}
			h.assertFiles(new)
			assertLive(t, helper, "1.0.0")
			// Both replacement helpers must actually execute as the new version,
			// while the old shim-shaped helper keeps its original PID/version.
			for _, file := range files[1:] {
				p := h.start(file, "helper", false)
				assertVersion(t, p.info, "1.1.0", false)
				p.close()
			}
			t.Logf("all three installed SHA-256 hashes match v1.1.0; both new helpers run; old helper PID %d still serves v1.0.0", helper.info.PID)
			if !tc.native {
				return
			}
			ack := app.request(t, "relaunch")
			if ack.Error != "" {
				t.Fatal(ack.Error)
			}
			restarted := h.accept(nil)
			assertVersion(t, restarted.info, "1.1.0", true)
			if restarted.info.PID == app.info.PID || restarted.info.Exe != app.info.Exe || restarted.info.CWD != app.info.CWD || restarted.info.Role != "app" {
				t.Fatalf("Relaunch did not preserve executable/arguments/cwd or change PID: before=%+v after=%+v", app.info, restarted.info)
			}
			app.wait(t)
			assertLive(t, helper, "1.0.0")
			current := restarted.request(t, "check")
			if !current.Checked || !current.UpToDate || current.Error != "" || current.Update != "" {
				t.Fatalf("restarted app should be up to date: %+v", current)
			}
			t.Logf("official App.Run/Relaunch: PID %d -> %d, v1.1.0 ready and up to date; old helper PID %d still responds", app.info.PID, restarted.info.PID, helper.info.PID)
		})
	}
}

func buildFixture(t *testing.T, output, version, feed string, pub ed25519.PublicKey) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// mygo.json's CLI validator requires HTTPS. Its runtime explicitly permits
	// loopback HTTP; inject exactly the build metadata used by mygo build,
	// without changing the product's feed/key or modifying the module cache.
	flags := "-H=windowsgui -X github.com/egoist/mygo.production=1" +
		" -X github.com/egoist/mygo.packageIdentifier=dev.mirrorbird.update-fixture" +
		" -X github.com/egoist/mygo.packageName=MirrorBirdUpdateFixture" +
		" -X github.com/egoist/mygo.packageVersion=" + version +
		" -X github.com/egoist/mygo.packageUpdateFeed=" + feed + "/update.json" +
		" -X github.com/egoist/mygo.packageUpdateKey=" + base64.StdEncoding.EncodeToString(pub)
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go.exe"),
		"build", "-mod=readonly", "-ldflags", flags, "-o", output, "./testdata/fixture")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off", "GOFLAGS=", "CGO_ENABLED=0", "GOOS=windows", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture %s: %v\n%s", version, err, out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func makeArchive(t *testing.T, exe []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gz)
	// Windows MyGo archives contain the app directory's entries at the root.
	for _, name := range files {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(exe))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(exe); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type reply struct {
	Event, Version, Role, Exe, CWD, Update, Error string
	PID                                           int
	Ready, Enabled, Dev                           bool
	Checked, UpToDate, Installed                  bool
	Downloaded, Total                             int64
}

type peer struct {
	conn net.Conn
	info reply
	done <-chan error
}

func (p *peer) request(t *testing.T, command string) reply {
	t.Helper()
	_ = p.conn.SetDeadline(time.Now().Add(40 * time.Second))
	if err := json.NewEncoder(p.conn).Encode(command); err != nil {
		t.Fatal(err)
	}
	var r reply
	if err := json.NewDecoder(p.conn).Decode(&r); err != nil {
		t.Fatalf("%s PID %d: %v", command, p.info.PID, err)
	}
	if r.Event != command || r.PID != p.info.PID {
		t.Fatalf("unexpected reply to %s: %+v", command, r)
	}
	return r
}

func (p *peer) close() { _ = p.conn.Close() }

func (p *peer) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("fixture PID %d exited: %v", p.info.PID, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("fixture PID %d did not exit", p.info.PID)
	}
}

func assertVersion(t *testing.T, r reply, version string, ready bool) {
	t.Helper()
	if r.Version != version || r.Dev || !r.Enabled || r.Ready != ready {
		t.Fatalf("want production/update-enabled v%s, native-ready=%t: %+v", version, ready, r)
	}
}

func assertLive(t *testing.T, p *peer, version string) {
	t.Helper()
	r := p.request(t, "ping") // a fresh roundtrip, not a stale ready/heartbeat file
	assertVersion(t, r, version, false)
	if r.Error != "" {
		t.Fatal(r.Error)
	}
}

type harness struct {
	t          *testing.T
	app, state string
	listener   *net.TCPListener
}

func newHarness(t *testing.T, exe []byte) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{t: t, app: filepath.Join(root, "installed"), state: filepath.Join(root, "state")}
	for _, dir := range []string{h.app, h.state} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(h.app, name), exe, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	h.listener, err = net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.listener.Close() })
	return h
}

func (h *harness) start(file, role string, native bool) *peer {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(h.app, file), "-control", h.listener.Addr().String(), "-state", h.state, "-role", role, fmt.Sprintf("-native=%t", native))
	cmd.Dir = h.state // deliberately different from the executable directory
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "MYGO_") || key == "APPIMAGE" {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	for _, key := range []string{"APPDATA", "LOCALAPPDATA", "USERPROFILE", "HOME", "TMP", "TEMP"} {
		cmd.Env = append(cmd.Env, key+"="+h.state)
	}
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	done := track(h.t, cmd.Process, cmd.Wait)
	p := h.accept(done)
	if p.info.PID != cmd.Process.Pid {
		h.t.Fatalf("unexpected process connected: %+v", p.info)
	}
	return p
}

func (h *harness) accept(done <-chan error) *peer {
	h.t.Helper()
	_ = h.listener.SetDeadline(time.Now().Add(15 * time.Second))
	conn, err := h.listener.Accept()
	if err != nil {
		h.t.Fatal(err)
	}
	p := &peer{conn: conn, done: done}
	h.t.Cleanup(p.close)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := json.NewDecoder(conn).Decode(&p.info); err != nil {
		h.t.Fatalf("fixture handshake: %v", err)
	}
	if p.info.Event != "ready" && p.info.Event != "init-error" {
		h.t.Fatalf("unexpected handshake: %+v", p.info)
	}
	if done == nil { // App.Relaunch, NOT exec.Command, started this process.
		proc, err := os.FindProcess(p.info.PID)
		if err != nil {
			h.t.Fatal(err)
		}
		p.done = track(h.t, proc, func() error {
			state, err := proc.Wait()
			if err == nil && !state.Success() {
				err = fmt.Errorf("restarted fixture: %s", state)
			}
			return err
		})
		// Close before the process tracker waits (Cleanup runs LIFO).
		h.t.Cleanup(p.close)
	}
	return p
}

func track(t *testing.T, proc *os.Process, wait func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = proc.Kill() // only this test's exact process handle, never a name
			<-done
		}
	})
	return done
}

func (h *harness) assertFiles(want []byte) {
	h.t.Helper()
	expected := sha256.Sum256(want)
	for _, name := range files {
		f, err := os.Open(filepath.Join(h.app, name))
		if err != nil {
			h.t.Fatal(err)
		}
		hash := sha256.New()
		_, err = io.Copy(hash, f)
		_ = f.Close() // don't hold Windows rename/delete locks during Install
		if err != nil || !bytes.Equal(hash.Sum(nil), expected[:]) {
			h.t.Fatalf("%s hash mismatch: got %x, want %x (%v)", name, hash.Sum(nil), expected, err)
		}
	}
}
