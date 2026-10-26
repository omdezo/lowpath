package diag

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// Wi-Fi link readings and tuning. A Wi-Fi link loses and delays packets in
// ways a cable doesn't: a weak or crowded radio channel, a slow PHY rate,
// and the adapter's periodic background scans, which leave the channel for
// tens to hundreds of milliseconds to look for other access points. WLAN
// reads the link and can pause those scans while a game is boosted.
//
// The implementation is per OS (the Native Wifi API on Windows,
// /proc/net/wireless on Linux); a part the OS doesn't offer reports an
// error matching errors.ErrUnsupported, and the UI shows "—" with the
// reason instead of a number.

// WLAN errors. Each says why a reading is missing, so the UI can show it
// next to the "—".
var (
	// ErrNoWiFi: the PC has no Wi-Fi adapter, or the system's Wi-Fi
	// service doesn't run.
	ErrNoWiFi = errors.New("diag: no Wi-Fi adapter")
	// ErrWiFiDisconnected: the adapter isn't connected to a network.
	ErrWiFiDisconnected = errors.New("diag: Wi-Fi isn't connected")
	// ErrLocationDenied: the system lets only apps with location access
	// read the access point's details (Windows 11 24H2 and later).
	ErrLocationDenied = errors.New("diag: Wi-Fi details need location access")
)

// wifiUnsupported matches both ErrUnsupported and errors.ErrUnsupported:
// diag's callers test the first, the platform layer's the second.
type wifiUnsupported struct{ what string }

func (e wifiUnsupported) Error() string { return "diag: " + e.what + " isn't available on this system" }
func (e wifiUnsupported) Is(target error) bool {
	return target == ErrUnsupported || target == errors.ErrUnsupported
}

// WLAN reads the Wi-Fi link and tunes the adapter for games. index is the
// network interface index of the default route (Route.Index); 0 means the
// first connected Wi-Fi adapter. Calls talk to the system's Wi-Fi service
// and may block while it is busy, so callers run them off the UI's path.
type WLAN interface {
	// Link reads the radio link. It never needs location access and never
	// triggers a scan.
	Link(index int) (WiFiLink, error)
	// Survey reads the networks the adapter heard in its last scan: the
	// access point's own BSS Load report and the other networks on its
	// channel. It doesn't scan either. On Windows 11 24H2 and later it
	// needs location access (ErrLocationDenied), and the first call may
	// show Windows' location prompt, so call it only when the user asks.
	Survey(index int) (WiFiSurvey, error)
	// Tune pauses the adapter's background scans and turns on its media
	// streaming mode until the returned tuning is closed. The system
	// undoes both on its own when Lowpath exits or the Wi-Fi disconnects.
	Tune(index int) (WiFiTuning, error)
}

// WiFiTuning is a running Tune.
type WiFiTuning interface {
	// State is what is applied now.
	State() WiFiTuningState
	// Refresh applies the tuning again where the system dropped it (it does
	// after a reconnect) and returns the new state.
	Refresh() WiFiTuningState
	// Close reverts both settings. It is safe to call more than once.
	Close() error
}

// WiFiTuningState says what a tuning holds. A setting that couldn't be
// applied has a reason in Notes.
type WiFiTuningState struct {
	ScanPaused bool     `json:"scan_paused"`
	Streaming  bool     `json:"streaming"`
	Notes      []string `json:"notes,omitempty"`
}

// WiFiLink is one reading of the Wi-Fi link. Nil fields and "" are what the
// system didn't report; Notes says why where that is known.
type WiFiLink struct {
	At        time.Time `json:"at"`
	Interface string    `json:"interface"` // the adapter's name ("Wi-Fi")
	Adapter   string    `json:"adapter"`   // its description (the chip and driver)
	// RSSI is the received signal strength, in dBm. Quality is the
	// system's 0-100 signal quality, linear from -100 dBm (0) to -50 dBm
	// (100) on Windows.
	RSSI    *int `json:"rssi_dbm"`
	Quality *int `json:"quality_pct"`
	// Band is "2.4", "5" or "6" (GHz); Channel and FreqMHz the channel the
	// link is on; WidthMHz the channel width (20 to 320 MHz).
	Band     string `json:"band"`
	Channel  *int   `json:"channel"`
	FreqMHz  *int   `json:"freq_mhz"`
	WidthMHz *int   `json:"width_mhz"`
	// Standard is the PHY ("802.11ax"), Generation its Wi-Fi Alliance name
	// ("Wi-Fi 6", "Wi-Fi 6E" on 6 GHz).
	Standard   string `json:"standard"`
	Generation string `json:"generation"`
	// TxMbps and RxMbps are the PHY rates the adapter uses now: an upper
	// bound for the link, not a throughput measurement.
	TxMbps *float64 `json:"tx_mbps"`
	RxMbps *float64 `json:"rx_mbps"`
	// MLO: a Wi-Fi 7 multi-link connection; Links lists every radio link
	// (one entry on ordinary connections).
	MLO   bool        `json:"mlo"`
	Links []RadioLink `json:"links,omitempty"`
	// Source names the system API the reading came from, for the report.
	Source string   `json:"source"`
	Notes  []string `json:"notes,omitempty"`
}

