// lpcheck sends STUN binding requests to a public STUN server and prints the
// round trip time, jitter, loss and the public address the server saw.
//
// It is a stand-in "game" for checking Lowpath end to end: add lpcheck.exe as
// a custom game and press Apply. While Lowpath routes it, the public address
// becomes the egress relay's and the RTT is the relayed route's.
//
//	lpcheck                       10 s at 20 requests/s to stun.l.google.com:19302
//	lpcheck -for 0                until Ctrl+C
//	lpcheck -server stun.cloudflare.com:3478 -rate 64
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"lowpath/internal/stun"
)

// A request unanswered after this long counts as lost; a later reply is ignored.
const replyTimeout = time.Second

func main() {
	server := flag.String("server", "stun.l.google.com:19302", "STUN server host:port")
	rate := flag.Float64("rate", 20, "requests per second")
	dur := flag.Duration("for", 10*time.Second, "how long to send (0 = until Ctrl+C)")
	flag.Parse()
	if *rate <= 0 || *rate > 1000 {
		fmt.Fprintln(os.Stderr, "lpcheck: -rate must be between 0 and 1000")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}
	os.Exit(run(ctx, *server, *rate, *dur))
}

func run(ctx context.Context, server string, rate float64, dur time.Duration) int {
	// IPv4 only: that is all Lowpath's capture routes, so an IPv6 result
	// would say nothing about the route.
	ua, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lpcheck:", err)
		return 2
	}
	conn, err := net.DialUDP("udp4", nil, ua)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lpcheck:", err)
		return 2
	}
	c := newChecker(conn)
	span := "until Ctrl+C"
	if dur > 0 {
		span = "for " + dur.String()
	}
	fmt.Printf("lpcheck: STUN %s (%s) at %g/s %s, local %s\n\n", server, ua, rate, span, conn.LocalAddr())

	readDone := make(chan struct{})
	go func() {
		c.readLoop()
		close(readDone)
	}()
	elapsed := c.sendLoop(ctx, time.Duration(float64(time.Second)/rate))

	// Give replies still in flight their full timeout before the summary.
	for deadline := time.Now().Add(replyTimeout); c.inFlight() > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	conn.Close()
	// The last, partial second; anything still unanswered now is lost.
	c.report(elapsed.Round(time.Second), time.Now().Add(replyTimeout), true)
	<-readDone
	return c.summary(server)
}

type checker struct {
	conn    *net.UDPConn
	session [4]byte // first bytes of every transaction id, to spot stray replies

	mu       sync.Mutex
	seq      uint64
	pending  map[uint64]time.Time // seq -> send time
	second   window               // the current one-second line
	total    window
	mapped   netip.AddrPort   // latest public address seen
	seen     []netip.AddrPort // every distinct public address, in order
	changes  []string         // printed with the next line
	sendErrs int
	lastErr  error
}

// window accumulates RTT statistics; jitter is the standard deviation, the
// same definition Lowpath's UI uses.
type window struct {
	sent, recv, lost int
	sum, sumSq       float64 // milliseconds
	min, max         float64
}

func (w *window) add(rtt time.Duration) {
	ms := float64(rtt) / float64(time.Millisecond)
	if w.recv == 0 || ms < w.min {
		w.min = ms
	}
	if ms > w.max {
		w.max = ms
	}
	w.recv++
	w.sum += ms
	w.sumSq += ms * ms
}

func (w *window) avg() float64 { return w.sum / float64(w.recv) }

func (w *window) jitter() float64 {
	m := w.avg()
	return math.Sqrt(max(w.sumSq/float64(w.recv)-m*m, 0))
}

// lossPct is the share of resolved requests (answered or timed out) that
// were lost; requests still in flight are not counted either way.
func (w *window) lossPct() (float64, bool) {
	n := w.recv + w.lost
	if n == 0 {
		return 0, false
	}
	return 100 * float64(w.lost) / float64(n), true
}

func newChecker(conn *net.UDPConn) *checker {
	c := &checker{conn: conn, pending: map[uint64]time.Time{}}
	rand.Read(c.session[:])
	return c
}

// sendLoop sends requests and prints a line per second until ctx is done; it
// returns how long it ran.
func (c *checker) sendLoop(ctx context.Context, every time.Duration) time.Duration {
	send := time.NewTicker(every)
	defer send.Stop()
	report := time.NewTicker(time.Second)
	defer report.Stop()
	start := time.Now()
	buf := make([]byte, 0, stun.HeaderLen)
	c.send(buf)
	for {
		select {
		case <-ctx.Done():
			return time.Since(start)
		case <-send.C:
			c.send(buf)
		case now := <-report.C:
			c.report(now.Sub(start).Round(time.Second), now, false)
		}
	}
}

