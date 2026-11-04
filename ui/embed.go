// Package ui embeds the front end the Lowpath window shows.
//
// npm run build writes the UI to ui/dist/app, which git ignores, so a build
// never changes a tracked file. ui/dist/placeholder is tracked: it gives
// //go:embed a file to match on a fresh clone (a pattern that matches
// nothing fails the build), so go vet and go test work before the UI is
// built, and it is the page such a build shows instead of the UI.
//
// A build with -tags release (build.ps1 -Release) must carry the UI: it
// fails to compile without ui/dist/app/index.html, and its FS never returns
// the placeholder.
package ui

import (
	"embed"
	"errors"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

const (
	appDir         = "dist/app"         // npm run build (vite.config.ts build.outDir)
	placeholderDir = "dist/placeholder" // tracked; shown when there is no build
)

// ErrNoBuild is FS's error in a release build that embeds only the
// placeholder page. A program that gets it refuses to start.
var ErrNoBuild = errors.New("this release build has no UI, only the placeholder page: run npm run build in ui, then build Lowpath again")

// FS returns the front end to serve, rooted at its index.html: the UI build
// when this program was built after npm run build, else the placeholder
// page that says how to build it. A release build never returns the
// placeholder: it returns ErrNoBuild instead.
func FS() (fs.FS, error) {
	return serve(dist, releaseBuild)
}

// Built reports whether this program embeds the UI build rather than the
// placeholder page.
func Built() bool {
	_, built, err := pick(dist)
	return err == nil && built
}

// serve is FS for root: what pick chooses, except that a release build
// refuses the placeholder.
func serve(root fs.FS, release bool) (fs.FS, error) {
	fsys, built, err := pick(root)
	if err == nil && release && !built {
		return nil, ErrNoBuild
	}
	return fsys, err
}

// pick chooses between the build and the placeholder in root. A build
// counts only with its index.html: one that failed or was stopped part way
// can leave assets without it.
func pick(root fs.FS) (fsys fs.FS, built bool, err error) {
	for _, c := range []struct {
		dir   string
		built bool
	}{{appDir, true}, {placeholderDir, false}} {
		if st, err := fs.Stat(root, c.dir+"/index.html"); err != nil || st.IsDir() {
			continue
		}
		sub, err := fs.Sub(root, c.dir)
		return sub, c.built, err
	}
	return nil, false, errors.New("no front end embedded: neither " + appDir + " nor " + placeholderDir + " has an index.html")
}