// RadioLink is one radio link of a connection.
type RadioLink struct {
	Band     string `json:"band"`
	Channel  *int   `json:"channel"`
	FreqMHz  int    `json:"freq_mhz"`
	WidthMHz *int   `json:"width_mhz"`
	RSSI     *int   `json:"rssi_dbm"`
}

// Signal is the part of the reading the verdict engine uses.
func (l WiFiLink) Signal() WiFiSignal {
	return WiFiSignal{RSSI: l.RSSI, Band: l.Band, Channel: l.Channel, WidthMHz: l.WidthMHz, Generation: l.Generation, TxMbps: l.TxMbps}
}

// WiFiSurvey is what the adapter's last scan says about the channel.
type WiFiSurvey struct {
	At time.Time `json:"at"`
	// Load is the access point's own BSS Load report; nil when it doesn't
	// send one (it is optional, and many home routers leave it out) or it
	// wasn't in the scan. LoadNote says which.
	Load     *BSSLoad `json:"load"`
	LoadNote string   `json:"load_note,omitempty"`
	// Networks is every access point heard (each band of a router counts);
	// SameChannel the other ones on the link's channel, which share its
	// airtime; Overlapping the 2.4 GHz ones close enough to overlap it.
	Networks    int `json:"networks"`
	SameChannel int `json:"same_channel"`
	Overlapping int `json:"overlapping"`
	// LoudestNeighbor is the strongest other network on the same channel,
	// in dBm (nil: none heard).
	LoudestNeighbor *int `json:"loudest_neighbor_dbm"`
	FreqMHz         int  `json:"freq_mhz"` // the link's channel the counts are for
}

// BSSLoad is the BSS Load element (802.11 element 11) an access point puts
// in its beacons: how many devices are associated and how busy the channel
// is by the access point's own measurement.
type BSSLoad struct {
	Stations int `json:"stations"`
	// BusyPct is the share of time the access point sensed the channel busy
	// (its 0-255 channel utilization scaled to percent).
	BusyPct float64 `json:"busy_pct"`
	// AdmissionCapacity is the remaining time for admission-controlled
	// traffic, in units of 32 µs per second (nil: not sent).
	AdmissionCapacity *int `json:"admission_capacity,omitempty"`
}

// BSSEntry is one access point from a scan. The BSSID only matches the
// connected access point inside diag; it is never put in JSON or reports,
// because a BSSID can locate a home.
type BSSEntry struct {
	BSSID   [6]byte
	FreqMHz int
	RSSI    int
	IEs     []byte // the information elements of its beacon or probe response
}

// elementBSSLoad is the BSS Load element's ID.
const elementBSSLoad = 11

// ParseBSSLoad finds the BSS Load element in a frame's information
// elements. A length-5 element is the 802.11 layout (station count 2
// octets, channel utilization 1, available admission capacity 2); ok is
// false when there is none or the elements are malformed.
func ParseBSSLoad(ies []byte) (BSSLoad, bool) {
	for len(ies) >= 2 {
		id, n := ies[0], int(ies[1])
		if len(ies) < 2+n {
			return BSSLoad{}, false
		}
		body := ies[2 : 2+n]
		ies = ies[2+n:]
		if id != elementBSSLoad || n < 3 {
			continue
		}
		l := BSSLoad{Stations: int(binary.LittleEndian.Uint16(body)), BusyPct: math.Round(float64(body[2])*1000/255) / 10}
		if n >= 5 {
			c := int(binary.LittleEndian.Uint16(body[3:]))
			l.AdmissionCapacity = &c
		}
		return l, true
	}
	return BSSLoad{}, false
}

