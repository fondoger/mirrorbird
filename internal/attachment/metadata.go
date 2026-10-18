package attachment

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mirrorbird/internal/platform"
)

const EnvHome = "MIRRORBIRD_HOME"
const EnvRealCodex = "MIRRORBIRD_REAL_CODEX"

// Metadata is local-only. Capability must never appear in logs or the phone API.
type Metadata struct {
	Schema          int       `json:"schema"`
	Pipe            string    `json:"pipe"`
	Capability      string    `json:"capability"`
	ShimPID         int       `json:"shimPid"`
	EnginePID       int       `json:"enginePid"`
	Executable      string    `json:"executable"`
	StartedAt       time.Time `json:"startedAt"`
	ProtocolVersion int       `json:"protocolVersion,omitempty"`
}

func Home() (string, error) {
	if value := os.Getenv(EnvHome); value != "" {
		if !filepath.IsAbs(value) {
			return "", errors.New("MIRRORBIRD_HOME must be an absolute path")
		}
		return filepath.Clean(value), nil
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "MirrorBird"), nil
}

func NewMetadata(userID, executable string, enginePID int) (Metadata, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Metadata{}, err
	}
	userHash := sha256.Sum256([]byte(userID))
	return Metadata{
		Schema: 1, Pipe: fmt.Sprintf(`\\.\pipe\mirrorbird-%x-%d`, userHash[:8], os.Getpid()),
		Capability: hex.EncodeToString(secret[:]), ShimPID: os.Getpid(), EnginePID: enginePID,
		Executable: executable, StartedAt: time.Now().UTC(),
	}, nil
}

func (m Metadata) Validate() error {
	if m.Schema != 1 || m.ShimPID <= 0 || m.EnginePID <= 0 || !strings.HasPrefix(m.Pipe, `\\.\pipe\mirrorbird-`) {
		return errors.New("invalid attachment metadata")
	}
	if decoded, err := hex.DecodeString(m.Capability); err != nil || len(decoded) != 32 {
		return errors.New("invalid attachment capability")
	}
	return nil
}

func Save(home string, m Metadata) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	dir := filepath.Join(home, "attachments")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := platform.SecureDirectory(dir); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, ".attachment-*")
	if err != nil {
		return "", err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	closeErr := file.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", m.ShimPID))
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

func List(home string) ([]Metadata, error) {
	paths, err := filepath.Glob(filepath.Join(home, "attachments", "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Metadata
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.Size() > 8192 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m Metadata
		if json.Unmarshal(data, &m) == nil && m.Validate() == nil {
			out = append(out, m)
		}
	}
	return out, nil
}
