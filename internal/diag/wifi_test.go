package diag

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestChannelOf(t *testing.T) {
	for _, c := range []struct {
		mhz  int
		ch   int
		band string
	}{
		{2412, 1, "2.4"}, {2437, 6, "2.4"}, {2472, 13, "2.4"}, {2484, 14, "2.4"},
		{5180, 36, "5"}, {5500, 100, "5"}, {5825, 165, "5"}, {5885, 177, "5"},
		{4920, 184, "5"}, {4980, 196, "5"},
		{5935, 2, "6"}, {5955, 1, "6"}, {6115, 33, "6"}, {7115, 233, "6"},
		{0, 0, ""}, {2300, 0, ""}, {60480, 0, ""},
	} {
		ch, band := ChannelOf(c.mhz)
		if ch != c.ch || band != c.band {
			t.Errorf("ChannelOf(%d) = %d, %q; want %d, %q", c.mhz, ch, band, c.ch, c.band)
		}
	}
}

// A link may give its whole channel's center (5210 MHz for channels 36 to
// 48 at 80 MHz) where a scan gives the primary channel (5180).
func TestOnChannel(t *testing.T) {
	w := func(v int) *int { return &v }
	for _, c := range []struct {
		link    int
		width   *int
		primary int
		want    bool
	}{
		{5180, nil, 5180, true}, {5180, w(20), 5200, false}, {2437, nil, 2438, true},
		{5210, w(80), 5180, true}, {5210, w(80), 5240, true}, {5210, w(80), 5260, false},
		{5180, w(80), 5180, true}, {5190, w(40), 5180, true}, {5190, w(40), 5220, false},
		{6185, w(160), 6115, true}, {6185, w(160), 6275, false},
		{0, nil, 5180, false}, {5180, nil, 0, false},
	} {
		if got := OnChannel(c.link, c.width, c.primary); got != c.want {
			t.Errorf("OnChannel(%d, %v, %d) = %v", c.link, c.width, c.primary, got)
		}
	}
}

// From the location rule's build on, a background poll never uses the
// current_connection query (it would show Windows' location prompt).
func TestPollsMayQueryConnection(t *testing.T) {
	for build, want := range map[uint32]bool{0: false, 19045: true, 22631: true, 25975: true, 25976: false, 26100: false, 26200: false} {
		if got := pollsMayQueryConnection(build); got != want {
			t.Errorf("build %d: %v, want %v", build, got, want)
		}
	}
}

func TestWiFiUnitHeuristics(t *testing.T) {
	// Rates: kbit/s as documented, Mbit/s when only that fits, absurd = unknown.
	for in, want := range map[uint32]float64{866700: 866.7, 1201000: 1201, 54: 54, 6000: 6} {
		if got := rateMbps(in); got == nil || *got != want {
			t.Errorf("rateMbps(%d) = %v, want %v", in, got, want)
		}
	}
	if rateMbps(0) != nil || rateMbps(0xffffffff) != nil {
		t.Error("zero or absurd rate read as a number")
	}
	if freqMHz(5180000) != 5180 || freqMHz(5180) != 5180 || freqMHz(2412000) != 2412 {
		t.Error("center frequency units")
	}
	if w := widthMHz(80); w == nil || *w != 80 {
		t.Error("80 MHz width")
	}
	if w := widthMHz(160000); w == nil || *w != 160 {
		t.Error("160 MHz width in kHz")
	}
	if widthMHz(0) != nil || widthMHz(30) != nil {
		t.Error("impossible width accepted")
	}
	if validRSSI(-61) == nil || validRSSI(0) != nil || validRSSI(10) != nil || validRSSI(-200) != nil {
		t.Error("RSSI range")
	}
	if qualityRSSI(0) != -100 || qualityRSSI(100) != -50 || qualityRSSI(60) != -70 {
		t.Error("quality to dBm")
	}
	if s, g := phyStandard(10, "6"); s != "802.11ax" || g != "Wi-Fi 6E" {
		t.Errorf("HE on 6 GHz: %s %s", s, g)
	}
	if s, g := phyStandard(11, "5"); s != "802.11be" || g != "Wi-Fi 7" {
		t.Errorf("EHT: %s %s", s, g)
	}
	if s, _ := phyStandard(0x80000001, "5"); s != "" {
		t.Errorf("vendor PHY named %q", s)
	}
}

