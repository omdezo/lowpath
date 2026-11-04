// Package proto defines the Lowpath UDP overlay framing used between the
// client engine and the relays.
//
// Wire layout (big endian):
//
//	magic   u8   0xA7
//	type    u8   Data / Probe / ProbeReply / Stats
//	token   u64  relay access token (TokenFromKey); 0 when the relay is open
//	flow    u64  random per game flow, chosen by the client
//	seq     u32  per-flow, per-direction sequence number (dedup key)
//	ts      u32  sender clock in ms, echoed back by probes
//	alen    u8   0, 4 or 16
//	addr    alen bytes  game server IP (client->relay) or source IP (relay->client)
//	port    u16  present only when alen > 0
//	payload ...
//
// A Probe may carry the game server address; the egress relay then answers
// with a ProbeReply whose payload is a ServerLeg block (see AppendServerLeg):
// the relay->server leg's RTT, which the client adds to the path RTT, then
// that leg's loss and jitter.
//
// About once a second the egress relay sends each of a flow's return
// addresses a Stats packet (flow = the data flow, seq = a per-flow report
// counter, ts = the relay's clock, addr = the game server) whose payload
// is the flow's upstream lane statistics, encoded by package tstats.
// Clients that predate it ignore the type.
package proto

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"net/netip"
	"time"
)

const Magic = 0xA7

type Type uint8

const (
	TData       Type = 1
	TProbe      Type = 2
	TProbeReply Type = 3
	TStats      Type = 4 // egress -> client: per-lane delivery statistics (tstats.Parse)
)

// FixedLen is the header size before the address block.
const FixedLen = 1 + 1 + 8 + 8 + 4 + 4 + 1

// MaxOverhead is the largest header we ever add (IPv6 address + port).
const MaxOverhead = FixedLen + 16 + 2

type Header struct {
	Type  Type
	Token uint64
	Flow  uint64
	Seq   uint32
	TS    uint32
	Addr  netip.AddrPort // zero value = no address block
}

var (
	ErrShort = errors.New("proto: packet too short")
	ErrMagic = errors.New("proto: bad magic")
	ErrAddr  = errors.New("proto: bad address length")
)

// TokenFromKey derives the 64-bit access token a relay started with -key
// expects on every packet. An empty key means an open relay (token 0).
func TokenFromKey(key string) uint64 {
	if key == "" {
		return 0
	}
	sum := sha256.Sum256([]byte("lowpath-relay-v1:" + key))
	t := binary.BigEndian.Uint64(sum[:8])
	if t == 0 {
		t = 1
	}
	return t
}

// Append encodes h followed by payload onto dst and returns the extended slice.
func Append(dst []byte, h Header, payload []byte) []byte {
	var fixed [FixedLen]byte
	fixed[0] = Magic
	fixed[1] = byte(h.Type)
	binary.BigEndian.PutUint64(fixed[2:], h.Token)
	binary.BigEndian.PutUint64(fixed[10:], h.Flow)
	binary.BigEndian.PutUint32(fixed[18:], h.Seq)
	binary.BigEndian.PutUint32(fixed[22:], h.TS)
	if h.Addr.IsValid() {
		ip := h.Addr.Addr().Unmap()
		raw := ip.AsSlice()
		fixed[26] = byte(len(raw))
		dst = append(dst, fixed[:]...)
		dst = append(dst, raw...)
		dst = binary.BigEndian.AppendUint16(dst, h.Addr.Port())
	} else {
		dst = append(dst, fixed[:]...)
	}
	return append(dst, payload...)
}

// Parse decodes a packet. The returned payload aliases b.
func Parse(b []byte) (Header, []byte, error) {
	var h Header
	if len(b) < FixedLen {
		return h, nil, ErrShort
	}
	if b[0] != Magic {
		return h, nil, ErrMagic
	}
	h.Type = Type(b[1])
	h.Token = binary.BigEndian.Uint64(b[2:])
	h.Flow = binary.BigEndian.Uint64(b[10:])
	h.Seq = binary.BigEndian.Uint32(b[18:])
	h.TS = binary.BigEndian.Uint32(b[22:])
	alen := int(b[26])
	rest := b[FixedLen:]
	switch alen {
	case 0:
		return h, rest, nil
	case 4, 16:
	default:
		return h, nil, ErrAddr
	}
	if len(rest) < alen+2 {
		return h, nil, ErrShort
	}
	ip, _ := netip.AddrFromSlice(rest[:alen])
	h.Addr = netip.AddrPortFrom(ip, binary.BigEndian.Uint16(rest[alen:]))
	return h, rest[alen+2:], nil
}

