package capture

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"lowpath/internal/engine"
	"lowpath/internal/netlink"
	"lowpath/internal/netlink/nft"
	"lowpath/internal/netlink/rtnl"
	"lowpath/internal/owner"
	"lowpath/internal/relay"
)

// The TUN backend for real, as root, without touching this machine's
// network: the test runs itself again in a new network namespace (the
// "client"), which starts a second one (the "server") joined to it by a veth
// pair, with documentation addresses only:
//
//	client 203.0.113.2/24 (lpa) ── veth ── (lpb) 203.0.113.1/24, 198.51.100.7/24 server
//
// The server answers every UDP datagram on port 3478 with
// "echo:<source it saw>:<payload>". Both namespaces vanish with their
// processes, so nothing is left behind even when the test fails.

const (
	tunRoleEnv    = "LOWPATH_TUN_ROLE"
	tunPortEnv    = "LOWPATH_TUN_GAME_PORT"
	tunUnownedEnv = "LOWPATH_TUN_UNOWNED"
	serverAddr    = "198.51.100.7:3478"
	clientIP      = "203.0.113.2"
	serverLinkIP  = "203.0.113.1"
	relayAddr     = "203.0.113.1:7000"
	lpcheckEnv    = "LOWPATH_LPCHECK"
	readyLine     = "lowpath-tun-ready"
	capturingLine = "lowpath-tun-capturing"
)

func TestTUNRouting(t *testing.T) {
	switch os.Getenv(tunRoleEnv) {
	case "client":
		tunClient(t)
		return
	case "server":
		tunServer(t)
		return
	case "capturer":
		tunCapturer(t)
		return
	case "norights":
		tunNoRights(t)
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root (it runs in network namespaces of its own and changes nothing on this machine)")
	}
	cmd := tunRole(t, "client", true, nil)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		t.Log(sc.Text())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("client namespace: %v", err)
	}
}

// tunRole is this test binary in another role, in a new network namespace
// when newNet.
func tunRole(t *testing.T, role string, newNet bool, env []string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTUNRouting$", "-test.v", "-test.count=1")
	cmd.Env = append(append(os.Environ(), tunRoleEnv+"="+role), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if newNet {
		cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWNET
	}
	return cmd
}

// asNobody runs cmd as an ordinary user (nobody) with the given
// capabilities: what `setcap cap_net_admin,cap_net_raw+ep` gives a program.
func asNobody(cmd *exec.Cmd, caps ...uintptr) *exec.Cmd {
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 65534, Gid: 65534}
	cmd.SysProcAttr.AmbientCaps = caps
	return cmd
}

// waitLine reads r until want (other lines are logged), or fails.
func waitLine(t *testing.T, r io.Reader, want string) *bufio.Scanner {
	t.Helper()
	sc := bufio.NewScanner(r)
	got := make(chan bool, 1)
	go func() {
		for sc.Scan() {
			if strings.Contains(sc.Text(), want) {
				got <- true
				return
			}
			t.Log("  | " + sc.Text())
		}
		got <- false
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("helper ended before %q", want)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no %q from the helper", want)
	}
	return sc
}

func rtDo(t *testing.T, rt *netlink.Conn, m netlink.Message, err error) {
	t.Helper()
	if err == nil {
		err = rt.Do(m)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func linkIndex(t *testing.T, name string) int {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if i, err := net.InterfaceByName(name); err == nil {
			return i.Index
		} else if time.Now().After(end) {
			t.Fatalf("no interface %s: %v", name, err)
		}
	}
}

func tunServer(t *testing.T) {
	rt, err := netlink.Dial(netlink.Route)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rtDo(t, rt, rtnl.LinkUp(linkIndex(t, "lo"), 0), nil)
	idx := linkIndex(t, "lpb") // the client moves it here
	m, err := rtnl.AddAddr(idx, netip.MustParsePrefix(serverLinkIP+"/24"), rtnl.ScopeUniverse)
	rtDo(t, rt, m, err)
	m, err = rtnl.AddAddr(idx, netip.MustParsePrefix("198.51.100.7/24"), rtnl.ScopeUniverse)
	rtDo(t, rt, m, err)
	rtDo(t, rt, rtnl.LinkUp(idx, 0), nil)
	c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort(serverAddr)))
	if err != nil {
		t.Fatal(err)
	}
	// The relay for the lpcheck check, on the link address.
	eg, err := relay.ListenEgressOpts(relayAddr, relay.Options{AllowPrivate: true}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	go eg.Serve(context.Background())
	fmt.Println(readyLine)
	buf := make([]byte, 2048)
	for {
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatal(err)
		}
		if reply, ok := stunReply(buf[:n], from); ok {
			c.WriteToUDPAddrPort(reply, from)
			continue
		}
		c.WriteToUDPAddrPort([]byte(fmt.Sprintf("echo:%s:%s", from, buf[:n])), from)
	}
}

