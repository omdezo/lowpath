//go:build !windows

package detect

// Outside Windows the process list comes from an owner.ProcessTable; a
// System built without one sees no processes rather than guessing.
func running() map[string]bool { return map[string]bool{} }

func runningExe(names []string) string { return "" }
