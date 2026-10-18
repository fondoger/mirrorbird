//go:build windows

package platform

import (
	"context"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
)

func listenPipe(path string) (net.Listener, error) {
	sid, err := CurrentUserID()
	if err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{
		// Never use winio's default descriptor: it also grants other accounts
		// access. P protects this DACL from inheritance; only this user gets
		// an allow ACE. The descriptor is applied at creation, not afterwards.
		SecurityDescriptor: "O:" + sid + "D:P(A;;GA;;;" + sid + ")",
		MessageMode:        false,
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	// go-winio v0.6.2 sets FILE_PIPE_REJECT_REMOTE_CLIENTS on every instance.
	// Its retained first handle uses NtCreateNamedPipeFile FILE_CREATE (the
	// exclusive first-instance guarantee), not an open of an existing pipe.
	// Preserve both guarantees when upgrading or replacing the dependency.
	if err != nil {
		return nil, fmt.Errorf("listen on local pipe: %w", err)
	}
	return listener, nil
}

func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	// winio's default SECURITY_ANONYMOUS impersonation level also prevents a
	// server from impersonating this client. This is not server authentication.
	conn, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("dial local pipe: %w", err)
	}
	return conn, nil
}
