package desktop

import (
	"context"
	"encoding/json"
	"errors"
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
	for _, path := range []string{installation.Desktop, installation.Codex, shim} {
		if !filepath.IsAbs(path) {
			return errors.New("select valid absolute Desktop, Codex and shim paths")
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return errors.New("a required executable is missing")
		}
	}
	// An already-running Electron instance will ignore the new launch environment.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	check := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `if(Get-Process -Name ChatGPT -ErrorAction SilentlyContinue){exit 3}`)
	platform.HideWindow(check)
	if err := check.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			return errors.New("请先完全退出 ChatGPT，再通过 MirrorBird 打开；现有任务不会被强制终止")
		}
		return errors.New("cannot verify whether Desktop is running")
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
