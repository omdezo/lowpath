//go:build !windows

package app

import "errors"

// Route mode, and so the driver, exists only on Windows.
func programFiles() (string, error) {
	return "", errors.New("no Program Files folder on this platform")
}
