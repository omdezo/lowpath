package diag

import (
	"math/rand"
	"testing"
)

// Operation elements as access points send them (see wifi_ies.go).

// htOp is an HT Operation element: secondary channel offset 1 (above) or
// 3 (below) with wide set allows 40 MHz; ccfs2 is the 160 MHz center for
// clients that can't do it with every stream.
func htOp(primary, offset byte, wide bool, ccfs2 byte) []byte {
	b := make([]byte, 2+htOperationLen)
	b[0], b[1], b[2] = elementHTOperation, htOperationLen, primary
	b[3] = offset & 0x03
	if wide {
		b[3] |= 0x04
	}
	v := uint16(ccfs2) << 5
	b[4], b[5] = byte(v), byte(v>>8)
	return b
}

func vhtOp(width, c0, c1 byte) []byte {
	return []byte{elementVHTOperation, vhtOperationLen, width, c0, c1, 0xfc, 0xff}
}

// heOp6 is an HE Operation element with 6 GHz Operation Information
// (width 0-3: 20, 40, 80, 160 MHz).
func heOp6(primary, width, c0, c1 byte) []byte {
	return []byte{elementExtension, 1 + heFixedLen + he6GHzInfoLen, extHEOperation,
		0, 0, 0x02, // parameters: bit 17, 6 GHz Operation Information present
		0x01, 0xfc, 0xff, // BSS color, basic HE-MCS
		primary, width, c0, c1, 6}
}

// ehtOp is an EHT Operation element with EHT Operation Information (width
// 0-4: 20, 40, 80, 160, 320 MHz).
func ehtOp(width, c0, c1 byte) []byte {
	return []byte{elementExtension, 1 + ehtFixedLen + ehtInfoLen, extEHTOperation,
		0x01, 0x44, 0x44, 0x44, 0x44, // parameters (information present), basic EHT-MCS
		width, c0, c1}
}

