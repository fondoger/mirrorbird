// Package autostart changes only MirrorBird's current-user Windows Run value.
// Nothing happens during construction or normal application startup.
package autostart

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	keyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName = "MirrorBird"
)

// Command is pure and always quotes the absolute executable, even without
// spaces. Paths with quotes/control characters are rejected rather than treated
// as shell syntax. No shell, global environment change, or installer is used.
func Command(executable string) (string, error) {
	if !filepath.IsAbs(executable) || strings.TrimSpace(executable) != executable ||
		strings.ContainsAny(executable, `"`) || strings.IndexFunc(executable, unicode.IsControl) >= 0 ||
		!strings.EqualFold(filepath.Ext(executable), ".exe") {
		return "", errors.New("autostart requires an absolute executable path")
	}
	return `"` + filepath.Clean(executable) + `" --background`, nil
}

type value struct {
	text   string
	exists bool
	expand bool // Preserve an existing REG_EXPAND_SZ value on rollback.
}

type store interface {
	read() (value, error)
	write(value) error
}

// apply captures the original value before touching it. Its rollback only
// restores if the value still equals the one we wrote, avoiding overwriting a
// concurrent user edit. The controller invokes rollback when settings.Save fails.
func apply(s store, executable string, enabled bool) (func() error, error) {
	command, err := Command(executable)
	if err != nil {
		return nil, err
	}
	old, err := s.read()
	if err != nil {
		return nil, err
	}
	next := value{}
	if enabled {
		next = value{text: command, exists: true}
	}
	if old == next {
		return func() error { return nil }, nil
	}
	if err := s.write(next); err != nil {
		return nil, err
	}
	return func() error {
		current, err := s.read()
		if err != nil {
			return err
		}
		if current != next {
			return errors.New("autostart changed after this operation; not overwriting it")
		}
		return s.write(old)
	}, nil
}
