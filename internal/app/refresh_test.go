package app

import (
	"context"
	"testing"

	"mirrorbird/internal/session"
)

func TestRefreshOrderingFailureStopsBeforeReplacingAttachment(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.session.mu.Lock()
	attempts := 0
	f.session.refreshFn = func(context.Context) error {
		attempts++
		if attempts < 3 {
			return session.ErrResyncRequired
		}
		return nil
	}
	f.session.mu.Unlock()

	f.c.refresh()
	waitIdle(t, f.c)
	f.session.mu.Lock()
	connects := f.session.connects
	f.session.mu.Unlock()
	if f.api.Sharing() || connects != 0 {
		t.Fatal("ordering failure must stop the old remote lifetime before reconnecting")
	}

	f.c.refresh()
	waitIdle(t, f.c)
	f.session.mu.Lock()
	connects = f.session.connects
	f.session.mu.Unlock()
	if connects != 1 || !f.session.Connected() || f.api.Sharing() {
		t.Fatal("explicit second refresh must reconnect without restarting remote access")
	}
}