func (c *checker) send(buf []byte) {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()
	var id stun.TxID
	copy(id[:4], c.session[:])
	binary.BigEndian.PutUint64(id[4:], seq)
	now := time.Now()
	_, err := c.conn.Write(stun.AppendBindingRequest(buf[:0], id))
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.sendErrs++
		c.lastErr = err
		return
	}
	c.pending[seq] = now
	c.second.sent++
	c.total.sent++
}

func (c *checker) readLoop() {
	buf := make([]byte, 1500)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. ICMP port unreachable surfaced on the socket
		}
		now := time.Now()
		id, mapped, err := stun.ParseBindingResponse(buf[:n])
		if err != nil || [4]byte(id[:4]) != c.session {
			continue
		}
		c.reply(binary.BigEndian.Uint64(id[4:]), mapped, now)
	}
}

func (c *checker) reply(seq uint64, mapped netip.AddrPort, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sent, ok := c.pending[seq]
	if !ok {
		return // duplicate, or arrived after it was counted lost
	}
	delete(c.pending, seq)
	rtt := now.Sub(sent)
	c.second.add(rtt)
	c.total.add(rtt)
	if mapped != c.mapped {
		if c.mapped.IsValid() {
			c.changes = append(c.changes, fmt.Sprintf("public address changed: %s -> %s", c.mapped, mapped))
		}
		c.mapped = mapped
		if !containsAddr(c.seen, mapped) {
			c.seen = append(c.seen, mapped)
		}
	}
}

func containsAddr(s []netip.AddrPort, a netip.AddrPort) bool {
	for _, x := range s {
		if x == a {
			return true
		}
	}
	return false
}

// expireLocked counts requests older than replyTimeout as lost.
func (c *checker) expireLocked(now time.Time) {
	for seq, sent := range c.pending {
		if now.Sub(sent) > replyTimeout {
			delete(c.pending, seq)
			c.second.lost++
			c.total.lost++
		}
	}
}

func (c *checker) inFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// report prints one line for the requests since the previous one; requests
// older than replyTimeout at now are counted lost first. A final report with
// nothing in it is skipped (the run ended right on a line).
func (c *checker) report(elapsed time.Duration, now time.Time, final bool) {
	c.mu.Lock()
	c.expireLocked(now)
	w := c.second
	if final && w.sent == 0 && w.recv == 0 && w.lost == 0 {
		c.mu.Unlock()
		return
	}
	c.second = window{}
	changes := c.changes
	c.changes = nil
	mapped := c.mapped
	sendErrs, lastErr := c.sendErrs, c.lastErr
	c.sendErrs = 0
	c.mu.Unlock()

	for _, ch := range changes {
		fmt.Println("  " + ch)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%4.0fs  sent %3d  recv %3d", elapsed.Seconds(), w.sent, w.recv)
	if w.recv > 0 {
		fmt.Fprintf(&b, "  rtt %6.1f ms  jitter %5.1f ms", w.avg(), w.jitter())
	} else {
		fmt.Fprintf(&b, "  rtt %6s ms  jitter %5s ms", "-", "-")
	}
	if loss, ok := w.lossPct(); ok {
		fmt.Fprintf(&b, "  loss %5.1f%%", loss)
	} else {
		fmt.Fprintf(&b, "  loss %6s", "-")
	}
	if mapped.IsValid() {
		fmt.Fprintf(&b, "  public %s", mapped)
	} else {
		b.WriteString("  public -")
	}
	if sendErrs > 0 {
		fmt.Fprintf(&b, "  send errors %d (%v)", sendErrs, lastErr)
	}
	fmt.Println(b.String())
}

// summary prints the totals and returns the process exit code.
func (c *checker) summary(server string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now().Add(replyTimeout)) // whatever is still pending is lost
	t := c.total
	fmt.Printf("\n--- %s ---\n", server)
	loss, _ := t.lossPct()
	fmt.Printf("sent %d, received %d, lost %d (%.1f%%)\n", t.sent, t.recv, t.lost, loss)
	if t.recv == 0 {
		fmt.Println("no replies: UDP to the STUN server is blocked, or the route in use is not delivering")
		return 1
	}
	fmt.Printf("rtt min/avg/max = %.1f/%.1f/%.1f ms, jitter %.1f ms\n", t.min, t.avg(), t.max, t.jitter())
	if len(c.seen) == 1 {
		fmt.Printf("public address: %s\n", c.seen[0])
	} else {
		parts := make([]string, len(c.seen))
		for i, a := range c.seen {
			parts[i] = a.String()
		}
		fmt.Printf("public addresses, in order seen: %s\n", strings.Join(parts, ", "))
	}
	return 0
}
