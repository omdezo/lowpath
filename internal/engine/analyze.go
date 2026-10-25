package engine

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"lowpath/internal/icmp"
	"lowpath/internal/proto"
)

// AnalyzeOptions says how to measure the direct (un-optimized) route.
type AnalyzeOptions struct {
	// Direct is a UDP-echo baseline target; only the bundled simulator
	// answers it (demo mode). It takes precedence over Server for the
	// baseline.
	Direct netip.AddrPort
	// Server is a real game server: the baseline pings it over ICMP, and the
	// path probes ask each egress relay for its own leg to it.
	Server netip.Addr
}

type Analysis struct {
	Direct      Stats            `json:"direct"`
	DirectKnown bool             `json:"direct_known"` // false: no baseline target, or it ignored every probe
	Paths       map[string]Stats `json:"paths"`
}

// Analyze probes every candidate path and the direct route for dur, without
// touching any game traffic. This is the "Analyze" button. Path RTTs include
// the egress relay's leg to o.Server when the relay reports one; with no
// server they are RTTs to the relay only.
func Analyze(ctx context.Context, specs []PathSpec, o AnalyzeOptions, dur time.Duration) (Analysis, error) {
	start := time.Now()
	flow := randID()
	var paths []*path
	defer func() {
		for _, p := range paths {
			p.conn.Close()
		}
	}()
	for _, s := range specs {
		p, err := dialPath(s)
		if err != nil {
			return Analysis{}, err
		}
		paths = append(paths, p)
	}
	var dc *net.UDPConn
	if o.Direct.IsValid() {
		var err error
		if dc, err = net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(o.Direct)); err != nil {
			return Analysis{}, err
		}
		defer dc.Close()
	}
	var dst netip.AddrPort
	if o.Server.IsValid() {
		dst = netip.AddrPortFrom(o.Server, 0) // the relay pings the IP; the port is unused
	}

	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 2048)
			for {
				n, err := p.conn.Read(buf)
				if err != nil {
					if isClosed(err) {
						return
					}
					continue
				}
				if h, payload, err := proto.Parse(buf[:n]); err == nil && h.Type == proto.TProbeReply && h.Flow == flow {
					p.probes.recvLeg(h.Seq, time.Now(), replyLeg(payload))
				}
			}
		}()
	}
	var before tracker
	if dc != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 2048)
			for {
				n, err := dc.Read(buf)
				if err != nil {
					if isClosed(err) {
						return
					}
					continue
				}
				if seq, ok := parseDirectProbe(buf[:n]); ok {
					before.recv(seq, time.Now())
				}
			}
		}()
	}
	icmpBaseline := dc == nil && o.Server.IsValid()

	// Probe faster than the live loop so a short analysis still has enough
	// samples to fill the stats window. ICMP keeps the live 5 Hz: servers
	// that do answer ping often rate-limit it.
	every := probeInterval / 2
	pingEvery := int(pingInterval / every)
	tick := time.NewTicker(every)
	deadline := time.NewTimer(dur)
	defer tick.Stop()
	defer deadline.Stop()
	var pings sync.WaitGroup
loop:
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			break loop
		case <-deadline.C:
			break loop
		case <-tick.C:
		}
		for _, p := range paths {
			p.probe(flow, start, dst)
		}
		switch {
		case dc != nil:
			b := directProbe(before.send(time.Now()))
			dc.Write(b[:])
		case icmpBaseline && i%pingEvery == 0:
			seq := before.send(time.Now())
			pings.Add(1)
			go func() {
				defer pings.Done()
				if rtt, err := icmp.Ping(o.Server, probeTimeout); err == nil {
					before.recvRTT(seq, rtt)
				}
			}()
		}
	}
	// Let in-flight probes land, then judge anything still missing as lost.
	time.Sleep(probeTimeout)
	pings.Wait()
	now := time.Now()
	n := min(ringSize, int(dur/every))
	out := Analysis{Paths: map[string]Stats{}}
	if dc != nil || icmpBaseline {
		out.Direct = before.statsN(now, n)
	}
	out.DirectKnown = out.Direct.Samples > 0
	for _, p := range paths {
		out.Paths[p.spec.ID] = p.probes.statsN(now, n)
	}
	for _, p := range paths {
		p.conn.Close()
	}
	if dc != nil {
		dc.Close()
	}
	wg.Wait()
	paths = nil
	return out, ctx.Err()
}
