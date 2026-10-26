package capture

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"lowpath/internal/divert"
	"lowpath/internal/netlink"
	"lowpath/internal/netlink/nft"
	"lowpath/internal/netlink/rtnl"
)

// drainWait is how long the receivers keep reading the TUN after the rule is
// gone: what is already queued in it still gets tunneled or sent on.
const drainWait = 50 * time.Millisecond

// linuxTUN is the real tunSystem.
type linuxTUN struct {
	status string // /proc/self/status
	dev    string // /dev/net/tun
	// unowned makes the nftables table without the owner flag, as on
	// kernels before 5.12 (tests).
	unowned bool
}

func newTUNSystem() tunSystem { return &linuxTUN{status: "/proc/self/status", dev: "/dev/net/tun"} }

func (l *linuxTUN) check() error {
	b, err := os.ReadFile(l.status)
	if err != nil {
		return fmt.Errorf("capture: read capabilities: %w", err)
	}
	ok, err := hasTUNRights(b)
	if err != nil {
		return err
	}
	if !ok {
		return tunRights(nil)
	}
	if _, err := os.Stat(l.dev); err != nil {
		return fmt.Errorf("%w: %v", ErrNoTUN, err)
	}
	return nil
}

func (l *linuxTUN) open(r tunRoute) (tunDevice, error) {
	if status, err := os.ReadFile(l.status); err == nil {
		r.uid, r.oneUser = tunUsers(status, os.Geteuid())
	}
	d := &tunLink{direct: map[uint16]bool{}}
	if err := d.setUp(l.dev, r, !l.unowned); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// recover removes a killed run's rule and table. Without the rights to do
// so it checks that they are inert: they only divert into a lowpath TUN
// device, and with none left the kernel skips the rule's empty table.
func (l *linuxTUN) recover() error {
	err := removeLeftovers()
	if err == nil || !errors.Is(err, syscall.EPERM) {
		return err
	}
	ifs, ierr := net.Interfaces()
	if ierr != nil {
		return errors.Join(err, ierr)
	}
	for _, i := range ifs {
		if strings.HasPrefix(i.Name, strings.TrimSuffix(tunName, "%d")) {
			return fmt.Errorf("capture: %s is still up and Lowpath lacks the rights to remove it: %w", i.Name, err)
		}
	}
	return nil
}

// removeLeftovers deletes every copy of the routing rule and the nftables
// table; none there is not an error.
func removeLeftovers() error {
	rt, err := netlink.Dial(netlink.Route)
	if err != nil {
		return err
	}
	defer rt.Close()
	errs := []error{deleteRules(rt)}
	nf, err := netlink.Dial(netlink.Netfilter)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	defer nf.Close()
	if err := nf.Do(nft.Batch(nft.DelTable(nft.IPv4, tunNftTable))...); err != nil && !errors.Is(err, syscall.ENOENT) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// deleteRules removes the rule until the kernel says there is none (a
// crashed run and a later one could each have added it).
func deleteRules(rt *netlink.Conn) error {
	for range 16 {
		err := rt.Do(rtnl.DelRule(tunRule))
		if errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return errors.New("capture: the routing rule keeps coming back")
}

// tunLink is the TUN device with what routes packets to it. It is the
// packet loop's device: Recv reads the diverted packets, Send puts a packet
// back out (outbound: through the raw socket, past the chain) or delivers a
// reply (inbound: into the TUN).
type tunLink struct {
	file  *os.File // the TUN; closing it deletes the device and its route
	raw   *os.File // raw IPv4 socket with tunBypassMark
	rawc  syscall.RawConn
	name  string
	index int
	rt    *netlink.Conn
	nf    *netlink.Conn // owns the nftables table

	mu      sync.Mutex
	ruleOn  bool
	tableOn bool
	owned   bool            // the table belongs to nf (Linux 5.12+)
	handle  uint64          // the table's, to tell it from one made again
	route   tunRoute        // what the table was made for, to make it again
	direct  map[uint16]bool // what the kernel's set holds

	shutOnce, closeOnce sync.Once
	closeErr            error
}

func (d *tunLink) setUp(dev string, r tunRoute, own bool) error {
	fd, err := unix.Open(dev, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return d.rightsOr(fmt.Errorf("capture: open %s: %w", dev, err), err)
	}
	ifr, err := unix.NewIfreq(tunName)
	if err != nil {
		unix.Close(fd)
		return err
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		return d.rightsOr(fmt.Errorf("capture: create the TUN device: %w", err), err)
	}
	d.name = ifr.Name()
	// Non-blocking: reads wait in Go's poller, so a read deadline can end
	// them and Close can't race a read on a reused descriptor.
	d.file = os.NewFile(uintptr(fd), "/dev/net/tun")
	ifi, err := net.InterfaceByName(d.name)
	if err != nil {
		return fmt.Errorf("capture: find %s: %w", d.name, err)
	}
	d.index = ifi.Index

	// Replies arrive on the TUN from servers that are routed elsewhere;
	// strict reverse-path filtering would drop them. The kernel uses the
	// larger of the "all" and per-device values, so loose (2) on this
	// device is loose whatever "all" says. Both settings go with the device.
	if err := writeSysctl("net/ipv4/conf/"+d.name+"/rp_filter", "2"); err != nil {
		return d.rightsOr(fmt.Errorf("capture: set rp_filter on %s: %w", d.name, err), err)
	}
	writeSysctl("net/ipv6/conf/"+d.name+"/disable_ipv6", "1") // missing when IPv6 is off

	// The rule set only marks packets; nothing follows the mark until the
	// rule exists. It comes before the route so that a table another
	// running copy owns stops this one before it touches table tunTable.
	if d.nf, err = netlink.Dial(netlink.Netfilter); err != nil {
		return err
	}
	err = d.nf.Do(tunRuleset(r, own)...)
	d.owned = own && err == nil
	if own && (errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EINVAL)) {
		// Before Linux 5.12 a table can't belong to a socket, and the
		// unknown flag is refused with EINVAL (EOPNOTSUPP since); Close
		// and Recover delete the table instead. (An owned table also
		// survives `nft flush ruleset` from a firewall reload; an unowned
		// one doesn't, and its packets then simply go direct.)
		err = d.nf.Do(tunRuleset(r, false)...)
	}
	if errors.Is(err, syscall.EPERM) {
		// The TUN device needed CAP_NET_ADMIN too, so this is not about
		// rights: the table belongs to another process's netlink socket.
		return fmt.Errorf("capture: nftables table %q belongs to another running program (another copy of Lowpath?); stop it and try again: %w", tunNftTable, err)
	}
	if err != nil {
		return d.rightsOr(fmt.Errorf("capture: nftables: %w", err), err)
	}
	d.tableOn = true
	d.route = r
	d.handle, _, _ = d.findTable() // without it, keepRoute checks the name only
	for _, p := range r.direct {
		d.direct[p] = true
	}

	if d.rt, err = netlink.Dial(netlink.Route); err != nil {
		return err
	}
	// Reverse-path filtering, loose or strict, drops what arrives on a
	// device without an IPv4 address, so the TUN gets one. Link-local and
	// /32: no subnet route, and nothing else picks it as a source.
	addr, err := rtnl.AddAddr(d.index, tunAddr, rtnl.ScopeLink)
	if err != nil {
		return err
	}
	if err := d.rt.Do(addr); err != nil {
		return d.rightsOr(fmt.Errorf("capture: address %s: %w", d.name, err), err)
	}
	if err := d.rt.Do(rtnl.LinkUp(d.index, tunMTU)); err != nil {
		return d.rightsOr(fmt.Errorf("capture: bring %s up: %w", d.name, err), err)
	}
	route, err := rtnl.AddRoute(rtnl.Route{Table: tunTable, Dst: netip.MustParsePrefix("0.0.0.0/0"), OIF: d.index})
	if err != nil {
		return err
	}
	if err := d.rt.Do(route); err != nil {
		return fmt.Errorf("capture: route table %d to %s: %w", tunTable, d.name, err)
	}

	// The raw socket that sends non-game packets on, marked so the chain
	// lets them pass.
	rfd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_RAW)
	if err != nil {
		return d.rightsOr(fmt.Errorf("capture: raw socket: %w", err), err)
	}
	if err := unix.SetsockoptInt(rfd, unix.SOL_SOCKET, unix.SO_MARK, tunBypassMark); err != nil {
		unix.Close(rfd)
		return d.rightsOr(fmt.Errorf("capture: mark the raw socket: %w", err), err)
	}
	d.raw = os.NewFile(uintptr(rfd), "raw")
	if d.rawc, err = d.raw.SyscallConn(); err != nil {
		return err
	}

	// Last: the rule that starts diverting. A rule a killed run left is
	// removed first, so there is exactly one.
	if err := deleteRules(d.rt); err != nil {
		return d.rightsOr(fmt.Errorf("capture: routing rule: %w", err), err)
	}
	if err := d.rt.Do(rtnl.AddRule(tunRule)); err != nil {
		return d.rightsOr(fmt.Errorf("capture: routing rule: %w", err), err)
	}
	d.mu.Lock()
	d.ruleOn = true
	d.mu.Unlock()
	return nil
}

// rightsOr reports a permission failure as the missing-rights error the UI
// explains, and anything else as err.
func (d *tunLink) rightsOr(err, cause error) error {
	if errors.Is(cause, syscall.EPERM) || errors.Is(cause, syscall.EACCES) {
		return tunRights(err)
	}
	return err
}

func writeSysctl(path, v string) error {
	return os.WriteFile("/proc/sys/"+path, []byte(v), 0o644)
}

func (d *tunLink) Recv(buf []byte, addr *divert.Address) (int, error) {
	n, err := d.file.Read(buf)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, os.ErrClosed) {
			return 0, divert.ErrClosed
		}
		return 0, err
	}
	*addr = divert.Address{}
	addr.SetOutbound(true)
	addr.SetIPv6(n > 0 && buf[0]>>4 == 6)
	return n, nil
}

