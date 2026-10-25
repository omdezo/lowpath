package engine

import (
	"errors"
	"net"
	"net/netip"
	"time"

	"lowpath/internal/proto"
)

// PathSpec is one relay chain as the client sees it: the address of its first
// hop. The rest of the chain (transit, egress) is configured on the relays.
type PathSpec struct {
	ID        string         `json:"id"`
	Label     string         `json:"label"`
	Region    string         `json:"region"`
	Continent string         `json:"continent"`
	Addr      netip.AddrPort `json:"addr"`
	Key       string         `json:"-"`                // relay access key; becomes the token on every packet
	Egress    string         `json:"egress,omitempty"` // paths ending at the same egress relay share this id
}

type path struct {
	spec   PathSpec
	token  uint64 // derived once: hashing the key per packet would be wasted work
	conn   *net.UDPConn
	probes tracker
}

func dialPath(spec PathSpec) (*path, error) {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(spec.Addr))
	if err != nil {
		return nil, err
	}
	return &path{spec: spec, token: proto.TokenFromKey(spec.Key), conn: c}, nil
}

// probe sends one path probe. dst (the game server, may be zero) asks the
// egress relay to report its own round trip to that server in the reply.
func (p *path) probe(flow uint64, start time.Time, dst netip.AddrPort) {
	now := time.Now()
	seq := p.probes.send(now)
	p.sendProbe(flow, seq, now, start, dst)
}

func (p *path) sendProbe(flow uint64, seq uint32, now, start time.Time, dst netip.AddrPort) {
	h := proto.Header{Type: proto.TProbe, Token: p.token, Flow: flow, Seq: seq, TS: uint32(now.Sub(start).Milliseconds()), Addr: dst}
	var buf [proto.MaxOverhead]byte
	p.conn.Write(proto.Append(buf[:0], h, nil))
}

// replyLeg extracts the relay->server leg a probe reply may carry; -1 when
// the relay didn't measure one (no server in the probe, server ignores ping,
// or a relay that predates server legs).
func replyLeg(payload []byte) time.Duration {
	if d, ok := proto.ParseServerRTT(payload); ok {
		return d
	}
	return -1
}

func isClosed(err error) bool { return errors.Is(err, net.ErrClosed) }
