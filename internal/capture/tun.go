package capture

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"lowpath/internal/owner"
)

// TUN is the Linux Backend. No kernel module and no driver: the kernel's
// own TUN device, an nftables rule set and one policy-routing rule.
//
//   - An nftables chain on the output hook (type route) marks outbound IPv4
//     UDP that could be game traffic: not DNS, not to LAN, loopback or
//     multicast addresses, not to a relay, not already marked by someone
//     else (a VPN's own packets), and from a user program's socket. Ports
//     that the socket table says belong to other programs are in a set the
//     chain skips, so their packets never leave the kernel.
//   - `ip rule fwmark <tunMark> lookup <tunTable>` sends marked packets to a
//     table whose only route is the TUN device, so they arrive here as raw
//     IP packets.
//   - The packet loop (Capture, the same as on Windows) checks each packet's
//     owner: the game's go to Config.OnPacket; the rest, and what OnPacket
//     declines, go back out through a raw socket with tunBypassMark, which
//     the chain lets through untouched.
//   - Replies are written into the TUN: to the kernel they arrive from the
//     server on that device and go to the game's socket.
//
// Fail-open: the TUN device belongs to the process. When the process ends
// for any reason, the kernel deletes the device and its route; the rule then
// finds an empty table and the kernel moves on to the next rule, so marked
// packets take the normal route. The nftables table is tied to the netlink
// socket that made it (Linux 5.12+) and goes too. Close removes all of it,
// the rule first; Recover removes whatever a killed run left behind.
type TUN struct {
	owners owner.Resolver
	sys    tunSystem
	self   uint32
}

// What the rule set and routing use. The mark and table spell "LP"; the
// rule's priority puts it before VPN clients' catch-all rules (wg-quick's
// sit just below main, at 32765 and up; Tailscale's at 5210 and up).
const (
	tunName       = "lowpath%d" // the kernel picks a free number
	tunNftTable   = "lowpath"
	tunNftChain   = "out"
	tunNftSet     = "direct"
	tunMark       = 0x4c50
	tunBypassMark = 0x4c51
	tunTable      = 0x4c50
	tunRulePrio   = 5100
	// tunMTU is the largest a TUN takes. Packets were sized for the route
	// they had before the mark moved them; a smaller MTU here would drop
	// the bigger ones (DF is set on most UDP) instead of reading them.
	tunMTU = 65535
	// maxDirectPorts bounds the kernel's fast-path set; ports past it are
	// decided here instead, which is only slower.
	maxDirectPorts = 4096
)

// tunAddr is the TUN's own address ("LP" in link-local space): the kernel
// only accepts the replies written into a device that has one.
var tunAddr = netip.MustParsePrefix("169.254.76.80/32")

// ErrNoTUN means the kernel has no TUN device to create.
var ErrNoTUN = errors.New("capture: the kernel has no TUN device (/dev/net/tun)")

// tunSystem is what the TUN backend needs from the OS, so the backend's
// logic runs in tests without root or Linux.
type tunSystem interface {
	// check is Available: nil, ErrNeedAdmin (a RightsError) or ErrNoTUN.
	check() error
	// open creates the TUN device, the nftables rules and the routing rule,
	// in an order that never diverts packets nobody reads, or fails having
	// undone all of it.
	open(r tunRoute) (tunDevice, error)
	// recover removes what a session that never closed left behind.
	recover() error
}

// tunRoute is what open sets up.
type tunRoute struct {
	exclude []netip.AddrPort // relay endpoints (port 0 = the whole host)
	direct  []uint16         // ports of other programs, for the fast path
}

// tunDevice is the packet loop's device, plus the kernel's fast path.
type tunDevice interface {
	device
	directSetter
}

// directSetter keeps a kernel-side list of local UDP ports whose packets go
// out directly without a look from user space: other programs' sockets.
type directSetter interface {
	setDirect(ports []uint16) error
}

// NewTUN makes the Linux backend; owners maps sockets to processes (/proc).
func NewTUN(owners owner.Resolver) *TUN {
	return &TUN{owners: owners, sys: newTUNSystem(), self: uint32(os.Getpid())}
}

func (t *TUN) Name() string     { return "tun" }
func (t *TUN) Available() error { return t.sys.check() }

