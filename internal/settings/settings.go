package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"mirrorbird/internal/platform"
)

type Config struct {
	Schema            int    `json:"schema"`
	Port              int    `json:"port"`
	TunnelID          string `json:"tunnelId,omitempty"`
	TunnelExecutable  string `json:"tunnelExecutable,omitempty"`
	DesktopExecutable string `json:"desktopExecutable,omitempty"`
	CodexExecutable   string `json:"codexExecutable,omitempty"`
	AutoStart         bool   `json:"autoStart"`
}

type Store struct {
	mu   sync.Mutex
	path string
}

func Open(home string) (*Store, Config, error) {
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, Config{}, err
	}
	if err := platform.SecureDirectory(home); err != nil {
		return nil, Config{}, err
	}
	store := &Store{path: filepath.Join(home, "settings.json")}
	config := Config{Schema: 1, Port: 43123}
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, config, nil
	}
	if err != nil {
		return nil, config, err
	}
	if len(data) > 64*1024 {
		return nil, config, errors.New("settings file is too large")
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, config, errors.New("settings must be a JSON object")
	}
	if err = json.Unmarshal(data, &config); err != nil {
		return nil, config, err
	}
	if err = validate(config); err != nil {
		return nil, config, err
	}
	return store, config, nil
}

func (s *Store) Save(config Config) error {
	if err := validate(config); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".settings-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, s.path)
}

func validate(c Config) error {
	if c.Schema != 1 {
		return errors.New("unsupported settings version")
	}
	if c.Port < 1024 || c.Port > 65535 {
		return errors.New("server port must be between 1024 and 65535")
	}
	for _, path := range []string{c.DesktopExecutable, c.CodexExecutable} {
		if path != "" && !filepath.IsAbs(path) {
			return errors.New("executable paths must be absolute")
		}
	}
	return nil
}
