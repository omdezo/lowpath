//go:build release

package ui

import _ "embed"

// releaseBuild is set by -tags release, the tag release artifacts are built with
// (build.ps1 -Release): they must carry the UI, never the placeholder.
const releaseBuild = true

// A release build without the UI fails to compile here, with "pattern
// dist/app/index.html: no matching files found": run npm run build in ui,
// then build again. FS refuses the placeholder too, in case this ever goes.
//
//go:embed dist/app/index.html
var _ []byte
