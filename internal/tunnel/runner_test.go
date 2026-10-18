package tunnel

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExecutableValidationNeverAcceptsCommandsOrScripts(t *testing.T) {
	valid := []string{"", "devtunnel"}
	if runtime.GOOS == "windows" {
		valid = append(valid, "devtunnel.exe", "DEVTUNNEL.EXE", `C:\Program Files\DevTunnel\devtunnel.exe`, "C:/trusted/devtunnel.exe")
	} else {
		valid = append(valid, "/usr/local/bin/devtunnel")
	}
	for _, input := range valid {
		if err := validateExecutable(input); err != nil {
			t.Errorf("valid executable %q: %v", input, err)
		}
	}
	for _, input := range []string{
		"devtunnel host --allow-anonymous", "devtunnel.cmd", "./devtunnel", "../devtunnel", " devtunnel", "devtunnel\n",
		"powershell.exe", "cmd.exe", "C:/trusted/devtunnel.cmd", "C:/trusted/other.exe", "C:/trusted/devtunnel.exe\x00",
		`\\server\share\devtunnel.exe`, "//server/share/devtunnel.exe", `C:\trusted\devtunnel.exe"`,
	} {
		if err := validateExecutable(input); !errors.Is(err, ErrExecutable) {
			t.Errorf("unsafe executable %q accepted", input)
		}
	}
}

func TestResolveRejectsMissingOrNonRegularExecutable(t *testing.T) {
	name := "devtunnel"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if _, err := (execRunner{}).Resolve(path); !errors.Is(err, ErrExecutable) {
		t.Fatalf("missing file err=%v", err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := (execRunner{}).Resolve(path); !errors.Is(err, ErrExecutable) {
		t.Fatalf("directory err=%v", err)
	}
}

func TestRestrictedChildEnvironment(t *testing.T) {
	parent := []string{
		"PATH=C:/Windows", "SystemRoot=C:/Windows", "USERPROFILE=C:/Users/test", "APPDATA=C:/UserData", "TEMP=C:/Temp",
		"https_proxy=http://proxy", "NO_PROXY=localhost", "LANG=en_US.UTF-8",
		"DEV_TUNNELS_ACCESS_TOKEN=secret", "TUNNEL_SERVICE_URL=https://evil", "GITHUB_TOKEN=secret", "AZURE_CLIENT_SECRET=secret",
		"DOTNET_STARTUP_HOOKS=evil.dll", "DOTNET_ADDITIONAL_DEPS=evil", "DOTNET_ROOT=evil", "LD_PRELOAD=evil.so", "NODE_OPTIONS=evil.js",
		"PYTHONPATH=evil", "MY_APP_SECRET=secret", "NO_COLOR=0", "DOTNET_CLI_UI_LANGUAGE=xx",
	}
	actual := childEnvironment(parent)
	joined := strings.Join(actual, "\n")
	for _, allowed := range parent[:8] {
		if !strings.Contains(joined, allowed) {
			t.Errorf("missing OS/browser/proxy setting %q", allowed)
		}
	}
	for _, banned := range []string{"secret", "evil", "NO_COLOR=0", "DOTNET_CLI_UI_LANGUAGE=xx"} {
		if strings.Contains(joined, banned) {
			t.Errorf("inherited unsafe environment: %q", banned)
		}
	}
	if !strings.Contains(joined, "NO_COLOR=1") || !strings.Contains(joined, "DOTNET_CLI_UI_LANGUAGE=en-US") {
		t.Fatal("readiness output is not pinned to plain English")
	}
	cmd := newCommand(context.Background(), "devtunnel", "--help")
	if cmd.Stdin != nil || cmd.WaitDelay == 0 || cmd.Stderr == nil {
		t.Fatal("command must not inherit terminal input/output or wait forever")
	}
}

func TestLimitedOutputDrainsWithoutRetainingExcess(t *testing.T) {
	var output limitedOutput
	first := bytes.Repeat([]byte("x"), maxOutput-2)
	if n, err := output.Write(first); n != len(first) || err != nil {
		t.Fatal("short write")
	}
	if n, err := output.Write([]byte("abcdefgh")); n != 8 || err != nil {
		t.Fatal("must drain overflowing output")
	}
	if output.Len() != maxOutput || !output.overflow || strings.Contains(output.String(), "cdefgh") {
		t.Fatal("output exceeded safety bound")
	}
	if n, err := output.Write(bytes.Repeat([]byte("secret"), maxOutput)); n != 6*maxOutput || err != nil || output.Len() != maxOutput {
		t.Fatal("overflow was not drained")
	}
}

func TestCommandErrorsNeverIncludeOutputOrRawError(t *testing.T) {
	for _, input := range []error{errors.New("secret credential output"), errOutputLimit} {
		err := commandError("inspect tunnel", context.Background(), input)
		if !errors.Is(err, ErrCommand) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "credential") {
			t.Fatalf("unsafe error=%v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := commandError("host", ctx, errors.New("secret")); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("cancel error=%v", err)
	}
}