// NewWiFiSurvey sums up a scan for the access point own on freqMHz (own
// zero: unknown, so no BSS Load is looked up).
func NewWiFiSurvey(at time.Time, own [6]byte, freqMHz int, entries []BSSEntry) WiFiSurvey {
	s := WiFiSurvey{At: at, FreqMHz: freqMHz, Networks: len(entries)}
	var zero [6]byte
	found := false
	ownCh, ownBand := ChannelOf(freqMHz)
	for _, e := range entries {
		if own != zero && e.BSSID == own {
			found = true
			if l, ok := ParseBSSLoad(e.IEs); ok {
				s.Load = &l
			}
			continue
		}
		ch, band := ChannelOf(e.FreqMHz)
		switch {
		case freqMHz > 0 && absInt(e.FreqMHz-freqMHz) <= 2:
			s.SameChannel++
			if s.LoudestNeighbor == nil || e.RSSI > *s.LoudestNeighbor {
				r := e.RSSI
				s.LoudestNeighbor = &r
			}
		case band == "2.4" && ownBand == "2.4" && ch > 0 && ownCh > 0 && absInt(ch-ownCh) < 5:
			// 20 MHz channels 5 MHz apart: up to 4 channels away overlap.
			s.Overlapping++
		}
	}
	switch {
	case own == zero:
		s.LoadNote = "Lowpath couldn't tell which access point you're connected to."
	case !found:
		s.LoadNote = "Your access point wasn't in the adapter's last scan."
	case s.Load == nil:
		s.LoadNote = "Your access point doesn't report its load (it's optional, and many home routers leave it out)."
	}
	return s
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ChannelOf is the channel number and band ("2.4", "5", "6") of a center
// frequency in MHz; 0 and "" for anything else.
func ChannelOf(mhz int) (int, string) {
	switch {
	case mhz == 2484:
		return 14, "2.4"
	case mhz >= 2412 && mhz <= 2472:
		return (mhz - 2407) / 5, "2.4"
	case mhz == 5935:
		return 2, "6"
	case mhz >= 5955 && mhz <= 7115:
		return (mhz - 5950) / 5, "6"
	case mhz >= 4910 && mhz <= 5925:
		return (mhz - 5000) / 5, "5"
	}
	return 0, ""
}

// phyStandard names a Windows DOT11_PHY_TYPE (the 802.11 PHY) and its Wi-Fi
// generation; 6 GHz turns Wi-Fi 6 into Wi-Fi 6E.
func phyStandard(phy uint32, band string) (standard, generation string) {
	switch phy {
	case 2:
		return "802.11 (DSSS)", ""
	case 4:
		return "802.11a", ""
	case 5:
		return "802.11b", ""
	case 6:
		return "802.11g", ""
	case 7:
		return "802.11n", "Wi-Fi 4"
	case 8:
		return "802.11ac", "Wi-Fi 5"
	case 9:
		return "802.11ad", ""
	case 10:
		if band == "6" {
			return "802.11ax", "Wi-Fi 6E"
		}
		return "802.11ax", "Wi-Fi 6"
	case 11:
		return "802.11be", "Wi-Fi 7"
	}
	return "", ""
}

// rateMbps converts a Native Wifi PHY rate to Mbit/s. The API documents
// kbit/s; no real Wi-Fi rate is below 1 Mbit/s, so a smaller non-zero value
// can only already be in Mbit/s. Zero or absurd values are unknown.
func rateMbps(v uint32) *float64 {
	var r float64
	switch {
	case v == 0:
		return nil
	case v < 1000:
		r = float64(v)
	default:
		r = float64(v) / 1000
	}
	if r > 100000 {
		return nil
	}
	r = math.Round(r*10) / 10
	return &r
}

// freqMHz converts a Native Wifi center frequency to MHz: documented as
// kHz in some places and MHz in others, so the size decides.
func freqMHz(v uint32) int {
	if v >= 100000 {
		return int((v + 500) / 1000)
	}
	return int(v)
}

// widthMHz accepts the channel widths Wi-Fi has (in MHz, or kHz).
func widthMHz(v uint32) *int {
	if v >= 1000 {
		v /= 1000
	}
	switch v {
	case 20, 40, 80, 160, 320:
		w := int(v)
		return &w
	}
	return nil
}

// qualityRSSI is the dBm a Windows signal quality stands for: 0 is -100
// dBm, 100 is -50 dBm, linear in between.
func qualityRSSI(q int) int { return -100 + q/2 }

// validRSSI keeps dBm values a radio can report.
func validRSSI(v int32) *int {
	if v >= 0 || v < -120 {
		return nil
	}
	r := int(v)
	return &r
}

// fillChannel sets band, channel and frequency from a center frequency.
func (l *WiFiLink) fillChannel(mhz int) {
	ch, band := ChannelOf(mhz)
	if band == "" {
		return
	}
	l.Band, l.FreqMHz = band, &mhz
	l.Channel = &ch
}

func (l *WiFiLink) note(format string, a ...any) {
	l.Notes = append(l.Notes, fmt.Sprintf(format, a...))
}

// unsupportedWLAN is for systems with no Wi-Fi API in Lowpath yet.
type unsupportedWLAN struct{}

func (unsupportedWLAN) Link(int) (WiFiLink, error) {
	return WiFiLink{}, wifiUnsupported{"reading the Wi-Fi signal"}
}
func (unsupportedWLAN) Survey(int) (WiFiSurvey, error) {
	return WiFiSurvey{}, wifiUnsupported{"reading the Wi-Fi channel"}
}
func (unsupportedWLAN) Tune(int) (WiFiTuning, error) {
	return nil, wifiUnsupported{"pausing Wi-Fi scans"}
}

// NewWLAN returns the system's Wi-Fi reader.
func NewWLAN() WLAN { return newPlatformWLAN() }