func ies(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func span(lo, hi int) ChannelSpan { return ChannelSpan{lo, hi} }

func TestBSSSpan(t *testing.T) {
	ssid := []byte{0, 4, 'h', 'o', 'm', 'e'}
	for _, c := range []struct {
		name    string
		primary int
		ies     []byte
		want    ChannelSpan
	}{
		// Channel 36 (5170-5190 MHz) and its neighbours at every width.
		{"no elements", 5180, nil, span(5170, 5190)},
		{"20 MHz only", 5180, ies(ssid, htOp(36, 1, false, 0)), span(5170, 5190)},
		{"40 above", 5180, htOp(36, 1, true, 0), span(5170, 5210)},
		{"40 below", 5200, htOp(40, 3, true, 0), span(5170, 5210)},
		{"80", 5180, ies(htOp(36, 1, true, 0), vhtOp(1, 42, 0)), span(5170, 5250)},
		{"80 from 44", 5220, ies(htOp(44, 1, true, 0), vhtOp(1, 42, 0)), span(5170, 5250)},
		{"160", 5180, ies(htOp(36, 1, true, 0), vhtOp(1, 42, 50)), span(5170, 5330)},
		{"160 by HT CCFS2", 5180, ies(htOp(36, 1, true, 50), vhtOp(1, 42, 0)), span(5170, 5330)},
		{"160 deprecated", 5180, vhtOp(2, 50, 0), span(5170, 5330)},
		{"80+80: primary segment", 5180, vhtOp(1, 42, 106), span(5170, 5250)},
		{"80 on 52-64", 5260, vhtOp(1, 58, 0), span(5250, 5330)},
		{"VHT width 0 leaves HT's 40", 5180, ies(htOp(36, 1, true, 0), vhtOp(0, 0, 0)), span(5170, 5210)},
		{"EHT 160 on 5 GHz", 5180, ies(vhtOp(1, 42, 0), ehtOp(3, 42, 50)), span(5170, 5330)},
		{"2.4 GHz 40 below", 2462, htOp(11, 3, true, 0), span(2432, 2472)},

		// 6 GHz: HE's 6 GHz Operation Information, EHT for 320 MHz.
		{"6 GHz 20", 6135, nil, span(6125, 6145)},
		{"6 GHz 80", 6135, heOp6(37, 2, 39, 0), span(6105, 6185)},
		{"6 GHz 160", 6135, heOp6(37, 3, 39, 47), span(6105, 6265)},
		{"6 GHz 40", 6135, heOp6(37, 1, 35, 0), span(6105, 6145)},
		{"6 GHz 320-1", 6135, ies(heOp6(37, 3, 39, 47), ehtOp(4, 47, 31)), span(5945, 6265)},
		{"6 GHz 320-2", 6135, ies(heOp6(37, 3, 39, 47), ehtOp(4, 47, 63)), span(6105, 6425)},
		{"EHT without its information keeps HE", 6135,
			ies(heOp6(37, 2, 39, 0), []byte{elementExtension, 6, extEHTOperation, 0, 0x44, 0x44, 0x44, 0x44}), span(6105, 6185)},

		// Malformed, reserved or inconsistent: the next element, or 20 MHz.
		{"HT truncated", 5180, []byte{elementHTOperation, 4, 36, 0x05, 0, 0}, span(5170, 5190)},
		{"HT for another channel", 5180, htOp(40, 1, true, 0), span(5170, 5190)},
		{"HT reserved offset", 5180, htOp(36, 2, true, 0), span(5170, 5190)},
		{"VHT center away from the primary", 5180, ies(htOp(36, 1, true, 0), vhtOp(1, 58, 0)), span(5170, 5210)},
		{"VHT reserved spacing", 5180, ies(htOp(36, 1, true, 0), vhtOp(1, 42, 46)), span(5170, 5210)},
		{"VHT short", 5180, []byte{elementVHTOperation, 2, 1, 42}, span(5170, 5190)},
		{"VHT on 2.4 GHz", 2437, vhtOp(1, 42, 0), span(2427, 2447)},
		{"element past the end", 5180, ies(htOp(36, 1, true, 0)[:10]), span(5170, 5190)},
		{"elements before a truncated one count", 5180, ies(htOp(36, 1, true, 0), []byte{elementVHTOperation, 9, 1}), span(5170, 5210)},
		{"HE 6 GHz bit without the bytes", 6135, []byte{elementExtension, 7, extHEOperation, 0, 0, 0x02, 0x01, 0xfc, 0xff}, span(6125, 6145)},
		{"HE element past the end", 6135, heOp6(37, 2, 39, 0)[:9], span(6125, 6145)},
		{"HE 6 GHz for another channel", 6135, heOp6(33, 2, 39, 0), span(6125, 6145)},
		{"HE 6 GHz on 5 GHz", 5180, heOp6(36, 2, 42, 0), span(5170, 5190)},
		{"EHT 320 on 5 GHz", 5180, ies(vhtOp(1, 42, 0), ehtOp(4, 42, 50)), span(5170, 5250)},
		{"EHT reserved width", 6135, ies(heOp6(37, 2, 39, 0), ehtOp(5, 39, 47)), span(6105, 6185)},
		{"EHT center of channel 0", 6135, ehtOp(3, 39, 0), span(6125, 6145)},

		{"not a Wi-Fi channel", 3000, htOp(36, 1, true, 0), ChannelSpan{}},
		{"no frequency", 0, nil, ChannelSpan{}},
	} {
		if got := BSSSpan(c.primary, c.ies); got != c.want {
			t.Errorf("%s: BSSSpan(%d) = %v, want %v", c.name, c.primary, got, c.want)
		}
	}
}

// Elements come from the air: no input may panic, and whatever comes out
// holds the primary channel.
func TestBSSSpanHostileElements(t *testing.T) {
	good := ies(htOp(36, 1, true, 50), vhtOp(1, 42, 50), heOp6(37, 3, 39, 47), ehtOp(4, 47, 63))
	check := func(primary int, b []byte) {
		t.Helper()
		s := BSSSpan(primary, b)
		if !s.contains(primarySpan(primary)) {
			t.Fatalf("BSSSpan(%d, % x) = %v: misses the primary", primary, b, s)
		}
	}
	for i := range good {
		check(5180, good[:i])
		check(6135, good[i:])
	}
	r := rand.New(rand.NewSource(157))
	for range 20000 {
		b := make([]byte, r.Intn(48))
		for i := range b {
			b[i] = byte(r.Intn(256))
		}
		if len(b) > 2 && r.Intn(2) == 0 {
			b[0] = []byte{elementHTOperation, elementVHTOperation, elementExtension}[r.Intn(3)]
		}
		if len(b) > 3 && b[0] == elementExtension {
			b[2] = []byte{extHEOperation, extEHTOperation}[r.Intn(2)]
		}
		check([]int{2437, 5180, 5745, 6135}[r.Intn(4)], b)
	}
}

func TestChannelBlock(t *testing.T) {
	for _, c := range []struct {
		mhz, w int
		want   ChannelSpan
		ok     bool
	}{
		{5180, 40, span(5170, 5210), true}, {5220, 40, span(5210, 5250), true},
		{5180, 80, span(5170, 5250), true}, {5240, 80, span(5170, 5250), true},
		{5210, 80, span(5170, 5250), true}, // the center, as some systems report it
		{5260, 80, span(5250, 5330), true}, {5720, 80, span(5650, 5730), true},
		{5180, 160, span(5170, 5330), true}, {5500, 160, span(5490, 5650), true},
		{5745, 80, span(5735, 5815), true}, {5825, 80, span(5815, 5895), true},
		{5745, 160, span(5735, 5895), true}, {5765, 40, span(5735, 5775), true},
		{5955, 80, span(5945, 6025), true}, {6135, 160, span(6105, 6265), true},
		{7115, 40, span(7105, 7145), false}, // past channel 233's edge
		{5720, 160, ChannelSpan{}, false},   // no 160 MHz channel holds 144
		{2437, 40, ChannelSpan{}, false},    // 2.4 GHz: up or down isn't fixed
		{4920, 40, ChannelSpan{}, false},
		{5180, 320, ChannelSpan{}, false},
		{5180, 20, ChannelSpan{}, false},
	} {
		got, ok := channelBlock(c.mhz, c.w)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("channelBlock(%d, %d) = %v, %v; want %v, %v", c.mhz, c.w, got, ok, c.want, c.ok)
		}
	}
}

