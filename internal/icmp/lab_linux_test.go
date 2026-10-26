package icmp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"lowpath/internal/netlink"
	"lowpath/internal/netlink/rtnl"
)

// TTL-limited echo through real Linux routers, as root, without touching
// this machine's network: the test runs itself again in a new network
// namespace (the client), which starts a chain of three more joined by
// veth pairs, with documentation addresses only:
//
//	client .2 ── .1 router1 .17 ── .18 router2 .33 ── .34 destination
//	       192.0.2.0/28     192.0.2.16/28      192.0.2.32/28
//	       2001:db8:0:1::/64  2001:db8:0:2::/64  2001:db8:0:3::/64
//
// The routers forward, answer TTL 1 with "time exceeded" and have no route
// to 203.0.113.0/24 or 2001:db8:ffff::/48, so a probe there gets
// "unreachable" from router1. The client probes as root (raw socket), then as nobody
// with only net.ipv4.ping_group_range open (ping socket and its error
// queue), then as nobody with it closed. All namespaces vanish with their
// processes, so nothing is left behind even when the test fails.
//
//	sudo go test -run TestPingTTLRouteLab -v ./internal/icmp

const (
	labRoleEnv = "LOWPATH_ICMP_LAB"
	labReady   = "lowpath-icmp-lab-ready"
)

// labNode is one namespace: its addresses on the link toward the client
// (prev) and toward the destination (next), and its routes.
type labNode struct {
	name       string
	prev, next []string
	routes     [][2]string // destination, gateway
	router     bool
}

var labNodes = []labNode{
	{name: "client", next: []string{"192.0.2.2/28", "2001:db8:0:1::2/64"},
		routes: [][2]string{{"0.0.0.0/0", "192.0.2.1"}, {"::/0", "2001:db8:0:1::1"}}},
	{name: "router1", router: true,
		prev:   []string{"192.0.2.1/28", "2001:db8:0:1::1/64"},
		next:   []string{"192.0.2.17/28", "2001:db8:0:2::1/64"},
		routes: [][2]string{{"192.0.2.32/28", "192.0.2.18"}, {"2001:db8:0:3::/64", "2001:db8:0:2::2"}}},
	{name: "router2", router: true,
		prev:   []string{"192.0.2.18/28", "2001:db8:0:2::2/64"},
		next:   []string{"192.0.2.33/28", "2001:db8:0:3::1/64"},
		routes: [][2]string{{"192.0.2.0/28", "192.0.2.17"}, {"2001:db8:0:1::/64", "2001:db8:0:2::1"}}},
	{name: "destination", prev: []string{"192.0.2.34/28", "2001:db8:0:3::2/64"},
		routes: [][2]string{{"0.0.0.0/0", "192.0.2.33"}, {"::/0", "2001:db8:0:3::1"}}},
}

func TestPingTTLRouteLab(t *testing.T) {
	switch role := os.Getenv(labRoleEnv); {
	case role == "client":
		labClient(t)
		return
	case role == "probe":
		labProbe(t)
		return
	case role == "closed":
		labClosed(t)
		return
	case strings.HasPrefix(role, "node"):
		i, _ := strconv.Atoi(strings.TrimPrefix(role, "node"))
		labNodeRun(t, i)
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root (it runs in network namespaces of its own and changes nothing on this machine)")
	}
	cmd := labRole("client", true)
	labRun(t, cmd)
}

// labRole is this test binary in another role, in a new network namespace
// when newNet.
func labRole(role string, newNet bool) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestPingTTLRouteLab$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), labRoleEnv+"="+role)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if newNet {
		cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWNET
	}
	return cmd
}

// labRun runs cmd to the end, logging its output.
func labRun(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		t.Log("  | " + l)
	}
	if err != nil {
		t.Fatalf("%s: %v", cmd.Env[len(cmd.Env)-1], err)
	}
}

