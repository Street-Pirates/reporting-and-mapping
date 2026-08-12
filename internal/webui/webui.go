// Package webui embeds the static mobile-first frontend so the server binary is
// fully self-contained (no external asset directory to deploy).
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var staticFS embed.FS

// FS returns the frontend filesystem rooted at the static/ directory, so paths
// like "index.html" and "app.js" resolve directly.
func FS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded, so this can only fail on a build-time mistake
	}
	return sub
}
