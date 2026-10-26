// Package netlink encodes and decodes Linux netlink messages, the kernel
// interface for routing (rtnetlink) and packet filtering (nftables), without
// cgo or a library.
//
// Building and parsing messages is plain byte work and is portable, so it is
// tested on every OS; only Conn, which talks to the kernel, is Linux-only
// (conn_linux.go). The subpackages build the messages: rtnl for links,
// addresses, routes and policy-routing rules, nft for nftables tables,
// chains, sets and rules.
package netlink

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
)

// Header flags (linux/netlink.h).
const (
	FlagRequest = 0x1
	FlagMulti   = 0x2
	FlagAck     = 0x4
	FlagDump    = 0x300 // NLM_F_ROOT | NLM_F_MATCH

	// For new objects.
	FlagReplace = 0x100
	FlagExcl    = 0x200
	FlagCreate  = 0x400
	FlagAppend  = 0x800

	// In error replies.
	flagCapped  = 0x100
	flagAckTLVs = 0x200
)

// Message types every netlink family shares.
const (
	typeNoop  = 1
	typeError = 2
	typeDone  = 3
)

// HeaderLen is the size of struct nlmsghdr.
const HeaderLen = 16

// attrNested marks an attribute whose payload is more attributes.
const attrNested = 0x8000

// errAttrMsg is NLMSGERR_ATTR_MSG: the kernel's text for an extended ACK.
const errAttrMsg = 1

// Message is one request to the kernel: the header's type and flags, and
// the body after the header.
type Message struct {
	Type  uint16
	Flags uint16
	Data  []byte
	// Op names the request in errors ("add rule").
	Op string
}

// Append encodes m with the given sequence number (port id 0: the kernel
// fills it in) and pads it to 4 bytes, as messages in one buffer must be.
func (m Message) Append(b []byte, seq uint32) []byte {
	n := HeaderLen + len(m.Data)
	b = binary.NativeEndian.AppendUint32(b, uint32(n))
	b = binary.NativeEndian.AppendUint16(b, m.Type)
	b = binary.NativeEndian.AppendUint16(b, m.Flags)
	b = binary.NativeEndian.AppendUint32(b, seq)
	b = binary.NativeEndian.AppendUint32(b, 0)
	b = append(b, m.Data...)
	return append(b, make([]byte, align(n)-n)...)
}

// Reply is one message from the kernel.
type Reply struct {
	Type  uint16
	Flags uint16
	Seq   uint32
	Data  []byte
}

// ParseReplies splits a buffer read from a netlink socket into messages.
func ParseReplies(b []byte) ([]Reply, error) {
	var out []Reply
	for len(b) > 0 {
		if len(b) < HeaderLen {
			return out, errors.New("netlink: truncated message header")
		}
		n := int(binary.NativeEndian.Uint32(b))
		if n < HeaderLen || n > len(b) {
			return out, fmt.Errorf("netlink: message length %d with %d bytes left", n, len(b))
		}
		out = append(out, Reply{
			Type:  binary.NativeEndian.Uint16(b[4:]),
			Flags: binary.NativeEndian.Uint16(b[6:]),
			Seq:   binary.NativeEndian.Uint32(b[8:]),
			Data:  b[HeaderLen:n],
		})
		b = b[min(align(n), len(b)):]
	}
	return out, nil
}

// Error is a request the kernel refused.
type Error struct {
	Op    string        // which request ("add rule")
	Errno syscall.Errno // why, as the kernel said
	Msg   string        // the kernel's extended explanation, if it gave one
}

func (e *Error) Error() string {
	s := "netlink: " + e.Op + ": " + e.Errno.Error()
	if e.Msg != "" {
		s += " (" + e.Msg + ")"
	}
	return s
}

// Unwrap lets errors.Is(err, syscall.ENOENT) and the like work.
func (e *Error) Unwrap() error { return e.Errno }

// ack decodes an NLMSG_ERROR reply: errno 0 is a plain acknowledgement.
func (r Reply) ack() (errno syscall.Errno, msg string, ok bool) {
	if r.Type != typeError || len(r.Data) < 4+HeaderLen {
		return 0, "", false
	}
	code := int32(binary.NativeEndian.Uint32(r.Data))
	if code < 0 {
		errno = syscall.Errno(-code)
	}
	if r.Flags&flagAckTLVs == 0 {
		return errno, "", true
	}
	// The TLVs follow the request: its header only when capped, all of it
	// otherwise.
	rest := r.Data[4:]
	skip := HeaderLen
	if r.Flags&flagCapped == 0 {
		skip = align(int(binary.NativeEndian.Uint32(rest)))
	}
	if skip > len(rest) {
		return errno, "", true
	}
	for _, a := range ParseAttrs(rest[skip:]) {
		if a.Type == errAttrMsg {
			msg = cString(a.Data)
		}
	}
	return errno, msg, true
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func align(n int) int { return (n + 3) &^ 3 }

// Attrs builds a list of netlink attributes (type-length-value, each padded
// to 4 bytes).
type Attrs struct{ b []byte }

// Bytes is the encoded list.
func (a *Attrs) Bytes() []byte { return a.b }

// Add appends one attribute with payload v.
func (a *Attrs) Add(typ uint16, v []byte) {
	n := 4 + len(v)
	a.b = binary.NativeEndian.AppendUint16(a.b, uint16(n))
	a.b = binary.NativeEndian.AppendUint16(a.b, typ)
	a.b = append(a.b, v...)
	a.b = append(a.b, make([]byte, align(n)-n)...)
}

// String appends a NUL-terminated string.
func (a *Attrs) String(typ uint16, s string) { a.Add(typ, append([]byte(s), 0)) }

// U8 appends one byte.
func (a *Attrs) U8(typ uint16, v uint8) { a.Add(typ, []byte{v}) }

// U32 appends a 32-bit value in host byte order (rtnetlink's convention).
func (a *Attrs) U32(typ uint16, v uint32) { a.Add(typ, binary.NativeEndian.AppendUint32(nil, v)) }

// BE32 appends a 32-bit value in network byte order (nftables' convention).
func (a *Attrs) BE32(typ uint16, v uint32) { a.Add(typ, binary.BigEndian.AppendUint32(nil, v)) }

// Nested appends an attribute holding the attributes f adds.
func (a *Attrs) Nested(typ uint16, f func(*Attrs)) {
	var in Attrs
	f(&in)
	a.Add(typ|attrNested, in.b)
}

// Attr is one decoded attribute.
type Attr struct {
	Type uint16 // without the nested and byte-order flags
	Data []byte
}

// ParseAttrs decodes an attribute list, stopping at the first malformed one.
func ParseAttrs(b []byte) []Attr {
	var out []Attr
	for len(b) >= 4 {
		n := int(binary.NativeEndian.Uint16(b))
		if n < 4 || n > len(b) {
			break
		}
		out = append(out, Attr{Type: binary.NativeEndian.Uint16(b[2:]) & 0x3fff, Data: b[4:n]})
		b = b[min(align(n), len(b)):]
	}
	return out
}
