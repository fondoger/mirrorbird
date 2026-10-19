package desktop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"mirrorbird/internal/platform"
)

// Invisible, non-activating fixture windows model normal close and close-to-tray.
// These tests never enumerate or close real ChatGPT processes.
const restartFixtureScript = `
$ErrorActionPreference='Stop'
if($env:MIRRORBIRD_TEST_MODE -eq 'headless'){
  [Console]::WriteLine('ready')
  [System.Threading.Thread]::Sleep(-1)
  exit
}
Add-Type -AssemblyName System.Windows.Forms
Add-Type -ReferencedAssemblies System.Windows.Forms,System.Drawing -TypeDefinition @'
public sealed class RestartFixtureForm : System.Windows.Forms.Form {
  public void Ready() { System.Console.WriteLine("ready"); }
  protected override bool ShowWithoutActivation { get { return true; } }
  protected override System.Windows.Forms.CreateParams CreateParams {
    get { var p=base.CreateParams; p.ExStyle |= 0x08000080; p.ExStyle &= ~0x00040000; return p; }
  }
}
'@
$form=New-Object RestartFixtureForm
$form.Text='MirrorBird restart test'
# Keep an unowned main window; WS_EX_TOOLWINDOW prevents a taskbar button.
$form.ShowInTaskbar=$true
$form.StartPosition='Manual'
$form.Location=New-Object System.Drawing.Point(-32000,-32000)
$form.Size=New-Object System.Drawing.Size(1,1)
$form.Opacity=0
$form.add_Shown({$form.Ready()})
$form.add_FormClosing({
  if($env:MIRRORBIRD_TEST_MODE -eq 'tray'){
    $_.Cancel=$true
    $form.Hide()
    [Console]::WriteLine('hidden')
  }
})
[System.Windows.Forms.Application]::Run($form)
`

const fixtureOwnedScript = `
$ErrorActionPreference='Stop'
$session=[System.Diagnostics.Process]::GetCurrentProcess().SessionId
$sid=[System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$owned=@(Get-CimInstance Win32_Process -Filter "ProcessId=$env:MIRRORBIRD_TEST_PID")
`

type restartFixture struct {
	cmd    *exec.Cmd
	lines  chan string
	exited chan struct{}
	err    error
}

func newRestartFixture(t *testing.T, mode string) *restartFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", restartFixtureScript)
	cmd.Env = append(os.Environ(), "MIRRORBIRD_TEST_MODE="+mode)
	platform.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	f := &restartFixture{cmd: cmd, lines: make(chan string, 16), exited: make(chan struct{})}
	go func() {
		defer close(f.exited)
		defer close(f.lines)
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			f.lines <- scanner.Text()
		}
		f.err = cmd.Wait()
		if f.err == nil {
			f.err = scanner.Err()
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.exited:
		case <-time.After(10 * time.Second):
			t.Error("synthetic process failed to exit")
		}
	})
	select {
	case line := <-f.lines:
		if line != "ready" {
			cancel()
			<-f.exited
			t.Fatalf("fixture not ready: %q %s", line, &stderr)
		}
	case <-time.After(20 * time.Second):
		cancel()
		<-f.exited
		t.Fatalf("fixture startup timed out: %s", &stderr)
	}
	return f
}

func runFixtureShutdown(t *testing.T, f *restartFixture, path, extra string) (desktopExitResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Scope discovery to this exact child PID, never the production name filter.
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		fixtureOwnedScript+extra+desktopShutdownScript)
	cmd.Env = append(os.Environ(), fmt.Sprintf("MIRRORBIRD_TEST_PID=%d", f.cmd.Process.Pid), "MIRRORBIRD_RESTART_DESKTOP="+path)
	platform.HideWindow(cmd)
	data, err := cmd.Output()
	if err != nil {
		return desktopExitResult{}, err
	}
	var result desktopExitResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("invalid exit result: %q: %v", data, err)
	}
	return result, nil
}

func TestDesktopShutdownClosesTrayAndHeadlessProcesses(t *testing.T) {
	unrelated := newRestartFixture(t, "headless")
	for _, mode := range []string{"close", "tray", "headless"} {
		t.Run(mode, func(t *testing.T) {
			f := newRestartFixture(t, mode)
			result, err := runFixtureShutdown(t, f, f.cmd.Path, "")
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					t.Fatalf("fixture shutdown: %v: %s", err, exit.Stderr)
				}
				t.Fatal(err)
			}
			select {
			case <-f.exited:
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown reported success while target remained alive")
			}
			forced := 1
			if mode == "close" {
				forced = 0
				if f.err != nil {
					t.Fatalf("normal close was not graceful: %v", f.err)
				}
			}
			if result.Processes != 1 || result.Forced != forced {
				t.Fatalf("wrong exit mode: %+v", result)
			}
			if mode == "tray" {
				hidden := false
				for line := range f.lines {
					hidden = hidden || line == "hidden"
				}
				if !hidden {
					t.Fatal("fixture did not reproduce close-to-tray")
				}
			}
			select {
			case <-unrelated.exited:
				t.Fatal("shutdown killed an unselected process with the same executable")
			default:
			}
		})
	}
}

func TestDesktopShutdownRejectsUnverifiedIdentity(t *testing.T) {
	f := newRestartFixture(t, "headless")
	for _, tc := range []struct{ path, extra string }{
		{f.cmd.Path + ".different", ""},
		{f.cmd.Path, "\n$sid='not-the-owner'\n"},
		{f.cmd.Path, "\n$session=-1\n"},
	} {
		if _, err := runFixtureShutdown(t, f, tc.path, tc.extra); err == nil {
			t.Fatal("unverified process accepted")
		}
		select {
		case <-f.exited:
			t.Fatal("identity rejection terminated the process")
		default:
		}
	}
	if !strings.Contains(ownedDesktopScript, "Name='ChatGPT.exe' AND SessionId=$session") ||
		!strings.Contains(ownedDesktopScript, "$owner.Sid -eq $sid") {
		t.Fatal("production process discovery lost its user/session/name scope")
	}
}
