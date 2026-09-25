// Package frontend publishes the built panel (dist/) so it is embedded in
// the MockVision binary: the node serves its own panel and never loads
// anything from a CDN.
package frontend

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built panel.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
