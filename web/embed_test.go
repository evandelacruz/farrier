package web

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard is served straight out of this embedded tree, so a view
// whose stylesheet or script never made it into the binary fails silently
// in the browser rather than at build time. These assertions are the
// build-time check that costs nothing.
func TestAssetsCarryTheWholeDashboard(t *testing.T) {
	for _, name := range []string{"index.html", "app.css", "app.js"} {
		if _, err := fs.Stat(Assets, name); err != nil {
			t.Errorf("Assets is missing %s: %v", name, err)
		}
	}
}

func TestIndexReferencesEmbeddedAssets(t *testing.T) {
	index, err := fs.ReadFile(Assets, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}

	// The /ui/ prefix is the path internal/core/ui serves the static
	// assets under; a reference to any other origin would be a runtime
	// fetch the single-binary promise rules out.
	for _, want := range []string{`href="/ui/app.css"`, `src="/ui/app.js"`} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.html does not reference %s", want)
		}
	}
	for _, forbidden := range []string{"http://", "https://", "//cdn"} {
		if strings.Contains(string(index), forbidden) {
			t.Errorf("index.html references an off-machine origin (%q): the dashboard must fetch nothing at runtime", forbidden)
		}
	}
}
