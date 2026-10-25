// Package ui embeds the built front end (npm run build -> ui/dist).
package ui

import "embed"

//go:embed all:dist
var Dist embed.FS
