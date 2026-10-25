//go:build !windows

package detect

import (
	"errors"
	"image"
)

// ExeIcon is only implemented on Windows.
func ExeIcon(path string, size int) (image.Image, error) {
	return nil, errors.New("detect: program icons are only read on Windows")
}

// RunningExe is only implemented on Windows.
func RunningExe(names []string) string { return "" }