// bssLoadIE is a BSS Load element: stations, utilization (0-255), capacity.
func bssLoadIE(stations uint16, util byte, capacity uint16) []byte {
	b := []byte{elementBSSLoad, 5, 0, 0, util, 0, 0}
	binary.LittleEndian.PutUint16(b[2:], stations)
	binary.LittleEndian.PutUint16(b[5:], capacity)
	return b
}

func TestParseBSSLoad(t *testing.T) {
	ssid := []byte{0, 4, 'h', 'o', 'm', 'e'}
	ies := append(append(append([]byte{}, ssid...), 1, 2, 0x82, 0x84), bssLoadIE(12, 153, 31250)...)
	l, ok := ParseBSSLoad(ies)
	if !ok || l.Stations != 12 || l.BusyPct != 60 || l.AdmissionCapacity == nil || *l.AdmissionCapacity != 31250 {
		t.Fatalf("got %+v %v", l, ok)
	}
	// An element without the capacity (3 octets) still gives the load.
	if l, ok := ParseBSSLoad([]byte{elementBSSLoad, 3, 2, 0, 255}); !ok || l.Stations != 2 || l.BusyPct != 100 || l.AdmissionCapacity != nil {
		t.Errorf("short element: %+v %v", l, ok)
	}
	for name, bad := range map[string][]byte{
		"none":      ssid,
		"truncated": {0, 4, 'h', 'o', elementBSSLoad, 5, 1},
		"too short": {elementBSSLoad, 2, 1, 0},
		"empty":     nil,
	} {
		if _, ok := ParseBSSLoad(bad); ok {
			t.Errorf("%s: found a load", name)
		}
	}
}

var (
	ownAP   = [6]byte{0x02, 0, 0x5e, 0, 0, 1}
	otherAP = [6]byte{0x02, 0, 0x5e, 0, 0, 2}
)

func TestNewWiFiSurvey(t *testing.T) {
	entries := []BSSEntry{
		{BSSID: ownAP, FreqMHz: 5180, RSSI: -55, IEs: bssLoadIE(9, 64, 0)},
		{BSSID: otherAP, FreqMHz: 5180, RSSI: -72},
		{BSSID: [6]byte{2, 0, 0x5e, 0, 0, 3}, FreqMHz: 5180, RSSI: -64},
		{BSSID: [6]byte{2, 0, 0x5e, 0, 0, 4}, FreqMHz: 5200, RSSI: -50},
		{BSSID: [6]byte{2, 0, 0x5e, 0, 0, 5}, FreqMHz: 2437, RSSI: -40},
	}
	s := NewWiFiSurvey(vNow, ownAP, 5180, entries)
	if s.Networks != 5 || s.SameChannel != 2 || s.Overlapping != 0 || s.LoudestNeighbor == nil || *s.LoudestNeighbor != -64 {
		t.Errorf("counts: %+v", s)
	}
	if s.Load == nil || s.Load.Stations != 9 || s.Load.BusyPct != 25.1 || s.LoadNote != "" {
		t.Errorf("load: %+v %q", s.Load, s.LoadNote)
	}

	// 2.4 GHz: channel 6 overlaps channels 2 to 10, not 1 or 11.
	two := []BSSEntry{
		{BSSID: ownAP, FreqMHz: 2437, RSSI: -50},
		{BSSID: otherAP, FreqMHz: 2412, RSSI: -60},    // 1
		{BSSID: [6]byte{1}, FreqMHz: 2422, RSSI: -60}, // 3
		{BSSID: [6]byte{2}, FreqMHz: 2457, RSSI: -60}, // 10
		{BSSID: [6]byte{3}, FreqMHz: 2462, RSSI: -60}, // 11
		{BSSID: [6]byte{4}, FreqMHz: 2437, RSSI: -80}, // 6
	}
	s = NewWiFiSurvey(vNow, ownAP, 2437, two)
	if s.SameChannel != 1 || s.Overlapping != 2 {
		t.Errorf("2.4 GHz: same %d, overlapping %d", s.SameChannel, s.Overlapping)
	}
	if s.Load != nil || !strings.Contains(s.LoadNote, "doesn't report its load") {
		t.Errorf("no BSS Load element: %+v %q", s.Load, s.LoadNote)
	}

	if s := NewWiFiSurvey(vNow, ownAP, 5180, entries[1:]); s.Load != nil || !strings.Contains(s.LoadNote, "wasn't in the adapter's last scan") {
		t.Errorf("own AP missing: %q", s.LoadNote)
	}
	if s := NewWiFiSurvey(vNow, [6]byte{}, 0, entries); s.Load != nil || s.SameChannel != 0 || !strings.Contains(s.LoadNote, "couldn't tell") {
		t.Errorf("unknown AP: %+v", s)
	}
}

