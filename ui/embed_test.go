package ui

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

const marker = `<meta name="lowpath-ui" content="placeholder" />`

func TestPick(t *testing.T) {
	page := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	placeholder := page("placeholder")
	for _, tc := range []struct {
		name  string
		files fstest.MapFS
		want  string // index.html of the chosen front end; "" = error
		built bool
		asset string // a file the chosen front end must serve too
	}{
		{
			name: "build and placeholder",
			files: fstest.MapFS{
				"dist/app/index.html":         page("build"),
				"dist/app/assets/index-1.js":  page("js"),
				"dist/placeholder/index.html": placeholder,
			},
			want: "build", built: true, asset: "assets/index-1.js",
		},
		{
			name:  "fresh clone: only the placeholder",
			files: fstest.MapFS{"dist/placeholder/index.html": placeholder},
			want:  "placeholder",
		},
		{
			name: "build stopped part way: assets, no index.html",
			files: fstest.MapFS{
				"dist/app/assets/index-1.js":  page("js"),
				"dist/placeholder/index.html": placeholder,
			},
			want: "placeholder",
		},
		{
			name: "index.html that is a folder",
			files: fstest.MapFS{
				"dist/app/index.html/x":       page("x"),
				"dist/placeholder/index.html": placeholder,
			},
			want: "placeholder",
		},
		{
			// A clone from before the build moved to dist/app keeps its
			// old, ignored output until it is deleted; it is never served.
			name: "old build in dist itself",
			files: fstest.MapFS{
				"dist/index.html":             page("old build"),
				"dist/assets/index-0.js":      page("js"),
				"dist/placeholder/index.html": placeholder,
			},
			want: "placeholder",
		},
		{
			name:  "nothing",
			files: fstest.MapFS{"dist/other.txt": page("x")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, built, err := pick(tc.files)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("pick = %v, want an error", fsys)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := fs.ReadFile(fsys, "index.html")
			if err != nil || string(b) != tc.want || built != tc.built {
				t.Fatalf("index.html = %q (%v), built %v; want %q, built %v", b, err, built, tc.want, tc.built)
			}
			if tc.asset != "" {
				if _, err := fs.Stat(fsys, tc.asset); err != nil {
					t.Fatalf("asset: %v", err)
				}
			}
		})
	}
}

// The tracked placeholder is always embedded, so every build has a page to
// show, and it loads nothing: a build without the UI must not reach out.
func TestPlaceholderEmbedded(t *testing.T) {
	b, err := fs.ReadFile(dist, placeholderDir+"/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	if !strings.Contains(page, marker) {
		t.Errorf("placeholder lacks %s", marker)
	}
	for _, s := range []string{"<script", " src=", `rel="stylesheet"`, "@import", "url("} {
		if strings.Contains(page, s) {
			t.Errorf("placeholder loads something (%q)", s)
		}
	}
}

// FS and Built agree with what this checkout embeds: the UI after npm run
// build, the placeholder before it.
func TestFS(t *testing.T) {
	fsys, err := FS()
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if got := !strings.Contains(string(b), marker); got != Built() {
		t.Fatalf("Built() = %v, but index.html is the build: %v", Built(), got)
	}
}
