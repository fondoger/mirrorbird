package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizePipeName(t *testing.T) {
	maxName := appPipePrefix + strings.Repeat("a", maxPipePath-len(localPipePrefix)-len(appPipePrefix))
	for _, name := range []string{
		"mirrorbird-a", "mirrorbird-1A_b-9", "mirrorbird-abcdef0123456789-1234", maxName,
	} {
		t.Run(name[:min(len(name), 30)], func(t *testing.T) {
			for _, input := range []string{name, localPipePrefix + name} {
				got, err := normalizePipeName(input)
				if err != nil || got != localPipePrefix+name {
					t.Fatalf("normalizePipeName(%q) = %q, %v", input, got, err)
				}
			}
		})
	}
}

func TestRejectPipeNames(t *testing.T) {
	for _, name := range []string{
		"", "mirrorbird-", "MirrorBird-test", "unrelated-test", "mirrorbird",
		" mirrorbird-test", "mirrorbird-test ", "mirrorbird-test\x00hidden", "mirrorbird-test\n",
		"mirrorbird-日本語", "mirrorbird-a.b", "mirrorbird-a/b", `mirrorbird-a\b`,
		"mirrorbird-..", "mirrorbird-a:b", "mirrorbird-a%2fb", "mirrorbird-a?b",
		`\\server\pipe\mirrorbird-test`, `\\localhost\pipe\mirrorbird-test`,
		`\\?\pipe\mirrorbird-test`, `\\.\PIPE\mirrorbird-test`, `\Device\NamedPipe\mirrorbird-test`,
		localPipePrefix + localPipePrefix + "mirrorbird-test",
		appPipePrefix + strings.Repeat("a", maxPipePath-len(localPipePrefix)-len(appPipePrefix)+1),
	} {
		t.Run(name[:min(len(name), 40)], func(t *testing.T) {
			if got, err := normalizePipeName(name); err == nil || got != "" {
				t.Fatalf("invalid name accepted: %q, %v", got, err)
			}
			if listener, err := ListenPipe(name); err == nil || listener != nil {
				if listener != nil {
					listener.Close()
				}
				t.Fatal("ListenPipe accepted an invalid name")
			}
			if conn, err := DialPipe(context.Background(), name); err == nil || conn != nil {
				if conn != nil {
					conn.Close()
				}
				t.Fatal("DialPipe accepted an invalid name")
			}
		})
	}
}

func TestDialPipeContextValidation(t *testing.T) {
	if conn, err := DialPipe(nil, "mirrorbird-test"); err == nil || conn != nil {
		t.Fatal("DialPipe accepted nil context")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, err := DialPipe(ctx, "mirrorbird-test"); !errors.Is(err, context.Canceled) || conn != nil {
		t.Fatalf("canceled dial returned %v, %v", conn, err)
	}
}

func FuzzNormalizePipeName(f *testing.F) {
	for _, seed := range []string{"mirrorbird-test", `\\.\pipe\mirrorbird-test`, `\\server\pipe\mirrorbird-test`, "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		path, err := normalizePipeName(name)
		if err != nil {
			if path != "" {
				t.Fatal("invalid name produced a path")
			}
			return
		}
		if len(path) > maxPipePath || !strings.HasPrefix(path, localPipePrefix+appPipePrefix) {
			t.Fatalf("invalid normalized path %q", path)
		}
		suffix := strings.TrimPrefix(path, localPipePrefix+appPipePrefix)
		if suffix == "" || strings.Trim(suffix, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			t.Fatalf("invalid suffix %q", suffix)
		}
		if again, err := normalizePipeName(path); err != nil || again != path {
			t.Fatal("normalization is not idempotent")
		}
	})
}
