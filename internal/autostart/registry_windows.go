//go:build windows

package autostart

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// Apply is called only after an explicit local checkbox action. It changes
// HKCU, never HKLM, and returns a best-effort undo for a failed settings save.
func Apply(executable string, enabled bool) (func() error, error) {
	return apply(registryStore{}, executable, enabled)
}

type registryStore struct{}

func (registryStore) read() (value, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return value{}, nil
	}
	if err != nil {
		return value{}, err
	}
	defer key.Close()
	text, kind, err := key.GetStringValue(valueName)
	if errors.Is(err, registry.ErrNotExist) {
		return value{}, nil
	}
	if err != nil {
		// Do not replace an unexpected non-string value that cannot be restored.
		return value{}, err
	}
	return value{text: text, exists: true, expand: kind == registry.EXPAND_SZ}, nil
}

func (registryStore) write(v value) error {
	if !v.exists {
		key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer key.Close()
		err = key.DeleteValue(valueName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if v.expand {
		return key.SetExpandStringValue(valueName, v.text)
	}
	return key.SetStringValue(valueName, v.text)
}
