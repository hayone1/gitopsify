// Package web holds the built web UI (make ui writes it to web/dist), which
// the gitopsify binary serves.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built UI, or nil when this build has none (dist holds only
// its placeholder: go install and plain go build skip the Node build).
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
