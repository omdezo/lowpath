package diag

import "encoding/binary"

// Operation elements: how wide a scanned access point's channel is. A scan
// entry gives only the primary 20 MHz channel; the access point's beacon
// says the rest in the operation element of each Wi-Fi generation it
// speaks, each newer one able to describe wider channels:
//
//   - HT Operation (element 61, Wi-Fi 4): 40 MHz, with the secondary
//     channel above or below the primary;
//   - VHT Operation (element 192, Wi-Fi 5, 5 GHz): 80, 160 and 80+80 MHz;
//   - HE Operation (element 255, extension 36, Wi-Fi 6): on 6 GHz its 6 GHz
//     Operation Information, up to 160 MHz (there is no HT or VHT there);
//   - EHT Operation (element 255, extension 106, Wi-Fi 7): up to 320 MHz.
//
// BSSSpan reads them from the newest down and takes the first that gives a
// channel holding the primary one. The layouts are 802.11-2020 9.4.2.56
// (HT), 9.4.2.158 (VHT), 802.11ax 9.4.2.249 (HE) and 802.11be 9.4.2.311
// (EHT). Elements come from the air: every length is checked, and anything
// malformed, reserved or inconsistent with the primary channel is skipped,
// so the entry falls back to the next element or to its primary 20 MHz.

// Element IDs.
const (
	elementHTOperation  = 61
	elementVHTOperation = 192
	elementExtension    = 255
	extHEOperation      = 36
	extEHTOperation     = 106
)

// Fixed lengths of the parts read.
const (
	htOperationLen  = 22 // primary channel, HT Operation Information (5), Basic HT-MCS Set (16)
	vhtOperationLen = 5  // VHT Operation Information (3), Basic VHT-MCS And NSS Set (2)
	vhtInfoLen      = 3  // Channel Width, CCFS0, CCFS1
	heFixedLen      = 6  // HE Operation Parameters (3), BSS Color Information, Basic HE-MCS And NSS Set (2)
	he6GHzInfoLen   = 5  // Primary Channel, Control, CCFS0, CCFS1, Minimum Rate
	ehtFixedLen     = 5  // EHT Operation Parameters, Basic EHT-MCS And NSS Set (4)
	ehtInfoLen      = 3  // Control, CCFS0, CCFS1 (a Disabled Subchannel Bitmap may follow)
)

// operationElements are the bodies of a frame's operation elements (nil:
// not there); an extension element's body starts after its extension ID.
type operationElements struct {
	ht, vht, he, eht []byte
}

// findOperationElements walks a frame's information elements. Elements
// wholly inside ies are kept; a truncated one ends the walk. The first of
// each kind counts.
func findOperationElements(ies []byte) operationElements {
	var o operationElements
	for len(ies) >= 2 {
		id, n := ies[0], int(ies[1])
		if len(ies) < 2+n {
			break
		}
		body := ies[2 : 2+n : 2+n]
		ies = ies[2+n:]
		switch {
		case id == elementHTOperation && o.ht == nil:
			o.ht = body
		case id == elementVHTOperation && o.vht == nil:
			o.vht = body
		case id == elementExtension && n >= 1 && body[0] == extHEOperation && o.he == nil:
			o.he = body[1:]
		case id == elementExtension && n >= 1 && body[0] == extEHTOperation && o.eht == nil:
			o.eht = body[1:]
		}
	}
	return o
}

// BSSSpan is the channel an access point heard on primaryMHz operates on,
// read from its information elements. Without a usable operation element
// it is the primary 20 MHz channel; the zero span when primaryMHz isn't a
// Wi-Fi channel.
func BSSSpan(primaryMHz int, ies []byte) ChannelSpan {
	ch, band := ChannelOf(primaryMHz)
	primary := primarySpan(primaryMHz)
	if band == "" || !primary.Valid() {
		return ChannelSpan{}
	}
	o := findOperationElements(ies)
	vhtInfo, he6 := o.heFields()
	at := func(ccfs byte, w int) ChannelSpan { return centeredSpan(channelMHz(band, primaryMHz, int(ccfs)), w) }
	for _, s := range []ChannelSpan{
		ehtSpan(o.eht, band, at),
		he6GHzSpan(he6, band, ch, at),
		vhtSpan(o.vht, vhtOperationLen, band, o.htCCFS2(), at),
		vhtSpan(vhtInfo, vhtInfoLen, band, o.htCCFS2(), at),
		htSpan(o.ht, ch, primaryMHz),
	} {
		if s.contains(primary) {
			return s
		}
	}
	return primary
}

// channelMHz is the center frequency of channel ch on band, numbered as on
// the primary's band (4.9 GHz channels count from 4000 MHz); 0 for none.
func channelMHz(band string, primaryMHz, ch int) int {
	if ch <= 0 {
		return 0
	}
	switch band {
	case "2.4":
		if ch == 14 {
			return 2484
		}
		return 2407 + 5*ch
	case "5":
		if primaryMHz < 5000 {
			return 4000 + 5*ch
		}
		return 5000 + 5*ch
	case "6":
		if ch == 2 {
			return 5935
		}
		return 5950 + 5*ch
	}
	return 0
}

// htSpan reads HT Operation: the primary channel, then HT Operation
// Information, whose first octet has the Secondary Channel Offset (bits
// 0-1: 1 above, 3 below) and STA Channel Width (bit 2: 0 means 20 MHz
// only). An element for another primary channel is ignored.
func htSpan(body []byte, ch, primaryMHz int) ChannelSpan {
	if len(body) < htOperationLen || int(body[0]) != ch || body[1]&0x04 == 0 {
		return ChannelSpan{}
	}
	switch body[1] & 0x03 {
	case 1:
		return ChannelSpan{primaryMHz - 10, primaryMHz + 30}
	case 3:
		return ChannelSpan{primaryMHz - 30, primaryMHz + 10}
	}
	return ChannelSpan{}
}

