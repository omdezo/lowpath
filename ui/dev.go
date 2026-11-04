//go:build !release

package ui

// releaseBuild is false in development builds: before npm run build they embed
// the placeholder page, so go vet, go test and go build work on a fresh
// clone. Release artifacts are built with -tags release (release.go).
const releaseBuild = false
