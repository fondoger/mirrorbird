// Package web serves the checked-in, self-contained phone frontend.
// Rebuild static after source changes with: npm --prefix web ci && npm --prefix web run build.
package web

import (
	"embed"
	"io/fs"
)

// static is deliberately checked in: a clean Go build never silently ships an
// empty UI or depends on Node being installed. A missing build fails go:embed.
//
//go:embed static
var production embed.FS

// Assets returns the production frontend rooted at index.html.
func Assets() fs.FS {
	assets, err := fs.Sub(production, "static")
	if err != nil {
		panic("mirrorbird/web: embedded production assets unavailable: " + err.Error())
	}
	return assets
}
