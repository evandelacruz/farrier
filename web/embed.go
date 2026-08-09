// Package web holds the dashboard the `ui` command serves (UI-001,
// UI-002), embedded into the binary via go:embed so `farrier` stays one
// static file with zero runtime dependencies (spec.md "Language").
//
// The dashboard is plain static assets: HTML, CSS, and browser JavaScript
// as they sit in assets/, with no framework, no build toolchain, and
// nothing fetched from a CDN at runtime. That is a deliberate consequence
// of the single-binary promise rather than a stack choice — anything
// requiring Node at build time would put a second toolchain between the
// tree and `go build`, and anything fetched at runtime would break a
// dashboard served from a machine with no route to the internet.
//
// The dashboard contains zero logic (CLAUDE.md "one core, thin skins").
// Everything it displays comes from internal/api over the same loopback
// origin it is served from — status and replication lag from GET /status,
// backup history from GET /snapshots, drill and promotion from their job
// verbs streaming the one CORE-002 event schema the CLI also renders —
// which is why internal/core/ui serves both from one listener.
package web

import (
	"embed"
	"io/fs"
)

//go:embed assets
var embedded embed.FS

// Assets is the dashboard's file tree, rooted so that "index.html" is the
// page internal/core/ui serves at /.
var Assets fs.FS = mustSub("assets")

func mustSub(dir string) fs.FS {
	sub, err := fs.Sub(embedded, dir)
	if err != nil {
		// Unreachable: dir is embedded above, so fs.Sub cannot fail.
		panic("web: " + err.Error())
	}
	return sub
}
