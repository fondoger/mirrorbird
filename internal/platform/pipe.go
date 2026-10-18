// Package platform provides the OS boundaries used by MirrorBird's local shim.
package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrUnsupported means that the platform cannot provide the requested security
// or process-lifetime guarantee. Callers must not fall back to unsecured IPC or
// an uncontained backend when they receive this error.
var ErrUnsupported = errors.New("operation unsupported on this platform")

const (
	localPipePrefix = `\\.\pipe\`
	appPipePrefix   = "mirrorbird-"
	maxPipePath     = 256
)

// ListenPipe creates a local, current-user-only Windows named pipe. The name
// must be mirrorbird-<suffix> or \\.\pipe\mirrorbird-<suffix>, where suffix is
// nonempty and contains only ASCII letters, digits, hyphens, and underscores.
// The full path may not exceed 256 characters. Remote clients are rejected, and
// an already existing pipe is an error rather than an endpoint to join.
//
// The pipe's ACL is not application authentication; peers must also complete
// MirrorBird's capability handshake before exchanging protocol messages.
func ListenPipe(name string) (net.Listener, error) {
	path, err := normalizePipeName(name)
	if err != nil {
		return nil, err
	}
	return listenPipe(path)
}

// DialPipe connects to a local MirrorBird named pipe, waiting at most until ctx
// is canceled. It accepts the same restricted names as ListenPipe. It does not
// perform the capability handshake.
func DialPipe(ctx context.Context, name string) (net.Conn, error) {
	if ctx == nil {
		return nil, errors.New("dial pipe: nil context")
	}
	path, err := normalizePipeName(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return dialPipe(ctx, path)
}

func normalizePipeName(name string) (string, error) {
	name = strings.TrimPrefix(name, localPipePrefix)
	if !strings.HasPrefix(name, appPipePrefix) {
		return "", errors.New("pipe name must start with mirrorbird-")
	}
	suffix := name[len(appPipePrefix):]
	if suffix == "" {
		return "", errors.New("pipe name must have a nonempty suffix")
	}
	if len(localPipePrefix)+len(name) > maxPipePath {
		return "", fmt.Errorf("pipe path exceeds %d characters", maxPipePath)
	}
	for i := 0; i < len(suffix); i++ {
		c := suffix[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return "", errors.New("pipe name suffix must contain only ASCII letters, digits, hyphens, and underscores")
		}
	}
	return localPipePrefix + name, nil
}
