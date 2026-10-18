package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func stagedFixture(t *testing.T) (Installation, string, string) {
	t.Helper()
	root := t.TempDir()
	desktop := filepath.Join(root, "WindowsApps", "OpenAI.Codex_test", "app", "ChatGPT.exe")
	writeFixture(t, desktop, []byte("not executable; metadata test only"))
	resources := filepath.Join(filepath.Dir(desktop), "resources")
	group := make([]runtimeFile, 0, len(runtimeFiles))
	for i, name := range runtimeFiles {
		content := []byte{byte(i + 1), 0, 't', 'e', 's', 't'}
		writeFixture(t, filepath.Join(resources, name), content)
		group = append(group, runtimeFile{name: name, size: int64(len(content)), hash: sha256.Sum256(content)})
	}
	local := filepath.Join(root, "Local")
	stage := filepath.Join(local, "OpenAI", "Codex", "bin", runtimeKey(group))
	for _, name := range runtimeFiles {
		data, err := os.ReadFile(filepath.Join(resources, name))
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(stage, name), data)
	}
	return Installation{Desktop: desktop, Version: "package-test"}, local, stage
}

func TestResolveClosedDesktopUsesMatchingStageWithoutExecution(t *testing.T) {
	selected, local, stage := stagedFixture(t)
	calls := 0
	got, err := resolveInstallation(context.Background(), Installation{}, local, func(context.Context) (Installation, error) { calls++; return selected, nil })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.Desktop != selected.Desktop || got.Codex != filepath.Join(stage, "codex.exe") || got.BackendSource != "desktop-staged" || got.Version != "package-test" {
		t.Fatalf("wrong installation: %+v, calls=%d", got, calls)
	}
	// Fixture .exe contents are not executable. Resolution can only succeed if it
	// performs no --version/process probe and needs no currently running Desktop.
}

func TestResolveRejectsMissingAlteredOrPartialStage(t *testing.T) {
	for _, mode := range []string{"missing", "changed", "helper-missing", "wrong-root"} {
		t.Run(mode, func(t *testing.T) {
			selected, local, stage := stagedFixture(t)
			switch mode {
			case "missing":
				if err := os.RemoveAll(stage); err != nil {
					t.Fatal(err)
				}
			case "changed":
				writeFixture(t, filepath.Join(stage, "codex.exe"), []byte{9, 0, 't', 'e', 's', 't'})
			case "helper-missing":
				if err := os.Remove(filepath.Join(stage, "codex-command-runner.exe")); err != nil {
					t.Fatal(err)
				}
			case "wrong-root":
				local = filepath.Join(local, "other")
			}
			_, err := resolveInstallation(context.Background(), selected, local, func(context.Context) (Installation, error) {
				t.Fatal("unexpected global detection")
				return Installation{}, nil
			})
			if !errors.Is(err, ErrStagedBackend) {
				t.Fatal(err)
			}
			// No fresh copy/staging creation and no fallback to WindowsApps or PATH.
			if mode == "missing" {
				if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("resolver recreated missing stage")
				}
			}
		})
	}
}

func TestConfiguredPathsRemainAuthoritative(t *testing.T) {
	base := t.TempDir()
	custom := Installation{Desktop: filepath.Join(base, "custom", "ChatGPT.exe"), Codex: filepath.Join(base, "backend", "custom.exe")}
	writeFixture(t, custom.Desktop, []byte("custom desktop"))
	writeFixture(t, custom.Codex, []byte("custom backend"))
	for _, explicitDesktop := range []bool{true, false} {
		selected := custom
		if !explicitDesktop {
			selected.Desktop = ""
		}
		calls := 0
		got, err := resolveInstallation(context.Background(), selected, "", func(context.Context) (Installation, error) {
			calls++
			return Installation{Desktop: custom.Desktop, Version: "custom-version"}, nil
		})
		if err != nil || got.Desktop != custom.Desktop || got.Codex != custom.Codex || got.BackendSource != "configured" {
			t.Fatal(got, err)
		}
		if explicitDesktop && calls != 0 {
			t.Fatal("explicit Desktop replaced")
		}
	}
	custom.Codex = filepath.Join(base, "missing.exe")
	if _, err := resolveInstallation(context.Background(), custom, "", func(context.Context) (Installation, error) {
		t.Fatal("fallback after invalid explicit path")
		return Installation{}, nil
	}); !errors.Is(err, ErrBackendMissing) {
		t.Fatal(err)
	}
}

func TestCustomDesktopResolvesOnlyItsOwnResources(t *testing.T) {
	base := t.TempDir()
	selected := Installation{Desktop: filepath.Join(base, "custom", "ChatGPT.exe")}
	writeFixture(t, selected.Desktop, []byte("desktop"))
	backend := filepath.Join(base, "custom", "resources", "codex.exe")
	writeFixture(t, backend, []byte("backend"))
	got, err := resolveInstallation(context.Background(), selected, "", func(context.Context) (Installation, error) {
		t.Fatal("custom Desktop must not use global package backend")
		return Installation{}, nil
	})
	if err != nil || got.Codex != backend || got.BackendSource != "bundled" {
		t.Fatal(got, err)
	}
}

func TestRuntimeKeyMatchesKnownOfficialGroup(t *testing.T) {
	hashes := []string{
		"1c534e8f1bdd37267c292562cb5b3c9ca7401415fcfdbc965988c5d200d20407",
		"6db35a0d164e6c3e38d15c8d6c779e414d145ca2b7d0d4b29c5c091ad34e4a90",
		"feb24d262ff7967353583974d58effea50dfea8d14cf42faa5e025616b5848d6",
		"db247e6ee26b86ddd236efc092a29d2a65ae2067f66c6b51c5d7d589811955d5",
	}
	var group []runtimeFile
	for i, name := range runtimeFiles {
		data, err := hex.DecodeString(hashes[i])
		if err != nil {
			t.Fatal(err)
		}
		entry := runtimeFile{name: name}
		copy(entry.hash[:], data)
		group = append(group, entry)
	}
	if got := runtimeKey(group); got != "12d47d08c108b3de" {
		t.Fatal(got)
	}
	group[0], group[1] = group[1], group[0]
	if runtimeKey(group) == "12d47d08c108b3de" {
		t.Fatal("group order ignored")
	}
}

func TestResolverCancellationAndPathValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveInstallation(ctx, Installation{}, "", func(context.Context) (Installation, error) {
		t.Fatal("discovery after cancellation")
		return Installation{}, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, path := range []string{"codex.exe", "", filepath.Join(t.TempDir(), "bad.cmd")} {
		if regularExecutable(path) == nil {
			t.Fatalf("invalid path accepted %q", path)
		}
	}
	for _, test := range []struct {
		path string
		want bool
	}{
		{`C:\Program Files\WindowsApps\pkg\app\resources\codex.exe`, true},
		{`C:\Program Files\windowsapps\pkg\app\resources\codex.exe`, true},
		{`C:\tmp\WindowsApps-copy\codex.exe`, false},
	} {
		if insideWindowsApps(test.path) != test.want {
			t.Fatal(test.path)
		}
	}
}

func TestContainedRuntimeRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.exe")
	writeFixture(t, outside, []byte("outside"))
	link := filepath.Join(root, "codex.exe")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink creation is unavailable for this test user")
	}
	if containedRegularFile(root, link) {
		t.Fatal("external symlink accepted")
	}
}