// labNodeRun sets up node i of the chain, starting the next one, and when
// i > 0 says it's ready and stays until killed.
func labNodeRun(t *testing.T, i int) {
	n := labNodes[i]
	rt, err := netlink.Dial(netlink.Route)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	labDo(t, rt, rtnl.LinkUp(labLink(t, "lo"), 0), nil)
	if n.router {
		for _, f := range []string{"ipv4/ip_forward", "ipv6/conf/all/forwarding", "ipv4/icmp_ratelimit", "ipv6/icmp/ratelimit"} {
			v := "1"
			if strings.HasSuffix(f, "ratelimit") {
				v = "0" // every probe gets its answer
			}
			labSysctl(t, f, v)
		}
	}
	if i > 0 {
		labAddrs(t, rt, labLink(t, "prev"), n.prev) // made by the previous node
	}
	if i < len(labNodes)-1 {
		next := labRole("node"+strconv.Itoa(i+1), true)
		out, _ := next.StdoutPipe()
		next.Stderr = next.Stdout
		stay, _ := next.StdinPipe() // the next node runs until this closes
		if err := next.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stay.Close()
			next.Process.Kill()
		})
		labDo(t, rt, rtnl.NewVeth("next", "prev", next.Process.Pid), nil)
		labAddrs(t, rt, labLink(t, "next"), n.next)
		labWait(t, out, labNodes[i+1].name)
	}
	for _, r := range n.routes {
		m, err := labRoute(netip.MustParsePrefix(r[0]), netip.MustParseAddr(r[1]))
		labDo(t, rt, m, err)
	}
	if i == 0 {
		return
	}
	fmt.Println(labReady)
	// Stay until the previous node ends (its end of stdin closes); a
	// goroutine parked in select{} would end the program as a deadlock.
	io.Copy(io.Discard, os.Stdin)
}

// labClient builds the chain and runs the probes: as root here, then as
// nobody with the ping socket allowed, then with it refused.
func labClient(t *testing.T) {
	labNodeRun(t, 0)
	t.Log("as root (raw socket):")
	labProbes(t)

	labSysctl(t, "ipv4/ping_group_range", "65534 65534")
	labRun(t, asNobody(labRole("probe", false)))
	labSysctl(t, "ipv4/ping_group_range", "1 0")
	labRun(t, asNobody(labRole("closed", false)))
}

func asNobody(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 65534, Gid: 65534}
	return cmd
}

// labProbe runs as nobody, where only the ping socket works.
func labProbe(t *testing.T) {
	if _, err := (rawSocket{}).sendTTL(netip.MustParseAddr("192.0.2.34"), 1, time.Second); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("nobody opened a raw socket (%v): this run wouldn't test the ping socket", err)
	}
	t.Log("as nobody (ping socket and its error queue):")
	labProbes(t)
}

// labClosed runs as nobody with net.ipv4.ping_group_range closed, so no
// socket works and the error says what the system needs.
func labClosed(t *testing.T) {
	for _, dst := range []string{"192.0.2.34", "2001:db8:0:3::2"} {
		_, err := PingTTL(netip.MustParseAddr(dst), 1, time.Second)
		if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "ping_group_range") {
			t.Errorf("%s: %v, want a permission error that names ping_group_range", dst, err)
		}
		t.Logf("as nobody, ping sockets refused: %s: %v", dst, err)
	}
}

// labProbes checks every hop over IPv4 and IPv6, then many probes at once.
func labProbes(t *testing.T) {
	for _, fam := range []struct {
		dst, unreachable string
		hops             [][]string // per TTL, the node's addresses
	}{
		{"192.0.2.34", "203.0.113.9", [][]string{{"192.0.2.1", "192.0.2.17"}, {"192.0.2.18", "192.0.2.33"}, {"192.0.2.34"}, {"192.0.2.34"}}},
		{"2001:db8:0:3::2", "2001:db8:ffff::9", [][]string{{"2001:db8:0:1::1", "2001:db8:0:2::1"}, {"2001:db8:0:2::2", "2001:db8:0:3::1"}, {"2001:db8:0:3::2"}, {"2001:db8:0:3::2"}}},
	} {
		dst := netip.MustParseAddr(fam.dst)
		check := func(ttl int, r Reply, err error) error {
			want := TimeExceeded
			if ttl >= 3 {
				want = Echo
			}
			if err != nil {
				return fmt.Errorf("TTL %d: %w", ttl, err)
			}
			// The first probe over a link may wait a second for neighbour
			// discovery, so the bound on the RTT is the probe's timeout.
			if r.Kind != want || !slices.Contains(fam.hops[ttl-1], r.From.String()) || r.RTT <= 0 || r.RTT > 2*time.Second {
				return fmt.Errorf("TTL %d: %v from %v in %v, want %v from %v", ttl, r.Kind, r.From, r.RTT, want, fam.hops[ttl-1])
			}
			return nil
		}
		for ttl := 1; ttl <= len(fam.hops); ttl++ {
			r, err := PingTTL(dst, ttl, 2*time.Second)
			if err := check(ttl, r, err); err != nil {
				t.Fatal(err)
			}
			t.Logf("  %-16s TTL %d  %-14s from %-16s %v", fam.dst, ttl, r.Kind, r.From, r.RTT.Round(time.Microsecond))
		}
		// A Linux router without a route answers a sender at most once a
		// second, and not the first time (net.ipv4.route.error_cost and
		// error_burst, which a namespace can't change): ask, wait, ask again.
		unreachable := netip.MustParseAddr(fam.unreachable)
		PingTTL(unreachable, 5, 100*time.Millisecond)
		time.Sleep(1200 * time.Millisecond)
		r, err := PingTTL(unreachable, 5, 2*time.Second)
		if err != nil || r.Kind != Unreachable || !slices.Contains(fam.hops[0], r.From.String()) {
			t.Fatalf("%s: %+v, %v; want unreachable from router1", fam.unreachable, r, err)
		}
		t.Logf("  %-16s TTL 5  %-14s from %-16s %v", fam.unreachable, r.Kind, r.From, r.RTT.Round(time.Microsecond))

		// A route trace has a probe per hop in flight at once; each must
		// get its own answer.
		var wg sync.WaitGroup
		errs := make(chan error, 3*8)
		for ttl := 1; ttl <= 3; ttl++ {
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, err := PingTTL(dst, ttl, 2*time.Second)
					if err := check(ttl, r, err); err != nil {
						errs <- err
					}
				}()
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("%s, 24 probes at once: %v", fam.dst, err)
		}
	}
}