// htCCFS2 is HT Operation's Channel Center Frequency Segment 2 (bits 13-20
// of HT Operation Information, so bits 5-12 of the element's octets 2-3):
// how an access point whose clients can't all do 160 MHz announces its
// 160 MHz channel (802.11-2020 Table 9-274); 0 when absent.
func (o operationElements) htCCFS2() byte {
	if len(o.ht) < htOperationLen {
		return 0
	}
	return byte(binary.LittleEndian.Uint16(o.ht[2:4]) >> 5)
}

// vhtSpan reads VHT Operation Information (the VHT Operation element, or
// the same three octets inside HE Operation; 5 GHz only): Channel Width
// 0 leaves the width to HT; 1 is 80 MHz centered on CCFS0, or 160 MHz
// centered on CCFS1 when that is 8 channels from CCFS0 (on CCFS2 from HT
// when CCFS1 is 0), or 80+80 MHz when they are further apart; 2 and 3 are
// the deprecated 160 and 80+80 MHz signalling, centered on CCFS0. For
// 80+80 MHz only the segment with the primary channel (segment 0) counts:
// the span model has one stretch, and undercounting is the honest side.
func vhtSpan(info []byte, minLen int, band string, ccfs2 byte, at func(byte, int) ChannelSpan) ChannelSpan {
	if len(info) < minLen || band != "5" {
		return ChannelSpan{}
	}
	c0, c1 := info[1], info[2]
	switch info[0] {
	case 1:
		if c1 == 0 {
			c1 = ccfs2
		}
		switch d := absInt(int(c1) - int(c0)); {
		case c1 == 0:
			return at(c0, 80)
		case d == 8:
			return at(c1, 160)
		case d > 16:
			return at(c0, 80)
		}
	case 2:
		return at(c0, 160)
	case 3:
		return at(c0, 80)
	}
	return ChannelSpan{}
}

// heFields finds the optional parts of HE Operation that follow its fixed
// six octets, in this order, each present when its bit in HE Operation
// Parameters is set: VHT Operation Information (bit 14, 3 octets), Max
// Co-Hosted BSSID Indicator (bit 15, 1 octet) and 6 GHz Operation
// Information (bit 17, 5 octets). nil for a part that isn't there or
// doesn't fit.
func (o operationElements) heFields() (vhtInfo, sixGHz []byte) {
	b := o.he
	if len(b) < heFixedLen {
		return nil, nil
	}
	params := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
	b = b[heFixedLen:]
	if params&(1<<14) != 0 {
		if len(b) < vhtInfoLen {
			return nil, nil
		}
		vhtInfo, b = b[:vhtInfoLen], b[vhtInfoLen:]
	}
	if params&(1<<15) != 0 {
		if len(b) < 1 {
			return vhtInfo, nil
		}
		b = b[1:]
	}
	if params&(1<<17) != 0 && len(b) >= he6GHzInfoLen {
		sixGHz = b[:he6GHzInfoLen]
	}
	return vhtInfo, sixGHz
}

// he6GHzSpan reads HE Operation's 6 GHz Operation Information: Primary
// Channel, Control (Channel Width in bits 0-1: 20, 40, 80, 160 or 80+80
// MHz), CCFS0 and CCFS1, which follow the VHT rules: 40 and 80 MHz center
// on CCFS0; for 160 MHz CCFS1 is the center and CCFS0 that of the primary
// 80 MHz, 8 channels away; further apart is 80+80, of which the primary's
// segment counts. An element for another primary channel is ignored.
func he6GHzSpan(info []byte, band string, ch int, at func(byte, int) ChannelSpan) ChannelSpan {
	if len(info) < he6GHzInfoLen || band != "6" || int(info[0]) != ch {
		return ChannelSpan{}
	}
	c0, c1 := info[2], info[3]
	switch info[1] & 0x03 {
	case 1:
		return at(c0, 40)
	case 2:
		return at(c0, 80)
	case 3:
		switch d := absInt(int(c1) - int(c0)); {
		case c1 != 0 && d == 8:
			return at(c1, 160)
		case c1 != 0 && d > 16:
			return at(c0, 80)
		}
	}
	return ChannelSpan{}
}

// ehtSpan reads EHT Operation: when bit 0 of EHT Operation Parameters says
// EHT Operation Information is there (after the 4-octet Basic EHT-MCS And
// NSS Set), its Control octet has the Channel Width (bits 0-2: 20, 40, 80,
// 160 or 320 MHz); 40 and 80 MHz center on CCFS0, 160 and 320 MHz on CCFS1
// (CCFS0 is then the center of the primary 80 or 160 MHz). 320 MHz exists
// only on 6 GHz. Without the information the width is the HE, VHT or HT
// one. A punctured subchannel (Disabled Subchannel Bitmap) still counts as
// part of the channel: the span is its outline.
func ehtSpan(body []byte, band string, at func(byte, int) ChannelSpan) ChannelSpan {
	if len(body) < ehtFixedLen+ehtInfoLen || body[0]&0x01 == 0 {
		return ChannelSpan{}
	}
	info := body[ehtFixedLen:]
	c0, c1 := info[1], info[2]
	switch info[0] & 0x07 {
	case 1:
		return at(c0, 40)
	case 2:
		return at(c0, 80)
	case 3:
		return at(c1, 160)
	case 4:
		if band == "6" {
			return at(c1, 320)
		}
	}
	return ChannelSpan{}
}
