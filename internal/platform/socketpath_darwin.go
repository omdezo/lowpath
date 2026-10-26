package platform

// DefaultSocketPath is the service's API socket on macOS. macOS has no /run
// (its system volume is read-only); /var/run (/private/var/run) is root's
// and emptied at boot, and is where launchd daemons keep their sockets.
const DefaultSocketPath = "/var/run/lowpath/api.sock"