// tunCapturer captures the client's game socket and swallows its packets,
// until it is killed.
func tunCapturer(t *testing.T) {
	res := newFakeOS()
	res.socks = nil
	for _, p := range strings.Split(os.Getenv(tunPortEnv), ",") {
		port, _ := strconv.Atoi(p)
		res.socks = append(res.socks, socket{Local: netip.AddrPortFrom(netip.IPv4Unspecified(), uint16(port)), PID: pidGame})
	}
	res.procs = map[uint32]string{pidGame: "game"}
	b := NewTUN(res)
	if os.Getenv(tunUnownedEnv) == "1" {
		b.sys = &linuxTUN{status: "/proc/self/status", dev: "/dev/net/tun", unowned: true}
	}
	s, err := b.Start(Config{Processes: []string{"game"}, OnPacket: func(_, _ netip.AddrPort, _ []byte) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fmt.Println(capturingLine)
	select {}
}

// tunClient sets up the two namespaces and runs the checks.
func tunClient(t *testing.T) {
	rt, err := netlink.Dial(netlink.Route)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rtDo(t, rt, rtnl.LinkUp(linkIndex(t, "lo"), 0), nil)

	srv := tunRole(t, "server", true, nil)
	out, _ := srv.StdoutPipe()
	srv.Stderr = srv.Stdout
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Process.Kill()
	rtDo(t, rt, rtnl.NewVeth("lpa", "lpb", srv.Process.Pid), nil)
	idx := linkIndex(t, "lpa")
	m, err := rtnl.AddAddr(idx, netip.MustParsePrefix(clientIP+"/24"), rtnl.ScopeUniverse)
	rtDo(t, rt, m, err)
	rtDo(t, rt, rtnl.LinkUp(idx, 0), nil)
	sc := waitLine(t, out, readyLine)
	go func() {
		for sc.Scan() {
			t.Log("server | " + sc.Text())
		}
	}()
	m, err = rtnl.AddRoute(rtnl.Route{Table: 254, Dst: netip.MustParsePrefix("0.0.0.0/0"), Gateway: netip.MustParseAddr(serverLinkIP), OIF: idx})
	rtDo(t, rt, m, err)

	t.Run("RouteAndRestore", tunRouteAndRestore)
	t.Run("RuleRemoved", tunRuleRemoved)
	t.Run("RuleRemovedWithCapabilities", tunRuleRemovedWithCapabilities)
	t.Run("RulesetFlushed", tunRulesetFlushed)
	t.Run("RulesetFlushedOldKernel", tunRulesetFlushedOldKernel)
	t.Run("StuckHandler", tunStuckHandler)
	t.Run("Killed", tunKilled)
	t.Run("KilledWithCapabilities", tunKilledWithCapabilities)
	t.Run("KilledOldKernel", tunKilledOldKernel)
	t.Run("NoRights", tunNoRightsCheck)
	t.Run("Lpcheck", tunLpcheck)
}

// peer is a UDP socket of a program on the client.
type peer struct {
	c      *net.UDPConn
	mu     sync.Mutex
	got    []string
	server netip.AddrPort
}

func newPeer(t *testing.T) *peer { return newPeerAs(t, 0) }

// newPeerAs makes the socket as user uid: the kernel takes a socket's owner
// from the file-system uid of the thread that creates it.
func newPeerAs(t *testing.T, uid int) *peer {
	t.Helper()
	runtime.LockOSThread()
	if uid != 0 {
		unix.Setfsuid(uid)
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if uid != 0 {
		unix.Setfsuid(0)
	}
	runtime.UnlockOSThread()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	p := &peer{c: c, server: netip.MustParseAddrPort(serverAddr)}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			p.mu.Lock()
			p.got = append(p.got, from.String()+" "+string(buf[:n]))
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *peer) port() uint16 { return uint16(p.c.LocalAddr().(*net.UDPAddr).Port) }

func (p *peer) send(t *testing.T, payload string) {
	t.Helper()
	if _, err := p.c.WriteToUDPAddrPort([]byte(payload), p.server); err != nil {
		t.Fatal(err)
	}
}

// await waits for a reply equal to want (from the server address).
func (p *peer) await(want string, within time.Duration) bool {
	full := serverAddr + " " + want
	return p.awaitMatch(func(r string) bool { return r == full }, within)
}

// awaitPrefix waits for a reply that starts with want (the read buffer cuts
// long ones short).
func (p *peer) awaitPrefix(want string, within time.Duration) bool {
	full := serverAddr + " " + want
	return p.awaitMatch(func(r string) bool { return strings.HasPrefix(r, full) }, within)
}

func (p *peer) awaitMatch(ok func(string) bool, within time.Duration) bool {
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		p.mu.Lock()
		found := slices.ContainsFunc(p.got, ok)
		p.mu.Unlock()
		if found {
			return true
		}
	}
	return false
}

func (p *peer) replies() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

func direct(p *peer, payload string) string {
	return fmt.Sprintf("echo:%s:%d:%s", clientIP, p.port(), payload)
}

func tunRulesNow(t *testing.T) []rtnl.Rule {
	t.Helper()
	rt, err := netlink.Dial(netlink.Route)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	replies, err := rt.Dump(rtnl.ListRules())
	if err != nil {
		t.Fatal(err)
	}
	var out []rtnl.Rule
	for _, r := range replies {
		if rule, ok := rtnl.ParseRule(r); ok && rule == tunRule {
			out = append(out, rule)
		}
	}
	return out
}

func tunDevices(t *testing.T) []string {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range ifs {
		if strings.HasPrefix(i.Name, "lowpath") {
			out = append(out, i.Name)
		}
	}
	return out
}

// tableGone reports whether the nftables table is gone: deleting it says
// whether it was there. Only used where it must be gone anyway.
func tableGone(t *testing.T) bool {
	t.Helper()
	nf, err := netlink.Dial(netlink.Netfilter)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Close()
	err = nf.Do(nft.Batch(nft.DelTable(nft.IPv4, tunNftTable))...)
	return errors.Is(err, syscall.ENOENT)
}

// assertRestored checks that nothing of the backend is left and that the
// game socket's packets go straight to the server again.
func assertRestored(t *testing.T, game *peer, tag string) {
	t.Helper()
	if r := tunRulesNow(t); len(r) != 0 {
		t.Errorf("%s: routing rule still there: %+v", tag, r)
	}
	if d := tunDevices(t); len(d) != 0 {
		t.Errorf("%s: TUN device still there: %v", tag, d)
	}
	if !tableGone(t) {
		t.Errorf("%s: nftables table %q was still there", tag, tunNftTable)
	}
	game.send(t, tag)
	if !game.await(direct(game, tag), time.Second) {
		t.Errorf("%s: the game's packet didn't reach the server directly: %v", tag, game.replies())
	}
}

func tunRouteAndRestore(t *testing.T) {
	game, other, stranger := newPeer(t), newPeer(t), newPeer(t)
	res := newFakeOS()
	res.socks = []socket{
		{Local: netip.AddrPortFrom(netip.IPv4Unspecified(), game.port()), PID: pidGame},
		{Local: netip.AddrPortFrom(netip.IPv4Unspecified(), other.port()), PID: pidOther},
	}
	res.procs = map[uint32]string{pidGame: "game", pidOther: "browser"}
	b := NewTUN(res)
	if err := b.Available(); err != nil {
		t.Fatalf("Available as root: %v", err)
	}

	var sess atomic.Pointer[Capture]
	var captured atomic.Int64
	s, err := b.Start(Config{
		Processes: []string{"game.exe"}, // a Linux program matches its Windows name
		Exclude:   []netip.AddrPort{netip.MustParseAddrPort("192.0.2.10:7000")},
		OnPacket: func(local, remote netip.AddrPort, payload []byte) bool {
			captured.Add(1)
			reply := "relay:" + string(payload)
			go func() {
				for sess.Load() == nil {
					time.Sleep(time.Millisecond)
				}
				if err := sess.Load().Inject(local, remote, []byte(reply)); err != nil {
					t.Errorf("Inject: %v", err)
				}
			}()
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := s.(*Capture)
	sess.Store(c)
	defer s.Close()
	if r := tunRulesNow(t); len(r) != 1 {
		t.Fatalf("routing rules while capturing: %+v, want exactly one", r)
	}
	if d := tunDevices(t); len(d) != 1 {
		t.Fatalf("TUN devices while capturing: %v", d)
	}

	// The game's packet is captured and the injected reply reaches its
	// socket from the server's address; the server never sees it.
	game.send(t, "g1")
	if !game.await("relay:g1", 2*time.Second) {
		t.Fatalf("no relayed reply on the game socket: %v (stats %+v)", game.replies(), c.Stats())
	}
	// Other programs keep their route: their packets reach the server from
	// this host's own address. A socket the table knew at the start is in
	// the kernel's fast path at once, so its packets never come up here.
	passed := func() uint64 { return c.Stats().Passed }
	p0 := passed()
	other.send(t, "o1")
	if !other.await(direct(other, "o1"), 2*time.Second) {
		t.Fatalf("another program's packet didn't go direct: %v (stats %+v)", other.replies(), c.Stats())
	}
	if p := passed(); p != p0 {
		t.Errorf("a known socket of another program went through user space: passed %d -> %d", p0, p)
	}
	// One the table doesn't know is checked here and sent on ...
	stranger.send(t, "s1")
	if !stranger.await(direct(stranger, "s1"), 2*time.Second) {
		t.Fatalf("an unknown socket's packet didn't go direct: %v (stats %+v)", stranger.replies(), c.Stats())
	}
	if p := passed(); p != p0+1 {
		t.Errorf("an unknown socket's packet: passed %d -> %d, want one through user space", p0, p)
	}
	for _, r := range game.replies() {
		if strings.Contains(r, "echo:") {
			t.Fatalf("the server saw a game packet: %q", r)
		}
	}
	// A datagram bigger than the link's MTU, which the kernel fragments: the
	// raw socket that sends it on won't, so the backend does.
	big := "big" + strings.Repeat("x", 3000)
	stranger.send(t, big)
	if !stranger.awaitPrefix(direct(stranger, "bigxxx"), 2*time.Second) {
		t.Fatalf("a datagram bigger than the MTU didn't get through: stats %+v", c.Stats())
	}
	// A second capture while this one runs: the table belongs to this one's
	// socket, which is no lack of rights, and this capture keeps working.
	if s2, err := NewTUN(res).Start(Config{Processes: []string{"game"}, OnPacket: func(_, _ netip.AddrPort, _ []byte) bool { return true }}); err == nil {
		s2.Close()
		t.Fatal("a second capture started while the first one runs")
	} else if errors.Is(err, ErrNeedAdmin) || !strings.Contains(err.Error(), "another running program") {
		t.Errorf("second capture: %v, want a table owned by another program", err)
	}
	game.send(t, "g-second")
	if !game.await("relay:g-second", 2*time.Second) {
		t.Fatalf("the first capture stopped working after a second one was refused: %v", game.replies())
	}
	// ... until the next refresh finds its owner and adds it to the fast path.
	res.add(socket{Local: netip.AddrPortFrom(netip.IPv4Unspecified(), stranger.port()), PID: pidOther + 1}, "chat")
	time.Sleep(2500 * time.Millisecond)
	p1 := passed()
	for i := range 5 {
		stranger.send(t, fmt.Sprint("fast", i))
	}
	if !stranger.await(direct(stranger, "fast4"), 2*time.Second) {
		t.Fatalf("fast path: %v", stranger.replies())
	}
	if p := passed(); p != p1 {
		t.Errorf("a socket the refresh found still went through user space: passed %d -> %d", p1, p)
	}
	game.send(t, "g2")
	if !game.await("relay:g2", 2*time.Second) {
		t.Fatalf("game after a refresh: %v", game.replies())
	}
	st := c.Stats()
	t.Logf("stats %+v, OnPacket calls %d", st, captured.Load())
	if st.Captured != 3 || st.Injected != 3 || st.Errors != 0 {
		t.Errorf("stats %+v, want 3 captured and injected, no errors", st)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed after Close")
	}
	assertRestored(t, game, "after-close")
}

func tunStuckHandler(t *testing.T) {
	game := newPeer(t)
	res := newFakeOS()
	res.socks = []socket{{Local: netip.AddrPortFrom(netip.IPv4Unspecified(), game.port()), PID: pidGame}}
	res.procs = map[uint32]string{pidGame: "game"}
	block := make(chan struct{})
	defer close(block)
	s, err := NewTUN(res).Start(Config{
		Processes: []string{"game"},
		OnPacket:  func(_, _ netip.AddrPort, _ []byte) bool { <-block; return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	game.send(t, "stuck")
	// The game keeps sending; what counts is how soon its packets reach the
	// server directly again.
	var back time.Duration
	for i := 0; back == 0 && time.Since(start) < 2*time.Second; i++ {
		tag := fmt.Sprint("p", i)
		game.send(t, tag)
		if game.await(direct(game, tag), 20*time.Millisecond) {
			back = time.Since(start)
		}
	}
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog didn't stop a capture whose handler is stuck")
	}
	t.Logf("game traffic direct %v after the stuck packet, capture done %v later: %v",
		back.Round(time.Millisecond), (time.Since(start) - back).Round(time.Millisecond), s.Err())
	if s.Err() == nil {
		t.Error("Err is nil after the watchdog stopped capturing")
	}
	if back == 0 || back > time.Second {
		t.Errorf("fail-open took %v, want under 1 s", back)
	}
	assertRestored(t, game, "after-stuck")
}

func tunKilled(t *testing.T) { tunKill(t, false, false) }

// tunKilledWithCapabilities is the same as an ordinary user with only
// CAP_NET_ADMIN and CAP_NET_RAW.
func tunKilledWithCapabilities(t *testing.T) { tunKill(t, true, false) }

// tunKilledOldKernel is the same with the table made as on kernels before
// 5.12, where it outlives the process: it must still divert nothing.
func tunKilledOldKernel(t *testing.T) { tunKill(t, false, true) }

// tableExists asks the kernel for the nftables table.
func tableExists(t *testing.T) bool {
	t.Helper()
	nf, err := netlink.Dial(netlink.Netfilter)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Close()
	tables, err := nf.Dump(nft.ListTables(nft.IPv4))
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(tables, func(r netlink.Reply) bool {
		tbl, _ := nft.ParseTable(r)
		return tbl.Name == tunNftTable
	})
}

func tunKill(t *testing.T, nobody, unowned bool) {
	game := newPeer(t)
	var bystander *peer
	env := []string{fmt.Sprintf("%s=%d", tunPortEnv, game.port())}
	if nobody {
		// Without root, the capture can only ever find the owners of its
		// own user's sockets, so it marks no one else's: a root socket goes
		// direct even listed as the game.
		game, bystander = newPeerAs(t, 65534), newPeer(t)
		env = []string{fmt.Sprintf("%s=%d,%d", tunPortEnv, game.port(), bystander.port())}
	}
	if unowned {
		env = append(env, tunUnownedEnv+"=1")
	}
	cmd := tunRole(t, "capturer", false, env)
	if nobody {
		asNobody(cmd, capNetAdmin, capNetRaw)
	}
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	waitLine(t, out, capturingLine)
	go io.Copy(io.Discard, out)

	game.send(t, "swallowed")
	if game.await(direct(game, "swallowed"), 300*time.Millisecond) {
		t.Fatal("the game's packet reached the server while another process captured it")
	}
	if bystander != nil {
		bystander.send(t, "root")
		if !bystander.await(direct(bystander, "root"), time.Second) {
			t.Fatal("a capture without root took another user's packet")
		}
	}
	// SIGKILL: no Close, no deferred cleanup.
	cmd.Process.Kill()
	killed := time.Now()
	cmd.Wait()
	var back time.Duration
	for i := 0; back == 0 && time.Since(killed) < 2*time.Second; i++ {
		tag := fmt.Sprint("k", i)
		game.send(t, tag)
		if game.await(direct(game, tag), 50*time.Millisecond) {
			back = time.Since(killed)
		}
	}
	if back == 0 {
		t.Fatalf("the game's packets never went direct after the capturing process was killed: %v", game.replies())
	}
	t.Logf("game traffic direct %v after kill -9", back.Round(time.Millisecond))
	if back > time.Second {
		t.Errorf("took %v, want under 1 s", back)
	}
	// The rule outlives the process but diverts nothing; Recover removes it.
	if r := tunRulesNow(t); len(r) != 1 {
		t.Errorf("rules after the kill: %+v, want the one left behind", r)
	}
	if d := tunDevices(t); len(d) != 0 {
		t.Errorf("TUN device outlived its process: %v", d)
	}
	// An owned table goes with the process's netlink socket; an unowned one
	// stays (and marks packets that the rule's empty table lets pass).
	if exists := tableExists(t); exists != unowned {
		t.Errorf("nftables table there after the kill: %v, want %v", exists, unowned)
	}
	if err := NewTUN(nil).Recover(Marker{Backend: "tun", PID: uint32(cmd.Process.Pid)}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := NewTUN(nil).Recover(Marker{Backend: "tun"}); err != nil {
		t.Fatalf("Recover with nothing to do: %v", err)
	}
	assertRestored(t, game, "after-kill")
}

// stunReply answers a STUN Binding request (RFC 5389) with the source it came
// from, as a public STUN server does; ok is false for anything else.
func stunReply(req []byte, from netip.AddrPort) ([]byte, bool) {
	const cookie = 0x2112A442
	if len(req) < 20 || binary.BigEndian.Uint16(req) != 0x0001 || binary.BigEndian.Uint32(req[4:]) != cookie {
		return nil, false
	}
	b := binary.BigEndian.AppendUint16(nil, 0x0101) // Binding success
	b = binary.BigEndian.AppendUint16(b, 12)        // one 8-byte attribute
	b = append(b, req[4:20]...)                     // cookie and transaction id
	b = binary.BigEndian.AppendUint16(b, 0x0020)    // XOR-MAPPED-ADDRESS
	b = binary.BigEndian.AppendUint16(b, 8)
	b = append(b, 0, 0x01)
	b = binary.BigEndian.AppendUint16(b, from.Port()^cookie>>16)
	ip := from.Addr().Unmap().As4()
	return binary.BigEndian.AppendUint32(b, binary.BigEndian.Uint32(ip[:])^cookie), true
}

// runLpcheck runs the lpcheck binary at path for a second and returns the
// public address it reports.
func runLpcheck(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command(path, "-server", serverAddr, "-rate", "20", "-for", "1500ms").CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", path, err, out)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if a, ok := strings.CutPrefix(l, "public address: "); ok {
			return strings.TrimSpace(a)
		}
	}
	t.Fatalf("%s reported no single public address:\n%s", path, out)
	return ""
}

// tunLpcheck is issue #41's acceptance test on the real pieces: lpcheck (a
// separate process, recognised by name through /proc) is routed by the
// engine through a relay, so the STUN server sees the relay's address; a
// copy under another name keeps the client's own address; after Close
// lpcheck sees the client's address again.
func tunLpcheck(t *testing.T) {
	bin := os.Getenv(lpcheckEnv)
	if bin == "" {
		t.Skip("set " + lpcheckEnv + " to a Linux lpcheck binary")
	}
	lp := t.TempDir() + "/lpcheck"
	other := t.TempDir() + "/othercheck"
	for _, dst := range []string{lp, other} {
		b, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := runLpcheck(t, lp); !strings.HasPrefix(got, clientIP+":") {
		t.Fatalf("lpcheck before boosting sees %s, want the client's %s", got, clientIP)
	}

	var holder atomic.Pointer[Capture]
	eng, err := engine.Start(engine.Config{
		Paths: []engine.PathSpec{{ID: "relay", Label: "Test relay", Addr: netip.MustParseAddrPort(relayAddr)}},
		Deliver: func(local, remote netip.AddrPort, payload []byte) {
			if c := holder.Load(); c != nil {
				c.Inject(local, remote, payload)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()
	s, err := NewTUN(owner.NewTable()).Start(Config{
		Processes: []string{"lpcheck"},
		Exclude:   []netip.AddrPort{netip.MustParseAddrPort(relayAddr)},
		OnPacket:  eng.Send,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	holder.Store(s.(*Capture))

	var routed, direct string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); routed = runLpcheck(t, lp) }()
	go func() { defer wg.Done(); direct = runLpcheck(t, other) }()
	wg.Wait()
	t.Logf("while boosting: lpcheck sees %s, another program sees %s (capture %+v)", routed, direct, s.Stats())
	if !strings.HasPrefix(routed, "198.51.100.7:") {
		t.Errorf("lpcheck while boosted sees %s, want the relay's 198.51.100.7", routed)
	}
	if !strings.HasPrefix(direct, clientIP+":") {
		t.Errorf("another program while boosting sees %s, want the client's own %s", direct, clientIP)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runLpcheck(t, lp); !strings.HasPrefix(got, clientIP+":") {
		t.Errorf("lpcheck after Stop sees %s, want the client's %s", got, clientIP)
	}
}

// tunNoRights runs as an ordinary user without capabilities: Available says
// rights are missing, and setting up anyway fails the same way (the kernel
// refuses) without leaving anything behind.
func tunNoRights(t *testing.T) {
	b := NewTUN(newFakeOS())
	if err := b.Available(); !errors.Is(err, ErrNeedAdmin) {
		t.Fatalf("Available as nobody: %v, want ErrNeedAdmin", err)
	}
	_, err := (&linuxTUN{status: "/proc/self/status", dev: "/dev/net/tun"}).open(tunRoute{})
	var re *RightsError
	if !errors.As(err, &re) || !errors.Is(err, ErrNeedAdmin) {
		t.Fatalf("open as nobody: %v, want a RightsError", err)
	}
	t.Logf("as nobody: %v", err)
	if d := tunDevices(t); len(d) != 0 {
		t.Fatalf("a failed open left %v", d)
	}
	// The kernel refuses the nftables batch on its header; that must not
	// read as a kernel that doesn't answer.
	start := time.Now()
	if err := b.Recover(Marker{Backend: "tun"}); err != nil || time.Since(start) > time.Second {
		t.Fatalf("Recover as nobody with nothing left: %v after %v", err, time.Since(start))
	}
	fmt.Println(readyLine)
}

func tunNoRightsCheck(t *testing.T) {
	cmd := asNobody(tunRole(t, "norights", false, nil))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), readyLine) {
		t.Fatalf("as nobody: %v\n%s", err, out)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "as nobody:") {
			t.Log(strings.TrimSpace(l))
		}
	}
	if r := tunRulesNow(t); len(r) != 0 {
		t.Fatalf("rules after a refused start: %+v", r)
	}
}

// relayingCapture captures the game socket in this process and answers each
// of its packets with "relay:<payload>", injected as a relay's reply would
// be. b decides how the kernel side is made.
func relayingCapture(t *testing.T, b *TUN) *Capture {
	t.Helper()
	var sess atomic.Pointer[Capture]
	s, err := b.Start(Config{
		Processes: []string{"game"},
		OnPacket: func(local, remote netip.AddrPort, payload []byte) bool {
			reply := "relay:" + string(payload)
			go func() {
				for sess.Load() == nil {
					time.Sleep(time.Millisecond)
				}
				sess.Load().Inject(local, remote, []byte(reply)) // ErrClosed once stopped
			}()
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := s.(*Capture)
	sess.Store(c)
	t.Cleanup(func() { c.Close() })
	return c
}

// gameBackend is a TUN backend whose only game socket is game's.
func gameBackend(game *peer) *TUN {
	res := newFakeOS()
	res.socks = []socket{{Local: netip.AddrPortFrom(netip.IPv4Unspecified(), game.port()), PID: pidGame}}
	res.procs = map[uint32]string{pidGame: "game"}
	return NewTUN(res)
}

// relayedAgain keeps sending tagged packets until one is answered through
// the relay, and says how long after since that was (0: not within 3 s).
// Packets in between may go direct: that is the fail-open.
func relayedAgain(t *testing.T, game *peer, tag string, since time.Time) time.Duration {
	t.Helper()
	for i := 0; time.Since(since) < 3*time.Second; i++ {
		p := fmt.Sprint(tag, i)
		game.send(t, p)
		if game.await("relay:"+p, 20*time.Millisecond) {
			return time.Since(since)
		}
	}
	return 0
}

// delTUNRule removes the routing rule as another program would.
func delTUNRule(t *testing.T) time.Time {
	t.Helper()
	rt, err := netlink.Dial(netlink.Route)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rtDo(t, rt, rtnl.DelRule(tunRule), nil)
	return time.Now()
}

// tunRuleRemoved is issue #151's acceptance test: another program deletes
// the routing rule mid-capture (as systemd-networkd does to rules its
// .network files don't declare), and within about a second the game's
// packets go through the relay again; Close still leaves nothing behind.
func tunRuleRemoved(t *testing.T) {
	game := newPeer(t)
	c := relayingCapture(t, gameBackend(game))
	game.send(t, "before")
	if !game.await("relay:before", 2*time.Second) {
		t.Fatalf("not captured: %v", game.replies())
	}
	for round := range 2 {
		removed := delTUNRule(t)
		back := relayedAgain(t, game, fmt.Sprint("r", round, "-"), removed)
		t.Logf("round %d: relayed again %v after the rule was removed (stats %+v)", round, back.Round(time.Millisecond), c.Stats())
		if back == 0 || back > 1500*time.Millisecond {
			t.Fatalf("round %d: back through the relay after %v, want about 1 s", round, back)
		}
		if r := tunRulesNow(t); len(r) != 1 {
			t.Fatalf("rules after putting it back: %+v, want exactly one", r)
		}
	}
	if st := c.Stats(); st.RouteRepairs != 2 || st.Errors != 0 {
		t.Errorf("stats %+v, want 2 route repairs and no errors", st)
	}
	select {
	case <-c.Done():
		t.Fatalf("capture stopped: %v", c.Err())
	default:
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertRestored(t, game, "after-rule-removed")
}

// tunRuleRemovedWithCapabilities is the same for an ordinary user with only
// CAP_NET_ADMIN and CAP_NET_RAW (setcap): listing and adding rules needs no
// more. The capturer swallows the game's packets, so "captured again" is a
// packet that gets no direct answer.
func tunRuleRemovedWithCapabilities(t *testing.T) {
	game := newPeerAs(t, 65534)
	cmd := asNobody(tunRole(t, "capturer", false, []string{fmt.Sprintf("%s=%d", tunPortEnv, game.port())}), capNetAdmin, capNetRaw)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			default: // nobody is waiting for it
			}
		}
		close(lines)
	}()
	awaitLine := func(want string) {
		t.Helper()
		for end := time.After(15 * time.Second); ; {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("capturer ended before %q", want)
				}
				if strings.Contains(l, want) {
					return
				}
			case <-end:
				t.Fatalf("no %q from the capturer", want)
			}
		}
	}
	awaitLine(capturingLine)
	game.send(t, "swallowed")
	if game.await(direct(game, "swallowed"), 300*time.Millisecond) {
		t.Fatal("not captured")
	}
	removed := delTUNRule(t)
	awaitLine("put it back")
	t.Logf("as nobody: the rule was back %v after it was removed", time.Since(removed).Round(time.Millisecond))
	if r := tunRulesNow(t); len(r) != 1 {
		t.Fatalf("rules after putting it back: %+v, want exactly one", r)
	}
	game.send(t, "swallowed-again")
	if game.await(direct(game, "swallowed-again"), 300*time.Millisecond) {
		t.Fatal("not captured after the rule was put back")
	}
	cmd.Process.Kill()
	cmd.Wait()
	if err := NewTUN(nil).Recover(Marker{Backend: "tun"}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertRestored(t, game, "after-rule-removed-nobody")
}

// flushRuleset is `nft flush ruleset` (what firewall reloads send): a table
// delete without a name or family, in one transaction with then.
func flushRuleset(t *testing.T, nf *netlink.Conn, then ...netlink.Message) {
	t.Helper()
	flush := netlink.Message{Type: 10<<8 | 2, Flags: netlink.FlagAck, Op: "flush ruleset", Data: []byte{0, 0, 0, 0}}
	if err := nf.Do(nft.Batch(append([]netlink.Message{flush}, then...)...)...); err != nil {
		t.Fatal(err)
	}
}

// tunRulesetFlushed: on Linux 5.12+ the table belongs to the capture's
// socket, and a flush passes it by; nothing needs putting back.
func tunRulesetFlushed(t *testing.T) {
	game := newPeer(t)
	c := relayingCapture(t, gameBackend(game))
	nf, err := netlink.Dial(netlink.Netfilter)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Close()
	flushRuleset(t, nf)
	if !tableExists(t) {
		t.Fatal("the flush removed an owned table")
	}
	time.Sleep(1200 * time.Millisecond) // a check ran
	game.send(t, "flushed")
	if !game.await("relay:flushed", 2*time.Second) {
		t.Fatalf("not captured after a flush: %v", game.replies())
	}
	if st := c.Stats(); st.RouteRepairs != 0 || st.Errors != 0 {
		t.Errorf("stats %+v, want no repairs", st)
	}
	c.Close()
	assertRestored(t, game, "after-flush")
}

// tunRulesetFlushedOldKernel: the table as kernels before 5.12 make it,
// which a flush deletes. It is made again within about a second, also when
// an empty table of that name took its place; when another program's
// socket takes its name in the same transaction it can't be, and the
// capture stops with the reason, leaving nothing of its own.
func tunRulesetFlushedOldKernel(t *testing.T) {
	game := newPeer(t)
	b := gameBackend(game)
	b.sys = &linuxTUN{status: "/proc/self/status", dev: "/dev/net/tun", unowned: true}
	c := relayingCapture(t, b)
	game.send(t, "before")
	if !game.await("relay:before", 2*time.Second) {
		t.Fatalf("not captured: %v", game.replies())
	}
	nf, err := netlink.Dial(netlink.Netfilter)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Close()
	flushRuleset(t, nf)
	flushed := time.Now()
	if tableExists(t) {
		t.Fatal("the flush left an unowned table")
	}
	back := relayedAgain(t, game, "f", flushed)
	t.Logf("relayed again %v after the flush (stats %+v)", back.Round(time.Millisecond), c.Stats())
	if back == 0 || back > 1500*time.Millisecond {
		t.Fatalf("back through the relay after %v, want about 1 s", back)
	}
	if st := c.Stats(); st.RouteRepairs != 1 {
		t.Errorf("stats %+v, want one repair", st)
	}

	// A flush followed by an empty table of that name (a firewall restoring
	// its ruleset): the name is there, but the handle isn't this table's,
	// and it is made again.
	flushRuleset(t, nf, nft.AddTable(nft.IPv4, tunNftTable, 0))
	replaced := time.Now()
	if !tableExists(t) {
		t.Fatal("no table after the flush and add")
	}
	back = relayedAgain(t, game, "e", replaced)
	t.Logf("relayed again %v after the table was replaced (stats %+v)", back.Round(time.Millisecond), c.Stats())
	if back == 0 || back > 1500*time.Millisecond {
		t.Fatalf("back through the relay after %v, want about 1 s", back)
	}
	if st := c.Stats(); st.RouteRepairs != 2 {
		t.Errorf("stats %+v, want two repairs", st)
	}

	// Another program's table of that name (owned by this test's socket).
	flushRuleset(t, nf, nft.AddTable(nft.IPv4, tunNftTable, nft.TableOwner))
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("capture went on without its table")
	}
	t.Logf("stopped: %v", c.Err())
	if !errors.Is(c.Err(), ErrRouteRemoved) || !strings.Contains(c.Err().Error(), "nftables table") {
		t.Fatalf("Err = %v, want ErrRouteRemoved for the table", c.Err())
	}
	nf.Close() // its table goes with it
	assertRestored(t, game, "after-flush-old-kernel")
}
