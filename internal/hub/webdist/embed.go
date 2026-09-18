// Package webdist embeds the SvelteKit static export. `make web` builds the
// front in web/ and copies it into build/ next to this file.
package webdist

import (
	"embed"
	"io/fs"
)

//go:embed all:build
var build embed.FS

// Build returns the export rooted at its index.html.
func Build() fs.FS {
	sub, err := fs.Sub(build, "build")
	if err != nil {
		panic(err)
	}
	return sub
}
