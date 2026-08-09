package web

import (
	"embed"
	"io/fs"
)

// staticFS holds the UI. It is compiled into the binary so a build produces a
// single self-contained executable with no runtime asset dependencies.
//
//go:embed all:static
var staticFS embed.FS

// StaticFS returns the embedded UI rooted at the static directory.
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // the embed pattern guarantees this directory exists
	}
	return sub
}
