package diag

import (
	"cmp"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"time"
)

// TCPConn is one TCP connection and the payload it has moved so far.
type TCPConn struct {
	// ID tells the connection apart from every other one for as long as
	// it lives (Linux: the socket cookie), so bytes are counted per
	// connection and a reused port pair starts from zero.
	ID            uint64
	Local, Remote netip.AddrPort
	// Sent is the payload the peer acknowledged, Received the payload
	// received in order (RFC 4898 HCThruOctetsAcked and
	// HCThruOctetsReceived): headers and retransmissions aren't counted,
	// the SYN and FIN count one each.
	Sent, Received uint64
	// Ref is what Owners needs to find the process holding the connection
	// (Linux: the socket inode; 0 when no process holds it any more).
	Ref uint64
}

// ConnStats reads per-connection byte counters, so the passive detector can
// leave LAN traffic out of "other traffic" and name the program filling
// the line. The implementation is per OS, behind NewConnStats; it must work
// without administrator rights, and where the OS only offers counters to
// administrators it reports ErrUnsupported (see docs/architecture.md).
type ConnStats interface {
	// TCPConns lists this network namespace's TCP connections, IPv4 and
	// IPv6, with their counters.
	TCPConns() ([]TCPConn, error)
	// Owners maps connections to the PIDs of the processes holding them,
	// by ID. Connections whose owner this process can't see (another
	// user's process) are left out.
	Owners(conns []TCPConn) map[uint64]uint32
}

// NewConnStats returns the platform's ConnStats.
func NewConnStats() ConnStats { return newPlatformConnStats() }

// Talker is one program's internet traffic, bytes/s.
type Talker struct {
	PID    uint32
	Name   string
	Rx, Tx float64
}

// LinkAddrs are the addresses that say where a connection's bytes go.
type LinkAddrs struct {
	// Iface are the default interface's own addresses with their on-link
	// prefixes (192.0.2.10/24, 2001:db8::10/64).
	Iface []netip.Prefix
	// Local are all of this computer's addresses, on any interface.
	Local []netip.Addr
}

// InterfaceAddrs reads LinkAddrs for interface index through the net
// package: GetAdaptersAddresses on Windows, netlink on Linux, getifaddrs on
// macOS. No rights are needed.
func InterfaceAddrs(index int) (LinkAddrs, error) {
	var l LinkAddrs
	ifc, err := net.InterfaceByIndex(index)
	if err != nil {
		return l, err
	}
	own, err := ifc.Addrs()
	if err != nil {
		return l, err
	}
	l.Iface = prefixes(own)
	all, err := net.InterfaceAddrs()
	if err != nil {
		return l, err
	}
	for _, p := range prefixes(all) {
		l.Local = append(l.Local, p.Addr())
	}
	return l, nil
}

// prefixes converts the net package's *net.IPNet addresses, unmapped and
// without zones (sock_diag gives link-local addresses without one).
func prefixes(addrs []net.Addr) []netip.Prefix {
	var out []netip.Prefix
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		ones, bits := n.Mask.Size()
		if bits == 128 && ip.Is4() {
			ones -= 96 // an IPv4 address with a 16-byte mask
		}
		if bits == 0 || ones < 0 || ones > ip.BitLen() {
			continue
		}
		out = append(out, netip.PrefixFrom(ip, ones))
	}
	return out
}

// peerKind says where a connection's bytes go.
type peerKind int

const (
	// peerElsewhere: not through the default interface (loopback, a VPN's
	// tunnel, a container bridge, this computer's own address), so not in
	// its counters either.
	peerElsewhere peerKind = iota
	peerLAN
	peerInternet
)