func (d *tunLink) Send(pkt []byte, addr *divert.Address) (int, error) {
	if !addr.Outbound() {
		n, err := d.file.Write(pkt) // a reply: into the TUN, to the game's socket
		if errors.Is(err, os.ErrClosed) {
			return 0, divert.ErrClosed
		}
		return n, err
	}
	if len(pkt) < ipv4HeaderLen || pkt[0]>>4 != 4 {
		return 0, errNotIPv4 // the chain only marks IPv4
	}
	to := &unix.SockaddrInet4{Addr: [4]byte(pkt[16:20])}
	err := d.sendRaw(pkt, to)
	if errors.Is(err, unix.EMSGSIZE) && pkt[6]&0x40 == 0 {
		// Bigger than the route's MTU and fragmentable: what the kernel
		// would have done before the mark moved it, the raw socket won't.
		err = d.sendFragments(pkt, to)
	}
	if errors.Is(err, os.ErrClosed) {
		return 0, divert.ErrClosed
	}
	if err != nil {
		return 0, fmt.Errorf("capture: send on: %w", err)
	}
	return len(pkt), nil
}

func (d *tunLink) sendRaw(pkt []byte, to unix.Sockaddr) error {
	var serr error
	err := d.rawc.Write(func(fd uintptr) bool {
		serr = unix.Sendto(int(fd), pkt, 0, to)
		return serr != unix.EAGAIN
	})
	if err != nil {
		return err
	}
	return serr
}

