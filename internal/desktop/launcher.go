package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/platform"
)

type Installation struct {
	Desktop       string `json:"desktop"`
	Codex         string `json:"codex"`
	Version       string `json:"version"`
	BackendSource string `json:"backendSource,omitempty"`
}

// DetectInstallation resolves the selected package and its associated runtime.
// It does not execute Codex or depend on a backend process being alive.
func DetectInstallation(ctx context.Context) (Installation, error) {
	return ResolveInstallation(ctx, Installation{})
}

func discoverDesktop(ctx context.Context) (Installation, error) {
	if runtime.GOOS != "windows" {
		return Installation{}, errors.New("Desktop integration currently requires Windows")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	script := `$ErrorActionPreference='Stop'; $packages=@(Get-AppxPackage -Name OpenAI.Codex | Sort-Object { [version]$_.Version } -Descending); $pkg=$packages | Select-Object -First 1; if(-not $pkg){throw 'ChatGPT Desktop package was not found'}; $d=Join-Path $pkg.InstallLocation 'app\ChatGPT.exe'; [pscustomobject]@{desktop=$d;version=[string]$pkg.Version}|ConvertTo-Json -Compress`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	platform.HideWindow(cmd)
	data, err := cmd.Output()
	if err != nil {
		return Installation{}, errors.New("cannot discover ChatGPT installation")
	}
	var found Installation
	if err = json.Unmarshal(data, &found); err != nil {
		return found, err
	}
	if found.Desktop == "" {
		return found, ErrDesktopMissing
	}
	return found, nil
}

func Launch(installation Installation, home, shim string) error {
	if err := validateLaunch(installation, shim); err != nil {
		return err
	}
	// An already-running Electron instance will ignore the new launch environment.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	running, err := Running(ctx)
	if err != nil {
		return err
	}
	if running {
		return errors.New("请先完全退出 ChatGPT Desktop，或确认由 MirrorBird 重启接入；现有任务不会被自动终止")
	}
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "CODEX_CLI_PATH") || strings.HasPrefix(strings.ToUpper(name), "MIRRORBIRD_") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "CODEX_CLI_PATH="+shim, attachment.EnvRealCodex+"="+installation.Codex, attachment.EnvHome+"="+home)
	cmd := exec.Command(installation.Desktop)
	cmd.Env = env
	cmd.Dir = filepath.Dir(installation.Desktop)
	if err := cmd.Start(); err != nil {
		return platform.LaunchError("launch Desktop", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func validateLaunch(installation Installation, shim string) error {
	for _, path := range []string{installation.Desktop, installation.Codex, shim} {
		if !filepath.IsAbs(path) {
			return errors.New("select valid absolute Desktop, Codex and shim paths")
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return errors.New("a required executable is missing")
		}
	}
	return nil
}

const ownedDesktopScript = `
$ErrorActionPreference='Stop'
$session=[System.Diagnostics.Process]::GetCurrentProcess().SessionId
$sid=[System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$owned=@(Get-CimInstance Win32_Process -Filter "Name='ChatGPT.exe' AND SessionId=$session" | Where-Object {
  $owner=Invoke-CimMethod -InputObject $_ -MethodName GetOwnerSid
  if($owner.ReturnValue -ne 0){throw 'Cannot verify Desktop process owner'}
  $owner.Sid -eq $sid
})
`

// Running is an observation only; it never launches or closes an application.
func Running(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		ownedDesktopScript+`ConvertTo-Json -InputObject ($owned.Count -gt 0) -Compress`)
	platform.HideWindow(cmd)
	data, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("无法检查当前用户的 ChatGPT Desktop 进程：%w", err)
	}
	var running bool
	if err := json.Unmarshal(data, &running); err != nil {
		return false, errors.New("Desktop 进程检查返回了无效结果")
	}
	return running, nil
}

// Restart is called only after an explicit native confirmation. Request a
// normal window close, respect save/cancel prompts, and never force-kill.
func Restart(ctx context.Context, installation Installation, home, shim string) error {
	if err := validateLaunch(installation, shim); err != nil {
		return err
	}
	script := ownedDesktopScript + `
$path=$env:MIRRORBIRD_RESTART_DESKTOP
if(@($owned | Where-Object {$_.ExecutablePath -ne $path}).Count){throw 'Running Desktop does not match the selected installation'}
$processes=@()
try {
  foreach($row in $owned){
    try {$p=Get-Process -Id $row.ProcessId -ErrorAction Stop} catch [Microsoft.PowerShell.Commands.ProcessCommandException] {continue}
    $null=$p.Handle
    if($p.Path -ne $path -or $p.SessionId -ne $session){$p.Dispose();throw 'Desktop process changed'}
    $processes+=@($p)
  }
  foreach($p in $processes){if(-not $p.HasExited -and $p.MainWindowHandle -ne [IntPtr]::Zero){$null=$p.CloseMainWindow()}}
  $deadline=[DateTime]::UtcNow.AddSeconds(20)
  while(@($processes | Where-Object {-not $_.HasExited}).Count){
    if([DateTime]::UtcNow -ge $deadline){throw 'Desktop did not exit; close it manually and retry'}
    Start-Sleep -Milliseconds 200
  }
} finally {foreach($p in $processes){$p.Dispose()}}
`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "MIRRORBIRD_RESTART_DESKTOP="+installation.Desktop)
	platform.HideWindow(cmd)
	if err := cmd.Run(); err != nil {
		return errors.New("ChatGPT Desktop 未能安全退出；未强制终止，也未启动第二个实例。请保存内容并完全退出 Desktop 后重试")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return Launch(installation, home, shim)
}
