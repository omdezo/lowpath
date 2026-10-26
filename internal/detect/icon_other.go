//go:build !windows

package detect

import (
	"errors"
	"fmt"
	"image"
	"runtime"
)

// Reading a program's icon is not implemented here yet (ELF binaries carry
// none; .desktop files and app bundles do, see issue #43).
func exeIcon(path string, size int) (image.Image, error) {
	return nil, fmt.Errorf("detect: program icons are not read on %s yet: %w", runtime.GOOS, errors.ErrUnsupported)
}

// Finding a running program's path is not implemented here yet: "" is the
// honest "don't know".
func runningExe(names []string) string { return "" }