func (t *TUN) Start(cfg Config) (Session, error) {
	if cfg.Owners == nil {
		cfg.Owners = t.owners
	}
	if err := cfg.check(); err != nil {
		return nil, err
	}
	if err := t.Available(); err != nil {
		return nil, err
	}
	// Map sockets to processes before the first packet arrives.
	o, err := newOwners(cfg.Processes, cfg.PIDs, t.self, cfg.Owners)
	if err != nil {
		return nil, fmt.Errorf("capture: list UDP sockets: %w", err)
	}
	dev, err := t.sys.open(tunRoute{exclude: cfg.Exclude, direct: o.cur.Load().directPorts(maxDirectPorts)})
	if err != nil {
		return nil, err
	}
	c := newCapture(cfg, dev, o)
	c.run(min(4, max(2, runtime.NumCPU()/2)))
	c.log.Info("capture started", "backend", "tun", "processes", cfg.Processes, "pids", cfg.PIDs,
		"game_pids", o.gamePIDs(), "excluded", len(cfg.Exclude))
	return c, nil
}

// Recover removes the routing rule and nftables table of a run that was
// killed. Its TUN device died with it, so they divert nothing any more; they
// are removed anyway, and Start removes them too.
func (t *TUN) Recover(Marker) error { return t.sys.recover() }

// RightsError is a missing-rights error that says, for this OS, which rights
// capture needs and how to get them. It matches ErrNeedAdmin.
type RightsError struct {
	// Reason is a phrase for a sentence: "it needs root rights or ...".
	Reason string
	// Fix is what the user can do, as a phrase: "start Lowpath with sudo".
	Fix string
	// Err is the refusal that showed it, if any.
	Err error
}

func (e *RightsError) Error() string {
	s := "capture: route mode can't start: " + e.Reason
	if e.Err != nil {
		s += " (" + e.Err.Error() + ")"
	}
	return s
}

func (e *RightsError) Is(target error) bool { return target == ErrNeedAdmin }
func (e *RightsError) Unwrap() error        { return e.Err }

// tunRights is the TUN backend's missing-rights error.
func tunRights(err error) *RightsError {
	return &RightsError{
		Reason: "it needs root rights or the CAP_NET_ADMIN and CAP_NET_RAW capabilities",
		Fix:    "start Lowpath with sudo, or give it the capabilities with: sudo setcap cap_net_admin,cap_net_raw+ep lowpath",
		Err:    err,
	}
}

// Linux capability numbers (linux/capability.h).
const (
	capNetAdmin = 12
	capNetRaw   = 13
)

// hasTUNRights reads the effective capabilities from /proc/self/status.
func hasTUNRights(status []byte) (bool, error) {
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "CapEff:")
		if !ok {
			continue
		}
		eff, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err != nil {
			return false, fmt.Errorf("capture: unreadable CapEff %q", v)
		}
		const need = 1<<capNetAdmin | 1<<capNetRaw
		return eff&need == need, nil
	}
	return false, errors.New("capture: no CapEff line in /proc/self/status")
}

// directPorts lists the local UDP ports owned only by programs that are not
// the game, sorted, at most limit of them. A port shared with a game socket
// (two sockets on different addresses) is left out: user space decides.
func (t *ownerTable) directPorts(limit int) []uint16 {
	game, other := map[uint16]bool{}, map[uint16]bool{}
	add := func(port uint16, pid uint32) {
		if t.game[pid] {
			game[port] = true
		} else {
			other[port] = true
		}
	}
	for a, pid := range t.exact {
		add(a.Port(), pid)
	}
	for p, pid := range t.wild {
		add(p, pid)
	}
	for p, pid := range t.wild6 {
		add(p, pid)
	}
	out := make([]uint16, 0, len(other))
	for p := range other {
		if !game[p] && p != 0 {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// tunExcluded are destinations the chain never marks: what a game server
// can't be (this host, LANs, link-local, CGNAT, multicast and reserved,
// "this network").
func tunExcluded() []netip.Prefix {
	return append([]netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("224.0.0.0/3"),
	}, lanPrefixes...)
}

// tunExcludes cleans up the relay endpoints for the rule set: IPv4 only,
// reachable, each once, at most maxExcludes (leaving some out is safe:
// tunnel packets come from this process, which is never captured).
func tunExcludes(exclude []netip.AddrPort) []netip.AddrPort {
	var out []netip.AddrPort
	seen := map[netip.AddrPort]bool{}
	for _, e := range exclude {
		a := netip.AddrPortFrom(e.Addr().Unmap(), e.Port())
		if seen[a] || !reachable(a.Addr()) {
			continue
		}
		if len(out) == maxExcludes {
			break
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}
