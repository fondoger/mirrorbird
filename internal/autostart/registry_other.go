//go:build !windows

package autostart

import "errors"

func Apply(executable string, enabled bool) (func() error, error) {
	return nil, errors.New("per-user autostart currently requires Windows")
}