// realtimeReply builds a WLAN_REALTIME_CONNECTION_QUALITY in the 64-bit
// SDK layout.
type rtLink struct {
	freq, width uint32
	rssi        int32
}

func realtimeReply(phy, quality, rx, tx uint32, mlo bool, links ...rtLink) []byte {
	b := make([]byte, sizeRealtimeHeader+len(links)*sizeRealtimeLink)
	put := func(off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
	put(0, phy)
	put(4, quality)
	put(8, rx)
	put(12, tx)
	if mlo {
		put(16, 1)
	}
	put(20, uint32(len(links)))
	for i, l := range links {
		o := sizeRealtimeHeader + i*sizeRealtimeLink
		b[o] = byte(i)
		put(o+4, l.freq)
		put(o+8, l.width)
		put(o+12, uint32(l.rssi))
	}
	return b
}

func TestDecodeRealtimeQuality(t *testing.T) {
	var l WiFiLink
	if err := decodeRealtimeQuality(realtimeReply(10, 78, 866700, 1201000, false, rtLink{5180000, 80, -61}), &l); err != nil {
		t.Fatal(err)
	}
	if *l.RSSI != -61 || l.Band != "5" || *l.Channel != 36 || *l.FreqMHz != 5180 || *l.WidthMHz != 80 || *l.Quality != 78 ||
		*l.TxMbps != 1201 || *l.RxMbps != 866.7 || l.Standard != "802.11ax" || l.Generation != "Wi-Fi 6" || l.MLO || len(l.Links) != 1 {
		t.Errorf("single link: %+v", l)
	}

	// Wi-Fi 7 multi-link: the strongest link is the one judged.
	l = WiFiLink{}
	reply := realtimeReply(11, 90, 2400000, 2882000, true, rtLink{5500, 160, -70}, rtLink{6115, 320, -55})
	if err := decodeRealtimeQuality(reply, &l); err != nil {
		t.Fatal(err)
	}
	if !l.MLO || len(l.Links) != 2 || l.Band != "6" || *l.Channel != 33 || *l.RSSI != -55 || *l.WidthMHz != 320 || l.Generation != "Wi-Fi 7" {
		t.Errorf("MLO: %+v", l)
	}

	// A link count larger than the reply is cut to what is there; no
	// link at all leaves the radio fields unknown.
	l = WiFiLink{}
	bad := realtimeReply(7, 50, 0, 0, false, rtLink{2437, 20, -66})
	binary.LittleEndian.PutUint32(bad[20:], 99)
	if err := decodeRealtimeQuality(bad, &l); err != nil || len(l.Links) != 1 || l.Band != "2.4" || l.TxMbps != nil {
		t.Errorf("overlong count: %+v %v", l, err)
	}
	l = WiFiLink{}
	if err := decodeRealtimeQuality(realtimeReply(10, 101, 0, 0, false), &l); err != nil || l.RSSI != nil || l.Band != "" || l.Quality != nil {
		t.Errorf("no links: %+v %v", l, err)
	}
	if err := decodeRealtimeQuality(make([]byte, 10), &l); !errors.Is(err, errShortWLAN) {
		t.Errorf("short reply: %v", err)
	}
}

func TestDecodeConnAttributes(t *testing.T) {
	b := make([]byte, sizeConnAttributes)
	binary.LittleEndian.PutUint32(b[0:], 1)
	copy(b[524:], "home-network") // SSID: read past, never kept
	copy(b[560:], ownAP[:])
	binary.LittleEndian.PutUint32(b[568:], 8)
	binary.LittleEndian.PutUint32(b[576:], 64)
	binary.LittleEndian.PutUint32(b[580:], 433300)
	binary.LittleEndian.PutUint32(b[584:], 585000)
	a, err := decodeConnAttributes(b)
	if err != nil || a.State != 1 || a.BSSID != ownAP || a.PHY != 8 || a.Quality != 64 || a.RxRate != 433300 || a.TxRate != 585000 {
		t.Errorf("%+v %v", a, err)
	}
	if _, err := decodeConnAttributes(b[:500]); !errors.Is(err, errShortWLAN) {
		t.Errorf("short: %v", err)
	}
}

// bssList builds a WLAN_BSS_LIST whose elements follow the entries, as
// Windows lays them out.
func bssList(entries []BSSEntry) []byte {
	head := 8 + len(entries)*sizeBSSEntry
	b := make([]byte, head)
	for i, e := range entries {
		o := 8 + i*sizeBSSEntry
		copy(b[o+40:], e.BSSID[:])
		binary.LittleEndian.PutUint32(b[o+56:], uint32(int32(e.RSSI)))
		binary.LittleEndian.PutUint32(b[o+92:], uint32(e.FreqMHz*1000))
		binary.LittleEndian.PutUint32(b[o+352:], uint32(len(b)-o))
		binary.LittleEndian.PutUint32(b[o+356:], uint32(len(e.IEs)))
		b = append(b, e.IEs...)
	}
	binary.LittleEndian.PutUint32(b[0:], uint32(len(b)))
	binary.LittleEndian.PutUint32(b[4:], uint32(len(entries)))
	return b
}

func TestDecodeBSSList(t *testing.T) {
	in := []BSSEntry{
		{BSSID: ownAP, FreqMHz: 5180, RSSI: -55, IEs: bssLoadIE(3, 26, 100)},
		{BSSID: otherAP, FreqMHz: 2437, RSSI: -81},
	}
	out, err := decodeBSSList(bssList(in))
	if err != nil || len(out) != 2 {
		t.Fatalf("%v %v", out, err)
	}
	if out[0].BSSID != ownAP || out[0].FreqMHz != 5180 || out[0].RSSI != -55 || len(out[0].IEs) != 7 || out[1].IEs != nil || out[1].RSSI != -81 {
		t.Errorf("%+v", out)
	}
	s := NewWiFiSurvey(vNow, ownAP, 5180, out)
	if s.Load == nil || s.Load.Stations != 3 {
		t.Errorf("survey from the list: %+v", s)
	}

	// Hostile replies: more items than bytes, elements past the end.
	raw := bssList(in)
	binary.LittleEndian.PutUint32(raw[4:], 1000)
	if _, err := decodeBSSList(raw); !errors.Is(err, errShortWLAN) {
		t.Errorf("item count past the end: %v", err)
	}
	raw = bssList(in)
	binary.LittleEndian.PutUint32(raw[8+352:], 1<<30)
	if out, err := decodeBSSList(raw); err != nil || out[0].IEs != nil {
		t.Errorf("element offset past the end: %v %v", out, err)
	}
	if _, err := decodeBSSList([]byte{1, 2}); !errors.Is(err, errShortWLAN) {
		t.Errorf("tiny reply: %v", err)
	}
}

func TestDecodeInterfaceList(t *testing.T) {
	b := make([]byte, 8+2*sizeInterfaceInfo)
	binary.LittleEndian.PutUint32(b, 2)
	for i, desc := range []string{"Example Wi-Fi 6E Adapter", "USB Wi-Fi"} {
		o := 8 + i*sizeInterfaceInfo
		b[o] = byte(i + 1)
		for j, c := range utf16.Encode([]rune(desc)) {
			binary.LittleEndian.PutUint16(b[o+16+2*j:], c)
		}
		binary.LittleEndian.PutUint32(b[o+528:], uint32(i*3+1))
	}
	list, err := decodeInterfaceList(b)
	if err != nil || len(list) != 2 || list[0].Description != "Example Wi-Fi 6E Adapter" || list[0].State != 1 || list[1].GUID[0] != 2 || list[1].State != 4 {
		t.Errorf("%+v %v", list, err)
	}
	binary.LittleEndian.PutUint32(b, 3)
	if _, err := decodeInterfaceList(b); !errors.Is(err, errShortWLAN) {
		t.Errorf("count past the end: %v", err)
	}
}

const procWireless = `Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE
 face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22
wlp2s0: 0000   54.  -56.  -256        0      0      0      0     73        0
 wlan1: 0000   40.  200.  -256        0      0      0      0      0        0
 wlan2: 0000    0.    0.     0        0      0      0      0      0        0
`

func TestParseProcWireless(t *testing.T) {
	rows := parseProcWireless(procWireless)
	if len(rows) != 3 || rows[0].Name != "wlp2s0" || *rows[0].Level != -56 || *rows[1].Level != -56 || rows[2].Level != nil {
		t.Fatalf("%+v", rows)
	}
}

func procRoot(t *testing.T, wireless string, oper map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if wireless != "" {
		os.MkdirAll(filepath.Join(root, "proc", "net"), 0o755)
		os.WriteFile(filepath.Join(root, "proc", "net", "wireless"), []byte(wireless), 0o644)
	}
	for ifc, st := range oper {
		d := filepath.Join(root, "sys", "class", "net", ifc)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "operstate"), []byte(st+"\n"), 0o644)
	}
	return root
}

