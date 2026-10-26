//go:build windows && amd64

package capture

import (
	"bufio"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/divert"
	"lowpath/internal/owner"
)

// The tests in this file need the real driver, so administrator rights; a
// normal `go test` skips them. They check the roadmap's C5 acceptance on a
// real WinDivert handle: when the process that captures is killed
// (`taskkill /F` is TerminateProcess, as is exec.Cmd.Process.Kill), or its
// packet handler deadlocks, the packets it held go out directly within 1 s.
//
// A helper process (this test binary, re-run) captures the test process's
// UDP and swallows it. The test watches its own packets leave the machine
// through a second, sniffing handle at a lower priority, which only sees
// what the capture let through.

const (
	helperEnv = "LOWPATH_FAILOPEN_HELPER" // "drop" or "stuck"
	helperPID = "LOWPATH_FAILOPEN_PID"
	// sinkAddr is TEST-NET-1 (RFC 5737): never answered, never local, so
	// the capture filter matches it like a game server.
	sinkAddr = "192.0.2.1:9"
	// failOpenWithin is the roadmap's acceptance bound.
	failOpenWithin = time.Second
)

// TestFailOpenHelper is the helper process, not a test: it only runs when
// the parent test sets helperEnv.
func TestFailOpenHelper(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("helper for TestFailOpenKilled and TestFailOpenStuck")
	}
	pid, err := strconv.Atoi(os.Getenv(helperPID))
	if err != nil {
		fmt.Println("bad pid:", err)
		os.Exit(2)
	}
	block := make(chan struct{})
	c, err := Start(Config{
		DriverDir: "../divert/bin",
		PIDs:      []uint32{uint32(pid)},
		Owners:    owner.New(),
		OnPacket: func(_, _ netip.AddrPort, _ []byte) bool {
			if mode == "stuck" {
				<-block // a deadlocked engine
			}
			return true // swallowed: the kill switch, or a tunnel that never sends
		},
	})
	if err != nil {
		fmt.Println("start:", err)
		os.Exit(2)
	}
	fmt.Println("ready")
	<-c.Done()
	fmt.Println("failed open:", c.Err())
	os.Exit(0)
}

// sniffer counts the test's packets to sinkAddr that actually leave.
type sniffer struct {
	h    *divert.Handle
	seen atomic.Int64
	at   atomic.Int64 // unix nanos of the first packet seen after arm
	arm  atomic.Bool
}

func newSniffer(t *testing.T, port uint16) *sniffer {
	t.Helper()
	sink := netip.MustParseAddrPort(sinkAddr)
	filter := fmt.Sprintf("outbound and !loopback and ip and udp and ip.DstAddr == %s and udp.DstPort == %d and udp.SrcPort == %d",
		sink.Addr(), sink.Port(), port)
	h, err := divert.Open(filter, divert.LayerNetwork, divert.PriorityLowest, divert.FlagSniff|divert.FlagRecvOnly)
	if err != nil {
		t.Fatalf("sniff handle: %v", err)
	}
	s := &sniffer{h: h}
	go func() {
		buf := make([]byte, divert.MTUMax)
		var a divert.Address
		for {
			if _, err := h.Recv(buf, &a); err != nil {
				return
			}
			s.seen.Add(1)
			if s.arm.Load() {
				s.at.CompareAndSwap(0, time.Now().UnixNano())
			}
		}
	}()
	t.Cleanup(func() { h.Shutdown(); h.Close() })
	return s
}

// sender sends one small datagram to sinkAddr every 10 ms until stopped.
func sender(t *testing.T) (*net.UDPConn, func()) {
	t.Helper()
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.MustParseAddrPort(sinkAddr)))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			conn.Write([]byte("lowpath fail-open test " + strconv.Itoa(i)))
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	return conn, func() { close(stop); <-done; conn.Close() }
}

// startHelper runs the capturing helper and waits until it captures.
func startHelper(t *testing.T, mode string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFailOpenHelper$", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperPID+"="+strconv.Itoa(os.Getpid()))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	sc := bufio.NewScanner(out)
	ready := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if l := sc.Text(); l == "ready" || strings.HasPrefix(l, "start:") || strings.HasPrefix(l, "bad") {
				ready <- l
				return
			}
		}
		ready <- "helper exited"
	}()
	select {
	case l := <-ready:
		if l != "ready" {
			t.Fatalf("helper: %s", l)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("helper did not start capturing")
	}
	return cmd, sc
}

func needAdmin(t *testing.T) {
	t.Helper()
	if os.Getenv(helperEnv) != "" {
		t.Skip("inside the helper")
	}
	if !divert.IsAdmin() {
		t.Skip("needs administrator rights (the real WinDivert driver)")
	}
	if err := divert.Load("../divert/bin"); err != nil {
		t.Fatal(err)
	}
}

// expectSwallowed checks the helper really captures before the test cuts it
// off: otherwise "traffic flows afterwards" would prove nothing.
func expectSwallowed(t *testing.T, s *sniffer) {
	t.Helper()
	before := s.seen.Load()
	time.Sleep(300 * time.Millisecond)
	if n := s.seen.Load() - before; n > 0 {
		t.Fatalf("%d packets left while the helper captured; the capture is not taking them", n)
	}
}

// waitFlowing waits for packets to leave again and returns how long after
// since that happened.
func waitFlowing(t *testing.T, s *sniffer, since time.Time) time.Duration {
	t.Helper()
	deadline := since.Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if at := s.at.Load(); at != 0 {
			return time.Unix(0, at).Sub(since)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("traffic did not go direct within 5 s")
	return 0
}

func TestFailOpenKilled(t *testing.T) {
	needAdmin(t)
	conn, stop := sender(t)
	defer stop()
	s := newSniffer(t, uint16(conn.LocalAddr().(*net.UDPAddr).Port))
	cmd, _ := startHelper(t, "drop")
	expectSwallowed(t, s)

	s.arm.Store(true)
	killed := time.Now()
	if err := cmd.Process.Kill(); err != nil { // TerminateProcess, like taskkill /F
		t.Fatal(err)
	}
	took := waitFlowing(t, s, killed)
	t.Logf("direct traffic %v after the capturing process was killed", took)
	if took > failOpenWithin {
		t.Fatalf("direct traffic after %v, want under %v", took, failOpenWithin)
	}
}

func TestFailOpenStuck(t *testing.T) {
	needAdmin(t)
	cmd, sc := startHelper(t, "stuck")
	conn, stop := sender(t)
	defer stop()
	s := newSniffer(t, uint16(conn.LocalAddr().(*net.UDPAddr).Port))
	// The first packet the helper takes blocks its handler; the watchdog
	// must release the traffic on its own, with the process still alive.
	s.arm.Store(true)
	stuck := time.Now()
	took := waitFlowing(t, s, stuck)
	t.Logf("direct traffic %v after the handler got stuck", took)
	// The watchdog can't fire before the stall limit: packets leaving much
	// sooner were never held, and the test would prove nothing.
	if took < DefaultStallLimit/2 {
		t.Fatalf("direct traffic after only %v: the helper was not holding the packets", took)
	}
	if took > failOpenWithin {
		t.Fatalf("direct traffic after %v, want under %v", took, failOpenWithin)
	}
	reason := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "failed open:") {
				reason <- l
				return
			}
		}
	}()
	select {
	case l := <-reason:
		if !strings.Contains(l, "stuck") {
			t.Fatalf("helper: %s", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not report failing open")
	}
	cmd.Wait()
}