func labSysctl(t *testing.T, name, value string) {
	t.Helper()
	if err := os.WriteFile("/proc/sys/net/"+name, []byte(value), 0); err != nil {
		t.Fatal(err)
	}
}

func labDo(t *testing.T, rt *netlink.Conn, m netlink.Message, err error) {
	t.Helper()
	if err == nil {
		err = rt.Do(m)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func labLink(t *testing.T, name string) int {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if i, err := net.InterfaceByName(name); err == nil {
			return i.Index
		} else if time.Now().After(end) {
			t.Fatalf("no interface %s: %v", name, err)
		}
	}
}

func labAddrs(t *testing.T, rt *netlink.Conn, idx int, addrs []string) {
	t.Helper()
	for _, a := range addrs {
		m, err := labAddr(idx, netip.MustParsePrefix(a))
		labDo(t, rt, m, err)
	}
	labDo(t, rt, rtnl.LinkUp(idx, 0), nil)
}

// labWait reads the next node's output until it is ready.
func labWait(t *testing.T, r io.Reader, name string) {
	t.Helper()
	sc := bufio.NewScanner(r)
	ready := make(chan bool, 1)
	go func() {
		for sc.Scan() {
			if sc.Text() == labReady {
				ready <- true
				break
			}
			fmt.Println(name + " | " + sc.Text())
		}
		ready <- false
		for sc.Scan() {
			fmt.Println(name + " | " + sc.Text())
		}
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatalf("%s ended before it was ready", name)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s isn't ready", name)
	}
}

// rtnl builds IPv4 requests only; the lab needs IPv6 too.
const (
	labAFInet6   = 10
	labNewAddr   = 20
	labNewRoute  = 24
	labIfaNoDAD  = 0x02 // IFA_F_NODAD: usable at once, no duplicate detection
	labRtaDst    = 1
	labRtaGW     = 5
	labTableMain = 254
)

func labAddr(idx int, p netip.Prefix) (netlink.Message, error) {
	if p.Addr().Is4() {
		return rtnl.AddAddr(idx, p, rtnl.ScopeUniverse)
	}
	b := []byte{labAFInet6, byte(p.Bits()), labIfaNoDAD, rtnl.ScopeUniverse}
	b = append(b, byte(idx), byte(idx>>8), byte(idx>>16), byte(idx>>24))
	var a netlink.Attrs
	a.Add(2, p.Addr().AsSlice()) // IFA_LOCAL
	a.Add(1, p.Addr().AsSlice()) // IFA_ADDRESS
	return netlink.Message{Type: labNewAddr, Flags: netlink.FlagAck | netlink.FlagCreate | netlink.FlagExcl, Op: "add an IPv6 address", Data: append(b, a.Bytes()...)}, nil
}

func labRoute(dst netip.Prefix, gw netip.Addr) (netlink.Message, error) {
	if dst.Addr().Is4() {
		return rtnl.AddRoute(rtnl.Route{Table: labTableMain, Dst: dst, Gateway: gw})
	}
	// struct rtmsg: family, dst_len, src_len, tos, table, protocol
	// (static), scope (universe), type (unicast), flags.
	b := []byte{labAFInet6, byte(dst.Bits()), 0, 0, labTableMain, 4, 0, 1, 0, 0, 0, 0}
	var a netlink.Attrs
	if dst.Bits() > 0 {
		a.Add(labRtaDst, dst.Masked().Addr().AsSlice())
	}
	a.Add(labRtaGW, gw.AsSlice())
	return netlink.Message{Type: labNewRoute, Flags: netlink.FlagAck | netlink.FlagCreate | netlink.FlagReplace, Op: "add an IPv6 route", Data: append(b, a.Bytes()...)}, nil
}
