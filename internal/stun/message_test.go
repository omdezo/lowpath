package stun

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
)

// Sample responses from RFC 5769 sections 2.2 and 2.3 (SOFTWARE,
// XOR-MAPPED-ADDRESS, MESSAGE-INTEGRITY and FINGERPRINT attributes).
const (
	rfc5769IPv4 = "0101003c2112a442b7e7a701bc34d686fa87dfae" +
		"8022000b7465737420766563746f7220" +
		"002000080001a147e112a643" +
		"000800142b91f599fd9e90c38c7489f92af9ba53f06be7d7" +
		"80280004c07d4c96"
	rfc5769IPv6 = "010100482112a442b7e7a701bc34d686fa87dfae" +
		"8022000b7465737420766563746f7220" +
		"002000140002a1470113a9faa5d3f179bc25f4b5bed2b9d9" +
		"00080014a382954e4be67bf11784c97c8292c275bfe3ed41" +
		"80280004c8fb0b4c"
)

var rfc5769ID = TxID{0xb7, 0xe7, 0xa7, 0x01, 0xbc, 0x34, 0xd6, 0x86, 0xfa, 0x87, 0xdf, 0xae}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBindingRequestEncoding(t *testing.T) {
	id := TxID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	got := AppendBindingRequest(nil, id)
	want := []byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
}

func TestParseRFC5769Vectors(t *testing.T) {
	for name, tc := range map[string]struct {
		msg  string
		want string
	}{
		"ipv4": {rfc5769IPv4, "192.0.2.1:32853"},
		"ipv6": {rfc5769IPv6, "[2001:db8:1234:5678:11:2233:4455:6677]:32853"},
	} {
		id, ap, err := ParseBindingResponse(mustHex(t, tc.msg))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if id != rfc5769ID {
			t.Fatalf("%s: transaction id % x", name, id)
		}
		if ap != netip.MustParseAddrPort(tc.want) {
			t.Fatalf("%s: mapped %v, want %v", name, ap, tc.want)
		}
	}
}

// response builds a success response carrying the given attributes.
func response(id TxID, msgType uint16, attrs ...[]byte) []byte {
	var body []byte
	for _, a := range attrs {
		body = append(body, a...)
	}
	b := binary.BigEndian.AppendUint16(nil, msgType)
	b = binary.BigEndian.AppendUint16(b, uint16(len(body)))
	b = binary.BigEndian.AppendUint32(b, MagicCookie)
	b = append(b, id[:]...)
	return append(b, body...)
}

func attr(t uint16, v []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, t)
	b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	b = append(b, v...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func TestParseMappedAddressFallback(t *testing.T) {
	id := TxID{9}
	plain := []byte{0, familyIPv4, 0x1F, 0x90, 198, 51, 100, 7} // 198.51.100.7:8080, not XORed
	software := attr(0x8022, []byte("odd"))                     // 3 bytes + 1 padding
	_, ap, err := ParseBindingResponse(response(id, typeBindingSuccess, software, attr(attrMappedAddress, plain)))
	if err != nil || ap != netip.MustParseAddrPort("198.51.100.7:8080") {
		t.Fatalf("got %v, %v", ap, err)
	}

	// With both present the XORed one wins, whatever the order.
	xored := mustHex(t, "0001a147e112a643") // 192.0.2.1:32853 under the RFC 5769 id
	_, ap, err = ParseBindingResponse(response(rfc5769ID, typeBindingSuccess, attr(attrMappedAddress, plain), attr(attrXorMappedAddress, xored)))
	if err != nil || ap != netip.MustParseAddrPort("192.0.2.1:32853") {
		t.Fatalf("got %v, %v", ap, err)
	}
}

func TestParseErrors(t *testing.T) {
	id := TxID{1}
	errCode := attr(attrErrorCode, append([]byte{0, 0, 4, 20}, "Unknown Attribute"...))
	_, _, err := ParseBindingResponse(response(id, typeBindingError, errCode))
	if err == nil || !strings.Contains(err.Error(), "420 Unknown Attribute") {
		t.Fatalf("error response: %v", err)
	}
	if _, _, err := ParseBindingResponse(response(id, typeBindingSuccess)); err != ErrNoAddress {
		t.Fatalf("no address: %v", err)
	}
	if _, _, err := ParseBindingResponse(AppendBindingRequest(nil, id)); err != ErrNotReply {
		t.Fatalf("request parsed as response: %v", err)
	}
	bad := mustHex(t, rfc5769IPv4)
	bad[4] ^= 0xFF
	if _, _, err := ParseBindingResponse(bad); err != ErrNotSTUN {
		t.Fatalf("wrong cookie: %v", err)
	}
	unknownFamily := []byte{0, 9, 0, 1, 1, 2, 3, 4}
	if _, _, err := ParseBindingResponse(response(id, typeBindingSuccess, attr(attrXorMappedAddress, unknownFamily))); err != ErrMalformed {
		t.Fatalf("unknown family: %v", err)
	}
}

func TestParseNeverPanicsOnTruncation(t *testing.T) {
	for _, msg := range []string{rfc5769IPv4, rfc5769IPv6} {
		full := mustHex(t, msg)
		for n := range len(full) {
			ParseBindingResponse(full[:n])
		}
		// Claim a longer attribute than the message holds.
		b := append([]byte(nil), full...)
		binary.BigEndian.PutUint16(b[22:24], 0x0100)
		if _, _, err := ParseBindingResponse(b); err != ErrMalformed {
			t.Fatalf("overlong attribute: %v", err)
		}
	}
}