// sendFragments sends pkt as IPv4 fragments, smaller ones if the route
// takes even less.
func (d *tunLink) sendFragments(pkt []byte, to unix.Sockaddr) error {
	var err error
	for _, size := range tunFragSizes {
		var frags [][]byte
		if frags, err = fragmentIPv4(pkt, size, uint16(rand.Uint32())); err != nil {
			return err
		}
		for i, f := range frags {
			if err = d.sendRaw(f, to); err != nil {
				if i == 0 && errors.Is(err, unix.EMSGSIZE) {
					break // try the next size
				}
				return err
			}
		}
		if err == nil {
			return nil
		}
	}
	return err
}

// CalcChecksums has nothing to do: appendUDP4 fills in both checksums, and
// the kernel takes the packet as written.
func (d *tunLink) CalcChecksums([]byte, *divert.Address) error { return nil }

// ShutdownRecv stops diverting (the rule goes first), then lets the
// receivers read what is already queued for drainWait.
func (d *tunLink) ShutdownRecv() error {
	var err error
	d.shutOnce.Do(func() {
		err = d.removeRule()
		d.file.SetReadDeadline(time.Now().Add(drainWait))
	})
	return err
}

// keepRoute (every second, from Capture.maintain) puts back the table and
// the rule when another program removed them; without either, the game's
// packets silently take the normal route. Two common causes:
//
//   - systemd-networkd deletes every rule its .network files don't declare
//     whenever it configures a link (ManageForeignRoutingPolicyRules=, on
//     by default since systemd 249), after a carrier change, a resume or a
//     `networkctl reload`; VPN scripts that flush rules do the same.
//   - A firewall reload's `nft flush ruleset` deletes the table on kernels
//     before 5.12. An owned table is skipped by the flush and only its own
//     socket may delete it, so there the check finds it every time.
//
// A table made again under the same name (by another program after a
// flush) has another handle, so it doesn't pass for this one; kernels
// before 4.16 give no handle, and there the name is all that is checked.
// One dump of the ip tables and one of the IPv4 rules: both cheap.
func (d *tunLink) keepRoute() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.ruleOn || !d.tableOn {
		return nil, nil // stopping; nothing comes back now
	}
	var fixed []string
	handle, ok, err := d.findTable()
	if err != nil {
		return nil, fmt.Errorf("capture: list the nftables tables: %w", err)
	}
	if ok && d.handle == 0 {
		d.handle = handle // the lookup at Start failed, or a kernel before 4.16
	}
	if !ok || handle != d.handle {
		// Gone, or a table of that name that isn't this one (made after a
		// flush). Made again as at Start, with the direct ports it held;
		// that fails when another program's socket owns the name.
		r := d.route
		r.direct = slices.Sorted(maps.Keys(d.direct))
		if err := d.nf.Do(tunRuleset(r, d.owned)...); err != nil {
			return nil, &routeLost{part: "nftables table", err: err}
		}
		d.handle, _, _ = d.findTable()
		fixed = append(fixed, "nftables table")
	}
	rules, err := d.rt.Dump(rtnl.ListRules())
	if err != nil {
		return fixed, fmt.Errorf("capture: list the routing rules: %w", err)
	}
	if !slices.ContainsFunc(rules, isTUNRule) {
		// The device, its route and table tunTable are still there: this is
		// the same last step as at Start.
		if err := d.rt.Do(rtnl.AddRule(tunRule)); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fixed, &routeLost{part: "routing rule", err: err}
		}
		fixed = append(fixed, "routing rule")
	}
	return fixed, nil
}

