package shim

import (
	"reflect"
	"testing"
)

func TestIsStdioServer(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"app-server"}, true},
		{[]string{"-c", "features.code_mode_host=true", "app-server", "--analytics-default-enabled"}, true},
		{[]string{"app-server", "--listen", "stdio://", "-c", "x=true"}, true},
		{[]string{"--version"}, false},
		{[]string{"app-server", "--help"}, false},
		{[]string{"app-server", "daemon", "version"}, false},
		{[]string{"app-server", "proxy"}, false},
		{[]string{"app-server", "--listen", "ws://127.0.0.1:1234"}, false},
		{[]string{"app-server", "--listen"}, false},
		{[]string{"exec", "app-server"}, false},
		{[]string{"-c"}, false},
	} {
		if got := IsStdioServer(test.args); got != test.want {
			t.Errorf("IsStdioServer(%q) = %v; want %v", test.args, got, test.want)
		}
	}
}

func TestBackendEnvironmentDoesNotRecurse(t *testing.T) {
	got := backendEnvironment([]string{"PATH=path", "CODEX_CLI_PATH=shim", "MIRRORBIRD_HOME=home", "MirrorBird_REAL_CODEX=real", "CODEX_HOME=codex", "CODEX_WINDOWS_SANDBOX_PACKAGE_FAMILY=family"}, "real.exe")
	want := []string{"PATH=path", "CODEX_HOME=codex", "CODEX_WINDOWS_SANDBOX_PACKAGE_FAMILY=family", "CODEX_CLI_PATH=real.exe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v", got)
	}
}
