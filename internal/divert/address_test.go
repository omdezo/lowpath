package divert

import (
	"testing"
	"unsafe"
)

func TestAddressLayout(t *testing.T) {
	var a Address
	if s := unsafe.Sizeof(a); s != 80 {
		t.Fatalf("sizeof(Address) = %d, want 80 (WINDIVERT_ADDRESS)", s)
	}
	if o := unsafe.Offsetof(a.bits); o != 8 {
		t.Fatalf("bitfield offset = %d, want 8", o)
	}
	if o := unsafe.Offsetof(a.data); o != 16 {
		t.Fatalf("union offset = %d, want 16", o)
	}
}

func TestAddressBits(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Address)
		get  func(*Address) bool
		bit  uint32
	}{
		{"outbound", func(a *Address) { a.SetOutbound(true) }, (*Address).Outbound, 1 << 17},
		{"loopback", func(a *Address) { a.SetLoopback(true) }, (*Address).Loopback, 1 << 18},
		{"impostor", func(a *Address) { a.SetImpostor(true) }, (*Address).Impostor, 1 << 19},
		{"ipv6", func(a *Address) { a.SetIPv6(true) }, (*Address).IPv6, 1 << 20},
	}
	for _, c := range cases {
		var a Address
		c.set(&a)
		if a.bits != c.bit || !c.get(&a) {
			t.Errorf("%s: bits = %#x, want %#x", c.name, a.bits, c.bit)
		}
	}

	a := Address{bits: 0x02_03 | 1<<16 | 1<<21 | 1<<22 | 1<<23} // event 2, layer 3
	if a.Layer() != LayerSocket || a.Event() != 2 || !a.Sniffed() {
		t.Errorf("layer/event/sniffed = %d/%d/%v", a.Layer(), a.Event(), a.Sniffed())
	}
	if !a.IPChecksum() || !a.TCPChecksum() || !a.UDPChecksum() {
		t.Error("checksum flags not read from bits 21-23")
	}
	a.SetOutbound(true)
	a.SetOutbound(false)
	a.SetLayer(LayerNetwork)
	if a.bits != 1<<16|1<<21|1<<22|1<<23 {
		t.Errorf("SetOutbound/SetLayer touched other bits: %#x", a.bits)
	}

	a.SetNetwork(0x01020304, 7)
	if a.IfIdx() != 0x01020304 || a.SubIfIdx() != 7 {
		t.Fatalf("IfIdx/SubIfIdx = %#x/%d", a.IfIdx(), a.SubIfIdx())
	}
	if a.data[0] != 0x04 || a.data[4] != 7 { // little-endian UINT32s
		t.Fatalf("union bytes = % x", a.data[:8])
	}
}