// ServerRTTUnknown is sent when the relay has no ping result for the server
// (not measured yet, or the server does not answer ICMP).
const ServerRTTUnknown = ^uint32(0)

// AppendServerRTT encodes the relay->game-server round trip time carried in a
// ProbeReply payload (microseconds, u32). Pass a negative d for "unknown".
func AppendServerRTT(dst []byte, d time.Duration) []byte {
	v := ServerRTTUnknown
	if d >= 0 {
		us := d.Microseconds()
		v = uint32(min(us, int64(ServerRTTUnknown-1)))
	}
	return binary.BigEndian.AppendUint32(dst, v)
}

// ParseServerRTT decodes a ProbeReply payload. ok is false when the relay sent
// no RTT block (older relay, no server address in the probe) or "unknown".
// The RTT is the first field of a ServerLeg block, so this is also how a
// client that predates the leg's loss and jitter reads a newer reply.
func ParseServerRTT(payload []byte) (d time.Duration, ok bool) {
	if len(payload) < 4 {
		return 0, false
	}
	v := binary.BigEndian.Uint32(payload)
	if v == ServerRTTUnknown {
		return 0, false
	}
	return time.Duration(v) * time.Microsecond, true
}

// ServerLeg is what an egress relay reports with each probe reply about its
// own leg to the game server the probe named. A value counts only when its
// Has flag is set, so the zero ServerLeg means "nothing known" and a field
// an older relay doesn't send can never read as a measured 0.
type ServerLeg struct {
	RTT     time.Duration // smoothed round trip time
	LossPct float64       // share of the relay's recent pings lost, 0..100
	Jitter  time.Duration // RFC 3550 jitter of those pings' round trips

	HasRTT, HasLoss, HasJitter bool
}

// A ServerLeg block, the ProbeReply payload, is big endian:
//
//	rtt     u32  microseconds; ServerRTTUnknown = unknown
//	loss    u16  hundredths of a percent (0..10000); ServerLossUnknown = unknown
//	jitter  u32  microseconds; ServerRTTUnknown = unknown
//
// Compatibility rests on length alone. Fields are only ever appended; a
// reader decodes the fields the payload is long enough for, ignores any
// bytes after them, and takes a field that isn't there as unknown. So a
// client that predates loss and jitter (ParseServerRTT) reads the RTT and
// skips the rest, and a new client reading an older relay's 4-byte reply
// gets the RTT with loss and jitter unknown. A version byte would only
// matter for a change that isn't an appended field.
const (
	ServerLegLen      = 4 + 2 + 4
	ServerLossUnknown = ^uint16(0)
	lossUnits         = 100 // loss field units per percent
)

// AppendServerLeg encodes l as a ServerLeg block (ServerLegLen bytes).
func AppendServerLeg(dst []byte, l ServerLeg) []byte {
	rtt, jitter := time.Duration(-1), time.Duration(-1)
	if l.HasRTT {
		rtt = l.RTT
	}
	if l.HasJitter {
		jitter = l.Jitter
	}
	loss := ServerLossUnknown
	if l.HasLoss && !math.IsNaN(l.LossPct) {
		loss = uint16(math.Round(min(max(l.LossPct, 0), 100) * lossUnits))
	}
	dst = AppendServerRTT(dst, rtt)
	dst = binary.BigEndian.AppendUint16(dst, loss)
	return AppendServerRTT(dst, jitter) // same encoding as the RTT
}

// ParseServerLeg decodes a ProbeReply payload of any length: a field that is
// missing (an older relay, a probe that named no server), reported unknown
// or out of range comes back without its Has flag.
func ParseServerLeg(payload []byte) ServerLeg {
	var l ServerLeg
	l.RTT, l.HasRTT = ParseServerRTT(payload)
	if len(payload) >= 6 {
		if v := binary.BigEndian.Uint16(payload[4:]); v <= 100*lossUnits {
			l.LossPct, l.HasLoss = float64(v)/lossUnits, true
		}
	}
	if len(payload) >= ServerLegLen {
		l.Jitter, l.HasJitter = ParseServerRTT(payload[6:])
	}
	return l
}
