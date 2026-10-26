//go:build !windows

package detect

// steamBases: Steam on Linux (~/.steam, Flatpak) and macOS
// (~/Library/Application Support/Steam) is tracked in issue #43.
func steamBases() []string { return nil }
