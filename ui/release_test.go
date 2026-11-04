package ui

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestServe(t *testing.T) {
	page := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	placeholderOnly := fstest.MapFS{"dist/placeholder/index.html": page("placeholder")}
	built := fstest.MapFS{
		"dist/app/index.html":         page("build"),
		"dist/placeholder/index.html": page("placeholder"),
	}
	for _, tc := range []struct {
		name    string
		root    fs.FS
		release bool
		want    string // index.html served; "" = ErrNoBuild
	}{
		{"development, fresh clone", placeholderOnly, false, "placeholder"},
		{"development, built", built, false, "build"},
		{"release, fresh clone", placeholderOnly, true, ""},
		{"release, built", built, true, "build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, err := serve(tc.root, tc.release)
			if tc.want == "" {
				if !errors.Is(err, ErrNoBuild) {
					t.Fatalf("serve = %v, %v; want ErrNoBuild", fsys, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b, err := fs.ReadFile(fsys, "index.html"); err != nil || string(b) != tc.want {
				t.Fatalf("index.html = %q (%v), want %q", b, err, tc.want)
			}
		})
	}
	// The program shows this error when it refuses to start: it has to
	// say how to get a build that starts.
	if !strings.Contains(ErrNoBuild.Error(), "npm run build") {
		t.Errorf("ErrNoBuild = %q, doesn't name npm run build", ErrNoBuild)
	}
}

// A development build (no release tag) still serves the placeholder before
// npm run build.
func TestDevelopmentServesPlaceholder(t *testing.T) {
	if releaseBuild {
		t.Skip("built with -tags release")
	}
	if Built() {
		t.Skip("this checkout has the UI build in ui/dist/app")
	}
	fsys, err := FS()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := fs.ReadFile(fsys, "index.html"); err != nil || !strings.Contains(string(b), marker) {
		t.Fatalf("FS serves %q (%v), want the placeholder", b, err)
	}
}

// go build -tags release fails without ui/dist/app/index.html, at the line
// that says to run npm run build, and works with it. It builds a copy of
// this package, so the checkout's own ui/dist/app doesn't matter.
func TestReleaseBuildNeedsUI(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command:", err)
	}
	dir := t.TempDir()
	write := func(name string, b []byte) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", []byte("module uicopy\n\ngo 1.22\n"))
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range append(srcs, filepath.Join("dist", "placeholder", "index.html")) {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.ToSlash(src), b)
	}
	build := func() (string, error) {
		cmd := exec.Command(gobin, "build", "-tags", "release", ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := build()
	if err == nil {
		t.Fatalf("release build with only the placeholder succeeded:\n%s", out)
	}
	if !strings.Contains(out, "release.go") || !strings.Contains(out, "dist/app/index.html") {
		t.Fatalf("release build failed, but not on the UI check:\n%s", out)
	}
	src, err := os.ReadFile("release.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "npm run build") {
		t.Error("release.go doesn't say to run npm run build where the build fails")
	}

	write("dist/app/index.html", []byte("<!doctype html><title>build</title>"))
	if out, err := build(); err != nil {
		t.Fatalf("release build with the UI: %v\n%s", err, out)
	}
}
