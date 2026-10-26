//go:build !windows && !linux && !darwin

package platform

// nativePrivileges is root without a prompt: no privilege helper for the
// BSDs yet.
func nativePrivileges() Privileges { return NewRoot() }