// findTable looks up the nftables table's handle; ok is false when there
// is no table of that name.
func (d *tunLink) findTable() (handle uint64, ok bool, err error) {
	tables, err := d.nf.Dump(nft.ListTables(nft.IPv4))
	if err != nil {
		return 0, false, err
	}
	for _, r := range tables {
		if t, _ := nft.ParseTable(r); t.Name == tunNftTable {
			return t.Handle, true, nil
		}
	}
	return 0, false, nil
}

func isTUNRule(r netlink.Reply) bool {
	rule, ok := rtnl.ParseRule(r)
	return ok && rule == tunRule
}

func (d *tunLink) removeRule() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.ruleOn {
		return nil
	}
	d.ruleOn = false
	return deleteRules(d.rt)
}

// Close undoes everything, the rule first, and keeps going past failures:
// the device and the table's socket close regardless, and with them the
// route and (Linux 5.12+) the table.
func (d *tunLink) Close() error {
	d.closeOnce.Do(func() {
		var errs []error
		if d.rt != nil {
			errs = append(errs, d.removeRule())
		}
		if d.nf != nil {
			d.mu.Lock()
			if d.tableOn {
				if err := d.nf.Do(nft.Batch(nft.DelTable(nft.IPv4, tunNftTable))...); err != nil && !errors.Is(err, syscall.ENOENT) {
					errs = append(errs, err)
				}
				d.tableOn = false
			}
			d.mu.Unlock()
			d.nf.Close()
		}
		if d.raw != nil {
			d.raw.Close()
		}
		if d.file != nil {
			errs = append(errs, d.file.Close())
		}
		if d.rt != nil {
			d.rt.Close()
		}
		d.closeErr = errors.Join(errs...)
	})
	return d.closeErr
}

func (d *tunLink) setDirect(ports []uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.tableOn {
		return nil
	}
	msgs := tunDirectUpdate(d.direct, ports)
	if msgs == nil {
		return nil
	}
	if err := d.nf.Do(msgs...); err != nil {
		return fmt.Errorf("capture: update the direct ports: %w", err)
	}
	clear(d.direct)
	for _, p := range ports {
		d.direct[p] = true
	}
	return nil
}