func TestProcWLAN(t *testing.T) {
	names := func(i int) (string, error) {
		return map[int]string{3: "wlp2s0", 4: "eth0", 5: "wlan2"}[i], nil
	}
	root := procRoot(t, procWireless, map[string]string{"wlp2s0": "up", "wlan2": "dormant"})
	w := newProcWLAN(root, names)
	l, err := w.Link(3)
	if err != nil || l.Interface != "wlp2s0" || l.RSSI == nil || *l.RSSI != -56 || l.Band != "" || len(l.Notes) == 0 {
		t.Errorf("Link(3) = %+v, %v", l, err)
	}
	if l, err := w.Link(0); err != nil || l.Interface != "wlp2s0" {
		t.Errorf("Link(0) = %+v, %v", l, err)
	}
	if _, err := w.Link(4); !errors.Is(err, ErrNoWiFi) {
		t.Errorf("Ethernet: %v", err)
	}
	if _, err := w.Link(5); !errors.Is(err, ErrWiFiDisconnected) {
		t.Errorf("not up: %v", err)
	}
	if _, err := newProcWLAN(procRoot(t, "", nil), names).Link(3); !errors.Is(err, ErrNoWiFi) {
		t.Errorf("no /proc/net/wireless: %v", err)
	}
	if _, err := newProcWLAN(procRoot(t, procWireless[:150], nil), names).Link(0); !errors.Is(err, ErrNoWiFi) {
		t.Errorf("header only: %v", err)
	}
	if _, err := w.Survey(3); !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, ErrUnsupported) {
		t.Errorf("Survey: %v", err)
	}
	if _, err := w.Tune(3); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Tune: %v", err)
	}
}

