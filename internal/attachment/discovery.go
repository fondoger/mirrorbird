package attachment

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store-packaged Desktop redirects a child shim's AppData writes into its
// package LocalCache. Discover that documented Windows location as well as the
// ordinary path; never scan session stores or arbitrary user directories.
func DiscoveryHomes(home string) []string {
	homes := []string{home}
	local := os.Getenv("LOCALAPPDATA")
	roaming := os.Getenv("APPDATA")
	for _, root := range []struct{ path, category string }{{local, "Local"}, {roaming, "Roaming"}} {
		if root.path == "" {
			continue
		}
		relative, err := filepath.Rel(root.path, home)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(local, "Packages", "OpenAI.Codex_*", "LocalCache", root.category, relative))
		homes = append(homes, matches...)
	}
	return homes
}

func Discover(ctx context.Context, home string) (*Client, error) {
	for _, candidateHome := range DiscoveryHomes(home) {
		entries, _ := List(candidateHome)
		for i := len(entries) - 1; i >= 0; i-- {
			probe, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
			client, err := Connect(probe, entries[i])
			cancel()
			if err == nil {
				return client, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
	return nil, ErrDisconnected
}
