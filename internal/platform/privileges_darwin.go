package platform

import "os"

// NewAdminPrompt returns the privilege helper for this process.
func NewAdminPrompt() *AdminPrompt {
	return &AdminPrompt{Root: *NewRoot(), x: osExec{}, exe: os.Executable}
}

func nativePrivileges() Privileges { return NewAdminPrompt() }