// kind classifies a connection. Its local address names the interface it
// leaves through: only the default interface's own addresses count. The
// peer is on the LAN when it is in a private (RFC 1918, fc00::/7) or
// link-local range, or in one of the interface's on-link prefixes (a
// public /29 at home, a global IPv6 /64); else the bytes cross the
// internet line.
func (l LinkAddrs) kind(c TCPConn) peerKind {
	local, remote := c.Local.Addr().Unmap().WithZone(""), c.Remote.Addr().Unmap().WithZone("")
	if !remote.IsValid() || remote.IsUnspecified() || remote.IsLoopback() || slices.Contains(l.Local, remote) {
		return peerElsewhere
	}
	onIface := false
	for _, p := range l.Iface {
		if p.Addr() == local {
			onIface = true
			break
		}
	}
	if !onIface {
		return peerElsewhere
	}
	if remote.IsPrivate() || remote.IsLinkLocalUnicast() {
		return peerLAN
	}
	for _, p := range l.Iface {
		if p.Masked().Contains(remote) {
			return peerLAN
		}
	}
	return peerInternet
}

// ConnSample is the default interface's TCP traffic in one interval, split
// by peer.
type ConnSample struct {
	LANRx, LANTx float64 // bytes/s to and from LAN peers
	// Talkers are the programs with the most internet traffic, biggest
	// first (at most maxTalkers; empty while the line is nearly idle).
	Talkers []Talker
}

const (
	// maxTalkers is how many programs a sample names.
	maxTalkers = 3
	// talkerConns is how many of the busiest connections are looked up
	// for their program; a download with more parallel connections still
	// shows up through its biggest ones.
	talkerConns = 32
	// talkerMinBps: internet traffic below this (1 Mbit/s) isn't worth
	// looking up owners for (on Linux that reads /proc/<pid>/fd).
	talkerMinBps = 125e3
	// addrsRefresh is how often the interface's addresses are read again
	// (DHCP renewals, IPv6 privacy addresses).
	addrsRefresh = 10 * time.Second
	// connGrace is how long a connection missing from the readings keeps
	// its last counters: one dump that failed for a family, or a socket
	// the kernel skipped, would otherwise count the connection's whole
	// lifetime as new when it shows up again. IDs are never reused, so a
	// remembered one can't be mistaken for a new socket.
	connGrace = 10 * time.Second
	// namesRefresh is how long one read of the process list names the
	// talkers: PIDs are reused (after 32768 processes with Linux's default
	// pid_max), so a cached name mustn't outlive its process for long.
	namesRefresh = time.Minute
)

type connBytes struct {
	sent, recv uint64
	seen       time.Time // the last reading that listed it
}

// ConnWatch turns successive ConnStats readings into per-second LAN and
// internet rates for the default interface. Not safe for concurrent use:
// the line watch calls it once a second.
type ConnWatch struct {
	conns ConnStats
	names func() (map[uint32]string, error)
	addrs func(index int) (LinkAddrs, error)

	off     bool // the OS has no counters: stop asking
	index   int
	link    LinkAddrs
	linkOK  bool
	linkAt  time.Time
	prev    map[uint64]connBytes
	prevAt  time.Time
	pidName map[uint32]string
	namesAt time.Time
}

// NewConnWatch reads conns; names maps PIDs to program names
// (owner.Resolver.ProcessNames) and addrs reads an interface's addresses
// (nil: InterfaceAddrs). A nil conns gives no samples.
func NewConnWatch(conns ConnStats, names func() (map[uint32]string, error), addrs func(index int) (LinkAddrs, error)) *ConnWatch {
	if addrs == nil {
		addrs = InterfaceAddrs
	}
	return &ConnWatch{conns: conns, names: names, addrs: addrs, off: conns == nil}
}

// Reset forgets the last reading (a session ended; the next starts over).
func (w *ConnWatch) Reset() { w.prev, w.prevAt, w.linkOK, w.pidName = nil, time.Time{}, false, nil }

