package diag

import (
	"errors"
	"os"
	"sync"
	"syscall"

	"lowpath/internal/netlink"
	"lowpath/internal/netlink/sockdiag"
	"lowpath/internal/owner"
)

// sockDiag reads the TCP connections from the kernel's sock_diag interface
// (NETLINK_SOCK_DIAG, what `ss -ti` uses) with their tcp_info byte
// counters, and their owners from /proc/<pid>/fd. No root is needed: the
// kernel answers every user's dump with tcp_info filled in, and this
// user's processes' fd folders are readable. Other users' sockets are
// counted but have no owner. UDP has no per-socket byte counters, so UDP
// LAN traffic still counts as other traffic (#200).
type sockDiag struct {
	procfs *owner.Procfs
	uid    uint32

	mu sync.Mutex
	// owners caches inode → PID (0: looked up, not found) for the sockets
	// of the last dump, so /proc is read only for new connections.
	owners map[uint64]uint32
}

func newPlatformConnStats() ConnStats {
	return &sockDiag{procfs: owner.NewProcfs(os.DirFS("/")), uid: uint32(os.Getuid()), owners: map[uint64]uint32{}}
}

func (s *sockDiag) TCPConns() ([]TCPConn, error) {
	c, err := netlink.Dial(netlink.SockDiag)
	if err != nil {
		if errors.Is(err, syscall.EPROTONOSUPPORT) {
			return nil, connsUnsupported{"this kernel has no sock_diag"}
		}
		return nil, err
	}
	defer c.Close()
	var out []TCPConn
	live := map[uint64]bool{}
	for _, v6 := range []bool{false, true} {
		replies, err := c.Dump(sockdiag.DumpTCP(v6))
		switch {
		case err != nil && v6:
			continue // IPv6 is off (ipv6.disable=1): no IPv6 sockets either
		case errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EOPNOTSUPP):
			// A kernel built without inet_diag (CONFIG_INET_DIAG=n).
			return nil, connsUnsupported{"this kernel has no inet_diag"}
		case err != nil:
			return nil, err
		}
		for _, r := range replies {
			sk, ok := sockdiag.Parse(r)
			if !ok {
				continue
			}
			if !sk.HasBytes {
				if sk.State == 1 { // established, yet no counters: Linux before 4.1
					return nil, connsUnsupported{"tcp_info has no byte counters before Linux 4.1"}
				}
				continue
			}
			conn := TCPConn{ID: sk.Cookie, Local: sk.Local, Remote: sk.Remote, Sent: sk.BytesAcked, Received: sk.BytesReceived}
			// Only root may read another user's fd folder: don't look.
			if sk.Inode != 0 && (s.uid == 0 || sk.UID == s.uid) {
				conn.Ref = uint64(sk.Inode)
				live[conn.Ref] = true
			}
			out = append(out, conn)
		}
	}
	s.mu.Lock()
	for ino := range s.owners {
		if !live[ino] {
			delete(s.owners, ino)
		}
	}
	s.mu.Unlock()
	return out, nil
}

func (s *sockDiag) Owners(conns []TCPConn) map[uint64]uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[uint64]bool{}
	for _, c := range conns {
		if _, ok := s.owners[c.Ref]; c.Ref != 0 && !ok {
			want[c.Ref] = true
		}
	}
	if len(want) > 0 {
		found, err := s.procfs.SocketOwners(want)
		if err != nil {
			found = nil // /proc not mounted: no owners, but the bytes still count
		}
		for ino := range want {
			s.owners[ino] = found[ino] // 0 when not found: not looked up again
		}
	}
	out := map[uint64]uint32{}
	for _, c := range conns {
		if pid := s.owners[c.Ref]; c.Ref != 0 && pid != 0 {
			out[c.ID] = pid
		}
	}
	return out
}
