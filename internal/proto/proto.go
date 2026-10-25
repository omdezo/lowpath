// Package proto defines the Lowpath UDP overlay framing used between the
// client engine and the relays.
//
// Wire layout (big endian):
//
//	magic   u8   0xA7
//	type    u8   Data / Probe / ProbeReply
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
// with a ProbeReply whose payload is a ServerRTT block (see AppendServerRTT),
// so the client can add the relay->server leg to the path RTT.
package proto

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/netip"
	"time"
)

const Magic = 0xA7

type Type uint8

const (
	TData       Type = 1
	TProbe      Type = 2
	TProbeReply Type = 3
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