func TestUnsupportedWLAN(t *testing.T) {
	var w WLAN = unsupportedWLAN{}
	_, e1 := w.Link(0)
	_, e2 := w.Survey(0)
	_, e3 := w.Tune(0)
	for _, err := range []error{e1, e2, e3} {
		if !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, ErrUnsupported) || errors.Is(err, ErrNoWiFi) {
			t.Errorf("%v", err)
		}
	}
}

func TestVerdictWiFiSignalReading(t *testing.T) {
	l := wifiLink()
	// A busy channel alone makes the radio link the suspect.
	busy := WiFiSignal{RSSI: ptr(-58), Band: "5", BusyPct: ptr(72.0)}
	d := diagnose(Evidence{Link: l, WiFi: &busy, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 4, 6)})
	wantVerdict(t, d, VerdictWiFi, ConfidenceHigh)
	if f := strings.Join(d.Verdict.Facts, "\n"); !strings.Contains(f, "-58 dBm on 5 GHz, channel 72% busy (weak or crowded)") {
		t.Errorf("facts: %s", f)
	}
	quiet := WiFiSignal{RSSI: ptr(-58), Band: "5", BusyPct: ptr(20.0)}
	d = diagnose(Evidence{Link: l, WiFi: &quiet, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 4, 6)})
	wantVerdict(t, d, VerdictHome, ConfidenceMedium)

	// A clean router on Wi-Fi still shows the signal under the check.
	d = diagnose(Evidence{Link: l, WiFi: &WiFiSignal{RSSI: ptr(-77), Band: "5"}, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 0, 3)})
	c := check(t, d, ruleHome)
	if c.Status != CheckClear || !strings.Contains(strings.Join(c.Facts, "\n"), "-77 dBm on 5 GHz (weak or crowded)") {
		t.Errorf("clear check: %+v", c)
	}
	// Not on a cable.
	d = diagnose(Evidence{Link: cableLink(), WiFi: &WiFiSignal{RSSI: ptr(-77)}, Bloat: bloatOn(cableLink(), 5*time.Minute, "A", 50, 0, 3)})
	if strings.Contains(strings.Join(check(t, d, ruleHome).Facts, "\n"), "Wi-Fi signal") {
		t.Error("Wi-Fi signal shown on a cable")
	}
}

