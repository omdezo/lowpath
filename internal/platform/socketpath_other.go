//go:build !darwin

package platform

// DefaultSocketPath is the service's API socket on Linux (and the name the
// tests use elsewhere). /run is writable only by root, so no other user can
// take the name first.
const DefaultSocketPath = "/run/lowpath/api.sock"
