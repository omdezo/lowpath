//go:build !linux

package diag

// newPlatformConnStats: Windows and macOS have per-connection counters, but
// not for unprivileged programs through a stable interface yet.
//
// Windows: GetPerTcpConnectionEStats only returns data bytes after
// SetPerTcpConnectionEStats enabled collection for the connection, and
// that needs administrator rights; the Kernel-Network ETW provider (which
// the flow watcher already reads) needs them too, or the Performance Log
// Users group. GetExtendedTcpTable and GetExtendedUdpTable need none, but
// carry no byte counts.
//
// macOS: the net.inet.tcp.pcblist_n sysctl (what `netstat -b` reads) has
// per-socket byte counts, in private xnu layouts that change between
// releases.
//
// Until those land (#198 for elevated Windows sessions, #199 for macOS),
// the detector counts LAN traffic as other traffic and names no program;
// its line rate is the last bufferbloat test's, or a peak that ages out
// (PassiveConfig.PeakWindow).
func newPlatformConnStats() ConnStats { return noConnStats{} }

type noConnStats struct{}

func (noConnStats) TCPConns() ([]TCPConn, error) {
	return nil, connsUnsupported{"not readable without administrator rights on this system"}
}

func (noConnStats) Owners([]TCPConn) map[uint64]uint32 { return nil }
