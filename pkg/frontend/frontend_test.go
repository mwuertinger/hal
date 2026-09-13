package frontend

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
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

// TestEveryShippedAssetBuilds runs the real embedded FS through the fallible
// half of the pipeline, and names the file at fault.
//
// Its predecessor re-checked contentTypes against the same files buildAssets
// had already validated at package-variable initialisation - so the only
// condition it asserted on killed the test binary before it could run, and it
// could never report anything. This can: buildAssetsFS returns its rejections.
func TestEveryShippedAssetBuilds(t *testing.T) {
	built, err := buildAssetsFS(staticFiles)
	if err != nil {
		t.Fatalf("the embedded assets do not build: %v", err)
	}
	if len(built) == 0 {
		t.Fatal("no assets were built")
	}
}

// TestBuildAssetsRejections covers the three ways the pipeline can be fed
// something it would otherwise mangle in silence. These run against a synthetic
// FS because the real one is built at package init, where a rejection would kill
// the test binary rather than fail an assertion.
func TestBuildAssetsRejections(t *testing.T) {
	tests := []struct {
		name    string
		files   fstest.MapFS
		wantErr string
	}{
		{
			name: "valid",
			files: fstest.MapFS{
				"css/hal.css": {Data: []byte(`a{background:url("/static/img/i.svg")}`)},
				"img/i.svg":   {Data: []byte(`<svg/>`)},
			},
		},
		{
			// The case longest-first sorting cannot save, and the one a previous
			// round argued was a false positive: hashing hal.css.map leaves
			// /static/css/hal.css intact inside the result.
			name: "one logical path is another plus a suffix",
			files: fstest.MapFS{
				"css/hal.css":     {Data: []byte(`a{}`)},
				"css/hal.css.map": {Data: []byte(`{}`)},
			},
			wantErr: `is a prefix of`,
		},
		{
			name: "a non-stylesheet references an asset",
			files: fstest.MapFS{
				"js/hal.js": {Data: []byte(`fetch("/static/img/i.svg")`)},
				"img/i.svg": {Data: []byte(`<svg/>`)},
			},
			wantErr: "is not a stylesheet",
		},
		{
			name:    "an extension is not pinned",
			files:   fstest.MapFS{"img/note.xyz": {Data: []byte("x")}},
			wantErr: "no content type pinned",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := buildAssetsFS(tt.files)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("buildAssetsFS() = %v, want nil", err)
				}
				css := string(out["css/hal.css"].content)
				if strings.Contains(css, `"/static/img/i.svg"`) {
					t.Errorf("stylesheet reference was left unhashed: %s", css)
				}
				return
			}
			if err == nil {
				t.Fatalf("buildAssetsFS() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("buildAssetsFS() = %q, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestAssetsAreServable asserts the two properties buildAssetsFS does not
// already reject on. The three that used to sit alongside them - empty content
// type, empty etag, a non-stylesheet naming /static/ - are all unreachable,
// because each is either impossible by construction or an error that takes the
// package down at init.
func TestAssetsAreServable(t *testing.T) {
	if len(assets) == 0 {
		t.Fatal("no assets were built")
	}

	for logical, a := range assets {
		if !hashedPath.MatchString(a.publicPath) {
			t.Errorf("%s: public path %q is not content-addressed", logical, a.publicPath)
		}
		if len(a.content) == 0 {
			t.Errorf("%s: no content", logical)
		}
	}
}

// TestAssetHashCoversContentType pins that the media type is part of the hash.
// Assets are served immutable for a year, so retyping one without changing its
// URL would leave everyone who had already visited with the old type - and a
// stylesheet served as the wrong type is refused outright.
func TestAssetHashCoversContentType(t *testing.T) {
	files := fstest.MapFS{"img/i.svg": {Data: []byte(`<svg/>`)}}

	before, err := buildAssetsFS(files)
	if err != nil {
		t.Fatal(err)
	}

	original := contentTypes[".svg"]
	contentTypes[".svg"] = "image/svg+xml; charset=utf-8"
	defer func() { contentTypes[".svg"] = original }()

	after, err := buildAssetsFS(files)
	if err != nil {
		t.Fatal(err)
	}

	if before["img/i.svg"].publicPath == after["img/i.svg"].publicPath {
		t.Errorf("the content type is not hashed: %q unchanged after retyping the asset",
			after["img/i.svg"].publicPath)
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

// TestRewriteAssetRefsPrefersLongestMatch checks that a shared stem does not make
// the shorter path win. This pair is safe by construction - the hash lands ahead
// of the single extension either way - so it only guards the sort order. The
// genuinely dangerous shape, one path being another plus a suffix, is rejected by
// buildAssetsFS instead, because no sort order can rewrite it correctly.
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
