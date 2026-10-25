package app

import "golang.org/x/sys/windows"

// programFiles is the 64-bit Program Files folder as Windows knows it.
func programFiles() (string, error) {
	return windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
}