func TestLinkSpan(t *testing.T) {
	w := func(v int) *int { return &v }
	for _, c := range []struct {
		name  string
		mhz   int
		width *int
		ap    ChannelSpan
		want  ChannelSpan
	}{
		{"80 from the primary", 5180, w(80), ChannelSpan{}, span(5170, 5250)},
		{"80 from the center", 5210, w(80), ChannelSpan{}, span(5170, 5250)},
		{"160", 5180, w(160), ChannelSpan{}, span(5170, 5330)},
		{"20", 5180, w(20), span(5170, 5250), span(5170, 5190)},
		{"width unknown, nothing heard", 5180, nil, ChannelSpan{}, span(5170, 5190)},
		{"width unknown: the access point's", 5180, nil, span(5170, 5250), span(5170, 5250)},
		{"narrower than the access point", 5180, w(80), span(5170, 5330), span(5170, 5250)},
		{"access point elsewhere", 5180, w(80), span(5250, 5330), span(5170, 5250)},
		{"2.4 GHz 40 without its direction", 2437, w(40), ChannelSpan{}, span(2427, 2447)},
		{"2.4 GHz 40 from the access point", 2437, w(40), span(2427, 2467), span(2427, 2467)},
		{"320 without EHT: the shared 160", 6135, w(320), ChannelSpan{}, span(6105, 6265)},
		{"320 from the access point", 6135, w(320), span(6105, 6425), span(6105, 6425)},
		{"no frequency", 0, w(80), ChannelSpan{}, ChannelSpan{}},
	} {
		if got := linkSpan(c.mhz, c.width, c.ap); got != c.want {
			t.Errorf("%s: linkSpan(%d) = %v, want %v", c.name, c.mhz, got, c.want)
		}
	}
}

func TestChannelSpanOverlaps(t *testing.T) {
	ch := func(mhz int) ChannelSpan { return primarySpan(mhz).reach() }
	if span(5170, 5190).Overlaps(span(5190, 5210)) {
		t.Error("channels 36 and 40 only touch")
	}
	if !span(5170, 5250).Overlaps(span(5210, 5230)) || !span(5210, 5230).Overlaps(span(5170, 5250)) {
		t.Error("44 inside 36-48")
	}
	if (ChannelSpan{}).Overlaps(span(5170, 5190)) {
		t.Error("unknown span overlaps")
	}
	// 2.4 GHz: channel 6 reaches 2 to 10, not 1 or 11.
	for c, want := range map[int]bool{1: false, 2: true, 5: true, 6: true, 7: true, 10: true, 11: false} {
		if got := ch(2437).Overlaps(ch(2407 + 5*c)); got != want {
			t.Errorf("2.4 GHz channels 6 and %d: %v", c, got)
		}
	}
}

