package tunnel

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStartupChecksRunInParallelAndPreviewDoesNotAuthorize(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "cancel"}[cancel], func(t *testing.T) {
			r := newFakeRunner()
			before, after := make(chan struct{}, 4), make(chan struct{}, 4)
			allowBefore, allowAfter := make(chan struct{}), make(chan struct{})
			r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] != "user" && args[0] != "show" {
					r.mu.Lock()
					ready := r.ready
					r.mu.Unlock()
					entered, allow := before, allowBefore
					if ready {
						entered, allow = after, allowAfter
					}
					entered <- struct{}{}
					select {
					case <-allow:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return r.normalRun(ctx, args)
			}
			m := testManager(t, r)
			progress := make(chan StartupProgress, 8)
			result := make(chan error, 1)
			go func() {
				_, err := m.StartWithProgress(context.Background(), Options{TunnelID: testID, Port: testPort},
					func(p StartupProgress) { progress <- p })
				result <- err
			}()
			for range 4 {
				waitClosed(t, before)
			}
			close(allowBefore)
			for range 4 {
				waitClosed(t, after)
			}
			var candidate string
			for len(progress) != 0 {
				p := <-progress
				if p.URL != "" {
					if p.Stage != CheckingAccess {
						t.Fatal("address escaped before authenticated host readback")
					}
					candidate = p.URL
				}
			}
			if candidate != testOrigin || m.State().Running || m.State().URL != "" {
				t.Fatal("missing informational URL or premature ready state", candidate, m.State())
			}
			if cancel {
				if err := m.Stop(); err != nil {
					t.Fatal(err)
				}
			} else {
				close(allowAfter)
			}
			select {
			case err := <-result:
				if cancel && !errors.Is(err, context.Canceled) || !cancel && err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("startup did not finish")
			}
			if m.State().Running == cancel || r.count("user") != 1 {
				t.Fatal("wrong final state or duplicate account check", m.State())
			}
		})
	}
}
