package diag

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"time"
)

// SysfsNetStats is the Linux NetStats: the default route from
// /proc/net/route (then /proc/net/ipv6_route), counters from
// /sys/class/net/<if>/statistics. Everything is world-readable, so no root
// is needed. It reads through an fs.FS rooted at "/" so tests can feed it
// fixtures on any OS.
type SysfsNetStats struct {
	fsys fs.FS
	now  func() time.Time
}

// NewSysfsNetStats reads from fsys, which must be rooted at "/" (on Linux:
// os.DirFS("/")).
func NewSysfsNetStats(fsys fs.FS, now func() time.Time) *SysfsNetStats {
	if now == nil {
		now = time.Now
	}
	return &SysfsNetStats{fsys: fsys, now: now}
}

// procRoute is one default route from /proc/net/route or ipv6_route.
type procRoute struct {
	iface   string
	gateway netip.Addr
	metric  uint64
}

// parseProcRoute returns the IPv4 default routes (destination and mask 0)
// that are up, from /proc/net/route. Addresses there are hex in host byte
// order, which is little-endian on every architecture Lowpath builds for.
func parseProcRoute(r io.Reader) []procRoute {
	var out []procRoute
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first { // header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&0x1 == 0 { // RTF_UP
			continue
		}
		pr := procRoute{iface: f[0]}
		if b, err := hex.DecodeString(f[2]); err == nil && len(b) == 4 {
			var a [4]byte
			binary.BigEndian.PutUint32(a[:], binary.LittleEndian.Uint32(b))
			if gw := netip.AddrFrom4(a); !gw.IsUnspecified() {
				pr.gateway = gw
			}
		}
		pr.metric, _ = strconv.ParseUint(f[6], 10, 64)
		out = append(out, pr)
	}
	return out
}

// parseIPv6Route returns the ::/0 routes from /proc/net/ipv6_route
// (fields: dst, dst len, src, src len, next hop, metric, refcnt, use,
// flags, device; all hex).
func parseIPv6Route(r io.Reader) []procRoute {
	var out []procRoute
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[1] != "00" || strings.Trim(f[0], "0") != "" || f[9] == "lo" {
			continue
		}
		flags, err := strconv.ParseUint(f[8], 16, 32)
		if err != nil || flags&0x1 == 0 || flags&0x200 != 0 { // RTF_UP, RTF_REJECT
			continue
		}
		pr := procRoute{iface: f[9]}
		if b, err := hex.DecodeString(f[4]); err == nil && len(b) == 16 {
			if gw := netip.AddrFrom16([16]byte(b)); !gw.IsUnspecified() {
				pr.gateway = gw
			}
		}
		pr.metric, _ = strconv.ParseUint(f[5], 16, 64)
		out = append(out, pr)
	}
	return out
}

func bestRoute(rs []procRoute) (procRoute, bool) {
	if len(rs) == 0 {
		return procRoute{}, false
	}
	best := rs[0]
	for _, r := range rs[1:] {
		if r.metric < best.metric {
			best = r
		}
	}
	return best, true
}

func (s *SysfsNetStats) readString(name string) (string, error) {
	b, err := fs.ReadFile(s.fsys, name)
	return strings.TrimSpace(string(b)), err
}

func (s *SysfsNetStats) readUint(name string) (uint64, error) {
	v, err := s.readString(name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(v, 10, 64)
}

func (s *SysfsNetStats) routes(name string, parse func(io.Reader) []procRoute) []procRoute {
	f, err := s.fsys.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	return parse(f)
}

func (s *SysfsNetStats) DefaultRoute() (Route, error) {
	r, ok := bestRoute(s.routes("proc/net/route", parseProcRoute))
	if !ok {
		if r, ok = bestRoute(s.routes("proc/net/ipv6_route", parseIPv6Route)); !ok {
			return Route{}, ErrNoRoute
		}
	}
	dir := path.Join("sys/class/net", r.iface)
	idx, err := s.readUint(path.Join(dir, "ifindex"))
	if err != nil {
		return Route{}, fmt.Errorf("diag: interface %s: %w", r.iface, err)
	}
	out := Route{Index: int(idx), Name: r.iface, Gateway: r.gateway, Kind: LinkOther}
	if _, err := fs.Stat(s.fsys, path.Join(dir, "wireless")); err == nil {
		out.Kind = LinkWiFi
	} else if t, err := s.readString(path.Join(dir, "type")); err == nil && t == "1" { // ARPHRD_ETHER
		out.Kind = LinkEthernet
	}
	return out, nil
}

// ifaceName finds the interface with this index under /sys/class/net.
func (s *SysfsNetStats) ifaceName(index int) (string, error) {
	ents, err := fs.ReadDir(s.fsys, "sys/class/net")
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if v, err := s.readUint(path.Join("sys/class/net", e.Name(), "ifindex")); err == nil && int(v) == index {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("diag: no interface with index %d", index)
}

func (s *SysfsNetStats) Counters(index int) (Counters, error) {
	name, err := s.ifaceName(index)
	if err != nil {
		return Counters{}, err
	}
	dir := path.Join("sys/class/net", name)
	rx, err := s.readUint(path.Join(dir, "statistics/rx_bytes"))
	if err != nil {
		return Counters{}, err
	}
	tx, err := s.readUint(path.Join(dir, "statistics/tx_bytes"))
	if err != nil {
		return Counters{}, err
	}
	c := Counters{At: s.now(), RxBytes: rx, TxBytes: tx}
	// "speed" is in Mbit/s; Wi-Fi and virtual links fail to read it or say -1.
	if v, err := s.readString(path.Join(dir, "speed")); err == nil {
		if mbps, err := strconv.ParseUint(v, 10, 64); err == nil && mbps > 0 {
			c.RxLinkBps, c.TxLinkBps = mbps*1e6, mbps*1e6
		}
	}
	return c, nil
}
