// Package owner maps local UDP sockets to the processes that own them: the
// socket-owner resolver that capture uses to tell a game's packets from
// everything else, and that "Add game" uses to list programs with UDP
// sockets.
//
// Resolver is the OS-neutral contract; New returns the implementation for
// the operating system the binary is built for (GetExtendedUdpTable and a
// Toolhelp32 snapshot on Windows). Where no implementation exists yet, every
// call fails with an error that matches errors.ErrUnsupported, so callers
// can say so instead of guessing.
package owner

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"
)

// Socket is one local UDP endpoint and the process that owns it. IPv6
// entries matter too: a dual-stack socket bound to [::] sends IPv4 packets.
type Socket struct {
	Local netip.AddrPort
	PID   uint32
}

// Process is a running program with at least one UDP socket.
type Process struct {
	PID      uint32   `json:"pid"`
	Name     string   `json:"name"`
	UDPPorts []uint16 `json:"udp_ports"`
}

// Resolver reads the operating system's socket and process tables. Both
// calls must work without administrator rights and must not block for long:
// capture calls them on the packet path when a socket is new.
type Resolver interface {
	// UDPSockets lists every IPv4 and IPv6 UDP socket with its owning PID.
	UDPSockets() ([]Socket, error)
	// ProcessNames maps every running PID to its image name ("cs2.exe").
	ProcessNames() (map[uint32]string, error)
}

// ListProcesses lists the programs that have UDP sockets open, sorted by
// name (case-insensitive), then PID. PIDs 0 to 4 (the idle and system
// processes on Windows, the kernel and init on Unix) and self are left out,
// as are sockets whose process exited between the two table reads.
func ListProcesses(r Resolver, self uint32) ([]Process, error) {
	socks, err := r.UDPSockets()
	if err != nil {
		return nil, err
	}
	names, err := r.ProcessNames()
	if err != nil {
		return nil, err
	}
	byPID := map[uint32]*Process{}
	for _, s := range socks {
		if s.PID <= 4 || s.PID == self {
			continue
		}
		name, ok := names[s.PID]
		if !ok {
			continue // exited since the socket table was read
		}
		p := byPID[s.PID]
		if p == nil {
			p = &Process{PID: s.PID, Name: name}
			byPID[s.PID] = p
		}
		if !slices.Contains(p.UDPPorts, s.Local.Port()) {
			p.UDPPorts = append(p.UDPPorts, s.Local.Port())
		}
	}
	out := make([]Process, 0, len(byPID))
	for _, p := range byPID {
		slices.Sort(p.UDPPorts)
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b Process) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
	return out, nil
}
