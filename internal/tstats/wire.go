package tstats

import (
	"encoding/binary"
	"errors"
)

// The payload of a TStats packet (proto.TStats), big endian:
//
//	version u8   1
//	self    u8   the lane this copy was sent to, 0xFF = none
//	dir     u8   Up / Down
//	lanes   u8   number of lane blocks
//	pairs   u8   number of pair blocks
//	seqs    u32
//	lost    u32
//	lane    index u8, id u32, expected u32, lost u32, late u32,
//	        n00 u32, n01 u32, n10 u32, n11 u32, jitter_us u32   (37 bytes each)
//	pair    a u8, b u8, both u32, lost_a u32, lost_b u32, joint u32  (18 bytes each)
//
// A reader ignores bytes after the blocks, so a later version can append
// fields. With MaxLanes lanes and every pair it is 813 bytes.
const (
	wireVersion = 1
	headLen     = 5 + 4 + 4
	laneLen     = 1 + 9*4
	pairLen     = 2 + 4*4
	// NoLane is the self value of a report not sent down a lane.
	NoLane = 0xFF
)

var (
	ErrShort   = errors.New("tstats: payload too short")
	ErrVersion = errors.New("tstats: unknown version")
	ErrLane    = errors.New("tstats: lane index out of range")
)

// Append encodes s onto dst as a TStats payload; self is the lane the
// packet goes down (its receiver learns which lane it is from it), or
// NoLane.
func (s Stats) Append(dst []byte, self int) []byte {
	if self < 0 || self >= MaxLanes {
		self = NoLane
	}
	nl, np := min(len(s.Lanes), MaxLanes), min(len(s.Pairs), npairs)
	dst = append(dst, wireVersion, byte(self), byte(s.Dir), byte(nl), byte(np))
	dst = binary.BigEndian.AppendUint32(dst, s.Seqs)
	dst = binary.BigEndian.AppendUint32(dst, s.Lost)
	for _, l := range s.Lanes[:nl] {
		dst = append(dst, l.Index)
		for _, v := range [...]uint32{l.ID, l.Expected, l.Lost, l.Late, l.N00, l.N01, l.N10, l.N11, l.JitterUS} {
			dst = binary.BigEndian.AppendUint32(dst, v)
		}
	}
	for _, p := range s.Pairs[:np] {
		dst = append(dst, p.A, p.B)
		for _, v := range [...]uint32{p.Both, p.LostA, p.LostB, p.Joint} {
			dst = binary.BigEndian.AppendUint32(dst, v)
		}
	}
	return dst
}

// Parse decodes a TStats payload. self is the lane the packet was sent
// down, or NoLane.
func Parse(b []byte) (s Stats, self int, err error) {
	if len(b) < headLen {
		return s, NoLane, ErrShort
	}
	if b[0] != wireVersion {
		return s, NoLane, ErrVersion
	}
	self, s.Dir = int(b[1]), Dir(b[2])
	nl, np := int(b[3]), int(b[4])
	if nl > MaxLanes || np > npairs || (self != NoLane && self >= MaxLanes) {
		return Stats{}, NoLane, ErrLane
	}
	if len(b) < headLen+nl*laneLen+np*pairLen {
		return Stats{}, NoLane, ErrShort
	}
	u32 := func(i int) uint32 { return binary.BigEndian.Uint32(b[i:]) }
	s.Seqs, s.Lost = u32(5), u32(9)
	at := headLen
	for range nl {
		l := Lane{Index: b[at]}
		if l.Index >= MaxLanes {
			return Stats{}, NoLane, ErrLane
		}
		v := at + 1
		l.ID, l.Expected, l.Lost, l.Late = u32(v), u32(v+4), u32(v+8), u32(v+12)
		l.N00, l.N01, l.N10, l.N11, l.JitterUS = u32(v+16), u32(v+20), u32(v+24), u32(v+28), u32(v+32)
		s.Lanes = append(s.Lanes, l)
		at += laneLen
	}
	for range np {
		p := Pair{A: b[at], B: b[at+1]}
		if p.A >= p.B || p.B >= MaxLanes {
			return Stats{}, NoLane, ErrLane
		}
		v := at + 2
		p.Both, p.LostA, p.LostB, p.Joint = u32(v), u32(v+4), u32(v+8), u32(v+12)
		s.Pairs = append(s.Pairs, p)
		at += pairLen
	}
	return s, self, nil
}
