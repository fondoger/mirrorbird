package app

import (
	"context"
	"testing"

	"mirrorbird/internal/tunnel"
)

func TestAccountCheckDoesNotTemporarilyShowLoggedOut(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.tunnel.progressFn = func(ctx context.Context, notify func(tunnel.StartupProgress)) error {
		notify(tunnel.StartupProgress{Stage: tunnel.CheckingAccount})
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	if !f.c.snapshot().LoggedIn {
		t.Fatal("fixture must begin logged in")
	}
	f.c.startSharing()
	receive(t, entered, "account check")
	if !f.c.snapshot().LoggedIn {
		t.Error("checking the account was presented as logging out")
	}
	close(release)
	waitIdle(t, f.c)
}

func TestSharingAddressPreviewIsRevokedOnFailureAndCancellation(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			f := newFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			f.tunnel.progressFn = func(ctx context.Context, notify func(tunnel.StartupProgress)) error {
				p := tunnel.StartupProgress{Stage: tunnel.CheckingAccess, URL: "https://owned-43123.usw2.devtunnels.ms"}
				notify(p)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					notify(p) // A late callback must not undo Stop's revoke.
					return ctx.Err()
				}
				if outcome == "failure" {
					return tunnel.ErrUnverified
				}
				return nil
			}
			f.c.startSharing()
			receive(t, entered, "address preview")
			state := f.c.snapshot()
			if state.PendingTunnelURL == "" || state.TunnelURL != "" || state.Sharing || f.api.Sharing() {
				t.Fatal("preview authorized access or was not visible", state)
			}
			if outcome == "cancel" {
				f.c.stopSharing()
			} else {
				close(release)
			}
			waitIdle(t, f.c)
			state = f.c.snapshot()
			if state.PendingTunnelURL != "" || state.Sharing != (outcome == "success") || f.api.Sharing() != state.Sharing {
				t.Fatal("incorrect startup completion", state)
			}
		})
	}
}
