package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/egoist/mygo"

	"mirrorbird/internal/app"
)

func TestBackgroundReachesAppWithoutEnablingAnything(t *testing.T) {
	var out, errOut bytes.Buffer
	calls := 0
	code := run([]string{"--background"}, &out, &errOut, func(options app.Options) error {
		calls++
		if !options.Background {
			t.Fatal("background was not passed through")
		}
		return nil
	})
	if code != 0 || calls != 1 || errOut.Len() != 0 {
		t.Fatalf("code=%d calls=%d error=%s", code, calls, errOut.String())
	}
}

func TestVersionAndInvalidFlagsDoNotStartApp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{{[]string{"--version"}, 0}, {[]string{"--help"}, 0}, {[]string{"--not-a-flag"}, 2}, {[]string{"extra"}, 2}} {
		var out, errOut bytes.Buffer
		code := run(tc.args, &out, &errOut, func(app.Options) error { t.Fatal("app started while parsing non-run flags"); return nil })
		if code != tc.code {
			t.Fatalf("%v: code %d want %d", tc.args, code, tc.code)
		}
		if tc.args[0] == "--version" && !strings.Contains(out.String(), "MirrorBird ") {
			t.Fatal("version missing")
		}
	}
}

func TestVersionUsesMyGoMetadataWithDevFallback(t *testing.T) {
	previous := mygo.App.Version()
	t.Cleanup(func() { mygo.App.SetVersion(previous) })
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"", "MirrorBird dev\n"},
		{"1.2.3", "MirrorBird 1.2.3\n"},
	} {
		mygo.App.SetVersion(tc.version)
		var out, errOut bytes.Buffer
		code := run([]string{"--version"}, &out, &errOut, func(app.Options) error {
			t.Fatal("version started the app")
			return nil
		})
		if code != 0 || out.String() != tc.want || errOut.Len() != 0 {
			t.Fatalf("version=%q code=%d out=%q error=%q", tc.version, code, out.String(), errOut.String())
		}
	}
}

func TestNativeFailureProducesNonzeroExit(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut, func(app.Options) error { return errors.New("test UI failure") })
	if code != 1 || !strings.Contains(errOut.String(), "test UI failure") {
		t.Fatalf("code %d error %s", code, errOut.String())
	}
}
