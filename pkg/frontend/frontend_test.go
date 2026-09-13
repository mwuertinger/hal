package frontend

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// hashedPath matches the content-addressed URLs buildAssets produces.
var hashedPath = regexp.MustCompile(`^/static/.+\.[0-9a-f]{16}\.[a-z0-9]+$`)

// TestContentTypesArePinned guards the ordering bug this replaced: the types
// used to come from mime.TypeByExtension, which buildAssets reads during
// package-variable initialisation - before any init() could register .woff2 -
// and which otherwise depends on /etc/mime.types being installed on the host.
//
// It reads contentTypes directly and never goes near the mime package. An
// earlier version of this test called a helper that fell back to
// mime.TypeByExtension, and so passed with contentTypes emptied entirely: on a
// developer machine the host's /etc/mime.types supplied every answer, which is
// precisely the thing that is absent on the target.
func TestContentTypesArePinned(t *testing.T) {
	want := map[string]string{
		".woff2": "font/woff2",
		".css":   "text/css; charset=utf-8",
		".js":    "text/javascript; charset=utf-8",
		".svg":   "image/svg+xml",
		".txt":   "text/plain; charset=utf-8",
	}
	for ext, typ := range want {
		if got := contentTypes[ext]; got != typ {
			t.Errorf("contentTypes[%q] = %q, want %q", ext, got, typ)
		}
	}
}

// TestEveryShippedExtensionIsPinned fails here rather than at startup on the
// target: an unpinned extension panics in buildAssets, which under
// Restart=on-failure would restart-loop the unit.
func TestEveryShippedExtensionIsPinned(t *testing.T) {
	err := fs.WalkDir(staticFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, ok := contentTypes[path.Ext(p)]; !ok {
			t.Errorf("%s: extension %q is not pinned, so buildAssets would panic", p, path.Ext(p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAssetsAreServable(t *testing.T) {
	if len(assets) == 0 {
		t.Fatal("no assets were built")
	}

	for logical, a := range assets {
		if a.contentType == "" {
			t.Errorf("%s: empty content type; ServeContent would then skip sniffing too", logical)
		}
		if !hashedPath.MatchString(a.publicPath) {
			t.Errorf("%s: public path %q is not content-addressed", logical, a.publicPath)
		}
		if len(a.content) == 0 {
			t.Errorf("%s: no content", logical)
		}
		if a.etag == "" {
			t.Errorf("%s: no etag", logical)
		}
		// Only stylesheets have their references rewritten, so an unhashed
		// /static/ reference anywhere else would 404 at runtime.
		if path.Ext(logical) != ".css" && strings.Contains(string(a.content), "/static/") {
			t.Errorf("%s: references /static/ but is not a stylesheet", logical)
		}
	}
}

// TestStylesheetReferencesAreHashed covers the font URLs inside hal.css: they are
// rewritten at startup, and an unrewritten one would be a silent 404.
func TestStylesheetReferencesAreHashed(t *testing.T) {
	css, ok := assets["css/hal.css"]
	if !ok {
		t.Fatal("css/hal.css is not an asset")
	}
	for _, ref := range regexp.MustCompile(`/static/[^"')]+`).FindAllString(string(css.content), -1) {
		if !hashedPath.MatchString(ref) {
			t.Errorf("unhashed reference %q in hal.css", ref)
		}
	}
}

// TestRewriteAssetRefsPrefers the longest match: a logical path that is a prefix
// of another must not be substituted inside it.
func TestRewriteAssetRefsPrefersLongestMatch(t *testing.T) {
	built := map[string]*staticAsset{
		"img/a.svg":  {publicPath: "/static/img/a.SHORT.svg"},
		"img/a.svgz": {publicPath: "/static/img/a.LONG.svgz"},
	}
	got := string(rewriteAssetRefs([]byte(`url("/static/img/a.svgz")`), built))
	if want := `url("/static/img/a.LONG.svgz")`; got != want {
		t.Errorf("rewriteAssetRefs = %s, want %s", got, want)
	}
}

func TestAssetURL(t *testing.T) {
	for _, logical := range []string{"css/hal.css", "js/hal.js", "img/favicon.svg"} {
		url, err := assetURL(logical)
		if err != nil {
			t.Errorf("assetURL(%q): %v", logical, err)
			continue
		}
		if !hashedPath.MatchString(url) {
			t.Errorf("assetURL(%q) = %q, not content-addressed", logical, url)
		}
	}
	if _, err := assetURL("css/nope.css"); err == nil {
		t.Error("assetURL of an unknown asset should fail, so a typo cannot reach a template")
	}
}
