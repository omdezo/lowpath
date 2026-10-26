// Package stun is the part of STUN (RFC 5389) Lowpath needs: a Binding
// request, its response, and a client that asks a server which public
// address it sees. lpcheck uses it to show which route a game takes; the
// diagnostics use it to find the user's public IP.
package stun

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// The subset of STUN (RFC 5389) needed to ask a server "what address do you
// see me as?": a Binding request and the address attributes of its response.
const (
	HeaderLen   = 20         // bytes in a message header (and in a Binding request)
	MagicCookie = 0x2112A442 // fixed header field that marks RFC 5389 messages

	typeBindingRequest = 0x0001
	typeBindingSuccess = 0x0101
	typeBindingError   = 0x0111

	attrMappedAddress    = 0x0001
	attrErrorCode        = 0x0009
	attrXorMappedAddress = 0x0020

	familyIPv4 = 0x01
	familyIPv6 = 0x02
)

// TxID is a transaction id; a response carries the id of its request.
type TxID [12]byte

var (
	ErrNotSTUN   = errors.New("stun: not a STUN message")
	ErrNotReply  = errors.New("stun: not a Binding response")
	ErrMalformed = errors.New("stun: malformed attribute")
	ErrNoAddress = errors.New("stun: response has no mapped address")
)

// AppendBindingRequest encodes a Binding request with no attributes.
func AppendBindingRequest(dst []byte, id TxID) []byte {
	dst = binary.BigEndian.AppendUint16(dst, typeBindingRequest)
	dst = binary.BigEndian.AppendUint16(dst, 0) // message length: no attributes
	dst = binary.BigEndian.AppendUint32(dst, MagicCookie)
	return append(dst, id[:]...)
}

// ParseBindingResponse decodes a Binding success response and returns its
// transaction id and the address the server saw. XOR-MAPPED-ADDRESS is
// preferred; MAPPED-ADDRESS is the fallback for servers that only send that
// (it is not XORed, so NATs that rewrite payloads can corrupt it).
func ParseBindingResponse(b []byte) (TxID, netip.AddrPort, error) {
	var id TxID
	if len(b) < HeaderLen || b[0]&0xC0 != 0 ||
		binary.BigEndian.Uint32(b[4:8]) != MagicCookie {
		return id, netip.AddrPort{}, ErrNotSTUN
	}
	msgType := binary.BigEndian.Uint16(b[0:2])
	msgLen := int(binary.BigEndian.Uint16(b[2:4]))
	if msgLen%4 != 0 || HeaderLen+msgLen > len(b) {
		return id, netip.AddrPort{}, ErrNotSTUN
	}
	copy(id[:], b[8:20])
	attrs := b[HeaderLen : HeaderLen+msgLen]

	switch msgType {
	case typeBindingSuccess:
	case typeBindingError:
		return id, netip.AddrPort{}, errorResponse(attrs)
	default:
		return id, netip.AddrPort{}, ErrNotReply
	}

	var mapped netip.AddrPort
	for len(attrs) > 0 {
		if len(attrs) < 4 {
			return id, netip.AddrPort{}, ErrMalformed
		}
		t := binary.BigEndian.Uint16(attrs[0:2])
		l := int(binary.BigEndian.Uint16(attrs[2:4]))
		if 4+l > len(attrs) {
			return id, netip.AddrPort{}, ErrMalformed
		}
		v := attrs[4 : 4+l]
		switch t {
		case attrXorMappedAddress:
			ap, err := decodeAddress(v, true, id)
			return id, ap, err
		case attrMappedAddress:
			ap, err := decodeAddress(v, false, id)
			if err != nil {
				return id, netip.AddrPort{}, err
			}
			mapped = ap
		}
		attrs = attrs[min(4+(l+3)&^3, len(attrs)):] // values are padded to 4 bytes
	}
	if !mapped.IsValid() {
		return id, netip.AddrPort{}, ErrNoAddress
	}
	return id, mapped, nil
}

// decodeAddress decodes a (XOR-)MAPPED-ADDRESS value. XORing with the magic
// cookie (and the transaction id for IPv6) keeps NATs that rewrite IPs inside
// payloads from "fixing" the address the server reports.
func decodeAddress(v []byte, xor bool, id TxID) (netip.AddrPort, error) {
	if len(v) < 4 {
		return netip.AddrPort{}, ErrMalformed
	}
	port := binary.BigEndian.Uint16(v[2:4])
	var key [16]byte
	binary.BigEndian.PutUint32(key[0:4], MagicCookie)
	copy(key[4:], id[:])
	if xor {
		port ^= MagicCookie >> 16
	}
	var ip []byte
	switch v[1] {
	case familyIPv4:
		if len(v) < 8 {
			return netip.AddrPort{}, ErrMalformed
		}
		ip = append(ip, v[4:8]...)
	case familyIPv6:
		if len(v) < 20 {
			return netip.AddrPort{}, ErrMalformed
		}
		ip = append(ip, v[4:20]...)
	default:
		return netip.AddrPort{}, ErrMalformed
	}
	if xor {
		for i := range ip {
			ip[i] ^= key[i]
		}
	}
	a, _ := netip.AddrFromSlice(ip)
	return netip.AddrPortFrom(a, port), nil
}

// errorResponse turns a Binding error response into a readable error.
func errorResponse(attrs []byte) error {
	for len(attrs) >= 4 {
		t := binary.BigEndian.Uint16(attrs[0:2])
		l := int(binary.BigEndian.Uint16(attrs[2:4]))
		if 4+l > len(attrs) {
			break
		}
		if v := attrs[4 : 4+l]; t == attrErrorCode && l >= 4 {
			code := int(v[2]&0x07)*100 + int(v[3])
			reason := strings.ToValidUTF8(string(v[4:]), "?")
			return fmt.Errorf("stun: server error %d %s", code, reason)
		}
		attrs = attrs[min(4+(l+3)&^3, len(attrs)):]
	}
	return errors.New("stun: server returned an error response")
}
