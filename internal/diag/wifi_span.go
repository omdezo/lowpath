package diag

// Channel spans. A Wi-Fi network transmits on a primary 20 MHz channel and,
// when it is wider, on the secondary channels next to it: an 80 MHz network
// on channel 36 also uses 40, 44 and 48. Two networks share airtime when
// their spans intersect, not only when their primaries match, so the survey
// compares spans.

// ChannelSpan is the stretch of spectrum a channel occupies, from its lower
// to its upper edge in MHz. The zero span is unknown.
type ChannelSpan struct {
	LowMHz, HighMHz int
}

// WidthMHz is the span's width.
func (s ChannelSpan) WidthMHz() int { return s.HighMHz - s.LowMHz }

// Valid says whether the span is a real stretch of spectrum.
func (s ChannelSpan) Valid() bool { return s.LowMHz > 0 && s.HighMHz > s.LowMHz }

// Overlaps says whether two spans share spectrum. Channels that only touch
// (36 ends at 5190 MHz where 40 begins) don't: 5 and 6 GHz channels are
// laid out edge to edge without interfering.
func (s ChannelSpan) Overlaps(o ChannelSpan) bool {
	return s.Valid() && o.Valid() && s.LowMHz < o.HighMHz && o.LowMHz < s.HighMHz
}

// contains says whether o lies wholly inside s.
func (s ChannelSpan) contains(o ChannelSpan) bool {
	return s.Valid() && o.Valid() && s.LowMHz <= o.LowMHz && o.HighMHz <= s.HighMHz
}

// centeredSpan is the w MHz channel centered on mhz.
func centeredSpan(mhz, w int) ChannelSpan {
	if mhz <= 0 || w <= 0 {
		return ChannelSpan{}
	}
	return ChannelSpan{mhz - w/2, mhz + w/2}
}

// primarySpan is the 20 MHz channel around a primary frequency: all a scan
// entry is known to use when its operation elements say nothing more.
func primarySpan(mhz int) ChannelSpan { return centeredSpan(mhz, 20) }

// reach is the spectrum a span's transmissions disturb: on 2.4 GHz, 1 MHz
// more on each side. 2.4 GHz channels sit only 5 MHz apart and their
// transmit masks spill past the nominal 20 MHz (802.11b's is 22 MHz wide),
// so channels up to four apart (20 MHz, as 1 and 5) still interfere, while
// 1 and 6 don't. 5 and 6 GHz channels are laid out edge to edge, so their
// spans stay as they are.
func (s ChannelSpan) reach() ChannelSpan {
	if s.Valid() && s.HighMHz <= 2500 { // the 2.4 GHz band ends at 2500 MHz
		s.LowMHz--
		s.HighMHz++
	}
	return s
}

// channelPlan is one stretch of the 5 or 6 GHz band whose wide channels are
// aligned blocks counted from origin: 40, 80 and 160 MHz channels never
// straddle a block edge.
type channelPlan struct{ origin, end int }

// channelPlans are the standard channelizations (802.11-2020 Annex E, and
// 802.11be for 6 GHz): on 5 GHz, blocks from the lower edge of channel 36
// (5170 MHz) up to channel 144 (5730), so 80 MHz is 36-48, 52-64, 100-112,
// 116-128, 132-144 and 160 MHz is 36-64, 100-128; from channel 149 (5735)
// up to 177 (5895) a second run, so 80 MHz is 149-161, 165-177 and 160 MHz
// 149-177. On 6 GHz, blocks from the lower edge of channel 1 (5945) to
// channel 233 (7125). The 4.9 GHz channels and 2.4 GHz have no such blocks
// (a 2.4 GHz 40 MHz channel may extend up or down; only the access point's
// elements tell which).
var channelPlans = []channelPlan{{5170, 5730}, {5735, 5895}, {5945, 7125}}

// channelBlock is the standard w MHz channel (40, 80 or 160) that contains
// mhz, a primary channel or the center of the whole channel (both lie
// inside it). ok is false where the band has no such channel. A 320 MHz
// channel isn't derived: 802.11be lays out two sets of them on 6 GHz that
// overlap by half (320-1 from channel 1, 320-2 from channel 33), and only
// the access point's EHT Operation element tells which one it uses; the
// caller falls back to the 160 MHz block, which both contain.
func channelBlock(mhz, w int) (ChannelSpan, bool) {
	switch w {
	case 40, 80, 160:
	default:
		return ChannelSpan{}, false
	}
	for _, p := range channelPlans {
		if mhz <= p.origin || mhz >= p.end {
			continue
		}
		low := p.origin + (mhz-p.origin)/w*w
		if low+w > p.end {
			return ChannelSpan{}, false
		}
		return ChannelSpan{low, low + w}, true
	}
	return ChannelSpan{}, false
}

// linkSpan is the channel the link occupies. mhz is its primary channel or
// the center of its whole channel (the system may report either), widthMHz
// the width the adapter reports (nil: unknown), and apSpan what the
// connected access point's own operation elements say (the zero span:
// not heard). In order:
//
//   - the access point's span when the link has its width (or the width is
//     unknown): the access point's own word, which also tells a 2.4 GHz
//     40 MHz channel's direction and the 320 MHz set on 6 GHz;
//   - the standard block of the link's width that holds mhz (a link
//     narrower than its access point, or one not in the scan);
//   - 320 MHz without the access point's word: the 160 MHz block that holds
//     mhz, which both 320 MHz sets contain;
//   - otherwise only the 20 MHz around mhz, which undercounts a wide link
//     rather than counting networks it may not share airtime with.
func linkSpan(mhz int, widthMHz *int, apSpan ChannelSpan) ChannelSpan {
	if mhz <= 0 {
		return ChannelSpan{}
	}
	// The access point's span counts only if it holds the link's channel:
	// a stale or foreign element mustn't move the link.
	apFits := apSpan.contains(ChannelSpan{mhz - 1, mhz + 1})
	w := 0
	if widthMHz != nil {
		w = *widthMHz
	}
	switch {
	case apFits && (w == 0 || w == apSpan.WidthMHz()):
		return apSpan
	case w > 20:
		if s, ok := channelBlock(mhz, w); ok {
			return s
		}
		if w == 320 {
			if s, ok := channelBlock(mhz, 160); ok {
				return s
			}
		}
	}
	return primarySpan(mhz)
}
