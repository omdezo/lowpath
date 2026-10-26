package diag

import (
	"encoding/binary"
	"errors"
	"unicode/utf16"
)

// Decoders for the Native Wifi (wlanapi.h) structures Windows returns. They
// work on bytes copied out of the API's buffers, with every offset from the
// 64-bit SDK layout written out, so the layouts are tested on every OS and
// a short or corrupt buffer is an error instead of a read past its end.

var errShortWLAN = errors.New("diag: Native Wifi reply too short")

// Native Wifi constants.
const (
	wlanInterfaceStateConnected = 1 // WLAN_INTERFACE_STATE wlan_interface_state_connected

	sizeInterfaceInfo  = 532 // WLAN_INTERFACE_INFO: GUID, WCHAR[256], state
	sizeRealtimeHeader = 24  // WLAN_REALTIME_CONNECTION_QUALITY up to linksInfo
	sizeRealtimeLink   = 272 // WLAN_REALTIME_CONNECTION_QUALITY_LINK_INFO
	sizeConnAttributes = 604 // WLAN_CONNECTION_ATTRIBUTES
	sizeBSSEntry       = 360 // WLAN_BSS_ENTRY
)

// wlanInterface is one WLAN_INTERFACE_INFO.
type wlanInterface struct {
	GUID        [16]byte
	Description string
	State       uint32
}

func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

// decodeInterfaceList reads a WLAN_INTERFACE_INFO_LIST: dwNumberOfItems,
// dwIndex, then the entries from offset 8.
func decodeInterfaceList(b []byte) ([]wlanInterface, error) {
	if len(b) < 8 {
		return nil, errShortWLAN
	}
	n := int(le32(b, 0))
	if n < 0 || n > (len(b)-8)/sizeInterfaceInfo {
		return nil, errShortWLAN
	}
	out := make([]wlanInterface, 0, n)
	for i := 0; i < n; i++ {
		e := b[8+i*sizeInterfaceInfo:]
		var it wlanInterface
		copy(it.GUID[:], e[:16])
		it.Description = utf16z(e[16:528])
		it.State = le32(e, 528)
		out = append(out, it)
	}
	return out, nil
}

// utf16z decodes a NUL-terminated little-endian UTF-16 string.
func utf16z(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// decodeRealtimeQuality reads a WLAN_REALTIME_CONNECTION_QUALITY into l:
// dot11PhyType@0, ulLinkQuality@4, ulRxRate@8, ulTxRate@12,
// bIsMLOConnection@16, ulNumLinks@20, linksInfo@24 (272 bytes each:
// ucLinkID@0, ulChannelCenterFrequencyMhz@4, ulBandwidth@8, lRssi@12,
// wlanRateSet@16).
func decodeRealtimeQuality(b []byte, l *WiFiLink) error {
	if len(b) < sizeRealtimeHeader {
		return errShortWLAN
	}
	phy := le32(b, 0)
	if q := int(le32(b, 4)); q <= 100 {
		l.Quality = &q
	}
	l.RxMbps, l.TxMbps = rateMbps(le32(b, 8)), rateMbps(le32(b, 12))
	l.MLO = le32(b, 16) != 0
	n := int(le32(b, 20))
	if n < 0 || n > (len(b)-sizeRealtimeHeader)/sizeRealtimeLink {
		n = (len(b) - sizeRealtimeHeader) / sizeRealtimeLink
	}
	best := -1
	for i := 0; i < n; i++ {
		e := b[sizeRealtimeHeader+i*sizeRealtimeLink:]
		mhz := freqMHz(le32(e, 4))
		ch, band := ChannelOf(mhz)
		rl := RadioLink{Band: band, FreqMHz: mhz, WidthMHz: widthMHz(le32(e, 8)), RSSI: validRSSI(int32(le32(e, 12)))}
		if band != "" {
			rl.Channel = &ch
		}
		l.Links = append(l.Links, rl)
		if best < 0 || (rl.RSSI != nil && (l.Links[best].RSSI == nil || *rl.RSSI > *l.Links[best].RSSI)) {
			best = i
		}
	}
	// The link the verdict judges is the strongest one: on a multi-link
	// connection traffic favours it.
	if best >= 0 {
		bl := l.Links[best]
		l.RSSI, l.WidthMHz = bl.RSSI, bl.WidthMHz
		l.fillChannel(bl.FreqMHz)
	}
	l.Standard, l.Generation = phyStandard(phy, l.Band)
	return nil
}

// connAttributes is what Lowpath uses from WLAN_CONNECTION_ATTRIBUTES. The
// SSID and profile name are skipped on purpose: they can name the home.
type connAttributes struct {
	State   uint32
	BSSID   [6]byte
	PHY     uint32
	Quality uint32
	RxRate  uint32
	TxRate  uint32
}

// decodeConnAttributes reads a WLAN_CONNECTION_ATTRIBUTES: isState@0,
// then WLAN_ASSOCIATION_ATTRIBUTES at 520 (after the 256-WCHAR profile
// name): dot11Ssid (36 bytes), dot11BssType@556, dot11Bssid@560,
// dot11PhyType@568, uDot11PhyIndex@572, wlanSignalQuality@576,
// ulRxRate@580, ulTxRate@584.
func decodeConnAttributes(b []byte) (connAttributes, error) {
	if len(b) < 588 {
		return connAttributes{}, errShortWLAN
	}
	a := connAttributes{State: le32(b, 0), PHY: le32(b, 568), Quality: le32(b, 576), RxRate: le32(b, 580), TxRate: le32(b, 584)}
	copy(a.BSSID[:], b[560:566])
	return a, nil
}

// decodeBSSList reads a WLAN_BSS_LIST: dwTotalSize@0, dwNumberOfItems@4,
// entries from 8, each a 360-byte WLAN_BSS_ENTRY with dot11Bssid@40,
// lRssi@56, ulChCenterFrequency@92 (kHz), ulIeOffset@352 and ulIeSize@356
// (the elements' place relative to the entry).
func decodeBSSList(b []byte) ([]BSSEntry, error) {
	if len(b) < 8 {
		return nil, errShortWLAN
	}
	if total := int(le32(b, 0)); total >= 8 && total < len(b) {
		b = b[:total]
	}
	n := int(le32(b, 4))
	if n < 0 || n > (len(b)-8)/sizeBSSEntry {
		return nil, errShortWLAN
	}
	out := make([]BSSEntry, 0, n)
	for i := 0; i < n; i++ {
		start := 8 + i*sizeBSSEntry
		e := b[start:]
		var be BSSEntry
		copy(be.BSSID[:], e[40:46])
		be.RSSI = int(int32(le32(e, 56)))
		be.FreqMHz = freqMHz(le32(e, 92))
		off, size := int(le32(e, 352)), int(le32(e, 356))
		if off >= sizeBSSEntry && size > 0 && start+off+size <= len(b) && start+off+size > start {
			be.IEs = b[start+off : start+off+size]
		}
		out = append(out, be)
	}
	return out, nil
}
