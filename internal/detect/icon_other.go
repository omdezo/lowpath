//go:build !windows

package detect

import (
	"errors"
	"fmt"
	"image"
	"runtime"
)

// Reading a program's icon is not implemented here yet: ELF binaries carry
// none (.desktop files and app bundles do), and a Windows game's .exe under
// Wine would need a PE resource reader.
func exeIcon(path string, size int) (image.Image, error) {
	return nil, fmt.Errorf("detect: program icons are not read on %s yet: %w", runtime.GOOS, errors.ErrUnsupported)
}