// The acceptance for #157: on an 80 MHz link on channels 36-48, a network
// on channel 44 shares the link's airtime and one on 52 doesn't.
func TestNewWiFiSurveyWideLink(t *testing.T) {
	w := func(v int) *int { return &v }
	ap := func(last byte) [6]byte { return [6]byte{0x02, 0, 0x5e, 0, 0x01, last} }
	own80 := ies(bssLoadIE(4, 40, 0), htOp(36, 1, true, 0), vhtOp(1, 42, 0))
	entries := []BSSEntry{
		{BSSID: ownAP, FreqMHz: 5180, RSSI: -50, IEs: own80},
		{BSSID: ap(1), FreqMHz: 5180, RSSI: -70},                                                   // 36: same primary
		{BSSID: ap(2), FreqMHz: 5220, RSSI: -60},                                                   // 44, 20 MHz: overlapping
		{BSSID: ap(3), FreqMHz: 5260, RSSI: -55},                                                   // 52, 20 MHz: no
		{BSSID: ap(4), FreqMHz: 5260, RSSI: -55, IEs: ies(htOp(52, 1, true, 0), vhtOp(1, 58, 0))},  // 52-64: no
		{BSSID: ap(5), FreqMHz: 5320, RSSI: -75, IEs: ies(htOp(64, 3, true, 0), vhtOp(1, 58, 50))}, // 36-64 at 160: overlapping
		{BSSID: ap(6), FreqMHz: 5500, RSSI: -80, IEs: vhtOp(1, 106, 0)},                            // 100-112: no
		{BSSID: ap(7), FreqMHz: 2437, RSSI: -45},                                                   // another band
	}
	s := NewWiFiSurvey(vNow, ownAP, 5180, w(80), entries)
	if s.Networks != 8 || s.SameChannel != 1 || s.Overlapping != 2 || *s.LoudestNeighbor != -70 || s.Load == nil {
		t.Errorf("80 MHz link: %+v", s)
	}

	// The width unknown (before Windows 11 24H2): the access point's own
	// elements say 80 MHz.
	if s := NewWiFiSurvey(vNow, ownAP, 5180, nil, entries); s.SameChannel != 1 || s.Overlapping != 2 {
		t.Errorf("access point's width: same %d, overlapping %d", s.SameChannel, s.Overlapping)
	}
	// Nothing says it is wide: only networks reaching over channel 36
	// count, not 44.
	plain := append([]BSSEntry{{BSSID: ownAP, FreqMHz: 5180}}, entries[1:]...)
	if s := NewWiFiSurvey(vNow, ownAP, 5180, nil, plain); s.SameChannel != 1 || s.Overlapping != 1 {
		t.Errorf("20 MHz link: same %d, overlapping %d", s.SameChannel, s.Overlapping)
	}
	// A 160 MHz link takes in 52 as well (36-64).
	if s := NewWiFiSurvey(vNow, ownAP, 5180, w(160), entries); s.SameChannel != 1 || s.Overlapping != 4 {
		t.Errorf("160 MHz link: same %d, overlapping %d", s.SameChannel, s.Overlapping)
	}
	// The link's frequency is the center of its 80 MHz channel (5210) and
	// its access point isn't in the scan: the block still holds 36 to 48.
	if s := NewWiFiSurvey(vNow, ownAP, 5210, w(80), entries[1:]); s.SameChannel != 0 || s.Overlapping != 3 {
		t.Errorf("link center: same %d, overlapping %d", s.SameChannel, s.Overlapping)
	}

	// 6 GHz: a 160 MHz link on 37 (33-61) shares airtime with 33 and 53,
	// not with 65.
	six := []BSSEntry{
		{BSSID: ownAP, FreqMHz: 6135, IEs: heOp6(37, 3, 39, 47)},
		{BSSID: ap(1), FreqMHz: 6115, IEs: heOp6(33, 2, 39, 0)},
		{BSSID: ap(2), FreqMHz: 6215},
		{BSSID: ap(3), FreqMHz: 6275, IEs: heOp6(65, 2, 71, 0)},
	}
	if s := NewWiFiSurvey(vNow, ownAP, 6135, w(160), six); s.SameChannel != 0 || s.Overlapping != 2 {
		t.Errorf("6 GHz: same %d, overlapping %d", s.SameChannel, s.Overlapping)
	}

	// 2.4 GHz: a 40 MHz network on 11 reaching down over 6 now counts.
	two := []BSSEntry{
		{BSSID: ownAP, FreqMHz: 2437},
		{BSSID: ap(1), FreqMHz: 2462, IEs: htOp(11, 3, true, 0)},
		{BSSID: ap(2), FreqMHz: 2462},
	}
	if s := NewWiFiSurvey(vNow, ownAP, 2437, w(20), two); s.Overlapping != 1 {
		t.Errorf("2.4 GHz 40 MHz neighbour: overlapping %d", s.Overlapping)
	}
}

// On a multi-link connection the width is the one of the link on the
// surveyed channel, not the strongest link's.
func TestWiFiLinkWidthAt(t *testing.T) {
	w := func(v int) *int { return &v }
	mlo := WiFiLink{FreqMHz: w(6115), WidthMHz: w(320), Links: []RadioLink{
		{FreqMHz: 5570, WidthMHz: w(160)}, // channels 100-128, by its center
		{FreqMHz: 6115, WidthMHz: w(320)},
	}}
	if got := mlo.widthAt(5500); got == nil || *got != 160 {
		t.Errorf("5 GHz link: %v", got)
	}
	if got := mlo.widthAt(6135); got == nil || *got != 320 {
		t.Errorf("6 GHz link: %v", got)
	}
	if got := mlo.widthAt(2437); got != nil {
		t.Errorf("no link on 2.4 GHz: %v", *got)
	}
	single := WiFiLink{FreqMHz: w(5180), WidthMHz: w(80)}
	if got := single.widthAt(5180); got == nil || *got != 80 {
		t.Errorf("single link: %v", got)
	}
	if got := single.widthAt(0); got != nil {
		t.Errorf("no frequency: %v", *got)
	}
}
