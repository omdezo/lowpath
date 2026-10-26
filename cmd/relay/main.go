// relay runs one hop of the Lowpath relay network on a server.
//
//	relay -listen :40000 -key SECRET                egress: dedup + deliver to game servers
//	relay -listen :40001 -forward EGRESS_HOST:40000 forwarding hop: pass packets to the next hop
//
// Every path a client uses must end at the same egress for a given game
// server, because that is where duplicate copies are dropped.
//
// The key can also come from the LOWPATH_RELAY_KEY environment variable, which
// keeps it out of the process list (the systemd unit from deploy/ does this).
// Run as root (or with CAP_NET_RAW) so the egress can ping game servers.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lowpath/internal/relay"
)

func main() {
	listen := flag.String("listen", ":40000", "UDP listen address")
	key := flag.String("key", "", "access key clients must present (default $LOWPATH_RELAY_KEY); empty = open relay")
	forward := flag.String("forward", "", "next hop host:port (makes this a forwarding hop)")
	allowPrivate := flag.Bool("allow-private", false, "let clients reach private, loopback and link-local destinations")
	statsEvery := flag.Duration("stats", 60*time.Second, "log traffic counters this often (0 = never)")
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", flag.Args())
		flag.Usage()
		os.Exit(2)
	}
	if *key == "" {
		*key = os.Getenv("LOWPATH_RELAY_KEY")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *forward != "" {
		runForwarder(ctx, log, *listen, *forward, *key != "" || *allowPrivate, *statsEvery)
		return
	}

	e, err := relay.ListenEgressOpts(*listen, relay.Options{Key: *key, AllowPrivate: *allowPrivate}, log)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	if *key == "" {
		if pub, ok := publicListenAddr(e.Addr().Addr()); ok {
			warnOpen(log, pub, e.Addr().Port())
		}
	}
	if *allowPrivate {
		log.Warn("-allow-private: clients can reach this server's private network and loopback services")
	}
	log.Info("egress relay up", "listen", e.Addr(), "key", *key != "", "allow_private", *allowPrivate)
	stats := func() { logEgressStats(log, e) }
	if *statsEvery > 0 {
		go logEvery(ctx, *statsEvery, stats)
	}
	e.Serve(ctx)
	stats()
}

// runForwarder runs a forwarding hop. It does not check tokens: the egress
// behind it does, and the hop can only ever reach that one egress.
func runForwarder(ctx context.Context, log *slog.Logger, listen, forward string, egressFlags bool, statsEvery time.Duration) {
	ua, err := net.ResolveUDPAddr("udp", forward)
	if err != nil {
		log.Error("bad -forward", "err", err)
		os.Exit(2)
	}
	next := ua.AddrPort()
	next = netip.AddrPortFrom(next.Addr().Unmap(), next.Port())
	if egressFlags {
		log.Warn("-key and -allow-private only apply to the egress; this hop passes packets through unchanged")
	}
	f, err := relay.ListenForwarder(listen, next, log)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	log.Info("forwarding hop up", "listen", f.Addr(), "next", next, "next_host", forward)
	stats := func() {
		s := f.Stats()
		log.Info("stats", "peers", s.Peers, "pkts_up", s.PacketsUp, "pkts_down", s.PacketsDown,
			"malformed", s.Malformed, "limit_drops", s.LimitDrops, "evicted", s.Evicted, "too_big", s.TooBig)
	}
	if statsEvery > 0 {
		go logEvery(ctx, statsEvery, stats)
	}
	f.Serve(ctx)
	stats()
}

// publicListenAddr reports whether the relay is reachable from the internet:
// bound to a public IP, or to all interfaces on a host that has one.
func publicListenAddr(bound netip.Addr) (netip.Addr, bool) {
	bound = bound.Unmap()
	if !bound.IsUnspecified() {
		return bound, relay.IsPublic(bound)
	}
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Addr{}, false
	}
	for _, ia := range ifaddrs {
		ipn, ok := ia.(*net.IPNet)
		if !ok {
			continue
		}
		if a, ok := netip.AddrFromSlice(ipn.IP); ok && relay.IsPublic(a.Unmap()) {
			return a.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

func warnOpen(log *slog.Logger, pub netip.Addr, port uint16) {
	fmt.Fprintf(os.Stderr, `
################################################################################
  OPEN RELAY: no -key set and this server has a public address (%s).
  Anyone who finds UDP port %d can send traffic to any public host through it,
  and it will look like it came from this server.
  Set -key (or LOWPATH_RELAY_KEY) and enter the same key in Lowpath.
################################################################################

`, pub, port)
	log.Warn("open relay on a public address: set -key", "public_ip", pub)
}

func logEvery(ctx context.Context, every time.Duration, logStats func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logStats()
		}
	}
}

func logEgressStats(log *slog.Logger, e *relay.Egress) {
	s := e.Stats()
	log.Info("stats",
		"flows", s.Flows, "servers", s.Servers,
		"pkts_up", s.PacketsUp, "pkts_down", s.PacketsDown, "dups", s.Duplicates,
		"probes", s.Probes, "token_drops", s.TokenDrops, "policy_drops", s.PolicyDrops,
		"limit_drops", s.LimitDrops, "too_big", s.TooBig, "malformed", s.Malformed,
		"seqs_settled", s.Settled, "seqs_missing", s.Missing, "tstats_sent", s.Reports)
}