// Sample reads the counters and returns the rates since the previous
// Sample on the same interface. ok is false for the first reading, while
// the counters or the interface's addresses can't be read, and for good
// once the OS turns out to have no counters.
func (w *ConnWatch) Sample(now time.Time, index int) (ConnSample, bool) {
	if w.off {
		return ConnSample{}, false
	}
	conns, err := w.conns.TCPConns()
	if err != nil {
		w.Reset()
		w.off = isUnsupported(err)
		return ConnSample{}, false
	}
	if index != w.index || !w.linkOK || now.Sub(w.linkAt) >= addrsRefresh || now.Before(w.linkAt) {
		if index != w.index {
			w.prev = nil // other adapter, other counters
		}
		w.index, w.linkAt = index, now
		w.link, err = w.addrs(index)
		w.linkOK = err == nil
	}
	prev, prevAt := w.prev, w.prevAt
	w.prev, w.prevAt = make(map[uint64]connBytes, len(conns)), now
	for _, c := range conns {
		w.prev[c.ID] = connBytes{c.Sent, c.Received, now}
	}
	for id, p := range prev {
		if _, listed := w.prev[id]; !listed && now.Sub(p.seen) < connGrace {
			w.prev[id] = p // what it moves meanwhile counts when it is back
		}
	}
	dt := now.Sub(prevAt).Seconds()
	if !w.linkOK || prev == nil || dt <= 0 {
		return ConnSample{}, false
	}

	type busy struct {
		conn     TCPConn
		sent, rx uint64
	}
	var out ConnSample
	var inet []busy
	var inetTotal uint64
	for _, c := range conns {
		var sent, rx uint64
		if p, ok := prev[c.ID]; ok {
			if c.Sent < p.sent || c.Received < p.recv {
				continue // can't happen for one socket; don't invent bytes
			}
			sent, rx = c.Sent-p.sent, c.Received-p.recv
		} else {
			// New since the last reading (or the grace): everything it
			// moved is new.
			sent, rx = c.Sent, c.Received
		}
		if sent == 0 && rx == 0 {
			continue
		}
		switch w.link.kind(c) {
		case peerLAN:
			out.LANTx += float64(sent) / dt
			out.LANRx += float64(rx) / dt
		case peerInternet:
			inet = append(inet, busy{c, sent, rx})
			inetTotal += sent + rx
		}
	}
	if float64(inetTotal)/dt < talkerMinBps {
		return out, true
	}
	slices.SortFunc(inet, func(a, b busy) int { return cmp.Compare(b.sent+b.rx, a.sent+a.rx) })
	inet = inet[:min(len(inet), talkerConns)]
	looked := make([]TCPConn, len(inet))
	for i, b := range inet {
		looked[i] = b.conn
	}
	pids := w.conns.Owners(looked)
	byPID := map[uint32]*Talker{}
	for _, b := range inet {
		pid, ok := pids[b.conn.ID]
		if !ok || pid == 0 {
			continue
		}
		t := byPID[pid]
		if t == nil {
			t = &Talker{PID: pid}
			byPID[pid] = t
		}
		t.Tx += float64(b.sent) / dt
		t.Rx += float64(b.rx) / dt
	}
	for pid, t := range byPID {
		t.Name = w.name(pid, now)
		out.Talkers = append(out.Talkers, *t)
	}
	slices.SortFunc(out.Talkers, func(a, b Talker) int {
		return cmp.Or(cmp.Compare(b.Rx+b.Tx, a.Rx+a.Tx), cmp.Compare(a.PID, b.PID))
	})
	out.Talkers = out.Talkers[:min(len(out.Talkers), maxTalkers)]
	return out, true
}

// name is a PID's program name; the process list is read again for a PID
// it hasn't seen, or once its last read is namesRefresh old.
func (w *ConnWatch) name(pid uint32, now time.Time) string {
	if n, ok := w.pidName[pid]; ok && now.Sub(w.namesAt) < namesRefresh && !now.Before(w.namesAt) {
		return n
	}
	if w.names != nil {
		if m, err := w.names(); err == nil {
			w.pidName, w.namesAt = m, now
		}
	}
	if n := w.pidName[pid]; n != "" {
		return n
	}
	return "PID " + strconv.FormatUint(uint64(pid), 10)
}

// connsUnsupported is what ConnStats reports on a system without
// per-connection counters for unprivileged programs. It matches both
// ErrUnsupported and errors.ErrUnsupported.
type connsUnsupported struct{ why string }

func (e connsUnsupported) Error() string { return "diag: per-connection byte counters: " + e.why }
func (e connsUnsupported) Is(target error) bool {
	return target == ErrUnsupported || target == errors.ErrUnsupported
}

func isUnsupported(err error) bool {
	return errors.Is(err, ErrUnsupported) || errors.Is(err, errors.ErrUnsupported)
}