func TestDiagnosisReportWiFiLine(t *testing.T) {
	l := wifiLink()
	sig := WiFiLink{RSSI: ptr(-61), Band: "5", Channel: ptr(36), WidthMHz: ptr(80), Generation: "Wi-Fi 6", TxMbps: ptr(1201.0)}.Signal()
	sig.BusyPct = ptr(12.0)
	ev := Evidence{At: vNow, Link: l, WiFi: &sig, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 0, 3)}
	txt := DiagnosisReport{Diagnosis: diagnose(ev), Evidence: ev}.Text()
	if !strings.Contains(txt, "Wi-Fi: -61 dBm, 5 GHz channel 36 (80 MHz), Wi-Fi 6, 1201 Mbit/s Tx, channel 12% busy.\n") {
		t.Errorf("report:\n%s", txt)
	}
	ev.Link = cableLink()
	if txt := (DiagnosisReport{Diagnosis: diagnose(ev), Evidence: ev}).Text(); strings.Contains(txt, "\nWi-Fi:") {
		t.Errorf("Wi-Fi line on a cable:\n%s", txt)
	}
}

func TestFileWLAN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wifi.json")
	f := NewFileWLAN(path)
	if _, err := f.Link(0); !errors.Is(err, ErrNoWiFi) {
		t.Errorf("missing file: %v", err)
	}
	os.WriteFile(path, []byte(`{"link":{"rssi_dbm":-61,"band":"5"},"survey_error":"location","tuning":{"scan_paused":true,"streaming":true}}`), 0o644)
	if l, err := f.Link(0); err != nil || *l.RSSI != -61 || l.Band != "5" {
		t.Errorf("Link: %+v %v", l, err)
	}
	if _, err := f.Survey(0); !errors.Is(err, ErrLocationDenied) {
		t.Errorf("Survey: %v", err)
	}
	tu, err := f.Tune(0)
	if err != nil || !tu.State().ScanPaused || f.Held() != 1 {
		t.Fatalf("Tune: %v", err)
	}
	tu.Close()
	tu.Close()
	if f.Held() != 0 || tu.State().ScanPaused {
		t.Error("Close didn't revert once")
	}
	os.WriteFile(path, []byte(`{"link_error":"disconnected"}`), 0o644)
	if _, err := f.Link(0); !errors.Is(err, ErrWiFiDisconnected) {
		t.Errorf("disconnected: %v", err)
	}
	os.WriteFile(path, []byte(`{`), 0o644)
	if _, err := f.Link(0); err == nil {
		t.Error("broken file read")
	}
}
